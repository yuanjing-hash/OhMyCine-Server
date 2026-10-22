package httpserver

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestShareAnalysisRoutesAreNoStoreBeforeAndAfterAuthentication(t *testing.T) {
	client := newTestClient(t)
	paths := []string{"/discovery/torrent-results/recognize", "/discovery/share-preview", "/discovery/share-preview/recognize"}
	for _, path := range paths {
		status, _ := client.request(t, http.MethodPost, "/api/v1"+path, map[string]any{}, false)
		if status != http.StatusUnauthorized || client.lastHeader.Get("Cache-Control") != "no-store" {
			t.Fatalf("browser denied path=%s status=%d headers=%v", path, status, client.lastHeader)
		}
		status, _, headers := client.playerRequest(t, http.MethodPost, "/api/v1/player"+path, "", map[string]any{})
		if status != http.StatusUnauthorized || headers.Get("Cache-Control") != "no-store" {
			t.Fatalf("player denied path=%s status=%d headers=%v", path, status, headers)
		}
	}
	client.setup(t)
	status, login, _ := client.playerRequest(t, http.MethodPost, "/api/v1/player/auth/login", "", map[string]any{"username": "owner", "password": "strong-owner-password", "device_id": "share-analysis", "device_name": "Share analysis fixture"})
	var credentials struct {
		AccessToken string `json:"access_token"`
	}
	if status != http.StatusOK || json.Unmarshal(login.Data, &credentials) != nil || credentials.AccessToken == "" {
		t.Fatalf("player login status=%d", status)
	}
	for _, path := range paths {
		status, _ := client.request(t, http.MethodPost, "/api/v1"+path, map[string]any{}, true)
		if status != http.StatusGone || client.lastHeader.Get("Cache-Control") != "no-store" {
			t.Fatalf("browser handler path=%s status=%d headers=%v", path, status, client.lastHeader)
		}
		status, _, headers := client.playerRequest(t, http.MethodPost, "/api/v1/player"+path, credentials.AccessToken, map[string]any{})
		if status != http.StatusGone || headers.Get("Cache-Control") != "no-store" {
			t.Fatalf("player handler path=%s status=%d headers=%v", path, status, headers)
		}
	}
}
