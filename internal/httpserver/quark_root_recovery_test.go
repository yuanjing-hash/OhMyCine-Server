package httpserver

import (
	"net/http"
	"strings"
	"testing"
)

func TestQuarkRootRecoveryRoutesRequireSessionCSRFAndPickerIntent(t *testing.T) {
	for _, suffix := range []string{"recovery", "new-attempt"} {
		t.Run(suffix, func(t *testing.T) {
			client := newTestClient(t)
			path := "/api/v1/connections/999/quark/verify-root/" + suffix
			status, _ := client.request(t, http.MethodPost, path, map[string]any{}, true)
			if status != http.StatusUnauthorized || client.lastHeader.Get("Cache-Control") != "no-store" {
				t.Fatalf("anonymous route=%d", status)
			}
			client.setup(t)
			status, _ = client.request(t, http.MethodPost, path, map[string]any{}, false)
			if status != http.StatusForbidden {
				t.Fatalf("missing CSRF=%d", status)
			}
			status, body := client.request(t, http.MethodPost, path, map[string]any{"fid": "private-cloud-target", "path": "/arbitrary", "provider_picker_token": "invalid"}, true)
			if status != http.StatusBadRequest || strings.Contains(body.Message, "private-cloud-target") || client.lastHeader.Get("Cache-Control") != "no-store" {
				t.Fatalf("arbitrary recovery target=%d", status)
			}
			payload := map[string]any{"provider_picker_token": "invalid"}
			if suffix == "new-attempt" {
				payload["recovery_token"] = "invalid"
			}
			status, _ = client.request(t, http.MethodPost, path, payload, true)
			if status != http.StatusBadRequest {
				t.Fatalf("raw picker accepted=%d", status)
			}
			status, _ = client.request(t, http.MethodGet, path, nil, false)
			if status != http.StatusNotFound && status != http.StatusMethodNotAllowed {
				t.Fatalf("GET recovery=%d", status)
			}
		})
	}
}
