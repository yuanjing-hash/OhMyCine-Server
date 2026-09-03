package httpserver

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
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

func TestBrowserMediaLibraryOverviewUsesSessionFiltersSourcesAndReturnsSafeDTOs(t *testing.T) {
	client := newTestClient(t)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/media-libraries/overview", nil)
	response := httptest.NewRecorder()
	client.router.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("anonymous status=%d cache=%q", response.Code, response.Header().Get("Cache-Control"))
	}

	client.setup(t)
	var owner models.User
	if err := client.db.Where("username_normalized = ?", "owner").First(&owner).Error; err != nil {
		t.Fatal(err)
	}
	var profile models.MediaClassificationProfile
	if err := client.db.Where("code = ?", "default-v1").First(&profile).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	storage := models.Storage{Name: "Overview local", NameNormalized: "overview-local", Type: models.StorageTypeLocal, RootPath: t.TempDir(), RootPathNormalized: "overview-local", Enabled: true, Capabilities: `{}`, CreatedAt: now, UpdatedAt: now}
	if err := client.db.Create(&storage).Error; err != nil {
		t.Fatal(err)
	}
	library := models.MediaLibrary{Name: "Overview library", NameNormalized: "overview-library", StorageID: storage.ID, ProfileID: profile.ID, ProfileRevision: profile.Revision, RelativeRoot: "/", Enabled: true, Recursive: true, VideoExtensionsJSON: `[".mkv"]`, IgnorePatternsJSON: `[]`, Status: models.MediaLibraryStatusListening, ArtifactStatus: models.MediaArtifactStatusCompleted, CreatedAt: now, UpdatedAt: now}
	if err := client.db.Select("*").Create(&library).Error; err != nil {
		t.Fatal(err)
	}
	workKey := "movie:tmdb:346"
	entry := models.MediaLibraryEntry{LibraryID: library.ID, RelativePath: "/Seven.Samurai.1954.mkv", Size: 100, ModifiedAt: now, MediaType: "movie", Title: "七武士", WorkKey: workKey, MatchStatus: "matched", CategoryName: "电影", LastGeneration: 1, CreatedAt: now, UpdatedAt: now}
	if err := client.db.Create(&entry).Error; err != nil {
		t.Fatal(err)
	}
	manualCollectionID := "11111111-1111-4111-8111-111111111111"
	favorite := models.PlayerMediaFavorite{UserID: owner.ID, LibraryID: library.ID, WorkKey: workKey, CreatedAt: now, UpdatedAt: now}
	manualCollection := models.PlayerMediaCollection{ID: manualCollectionID, OwnerID: &owner.ID, Source: models.PlayerMediaCollectionSourceManual, Kind: models.PlayerMediaCollectionKindCollection, Name: "周末片单", Visible: true, Revision: 1, CreatedAt: now, UpdatedAt: now}
	manualItem := models.PlayerMediaCollectionItem{CollectionID: manualCollectionID, LibraryID: library.ID, WorkKey: workKey, Origin: models.PlayerMediaCollectionItemOriginManual, CreatedAt: now, UpdatedAt: now}
	if err := client.db.Create(&favorite).Error; err != nil {
		t.Fatal(err)
	}
	if err := client.db.Create(&manualCollection).Error; err != nil {
		t.Fatal(err)
	}
	if err := client.db.Create(&manualItem).Error; err != nil {
		t.Fatal(err)
	}
	workToken := base64.RawURLEncoding.EncodeToString([]byte(workKey))
	identity := "server:v1:movie:" + uintString(library.ID) + ":" + workToken
	duration := 6000.0
	rows := []models.PlayerPlaybackHistory{
		{UserID: owner.ID, SyncKey: strings.Repeat("a", 64), HistoryIdentity: identity, SourceKind: "server", SourceID: "server-private-id", LibraryID: uintString(library.ID), ItemID: "work|" + uintString(library.ID) + "|" + workToken, ItemToken: "work|" + uintString(library.ID) + "|" + workToken, MediaIdentity: identity, Title: "七武士", DisplayTitle: "七武士", MediaType: "movie", PosterPath: "/poster-secret-path.jpg", Position: 1200, Duration: &duration, ClientUpdatedAt: now.UnixMilli(), Revision: 1, CreatedAt: now, UpdatedAt: now},
		{UserID: owner.ID, SyncKey: strings.Repeat("b", 64), SourceKind: "emby", SourceLocator: "https://emby.example.test", SourceID: "emby-private-id", MediaIdentity: "emby-item", Title: "外部 Emby 作品", DisplayTitle: "外部 Emby 作品", Position: 300, Duration: &duration, ClientUpdatedAt: now.Add(-time.Minute).UnixMilli(), Revision: 2, CreatedAt: now, UpdatedAt: now},
	}
	if err := client.db.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}

	status, envelope := client.request(t, http.MethodGet, "/api/v1/media-libraries/overview", nil, false)
	if status != http.StatusOK || client.lastHeader.Get("Cache-Control") != "no-store" {
		t.Fatalf("overview status=%d cache=%q data=%s", status, client.lastHeader.Get("Cache-Control"), envelope.Data)
	}
	var overview struct {
		Sections map[string]struct {
			List []json.RawMessage `json:"list"`
		} `json:"sections"`
	}
	if err := json.Unmarshal(envelope.Data, &overview); err != nil {
		t.Fatal(err)
	}
	if len(overview.Sections["continue_watching"].List) != 1 {
		t.Fatalf("continue watching=%s", envelope.Data)
	}
	if _, duplicated := overview.Sections["recent_history"]; duplicated {
		t.Fatalf("browser overview must not duplicate history: %s", envelope.Data)
	}
	for _, forbidden := range []string{"source_kind", "source_locator", "source_id", "item_token", "media_identity", "poster_path", "provider_id", "root_path"} {
		if jsonContainsKey(envelope.Data, forbidden) {
			t.Fatalf("browser overview leaked %q: %s", forbidden, envelope.Data)
		}
	}

	status, envelope = client.request(t, http.MethodGet, "/api/v1/media-libraries/history?page=1&page_size=24", nil, false)
	var page struct {
		List []json.RawMessage `json:"list"`
	}
	if err := json.Unmarshal(envelope.Data, &page); status != http.StatusOK || err != nil || len(page.List) != 1 {
		t.Fatalf("history status=%d err=%v data=%s", status, err, envelope.Data)
	}
	if bytes := string(envelope.Data); strings.Contains(bytes, "外部 Emby 作品") || strings.Contains(bytes, "server-private-id") || strings.Contains(bytes, "poster-secret-path") {
		t.Fatalf("history leaked foreign/private data: %s", bytes)
	}
	for _, path := range []string{
		"/api/v1/media-libraries/favorites",
		"/api/v1/media-libraries/collections?kind=collection",
		"/api/v1/media-libraries/collections/" + manualCollectionID + "/items",
	} {
		status, envelope = client.request(t, http.MethodGet, path, nil, false)
		if status != http.StatusOK || client.lastHeader.Get("Cache-Control") != "no-store" {
			t.Fatalf("%s status=%d cache=%q data=%s", path, status, client.lastHeader.Get("Cache-Control"), envelope.Data)
		}
		for _, forbidden := range []string{"item_token", "history_identity", "work_identity", "poster_path", "backdrop_path", "provider_id", "relative_path", "root_path"} {
			if jsonContainsKey(envelope.Data, forbidden) {
				t.Fatalf("%s leaked %q: %s", path, forbidden, envelope.Data)
			}
		}
	}

	if err := client.db.Delete(&entry).Error; err != nil {
		t.Fatal(err)
	}
	status, envelope = client.request(t, http.MethodGet, "/api/v1/media-libraries/history?page=1&page_size=24", nil, false)
	if err := json.Unmarshal(envelope.Data, &page); status != http.StatusOK || err != nil || len(page.List) != 0 {
		t.Fatalf("deleted catalog history status=%d err=%v data=%s", status, err, envelope.Data)
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
