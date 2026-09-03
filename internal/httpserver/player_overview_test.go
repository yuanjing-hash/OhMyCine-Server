package httpserver

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestPlayerOverviewUsesDeviceBearerAndAdvertisedVersionedCapabilities(t *testing.T) {
	client := newTestClient(t)
	status, _, _ := client.playerRequest(t, http.MethodGet, "/api/v1/player/overview", "", nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("anonymous overview status=%d", status)
	}
	client.setup(t)
	status, loginEnvelope, _ := client.playerRequest(t, http.MethodPost, "/api/v1/player/auth/login", "", map[string]any{
		"username": "owner", "password": "strong-owner-password",
		"device_id": "overview-device", "device_name": "Overview Player",
	})
	var login struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(loginEnvelope.Data, &login); status != http.StatusOK || err != nil || login.AccessToken == "" {
		t.Fatalf("login status=%d err=%v data=%s", status, err, loginEnvelope.Data)
	}

	status, bootstrapEnvelope, _ := client.playerRequest(t, http.MethodGet, "/api/v1/player/bootstrap", login.AccessToken, nil)
	var bootstrap struct {
		Capabilities []string `json:"capabilities"`
	}
	if err := json.Unmarshal(bootstrapEnvelope.Data, &bootstrap); status != http.StatusOK || err != nil {
		t.Fatalf("bootstrap status=%d err=%v data=%s", status, err, bootstrapEnvelope.Data)
	}
	for _, capability := range []string{"canonical_playback_history_v1", "persistent_category_artwork_v1", "media_overview_v1"} {
		if !containsString(bootstrap.Capabilities, capability) {
			t.Fatalf("missing capability %q in %v", capability, bootstrap.Capabilities)
		}
	}

	status, overviewEnvelope, headers := client.playerRequest(t, http.MethodGet, "/api/v1/player/overview", login.AccessToken, nil)
	if status != http.StatusOK || headers.Get("Cache-Control") != "no-store" {
		t.Fatalf("overview status=%d cache=%q data=%s", status, headers.Get("Cache-Control"), overviewEnvelope.Data)
	}
	var overview struct {
		Version  string `json:"version"`
		Sections map[string]struct {
			Status  string            `json:"status"`
			List    []json.RawMessage `json:"list"`
			HasMore bool              `json:"has_more"`
		} `json:"sections"`
	}
	if err := json.Unmarshal(overviewEnvelope.Data, &overview); err != nil || overview.Version != "v1" {
		t.Fatalf("invalid overview err=%v data=%s", err, overviewEnvelope.Data)
	}
	wanted := []string{"featured", "continue_watching", "recently_added", "favorites", "automatic_collections", "manual_collections", "recent_history", "media_libraries"}
	if len(overview.Sections) != len(wanted) {
		t.Fatalf("overview sections=%v", overview.Sections)
	}
	for _, key := range wanted {
		section, ok := overview.Sections[key]
		if !ok || section.Status != "ok" || section.List == nil {
			t.Fatalf("section %q invalid: found=%v section=%+v data=%s", key, ok, section, overviewEnvelope.Data)
		}
	}
	for _, forbidden := range []string{"root_path", "relative_root", "provider_id", "credential", "access_token", "user_id"} {
		if jsonContainsKey(overviewEnvelope.Data, forbidden) {
			t.Fatalf("overview leaked %q: %s", forbidden, overviewEnvelope.Data)
		}
	}
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func jsonContainsKey(raw []byte, key string) bool {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	return containsJSONKey(value, key)
}

func containsJSONKey(value any, key string) bool {
	switch typed := value.(type) {
	case map[string]any:
		for current, child := range typed {
			if current == key || containsJSONKey(child, key) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if containsJSONKey(child, key) {
				return true
			}
		}
	}
	return false
}
