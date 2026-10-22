package httpserver

import (
	"net/http"
	"strings"
	"testing"
)

func TestQuarkProofCleanupRouteRequiresSessionCSRFAndEmptyIntent(t *testing.T) {
	client := newTestClient(t)
	path := "/api/v1/connections/999/quark/cleanup"
	status, _ := client.request(t, http.MethodPost, path, map[string]any{}, true)
	if status != http.StatusUnauthorized || client.lastHeader.Get("Cache-Control") != "no-store" {
		t.Fatalf("anonymous cleanup status %d", status)
	}
	client.setup(t)
	status, _ = client.request(t, http.MethodPost, path, map[string]any{}, false)
	if status != http.StatusForbidden {
		t.Fatalf("cleanup without CSRF: %d", status)
	}
	status, body := client.request(t, http.MethodPost, path, map[string]any{"fid": "private-cloud-target", "path": "/arbitrary"}, true)
	if status != http.StatusBadRequest || strings.Contains(body.Message, "private-cloud-target") || client.lastHeader.Get("Cache-Control") != "no-store" {
		t.Fatalf("cleanup accepted/echoed an arbitrary target: %d", status)
	}
	status, _ = client.request(t, http.MethodPost, path, map[string]any{}, true)
	if status != http.StatusNotFound {
		t.Fatalf("empty authenticated cleanup was not routed to scoped connection service: %d", status)
	}
	status, _ = client.request(t, http.MethodGet, path, nil, false)
	if status != http.StatusNotFound && status != http.StatusMethodNotAllowed {
		t.Fatalf("GET unexpectedly exposes cleanup: %d", status)
	}
}
