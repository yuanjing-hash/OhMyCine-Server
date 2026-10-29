package httpserver

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/services"
)

func TestMediaVersionUpgradeRoutesScopeOpaqueDTOAndStrictSubmit(t *testing.T) {
	client := newTestClient(t)
	client.setup(t)
	root := t.TempDir()
	file := filepath.Join(root, "Show.S01E02.mkv")
	if err := os.WriteFile(file, []byte("old-video"), 0600); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(file)
	var profile models.MediaClassificationProfile
	if err := client.db.First(&profile).Error; err != nil {
		t.Fatal(err)
	}
	storage := models.Storage{Name: "Upgrade local", NameNormalized: "upgrade-local", Type: models.StorageTypeLocal, RootPath: root, RootPathNormalized: strings.ToLower(root), Enabled: true, Capabilities: "{}"}
	if err := client.db.Create(&storage).Error; err != nil {
		t.Fatal(err)
	}
	library := models.MediaLibrary{Name: "Upgrade library", NameNormalized: "upgrade-library", StorageID: storage.ID, ProfileID: profile.ID, ProfileRevision: profile.Revision, RelativeRoot: "/", Enabled: true, Recursive: true, Status: models.MediaLibraryStatusListening, BaselineGeneration: 1, DirtyGeneration: 1, VideoExtensionsJSON: `[".mkv"]`, IgnorePatternsJSON: "[]", MetadataLanguage: "zh-CN", MetadataRegion: "CN", MatchStrategy: "balanced", ProviderRatePerSecond: 1, ProviderConcurrency: 1, MetadataRatePerSecond: 1, MetadataConcurrency: 1, FullScanIntervalHours: 24, IncrementalMinutes: 15}
	if err := client.db.Create(&library).Error; err != nil {
		t.Fatal(err)
	}
	season, episode, tmdb := 1, 2, int64(42)
	entry := models.MediaLibraryEntry{LibraryID: library.ID, RelativePath: "/Show.S01E02.mkv", ProviderID: "opaque-private-file", Size: info.Size(), ModifiedAt: info.ModTime().UTC(), MediaType: "tv", Title: "Show", SeriesTitle: "Show", WorkKey: "series:42", Season: &season, Episode: &episode, TMDBID: &tmdb, MatchStatus: "matched", CategoryName: "TV", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := client.db.Create(&entry).Error; err != nil {
		t.Fatal(err)
	}
	work := base64.RawURLEncoding.EncodeToString([]byte(entry.WorkKey))
	preview := fmt.Sprintf("/api/v1/media-libraries/%d/catalog/%s/entries/%d/upgrade-preview", library.ID, work, entry.ID)
	status, response := client.request(t, http.MethodPost, preview, map[string]any{}, true)
	if status != http.StatusOK {
		t.Fatalf("preview status=%d data=%s", status, response.Data)
	}
	if client.lastHeader.Get("Cache-Control") != "no-store" {
		t.Fatal("private context cached")
	}
	var dto services.MediaVersionUpgradeDTO
	if err := json.Unmarshal(response.Data, &dto); err != nil {
		t.Fatal(err)
	}
	if dto.LibraryID != library.ID || dto.EntryID != entry.ID || dto.TMDBID != 42 || dto.Episode == nil || *dto.Episode != 2 {
		t.Fatalf("wrong target %+v", dto)
	}
	for _, private := range []string{root, entry.RelativePath, entry.ProviderID, "snapshot_json", "private_provider_id"} {
		if strings.Contains(string(response.Data), private) {
			t.Fatalf("private execution fact exposed %q", private)
		}
	}
	status, _ = client.request(t, http.MethodGet, "/api/v1/media-upgrades/"+dto.ID, nil, false)
	if status != http.StatusOK {
		t.Fatalf("get status=%d", status)
	}
	status, _ = client.request(t, http.MethodPost, "/api/v1/media-upgrades/"+dto.ID+"/submit", map[string]any{"revision": dto.Revision, "result_token": "not-a-result", "downloader_id": "executor", "media_library_id": 999}, true)
	if status != http.StatusBadRequest {
		t.Fatalf("public target override accepted: %d", status)
	}
	var count int64
	client.db.Model(&models.DownloadTask{}).Count(&count)
	if count != 0 {
		t.Fatal("preview/invalid submit created cloud work")
	}
	wrong := fmt.Sprintf("/api/v1/media-libraries/%d/catalog/%s/entries/%d/upgrade-preview", library.ID, base64.RawURLEncoding.EncodeToString([]byte("series:99")), entry.ID)
	status, _ = client.request(t, http.MethodPost, wrong, map[string]any{}, true)
	if status != http.StatusNotFound {
		t.Fatalf("cross-work entry accepted %d", status)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("preview mutated file %v", err)
	}
}
