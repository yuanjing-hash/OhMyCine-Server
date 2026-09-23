package httpserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPlayerArtworkRouteRequiresDeviceBearer(t *testing.T) {
	client := newTestClient(t)
	path := "/api/v1/player/artwork/00000000-0000-4000-8000-000000000000"
	status, _, _ := client.playerRequest(t, http.MethodGet, path, "", nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("anonymous artwork status=%d", status)
	}
	client.setup(t)
	status, login, _ := client.playerRequest(t, http.MethodPost, "/api/v1/player/auth/login", "", map[string]any{
		"username": "owner", "password": "strong-owner-password",
		"device_id": "artwork-route-test", "device_name": "Artwork route test",
	})
	if status != http.StatusOK {
		t.Fatalf("device login status=%d", status)
	}
	var token struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(login.Data, &token); err != nil || token.AccessToken == "" {
		t.Fatalf("device token parse error=%v", err)
	}
	status, _, _ = client.playerRequest(t, http.MethodGet, path, token.AccessToken, nil)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("authenticated artwork without configured gateway status=%d", status)
	}
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.AddCookie(client.cookie)
	response := httptest.NewRecorder()
	client.router.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("browser cookie crossed device artwork boundary: %d", response.Code)
	}
}
