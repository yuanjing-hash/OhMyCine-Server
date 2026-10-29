package httpserver

import (
	"encoding/json"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/services"
	"net/http"
	"testing"
)

func TestFollowPolicyDefaultsRoutesEnforceAuthCSRFAndCAS(t *testing.T) {
	client := newTestClient(t)
	status, _ := client.request(t, http.MethodGet, "/api/v1/settings/follows", nil, false)
	if status != http.StatusUnauthorized {
		t.Fatal(status)
	}
	client.setup(t)
	status, envelope := client.request(t, http.MethodGet, "/api/v1/settings/follows", nil, true)
	var policy services.FollowPolicyDefaultsSummary
	if status != http.StatusOK || json.Unmarshal(envelope.Data, &policy) != nil || policy.Revision != 1 || policy.Subtitles.Mode != "prefer" {
		t.Fatalf("defaults %d %s", status, envelope.Data)
	}
	payload := map[string]any{"revision": 1, "subtitles": map[string]any{"mode": "require", "languages": []string{"zh-CN", "zh-Hans"}}}
	status, _ = client.request(t, http.MethodPatch, "/api/v1/settings/follows", payload, false)
	if status != http.StatusForbidden {
		t.Fatal("missing csrf accepted")
	}
	status, envelope = client.request(t, http.MethodPatch, "/api/v1/settings/follows", payload, true)
	if status != http.StatusOK || json.Unmarshal(envelope.Data, &policy) != nil || policy.Revision != 2 || len(policy.Subtitles.Languages) != 1 || policy.Subtitles.Languages[0] != "zh-Hans" {
		t.Fatalf("updated %d %s", status, envelope.Data)
	}
	status, _ = client.request(t, http.MethodPatch, "/api/v1/settings/follows", payload, true)
	if status != http.StatusConflict {
		t.Fatal("stale CAS accepted")
	}
	if client.lastHeader.Get("Cache-Control") != "no-store" {
		t.Fatal("settings cacheable")
	}
}
