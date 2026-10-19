package logging

import (
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

const redacted = "***redacted***"

// Scope references must survive both field-count and byte-size compaction.
// Scoped readers authorize the complete observation, including its message.
var correlationFields = strings.Fields("time timestamp level message module component operation operation_label plugin_id request_id user_id job_id task_id related_job_id related_task_id follow_id follow_run_id scan_run_id attempt_number generation event_id event_code event_version stage outcome library_id source_library_id target_library_id site_id site_ids library_ids downloader_id connection_id source_connection_id target_connection_id storage_id source_storage_id target_storage_id resource_scope_incomplete event_truncated status duration_ms")

var (
	sensitiveKey    = regexp.MustCompile(`(?i)(authorization|cookie|password|passwd|secret|api[_-]?key|passkey|(^|[_-])token$|access[_-]?token|refresh[_-]?token|jwt|signature|(^|[_-])sig$)`)
	windowsPath     = regexp.MustCompile(`(?i)^[a-z]:[\\/]`)
	uncPath         = regexp.MustCompile(`^\\\\`)
	secretText      = regexp.MustCompile(`(?i)(bearer\s+)[a-z0-9._~+/=-]+`)
	cookieText      = regexp.MustCompile(`(?i)\bcookie\s*[:=]\s*[^\r\n]+`)
	secretPair      = regexp.MustCompile(`(?i)\b(authorization|cookie|password|passwd|secret|api[_-]?key|passkey|token|access[_-]?token|refresh[_-]?token|jwt|sig|signature)\s*[:=]\s*[^\s,;]+`)
	windowsPathText = regexp.MustCompile(`(?i)(?:[a-z]:[\\/]|\\\\)[^\s"']+`)
	unixPrivatePath = regexp.MustCompile(`(?:/home/|/Users/|/var/|/tmp/|/opt/|/srv/|/mnt/)[^\s"']+`)
)

func SanitizeJSON(line []byte) []byte {
	var event map[string]any
	if err := json.Unmarshal(line, &event); err != nil {
		return []byte(`{"level":"warn","message":"【运行日志】日志格式无效，已忽略该条记录","module":"logging","component":"sanitizer"}` + "\n")
	}
	clean := sanitizeMap(event, 0)
	encoded, err := json.Marshal(clean)
	if err != nil {
		return []byte(`{"level":"warn","message":"【运行日志】日志内容无法编码，已忽略该条记录","module":"logging","component":"sanitizer"}` + "\n")
	}
	if len(encoded) > maxEventBytes {
		clean = compactOversizedEvent(clean)
		encoded, _ = json.Marshal(clean)
	}
	return append(encoded, '\n')
}

func compactOversizedEvent(event map[string]any) map[string]any {
	compact := map[string]any{"event_truncated": true}
	scopeIncomplete, _ := event["resource_scope_incomplete"].(bool)
	for _, key := range correlationFields {
		if key == "resource_scope_incomplete" {
			continue
		}
		if value, ok := event[key]; ok {
			switch typed := value.(type) {
			case string:
				limit := 128
				if key == "message" {
					limit = 2048
				}
				if runes := []rune(typed); len(runes) > limit {
					typed = string(runes[:limit]) + "…[truncated]"
				}
				compact[key] = typed
			case []any:
				if key == "site_ids" || key == "library_ids" {
					fields := BusinessFields(map[string]any{key: typed})
					compact[key] = fields[key]
					if incomplete, _ := fields["resource_scope_incomplete"].(bool); incomplete {
						scopeIncomplete = true
					}
				} else {
					scopeIncomplete = true
				}
			case map[string]any:
				scopeIncomplete = true
			default:
				compact[key] = value
			}
		}
	}
	if scopeIncomplete {
		compact["resource_scope_incomplete"] = true
	}
	compact["event_truncated"] = true
	return compact
}

func sanitizeMap(input map[string]any, depth int) map[string]any {
	out := make(map[string]any, min(len(input), 64))
	count := 0
	// Preserve correlation before arbitrary diagnostics hit the field bound.
	if depth == 0 {
		for _, key := range correlationFields {
			if value, ok := input[key]; ok {
				out[key] = sanitizeValue(key, value, depth+1)
				count++
			}
		}
	}
	for key, value := range input {
		if count >= 64 {
			out["event_truncated"] = true
			break
		}
		if _, exists := out[key]; exists {
			continue
		}
		if sensitiveKey.MatchString(key) {
			out[key] = redacted
		} else {
			out[key] = sanitizeValue(key, value, depth+1)
		}
		count++
	}
	return out
}

func sanitizeValue(key string, value any, depth int) any {
	if depth > 6 {
		return "[truncated]"
	}
	switch typed := value.(type) {
	case string:
		return sanitizeString(key, typed)
	case map[string]any:
		return sanitizeMap(typed, depth)
	case []any:
		limit := 64
		if key == "site_ids" || key == "library_ids" {
			limit = 100
		}
		if len(typed) > limit {
			typed = typed[:limit]
		}
		out := make([]any, len(typed))
		for i := range typed {
			out[i] = sanitizeValue(key, typed[i], depth+1)
		}
		return out
	default:
		return value
	}
}

func sanitizeString(key, value string) string {
	value = secretText.ReplaceAllString(value, `${1}`+redacted)
	isURL := false
	if parsed, err := url.Parse(value); err == nil && parsed.Scheme != "" && parsed.Host != "" {
		isURL = true
		if parsed.User != nil {
			parsed.User = url.User(redacted)
		}
		query := parsed.Query()
		for name := range query {
			if sensitiveKey.MatchString(name) || strings.EqualFold(name, "exp") {
				query.Set(name, redacted)
			}
		}
		parsed.RawQuery = query.Encode()
		value = parsed.String()
	}
	if !isURL {
		value = cookieText.ReplaceAllString(value, "Cookie="+redacted)
		value = secretPair.ReplaceAllStringFunc(value, func(pair string) string {
			separator := strings.IndexAny(pair, ":=")
			if separator < 0 {
				return redacted
			}
			return pair[:separator+1] + redacted
		})
		value = windowsPathText.ReplaceAllString(value, "[local-path-redacted]")
		value = unixPrivatePath.ReplaceAllString(value, "[local-path-redacted]")
	}
	lowerKey := strings.ToLower(key)
	if strings.Contains(lowerKey, "path") && (windowsPath.MatchString(value) || uncPath.MatchString(value) || strings.HasPrefix(value, "/")) {
		return "[local-path-redacted]"
	}
	if utf8.RuneCountInString(value) > 4096 {
		value = string([]rune(value)[:4096]) + "…[truncated]"
	}
	return value
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
