package httpserver

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/services"
)

func TestGeneratedLibraryArtworkRechecksSourceForBrowserAndPlayer(t *testing.T) {
	owner := newTestClient(t)
	owner.setup(t)
	var role models.Role
	if err := owner.db.Where("code = ?", authz.RoleAdministrator).First(&role).Error; err != nil {
		t.Fatal(err)
	}
	status, envelope := owner.request(t, http.MethodPost, "/api/v1/users", map[string]any{"username": "artwork-viewer", "password": "viewer-strong-password", "role_ids": []uint{role.ID}}, true)
	if status != http.StatusCreated {
		t.Fatalf("create user: %d %s", status, envelope.Message)
	}
	var user models.User
	if err := owner.db.Where("username = ?", "artwork-viewer").First(&user).Error; err != nil {
		t.Fatal(err)
	}
	viewer := newTestClientWithRouter(owner.router)
	viewer.login(t, user.Username, "viewer-strong-password")
	status, envelope, _ = owner.playerRequest(t, http.MethodPost, "/api/v1/player/auth/login", "", map[string]any{"username": user.Username, "password": "viewer-strong-password", "device_id": "generated-cover", "device_name": "Generated cover"})
	var login struct {
		AccessToken string `json:"access_token"`
	}
	if status != http.StatusOK || json.Unmarshal(envelope.Data, &login) != nil || login.AccessToken == "" {
		t.Fatalf("player login: %d %s", status, envelope.Data)
	}
	storage := models.Storage{Name: "Cover source", NameNormalized: "cover-source", Type: models.StorageTypeLocal, RootPath: t.TempDir(), RootPathNormalized: "cover-source", Enabled: true, Capabilities: `{}`}
	if err := owner.db.Create(&storage).Error; err != nil {
		t.Fatal(err)
	}
	var profile models.MediaClassificationProfile
	if err := owner.db.Where("code = ?", "default-v1").First(&profile).Error; err != nil {
		t.Fatal(err)
	}
	library := models.MediaLibrary{Name: "Private cover library", NameNormalized: "private-cover-library", StorageID: storage.ID, ProfileID: profile.ID, ProfileRevision: profile.Revision, RelativeRoot: "/", Enabled: true}
	if err := owner.db.Create(&library).Error; err != nil {
		t.Fatal(err)
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 32, 32)), nil); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(encoded.Bytes())
	digest := hex.EncodeToString(hash[:])
	if err := os.MkdirAll(owner.artworkRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(owner.artworkRoot, digest+".jpg"), encoded.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	record := models.MediaCategoryArtwork{ScopeKind: "media_category", LibraryID: library.ID, CategoryKey: "movie", CategoryName: "电影", MediaType: "movie", TemplateVersion: "fixture", ContentHash: digest, RelativePath: digest + ".jpg", Status: "ready"}
	if err := owner.db.Create(&record).Error; err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/assets/generated-library-covers/" + digest
	request := func(token string, cookie bool, conditional bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		if cookie {
			r.AddCookie(viewer.cookie)
		}
		if conditional {
			r.Header.Set("If-None-Match", `"`+digest+`"`)
		}
		w := httptest.NewRecorder()
		owner.router.ServeHTTP(w, r)
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("missing no-store: %v", w.Header())
		}
		return w
	}
	if w := request("", false, false); w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous status=%d", w.Code)
	}
	if w := request("invalid", true, false); w.Code != http.StatusUnauthorized {
		t.Fatalf("invalid bearer fell back to cookie: %d", w.Code)
	}
	for _, tc := range []struct {
		token  string
		cookie bool
	}{{"", true}, {login.AccessToken, false}} {
		if w := request(tc.token, tc.cookie, false); w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), encoded.Bytes()) || w.Header().Get("Content-Type") != "image/jpeg" {
			t.Fatalf("authenticated cover status=%d headers=%v", w.Code, w.Header())
		}
	}
	policyPath := "/api/v1/users/" + strconv.FormatUint(uint64(user.ID), 10) + "/resource-access"
	status, envelope = owner.request(t, http.MethodGet, policyPath, nil, false)
	var policy services.UserResourceAccess
	if status != http.StatusOK || json.Unmarshal(envelope.Data, &policy) != nil {
		t.Fatalf("read policy: %d %s", status, envelope.Data)
	}
	for i := range policy.Policies {
		if policy.Policies[i].Scope == models.ResourceAccessScopeLibraryRead {
			policy.Policies[i].Mode = models.ResourceAccessModeAllowlist
			policy.Policies[i].ResourceIDs = []string{}
		}
	}
	if status, envelope := owner.request(t, http.MethodPut, policyPath, policy, true); status != http.StatusOK {
		t.Fatalf("revoke library: %d %s", status, envelope.Message)
	}
	for _, tc := range []struct {
		token  string
		cookie bool
	}{{"", true}, {login.AccessToken, false}} {
		if w := request(tc.token, tc.cookie, true); w.Code != http.StatusNotFound || w.Body.Len() != 0 {
			t.Fatalf("revoked cached/conditional cover status=%d body=%s", w.Code, w.Body.String())
		}
	}
}
