package httpserver

import (
	"bytes"
	"encoding/json"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/services"
)

func TestHistoryArtworkHTTPAuthenticationSyncAndSafeProjection(t *testing.T) {
	client := newTestClient(t)
	client.setup(t)
	status, envelope, _ := client.playerRequest(t, http.MethodPost, "/api/v1/player/auth/login", "", map[string]any{"username": "owner", "password": "strong-owner-password", "device_id": "artwork-client", "device_name": "Artwork client"})
	if status != 200 {
		t.Fatal(status, envelope.Message)
	}
	var login struct {
		Token string `json:"access_token"`
	}
	if err := json.Unmarshal(envelope.Data, &login); err != nil {
		t.Fatal(err)
	}
	key := strings.Repeat("a", 64)
	change := services.PlayerHistoryChange{SyncKey: key, SourceKind: "emby", SourceName: "Bedroom", SourceID: "private-source", SourceLocator: "https://example.test", MediaIdentity: "private-media", Title: "Movie", Position: 10, UpdatedAt: 1000}
	status, envelope, _ = client.playerRequest(t, http.MethodPost, "/api/v1/player/history/sync", login.Token, map[string]any{"changes": []services.PlayerHistoryChange{change}})
	if status != 200 {
		t.Fatal(status, envelope.Message)
	}
	var imageBody bytes.Buffer
	if err := png.Encode(&imageBody, image.NewRGBA(image.Rect(0, 0, 10, 20))); err != nil {
		t.Fatal(err)
	}
	request := func(method, path, token string, cookie bool, data []byte) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, bytes.NewReader(data))
		r.Header.Set("Content-Type", "image/png")
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		if cookie {
			r.AddCookie(client.cookie)
		}
		w := httptest.NewRecorder()
		client.router.ServeHTTP(w, r)
		return w
	}
	path := "/api/v1/player/history/" + key + "/artwork/poster"
	for _, cookie := range []bool{false, true} {
		if w := request(http.MethodPut, path, "", cookie, imageBody.Bytes()); w.Code != 401 {
			t.Fatalf("upload cookie=%v status=%d", cookie, w.Code)
		}
	}
	w := request(http.MethodPut, path, login.Token, false, imageBody.Bytes())
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	var receipt services.HistoryArtworkReceipt
	if err := json.Unmarshal(envelope.Data, &receipt); err != nil || receipt.AssetID == "" {
		t.Fatal(receipt, err)
	}
	playerPath := "/api/v1/player/history/artwork/" + receipt.AssetID
	browserPath := "/api/v1/media-libraries/history/artwork/" + receipt.AssetID
	for _, tc := range []struct {
		path, token string
		cookie      bool
		status      int
	}{
		{playerPath, login.Token, false, 200}, {playerPath, "", true, 401}, {browserPath, "", true, 200}, {browserPath, login.Token, false, 401},
	} {
		w = request(http.MethodGet, tc.path, tc.token, tc.cookie, nil)
		if w.Code != tc.status {
			t.Fatalf("%s cookie=%v status=%d", tc.path, tc.cookie, w.Code)
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("missing no-store")
		}
		if tc.status == 200 && (w.Header().Get("Content-Type") != "image/png" || w.Header().Get("X-Content-Type-Options") != "nosniff") {
			t.Fatalf("image headers=%v", w.Header())
		}
	}
	status, envelope = client.request(t, http.MethodGet, "/api/v1/media-libraries/history", nil, false)
	if status != 200 || !bytes.Contains(envelope.Data, []byte(`"total":0`)) || bytes.Contains(envelope.Data, []byte(browserPath)) || bytes.Contains(envelope.Data, []byte("private-source")) || bytes.Contains(envelope.Data, []byte("example.test")) {
		t.Fatalf("browser %d %s", status, envelope.Data)
	}
	status, envelope, _ = client.playerRequest(t, http.MethodGet, "/api/v1/player/history", login.Token, nil)
	if status != 200 || !bytes.Contains(envelope.Data, []byte(receipt.AssetID)) {
		t.Fatalf("sync association %d %s", status, envelope.Data)
	}
	change.UpdatedAt = time.Now().Add(24 * time.Hour).UnixMilli()
	status, envelope, _ = client.playerRequest(t, http.MethodPost, "/api/v1/player/history/sync", login.Token, map[string]any{"changes": []services.PlayerHistoryChange{change}})
	if status != 400 || !bytes.Contains(envelope.Data, []byte(services.CodeHistoryClockAhead)) {
		t.Fatalf("clock error %d %s", status, envelope.Data)
	}
}
