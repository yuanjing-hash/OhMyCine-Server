package logging

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"path"
	"regexp"
	"strings"
	"unicode"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
)

// BusinessEvent is a host-owned observation, never an instruction or a success
// receipt. Fields not in the explicit display/correlation allowlist are dropped.
type BusinessEvent struct {
	Code    string
	Stage   string
	Outcome string
	Message string
	Fields  map[string]any
}

type businessContextKey struct{}

var businessOrigin = uuid.NewString()
var businessCode = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,95}$`)
var businessErrorCode = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]{0,95}$`)
var businessURL = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^\s]+|\b(?:magnet|ed2k):[^\s]+`)
var businessAbsolutePath = regexp.MustCompile(`(^|[\s"'=：:（(])/[^\s"']+`)

var businessNameFields = fieldSet("title work_title resource_name media_display_name file_name old_name new_name source_name target_name site_name downloader_name library_name connection_name storage_name query_name episode_range")
var businessIDFields = fieldSet("job_id task_id related_job_id related_task_id follow_id follow_run_id request_id downloader_id")
var businessNumberFields = fieldSet("user_id attempt_number generation library_id source_library_id target_library_id scan_run_id site_id connection_id storage_id source_connection_id target_connection_id source_storage_id target_storage_id duration_ms size_bytes result_count raw_results deduplicated_count filtered_count selected_count created_count reused_count accepted_count rejected_count uncertain_count skipped_count failed_count success_count total_count completed_count processed_count omitted_count sample_count searched_sites_count planned_sites_count searched_names_count request_count candidates accepted_submissions missing_count present_count active_count matched_count unrecognized_count cache_hit_count enumerated_count batch_count queued_count published_count generated_count deleted_count uploaded_count conflict_count moved_count copied_count renamed_count retained_count bytes_processed identity_kept rejected chosen created reused failed_sites alias_requests omitted season episode_count updated_count")
var businessCodeFields = fieldSet("reason error_code status mode action conflict_decision trigger job_type source_type media_type phase transfer_mode outcome_code artifact_type")
var businessPathFields = fieldSet("source_relative_path target_relative_path relative_path")

func fieldSet(value string) map[string]bool {
	result := map[string]bool{}
	for _, key := range strings.Fields(value) {
		result[key] = true
	}
	return result
}

// BusinessDisplayName admits a bounded human display value, never a URL/root.
func BusinessDisplayName(value string) string {
	value = strings.Join(strings.FieldsFunc(value, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }), " ")
	if value == "" || windowsPath.MatchString(value) || uncPath.MatchString(value) || businessAbsolutePath.MatchString(value) || strings.Contains(value, "://") || strings.HasPrefix(strings.ToLower(value), "magnet:") {
		return "未命名对象"
	}
	value = sanitizeString("display_name", value)
	if runes := []rune(value); len(runes) > 160 {
		value = string(runes[:160]) + "…"
	}
	return value
}

// BusinessRelativePath is a narrow exception for already root-validated display
// locations. It cannot turn an absolute/provider path into a display location.
func BusinessRelativePath(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len([]rune(value)) > 768 || strings.Contains(value, ":") || strings.Contains(value, "\\") || strings.HasPrefix(value, "/") || strings.ContainsAny(value, "\x00\r\n\t") || secretPair.MatchString(value) || strings.Contains(value, "?") || strings.Contains(value, "#") {
		return ""
	}
	for _, part := range strings.Split(value, "/") {
		if part == ".." || part == "." || part == "" {
			return ""
		}
	}
	if path.Clean(value) != value {
		return ""
	}
	return value
}

// WithBusinessFields copies only safe typed fields. Queue identity cannot be
// overridden by a domain callback inheriting an existing execution context.
func WithBusinessFields(ctx context.Context, fields map[string]any) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	clean := BusinessFields(fields)
	if inherited, ok := ctx.Value(businessContextKey{}).(map[string]any); ok {
		for key, value := range inherited {
			if _, exists := clean[key]; !exists || key == "job_id" || key == "user_id" || key == "attempt_number" || key == "generation" {
				clean[key] = value
			}
		}
	}
	return context.WithValue(ctx, businessContextKey{}, clean)
}

func RecordBusiness(ctx context.Context, logger zerolog.Logger, operation Operation, observation BusinessEvent) {
	if !businessCode.MatchString(observation.Code) || !businessCode.MatchString(observation.Stage) {
		return
	}
	switch observation.Outcome {
	case "planned", "started", "accepted", "confirmed", "skipped", "waiting", "failed", "cancelled", "finished", "partial", "retry", "reused", "uncertain":
	default:
		return
	}
	fields := BusinessFields(observation.Fields)
	if ctx != nil {
		if inherited, ok := ctx.Value(businessContextKey{}).(map[string]any); ok {
			for key, value := range inherited {
				if _, exists := fields[key]; !exists || key == "job_id" || key == "user_id" || key == "attempt_number" || key == "generation" {
					fields[key] = value
				}
			}
		}
	}
	var event *zerolog.Event
	switch observation.Outcome {
	case "failed":
		event = logger.Error()
	case "partial", "waiting", "retry", "uncertain":
		event = logger.Warn()
	default:
		event = logger.Info()
	}
	// UUIDv7 preserves observed emission order for equal-resolution timestamps.
	operation.Event(event).Fields(fields).Str("business_origin", businessOrigin).Int("event_version", 1).Str("event_id", uuid.Must(uuid.NewV7()).String()).Str("event_code", observation.Code).Str("stage", observation.Stage).Str("outcome", observation.Outcome).Msg(operation.Message(BusinessMessage(observation.Message)))
}

// BusinessMessage also protects durable domain summaries projected beside logs.
func BusinessMessage(value string) string {
	message := strings.Join(strings.FieldsFunc(value, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }), " ")
	message = businessURL.ReplaceAllString(message, "[链接已隐藏]")
	message = businessAbsolutePath.ReplaceAllString(message, `${1}[local-path-redacted]`)
	message = sanitizeString("message", message)
	if runes := []rune(message); len(runes) > 2048 {
		message = string(runes[:2048]) + "…[truncated]"
	}
	return message
}

// BusinessFields is shared by the scoped read projection; it deliberately
// rejects arbitrary nested values and raw diagnostic fields.
func BusinessFields(input map[string]any) map[string]any {
	out := map[string]any{}
	for key, value := range input {
		switch {
		case businessNameFields[key]:
			if text, ok := value.(string); ok {
				out[key] = BusinessDisplayName(text)
			}
		case businessIDFields[key]:
			if text, ok := value.(string); ok && len(text) <= 128 && text != "" && !strings.ContainsAny(text, " /\\:?&#\r\n\t\x00") {
				out[key] = text
			}
		case businessNumberFields[key]:
			if number, ok := businessNumber(value); ok && (!strings.HasSuffix(key, "_id") || number > 0) {
				out[key] = number
			}
		case key == "error_code":
			if text, ok := value.(string); ok && businessErrorCode.MatchString(text) {
				out[key] = text
			}
		case businessCodeFields[key]:
			if text, ok := value.(string); ok && businessCode.MatchString(text) {
				out[key] = text
			}
		case businessPathFields[key]:
			if text, ok := value.(string); ok {
				if safe := BusinessRelativePath(text); safe != "" {
					out[key] = safe
				}
			}
		case key == "filter_summary" || key == "reason_counts":
			counts := map[string]any{}
			switch typed := value.(type) {
			case map[string]int:
				if len(typed) > 48 {
					out["event_truncated"] = true
				}
				for reason, count := range typed {
					if businessCode.MatchString(reason) && count >= 0 && len(counts) < 48 {
						counts[reason] = count
					}
				}
			case map[string]any:
				if len(typed) > 48 {
					out["event_truncated"] = true
				}
				for reason, count := range typed {
					if number, ok := businessNumber(count); ok && businessCode.MatchString(reason) && len(counts) < 48 {
						counts[reason] = number
					}
				}
			}
			out[key] = counts
		case key == "samples":
			samples := []string{}
			switch typed := value.(type) {
			case []string:
				if len(typed) > 20 {
					out["event_truncated"] = true
				}
				for _, item := range typed {
					if len(samples) < 20 {
						samples = append(samples, BusinessDisplayName(item))
					}
				}
			case []any:
				if len(typed) > 20 {
					out["event_truncated"] = true
				}
				for _, item := range typed {
					if text, ok := item.(string); ok && len(samples) < 20 {
						samples = append(samples, BusinessDisplayName(text))
					}
				}
			}
			out[key] = samples
		case key == "site_ids" || key == "library_ids":
			ids := []any{}
			switch typed := value.(type) {
			case []uint:
				if len(typed) > 100 {
					out["resource_scope_incomplete"] = true
				}
				for _, id := range typed {
					if id > 0 && len(ids) < 100 {
						ids = append(ids, id)
					} else if id == 0 {
						out["resource_scope_incomplete"] = true
					}
				}
			case []any:
				if len(typed) > 100 {
					out["resource_scope_incomplete"] = true
				}
				for _, id := range typed {
					if n, ok := businessNumber(id); ok && n > 0 && math.Trunc(n) == n && len(ids) < 100 {
						ids = append(ids, n)
					} else {
						out["resource_scope_incomplete"] = true
					}
				}
			default:
				out["resource_scope_incomplete"] = true
			}
			out[key] = ids
		case key == "resource_scope_incomplete":
			if incomplete, _ := value.(bool); incomplete {
				out[key] = true
			}
		}
	}
	return out
}

func businessNumber(value any) (float64, bool) {
	var n float64
	switch v := value.(type) {
	case int:
		n = float64(v)
	case int64:
		n = float64(v)
	case uint:
		n = float64(v)
	case uint64:
		n = float64(v)
	case float64:
		n = v
	case json.Number:
		parsed, err := v.Float64()
		if err != nil {
			return 0, false
		}
		n = parsed
	default:
		return 0, false
	}
	return n, !math.IsNaN(n) && !math.IsInf(n, 0) && n >= 0 && n <= 9007199254740991
}

func normalizeBusinessOrigin(line []byte) []byte {
	var event map[string]any
	if json.Unmarshal(line, &event) != nil {
		return line
	}
	proof, _ := event["business_origin"].(string)
	delete(event, "business_origin")
	plugin, _ := event["plugin_id"].(string)
	if proof != businessOrigin || plugin != "" {
		for _, key := range []string{"event_id", "event_version", "event_code", "stage", "outcome"} {
			delete(event, key)
		}
	}
	data, err := json.Marshal(event)
	if err != nil {
		return []byte(fmt.Sprintf(`{"level":"warn","message":%q,"module":"logging","component":"sanitizer"}`, "业务日志格式无效"))
	}
	return data
}
