package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/config"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/credential"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/handlers"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/middleware"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/services"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/metadata/tmdb"
	sitepkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/site"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/site/pansou"
)

type selectedIdentityDriver struct {
	routerCloudDriver
	mixed bool
}

func (d *selectedIdentityDriver) Capabilities() cloud.Capabilities {
	c := d.routerCloudDriver.Capabilities()
	c.ShareReceive = true
	return c
}
func (d *selectedIdentityDriver) InspectShare(ctx context.Context, raw string) (cloud.ShareSnapshot, error) {
	return d.InspectShareDirectory(ctx, raw, "0")
}
func (d *selectedIdentityDriver) InspectShareDirectory(_ context.Context, _ string, parent string) (cloud.ShareSnapshot, error) {
	items := []cloud.ShareItem{{ID: "target-folder", Name: "Fixture Show", IsDir: true}}
	if d.mixed && parent == "0" {
		items = append(items, cloud.ShareItem{ID: "foreign-folder", Name: "Other Work", IsDir: true})
	}
	if parent == "target-folder" {
		items = []cloud.ShareItem{{ID: "target-video", Name: "Fixture.Show.S01E11.mkv", Size: 100}}
	}
	if parent == "foreign-folder" {
		items = []cloud.ShareItem{{ID: "foreign-video", Name: "Other.Work.S01E11.mkv", Size: 100}}
	}
	return cloud.ShareSnapshot{ShareCode: "example", ReceiveCode: "abcd", Items: items}, nil
}
func (*selectedIdentityDriver) ReceiveShare(context.Context, cloud.ShareSnapshot, string) error {
	panic("HTTP validation must only queue the selected task")
}

type selectedIdentityAdapter struct{ routerSiteAdapter }

func (selectedIdentityAdapter) Kind() string { return pansou.Kind }
func (selectedIdentityAdapter) Search(context.Context, sitepkg.Config, sitepkg.Query) (sitepkg.Page, error) {
	return sitepkg.Page{Page: 1, Items: []sitepkg.Result{{TorrentID: "https://115.com/s/example?password=abcd", Title: "Misleading advertisement 1-99", SourceKind: "115_share", CloudProvider: "115"}}}, nil
}
func (selectedIdentityAdapter) ResolveSource(context.Context, sitepkg.Config, string) (sitepkg.Source, error) {
	return sitepkg.Source{ShareURL: "https://115.com/s/example?password=abcd", CloudProvider: "115"}, nil
}

type selectedIdentityMetadataTransport struct{}

func (selectedIdentityMetadataTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	// Pure fixture: no actual network or credential source is used. The only
	// default-transport consumer in this test is the fresh TMDB test service.
	if r.URL.Host != "api.tmdb.org" && r.URL.Host != "api.themoviedb.org" {
		return nil, fmt.Errorf("fixture rejects non-TMDB transport")
	}
	body := `{"results":[]}`
	switch {
	case strings.Contains(r.URL.Path, "/tv/100"):
		body = `{"id":100,"name":"Fixture Show","original_name":"Fixture Show","first_air_date":"2026-01-01","number_of_seasons":1,"alternative_titles":{"results":[]},"translations":{"translations":[]}}`
	case strings.Contains(r.URL.Path, "/tv/200"):
		body = `{"id":200,"name":"Other Work","original_name":"Other Work","first_air_date":"2026-01-01","number_of_seasons":1,"alternative_titles":{"results":[]},"translations":{"translations":[]}}`
	case strings.Contains(r.URL.Path, "/search/tv") && strings.Contains(strings.ToLower(r.URL.Query().Get("query")), "other"):
		body = `{"results":[{"id":200,"name":"Other Work","original_name":"Other Work","first_air_date":"2026-01-01","number_of_seasons":1,"popularity":1,"vote_count":100}]}`
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body)), Request: r}, nil
}

func TestDiscoveryExpectedIdentityPreservesExactSelectedShare(t *testing.T) {
	mode := gin.Mode()
	gin.SetMode(gin.TestMode)
	t.Cleanup(func() { gin.SetMode(mode) })
	for _, mode := range []string{"season_context", "mixed_target", "mixed_folder", "mixed_foreign", "mixed_both", "forged_entry", "changed_config"} {
		t.Run(mode, func(t *testing.T) {
			driver := &selectedIdentityDriver{mixed: mode != "season_context"}
			client := newTestClient(t, driver)
			client.setup(t)
			previous := http.DefaultTransport
			http.DefaultTransport = selectedIdentityMetadataTransport{}
			t.Cleanup(func() { http.DefaultTransport = previous })
			var owner models.User
			if err := client.db.First(&owner, "username_normalized = ?", "owner").Error; err != nil {
				t.Fatal(err)
			}
			actor, err := services.NewAuthorizationService(client.db).Resolve(owner.ID)
			if err != nil {
				t.Fatal(err)
			}
			audit := services.NewAuditService(client.db)
			store, err := credential.Open(filepath.Join(t.TempDir(), "synthetic.key"), "")
			if err != nil {
				t.Fatal(err)
			}
			connection, err := client.connections.Create(actor, services.ConnectionInput{Name: "Selected share", Provider: "pan115", Cookie: "UID=fixture_A1; CID=fixture-cid; SEID=fixture-only", Enabled: true}, services.RequestContext{})
			if err != nil {
				t.Fatal(err)
			}
			storage := models.Storage{Name: "Share storage", NameNormalized: "share storage", Type: "pan115", RootPath: "root-1", RootDisplayPath: "/媒体", ConnectionID: &connection.ID, Enabled: true}
			if err := client.db.Create(&storage).Error; err != nil {
				t.Fatal(err)
			}
			var profile models.MediaClassificationProfile
			if err := client.db.First(&profile, "code = ?", "default-v1").Error; err != nil {
				t.Fatal(err)
			}
			library, err := client.libraries.Create(context.Background(), actor, services.MediaLibraryInput{Name: "Selected target", StorageID: storage.ID, ProfileID: profile.ID, RelativeRoot: "/Selected", ProviderRootID: "root-1", Enabled: false, Recursive: true, TransferMode: models.MediaLibraryTransferCopy}, services.RequestContext{})
			if err != nil {
				t.Fatal(err)
			}
			if err := client.db.Model(&models.MediaLibrary{}).Where("id = ?", library.ID).Update("enabled", true).Error; err != nil {
				t.Fatal(err)
			}
			capabilities := downloadpkg.Capabilities{ShareReceive: true, OutputConstraint: downloadpkg.OutputConstraintProviderStorage}
			caps, _ := json.Marshal(capabilities)
			receiver := models.Downloader{ID: "selected-receiver", Name: "Selected receiver", NameNormalized: "selected receiver", Type: models.DownloaderTypePan115Offline, StorageID: &storage.ID, ProviderDirectoryID: "root-1", ProviderDirectoryPath: "/媒体", Enabled: true, CapabilitiesJSON: string(caps)}
			if err := client.db.Create(&receiver).Error; err != nil {
				t.Fatal(err)
			}
			registry := downloadpkg.NewRegistry()
			if err := registry.Register(models.DownloaderTypePan115Offline, capabilities, func(downloadpkg.Config) (downloadpkg.Client, error) { return downloadpkg.NewFakeClient(), nil }); err != nil {
				t.Fatal(err)
			}
			downloaders := services.NewDownloaderService(client.db, audit, store, registry)
			downloaders.SetConnectionService(client.connections)
			downloads := services.NewDownloadService(client.db, audit, store, downloaders, services.NewDownloadSettingsService(client.db, audit), client.queue, zerolog.Nop())
			sites := services.NewSiteServiceWithAdapters(client.db, audit, store, downloads, []sitepkg.Adapter{selectedIdentityAdapter{}}, zerolog.Nop())
			sites.SetMetadataSettings(services.NewMetadataSettingsService(client.db, audit, store, tmdb.Credential{Kind: tmdb.CredentialKindReadAccessToken, Value: "synthetic-metadata"}))
			site, err := sites.Create(context.Background(), actor, services.SiteInput{Name: "Selected share site", Kind: pansou.Kind, BaseURL: "http://fixture.invalid", CloudConfig: &sitepkg.CloudConfig{Provider: "115", Channels: []string{"fixture"}}, Enabled: true, Priority: 1, TimeoutSeconds: 30, RateLimitPerMinute: 120}, services.RequestContext{})
			if err != nil {
				t.Fatal(err)
			}
			season := 1
			search, err := sites.SearchMediaIdentity(context.Background(), actor, services.MediaIdentitySearchInput{MediaType: "tv", TMDBID: 100, Season: &season, SiteID: &site.ID, Page: 1})
			if err != nil || len(search.Groups) != 1 || len(search.Groups[0].Items) != 1 {
				t.Fatalf("search=%+v err=%v", search, err)
			}
			token := search.Groups[0].Items[0].Token
			preview, err := sites.PreviewShare(context.Background(), actor, token, receiver.ID)
			if err != nil {
				t.Fatal(err)
			}
			var selected []string
			for _, entry := range preview.Entries {
				if entry.Name == "Fixture.Show.S01E11.mkv" && mode != "mixed_foreign" || entry.Name == "Other.Work.S01E11.mkv" && (mode == "mixed_foreign" || mode == "mixed_both") {
					selected = append(selected, entry.Token)
				}
				if mode == "mixed_folder" && entry.Name == "Fixture Show" {
					selected = []string{entry.Token}
					break
				}
			}
			if mode == "forged_entry" {
				selected = []string{"forged"}
			}
			if mode == "changed_config" {
				client.db.Model(&receiver).Update("provider_directory_id", "outside-root")
			}
			api := handlers.NewAPI(config.Config{}, nil, nil, audit, nil, nil, nil, zerolog.Nop())
			api.SetSiteService(sites)
			router := gin.New()
			router.POST("/api/v1/player/discovery/downloads", middleware.NoStore(), func(c *gin.Context) { c.Set(middleware.ContextActor, actor) }, api.CreateDiscoveryDownload)
			raw, _ := json.Marshal(map[string]any{"result_token": token, "downloader_id": receiver.ID, "media_library_id": library.ID, "preview_token": preview.Token, "selected_entry_tokens": selected, "expected_tmdb_id": 100, "expected_media_type": "tv", "priority": 0})
			request := httptest.NewRequest(http.MethodPost, "/api/v1/player/discovery/downloads", bytes.NewReader(raw))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			var count int64
			client.db.Model(&models.DownloadTask{}).Count(&count)
			good := mode == "season_context" || mode == "mixed_target" || mode == "mixed_folder"
			if good && (response.Code != http.StatusCreated || count != 1) {
				t.Fatalf("selected target rejected: status=%d count=%d response=%s", response.Code, count, response.Body.String())
			}
			if !good && (response.Code < 400 || count != 0) {
				t.Fatalf("foreign/stale selection accepted: status=%d count=%d", response.Code, count)
			}
			if mode == "mixed_foreign" || mode == "mixed_both" {
				if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), services.CodeSiteResultIdentityMismatch) {
					t.Fatalf("expected identity rejection did not remain a client error: %d %s", response.Code, response.Body.String())
				}
			}
			if good {
				var task models.DownloadTask
				if err := client.db.First(&task).Error; err != nil {
					t.Fatal(err)
				}
				plain, err := store.Decrypt("download-task:"+task.ID+":source", task.SourceCiphertext)
				var source struct {
					ShareSelection *cloud.ShareSelection `json:"share_selection"`
				}
				if err != nil || json.Unmarshal([]byte(plain), &source) != nil || source.ShareSelection == nil || len(source.ShareSelection.Files) != 1 || source.ShareSelection.Files[0].ID != "target-video" {
					t.Fatal("frozen selected target was not the sole encrypted source file")
				}
			}
			if response.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("private response cacheable")
			}
		})
	}
}
