package quark

import (
	"context"
	"encoding/json"
	"net/http"
)

type uploadTaskObservation struct {
	FinishStatus *int `json:"finish_status,omitempty"`
	Pages        int  `json:"pages"`
	Complete     bool `json:"complete"`
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
	marker := 0
	seen := map[int]bool{}
	parts := map[int]string{}
	for page := 0; page < 128; page++ {
		body := map[string]any{"task_id": p.TaskID}
		if marker != 0 {
			body["next_part_number_marker"] = marker
		}
		raw, err := d.agentRead(ctx, http.MethodPost, "/open/v1/file/list_uploaded_parts", nil, body)
		if err != nil {
			return result, operationError("upload_task_query", err)
		}
		var out struct {
			FinishStatus *int         `json:"finish_status"`
			ID           string       `json:"fid"`
			Parts        []uploadPart `json:"uploaded_parts"`
			Next         int          `json:"next_part_number_marker"`
		}
		result.Observation.Pages++
		if json.Unmarshal(raw, &out) != nil || out.Next < 0 || out.Next > 10000 || len(out.Parts) > 10000 || out.FinishStatus != nil && (*out.FinishStatus < 0 || *out.FinishStatus > 2) {
			return result, operationError("upload_task_query", failure(CodeInvalid))
		}
		if out.FinishStatus != nil {
			if result.Observation.FinishStatus != nil && *out.FinishStatus < *result.Observation.FinishStatus {
				return result, operationError("upload_task_query", failure(CodeIncomplete))
			}
			result.Observation.FinishStatus = out.FinishStatus
			if *out.FinishStatus == 2 {
				if !scalar(out.ID, 4096) || result.Handle != "" && result.Handle != out.ID {
					return result, operationError("upload_task_query", failure(CodeInvalid))
				}
				result.Handle = out.ID
			}
		}
		for _, part := range out.Parts {
			if part.Number < 1 || part.Number > 10000 || !scalar(part.ETag, 1024) || p.PartSize > 0 && int64(part.Number) > 1+(p.Size-1)/p.PartSize {
				return result, operationError("upload_task_query", failure(CodeInvalid))
			}
			for _, saved := range p.Parts {
				if saved.Number == part.Number && saved.ETag != part.ETag {
					return result, operationError("upload_task_query", failure(CodeIdentity))
				}
			}
			if previous, ok := parts[part.Number]; ok {
				if previous != part.ETag {
					return result, operationError("upload_task_query", failure(CodeIdentity))
				}
				continue
			}
			parts[part.Number] = part.ETag
			result.Parts = append(result.Parts, part)
		}
		if out.Next == 0 {
			result.Observation.Complete = true
			return result, nil
		}
		if seen[out.Next] {
			return result, operationError("upload_task_query", failure(CodeIncomplete))
		}
		seen[out.Next], marker = true, out.Next
	}
	return result, operationError("upload_task_query", failure(CodeIncomplete))
}
