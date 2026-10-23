package quark

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
)

type uploadTaskObservation struct {
	FinishStatus *int   `json:"finish_status,omitempty"`
	Pages        int    `json:"pages"`
	Complete     bool   `json:"complete"`
	Reason       string `json:"reason,omitempty"`
	Field        string `json:"field,omitempty"`
	ValueType    string `json:"value_type,omitempty"`
}

type uploadTaskStatus struct {
	Handle      string
	Parts       []uploadPart
	Observation uploadTaskObservation
}

// The official SDK's syncServerParts reads every original-task page and
// treats finish_status=2 plus fid as completed, even with no remaining parts.
// This never creates a task, sends content, or repeats upload_finish.
func (d *Client) readUploadTask(ctx context.Context, p *uploadPayload) (uploadTaskStatus, error) {
	var result uploadTaskStatus
	p.Observation = &result.Observation
	if !scalar(p.TaskID, 4096) {
		return result, failure(CodeInvalid)
	}
	var marker json.RawMessage
	seen := map[string]bool{}
	parts := map[int]string{}
	for page := 0; page < 128; page++ {
		body := map[string]any{"task_id": p.TaskID}
		if len(marker) != 0 {
			body["next_part_number_marker"] = marker
		}
		raw, err := d.agentRead(ctx, http.MethodPost, "/open/v1/file/list_uploaded_parts", nil, body)
		if err != nil {
			return result, operationError("upload_task_query", err)
		}
		var out struct {
			FinishStatus *int            `json:"finish_status"`
			ID           string          `json:"fid"`
			Parts        []uploadPart    `json:"uploaded_parts"`
			Next         json.RawMessage `json:"next_part_number_marker"`
		}
		result.Observation.Pages++
		if err := json.Unmarshal(raw, &out); err != nil {
			result.Observation.Reason = "response_type_invalid"
			result.Observation.Field, result.Observation.ValueType = uploadTaskDecodeField(err)
			return result, operationError("upload_task_query", failure(CodeInvalid))
		}
		next, key, valid := uploadTaskMarker(out.Next)
		if !valid {
			result.Observation.Reason, result.Observation.Field = "marker_invalid", "next_part_number_marker"
			result.Observation.ValueType = uploadTaskJSONType(out.Next)
			return result, operationError("upload_task_query", failure(CodeInvalid))
		}
		if len(out.Parts) > 10000 {
			result.Observation.Reason, result.Observation.Field = "parts_limit", "uploaded_parts"
			return result, operationError("upload_task_query", failure(CodeInvalid))
		}
		if out.FinishStatus != nil && (*out.FinishStatus < 0 || *out.FinishStatus > 2) {
			result.Observation.Reason, result.Observation.Field = "finish_status_invalid", "finish_status"
			return result, operationError("upload_task_query", failure(CodeInvalid))
		}
		if out.FinishStatus != nil {
			if result.Observation.FinishStatus != nil && *out.FinishStatus < *result.Observation.FinishStatus {
				result.Observation.Reason, result.Observation.Field = "finish_status_regressed", "finish_status"
				return result, operationError("upload_task_query", failure(CodeIncomplete))
			}
			result.Observation.FinishStatus = out.FinishStatus
			if *out.FinishStatus == 2 {
				if !scalar(out.ID, 4096) {
					result.Observation.Reason, result.Observation.Field = "completed_fid_invalid", "fid"
					return result, operationError("upload_task_query", failure(CodeInvalid))
				}
				if result.Handle != "" && result.Handle != out.ID {
					result.Observation.Reason, result.Observation.Field = "completed_fid_changed", "fid"
					return result, operationError("upload_task_query", failure(CodeInvalid))
				}
				result.Handle = out.ID
			}
		}
		for _, part := range out.Parts {
			if part.Number < 1 || part.Number > 10000 || !scalar(part.ETag, 1024) || p.PartSize > 0 && int64(part.Number) > 1+(p.Size-1)/p.PartSize {
				result.Observation.Reason, result.Observation.Field = "part_invalid", "uploaded_parts"
				return result, operationError("upload_task_query", failure(CodeInvalid))
			}
			for _, saved := range p.Parts {
				if saved.Number == part.Number && saved.ETag != part.ETag {
					result.Observation.Reason, result.Observation.Field = "part_changed", "etag"
					return result, operationError("upload_task_query", failure(CodeIdentity))
				}
			}
			if previous, ok := parts[part.Number]; ok {
				if previous != part.ETag {
					result.Observation.Reason, result.Observation.Field = "part_changed", "etag"
					return result, operationError("upload_task_query", failure(CodeIdentity))
				}
				continue
			}
			parts[part.Number] = part.ETag
			result.Parts = append(result.Parts, part)
		}
		if len(next) == 0 {
			result.Observation.Complete = true
			return result, nil
		}
		if seen[key] {
			result.Observation.Reason, result.Observation.Field = "cursor_repeated", "next_part_number_marker"
			return result, operationError("upload_task_query", failure(CodeIncomplete))
		}
		seen[key], marker = true, next
	}
	result.Observation.Reason = "page_limit"
	return result, operationError("upload_task_query", failure(CodeIncomplete))
}

// The SDK forwards the provider marker without numeric coercion. Preserve its
// JSON scalar type, accept the empty-string terminator, and retain finite page,
// token-size and numeric bounds. A marker never establishes write permission.
func uploadTaskMarker(raw json.RawMessage) (json.RawMessage, string, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, "", true
	}
	if raw[0] == '"' {
		var marker string
		if json.Unmarshal(raw, &marker) != nil {
			return nil, "", false
		}
		if marker == "" {
			return nil, "", true
		}
		if !scalar(marker, 128) {
			return nil, "", false
		}
		encoded, _ := json.Marshal(marker)
		return encoded, string(encoded), true
	}
	var marker int
	if json.Unmarshal(raw, &marker) != nil || marker < 0 || marker > 10000 {
		return nil, "", false
	}
	if marker == 0 {
		return nil, "", true
	}
	key := strconv.Itoa(marker)
	return json.RawMessage(key), key, true
}

func uploadTaskJSONType(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return "missing"
	}
	switch raw[0] {
	case '"':
		return "string"
	case '{':
		return "object"
	case '[':
		return "array"
	case 't', 'f':
		return "boolean"
	case 'n':
		return "null"
	default:
		return "number"
	}
}

// Only fixed field/type labels may leave the private response decoder. The
// UnmarshalTypeError text and Value can include the provider's actual content.
func uploadTaskDecodeField(err error) (string, string) {
	field, valueType := "response", "unknown"
	var typed *json.UnmarshalTypeError
	if errors.As(err, &typed) {
		switch typed.Field {
		case "finish_status", "fid", "uploaded_parts", "uploaded_parts.part_number", "uploaded_parts.etag":
			field = typed.Field
		case "part_number", "etag":
			field = "uploaded_parts." + typed.Field
		}
		// Go versions that include array offsets in Field still project to
		// the same fixed label; the offset and original text never escape.
		path := strings.Split(typed.Field, ".")
		if len(path) == 3 && path[0] == "uploaded_parts" && (path[2] == "part_number" || path[2] == "etag") {
			if index, err := strconv.Atoi(path[1]); err == nil && index >= 0 && index < 10000 {
				field = "uploaded_parts." + path[2]
			}
		}
		switch typed.Value {
		case "string", "object", "array", "bool", "boolean":
			valueType = typed.Value
			if valueType == "bool" {
				valueType = "boolean"
			}
		default:
			if strings.HasPrefix(typed.Value, "number") {
				valueType = "number"
			}
		}
	}
	return field, valueType
}
