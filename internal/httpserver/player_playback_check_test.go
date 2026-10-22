package httpserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/services"
)

func TestPlayerPlaybackCheckHTTPContract(t *testing.T) {
	client := newTestClient(t)
	client.setup(t)
	var owner models.User
	if err := client.db.Where("username_normalized=?", "owner").First(&owner).Error; err != nil {
		t.Fatal(err)
	}
	createRouterSignedArtifact(t, client, services.Actor{User: owner, Permissions: map[string]struct{}{authz.PermissionConnectionsCreate: {}, authz.PermissionConnectionsRead: {}}})
	var entry models.MediaLibraryEntry
	if err := client.db.First(&entry).Error; err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/player/media-entries/" + uintString(entry.ID) + "/stream?check=1"
	status, _, _ := client.playerRequest(t, http.MethodGet, path, "", nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("anonymous check=%d", status)
	}
	browser := httptest.NewRequest(http.MethodGet, path, nil)
	browser.AddCookie(client.cookie)
	w := httptest.NewRecorder()
	client.router.ServeHTTP(w, browser)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("browser cookie crossed Player boundary=%d", w.Code)
	}
	status, envelope, _ := client.playerRequest(t, http.MethodPost, "/api/v1/player/auth/login", "", map[string]any{"username": "owner", "password": "strong-owner-password", "device_id": "playback-check-fixture", "device_name": "Fixture"})
	var login struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(envelope.Data, &login); err != nil || status != http.StatusOK || login.AccessToken == "" {
		t.Fatal("fixture login failed")
	}
	status, envelope, _ = client.playerRequest(t, http.MethodGet, "/api/v1/player/bootstrap", login.AccessToken, nil)
	var bootstrap struct {
		Capabilities []string `json:"capabilities"`
	}
	if err := json.Unmarshal(envelope.Data, &bootstrap); err != nil || status != 200 || !containsString(bootstrap.Capabilities, "playback_check_v1") {
		t.Fatal("capability missing")
	}
	// The check mode must not turn into a 302 or leak upstream media/location.
	for _, provider := range []string{models.StorageTypePan115, models.StorageTypeLocal} {
		var library models.MediaLibrary
		if err := client.db.First(&library, entry.LibraryID).Error; err != nil {
			t.Fatal(err)
		}
		if err := client.db.Model(&models.Storage{}).Where("id=?", library.StorageID).Update("type", provider).Error; err != nil {
			t.Fatal(err)
		}
		status, envelope, headers := client.playerRequest(t, http.MethodGet, path, login.AccessToken, nil)
		if status != 200 || envelope.Code != 0 || string(envelope.Data) != `{"ready":true,"version":1}` || headers.Get("Location") != "" || headers.Get("Cache-Control") != "no-store" || headers.Get("Content-Range") != "" {
			t.Fatalf("metadata contract provider=%s status=%d data=%s", provider, status, envelope.Data)
		}
	}
	if err := client.db.Model(&models.MediaLibrary{}).Where("id=?", entry.LibraryID).Update("enabled", false).Error; err != nil {
		t.Fatal(err)
	}
	status, _, headers := client.playerRequest(t, http.MethodGet, path, login.AccessToken, nil)
	if status == 200 || headers.Get("Location") != "" {
		t.Fatal("disabled library check succeeded")
	}
}
