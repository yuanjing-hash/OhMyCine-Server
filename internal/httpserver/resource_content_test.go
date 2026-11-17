package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/config"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/handlers"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/middleware"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/services"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/metadata/tmdb"
	sitepkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/site"
)

type contentRouterAdapter struct{ routerSiteAdapter }

func (contentRouterAdapter) Download(context.Context, sitepkg.Config, string) ([]byte, string, error) {
	name := "Actual.Show.S01E13.mkv"
	raw := []byte(fmt.Sprintf("d4:infod6:lengthi100e4:name%d:%s12:piece lengthi16384e6:pieces20:", len(name), name))
	raw = append(raw, bytes.Repeat([]byte{1}, 20)...)
	raw = append(raw, 'e', 'e')
	return raw, "fixture.torrent", nil
}

type offlineHTTPFixtureAdapter struct{ contentRouterAdapter }

func (offlineHTTPFixtureAdapter) Kind() string { return "nyaa" }
func (offlineHTTPFixtureAdapter) Search(context.Context, sitepkg.Config, sitepkg.Query) (sitepkg.Page, error) {
	return sitepkg.Page{Page: 1, Items: []sitepkg.Result{{TorrentID: "88", Title: "Fixture.Show.S01E13.1080p"}}}, nil
}
func (offlineHTTPFixtureAdapter) ResolveSource(context.Context, sitepkg.Config, string) (sitepkg.Source, error) {
	return sitepkg.Source{Magnet: "magnet:?xt=urn:btih:" + strings.Repeat("a", 40)}, nil
}

type offlineHTTPFixtureClient struct {
	*downloadpkg.FakeClient
	namespace string
}

func (c *offlineHTTPFixtureClient) OfflineOperation(source downloadpkg.Source) downloadpkg.OfflineOperation {
	if source.Kind != downloadpkg.SourceURL {
		return downloadpkg.OfflineOperation{}
	}
	return downloadpkg.OfflineOperation{Kind: downloadpkg.OfflineOperationCloud, AccountNamespace: c.namespace, KeepOutputStop: true}
}
func (c *offlineHTTPFixtureClient) ObserveOffline(ctx context.Context, id string) (downloadpkg.Task, error) {
	return c.Get(ctx, id)
}
func (*offlineHTTPFixtureClient) Submit(context.Context, downloadpkg.SubmitRequest) (downloadpkg.Task, error) {
	panic("HTTP acceptance must not dispatch a native download")
}

func TestResourceContentSessionAndPlayerRoutesUseOpaqueActualFiles(t *testing.T) {
	client := newTestClientWithSiteAdapters(t, []sitepkg.Adapter{contentRouterAdapter{}})
	for _, endpoint := range []string{"content-preview", "content-selection"} {
		status, _ := client.request(t, http.MethodPost, "/api/v1/discovery/"+endpoint, map[string]any{}, false)
		if status != http.StatusUnauthorized {
			t.Fatalf("anonymous %s status=%d", endpoint, status)
		}
	}
	client.setup(t)
	var owner models.User
	if err := client.db.Where("username_normalized = ?", "owner").First(&owner).Error; err != nil {
		t.Fatal(err)
	}
	actor, err := services.NewAuthorizationService(client.db).Resolve(owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	site, err := client.sites.Create(context.Background(), actor, services.SiteInput{Name: "Content HTTP fixture", Kind: "pttime", BaseURL: "https://pt.example.test", Cookie: "fixture-cookie", Enabled: true, Priority: 1, TimeoutSeconds: 15, RateLimitPerMinute: 120}, services.RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	groups, err := client.sites.Search(context.Background(), actor, services.SiteSearchInput{Keyword: "Seven", SiteID: &site.ID, Page: 1})
	if err != nil || len(groups) != 1 || len(groups[0].Items) != 1 {
		t.Fatalf("fixture results=%+v err=%v", groups, err)
	}
	token := groups[0].Items[0].Token
	status, envelope := client.request(t, http.MethodPost, "/api/v1/discovery/content-preview", map[string]any{"result_token": token}, false)
	if status != http.StatusForbidden {
		t.Fatalf("missing CSRF status=%d", status)
	}
	status, envelope = client.request(t, http.MethodPost, "/api/v1/discovery/content-preview", map[string]any{"result_token": token}, true)
	var preview services.ContentPreviewSummary
	if status != http.StatusOK || json.Unmarshal(envelope.Data, &preview) != nil || !preview.Complete || len(preview.Files) != 1 || preview.Files[0].Name != "Actual.Show.S01E13.mkv" {
		t.Fatalf("actual preview status=%d message=%s data=%s", status, envelope.Message, envelope.Data)
	}
	if client.lastHeader.Get("Cache-Control") != "no-store" {
		t.Fatal("inspection response was cacheable")
	}
	for _, secret := range []string{"fixture-cookie", "pt.example.test", "\"index\"", "\"v1\"", "announce"} {
		if strings.Contains(string(envelope.Data), secret) {
			t.Fatalf("private source field %q exposed", secret)
		}
	}
	status, envelope = client.request(t, http.MethodPost, "/api/v1/discovery/content-selection", map[string]any{"result_token": token, "preview_token": preview.Token, "selected_entry_tokens": []string{preview.Files[0].Token}}, true)
	var selection services.ContentSelectionSummary
	if status != http.StatusOK || json.Unmarshal(envelope.Data, &selection) != nil || selection.Token == "" || selection.SelectedBytes != 100 {
		t.Fatalf("opaque selection status=%d data=%s", status, envelope.Data)
	}
	status, _ = client.request(t, http.MethodPost, "/api/v1/discovery/content-selection", map[string]any{"result_token": token, "preview_token": preview.Token, "selected_entry_tokens": []string{preview.Files[0].Token}, "indices": []int{0}}, true)
	if status != http.StatusBadRequest {
		t.Fatalf("browser native index accepted %d", status)
	}
	status, loginEnvelope, _ := client.playerRequest(t, http.MethodPost, "/api/v1/player/auth/login", "", map[string]any{"username": "owner", "password": "strong-owner-password", "device_id": "content-inspection-test", "device_name": "Content fixture"})
	var login struct {
		AccessToken string `json:"access_token"`
	}
	if status != http.StatusOK || json.Unmarshal(loginEnvelope.Data, &login) != nil {
		t.Fatalf("Player fixture login %d", status)
	}
	status, envelope, headers := client.playerRequest(t, http.MethodPost, "/api/v1/player/discovery/content-preview", login.AccessToken, map[string]any{"result_token": token})
	if status != http.StatusOK || headers.Get("Cache-Control") != "no-store" || json.Unmarshal(envelope.Data, &preview) != nil {
		t.Fatalf("Player preview status=%d message=%s", status, envelope.Message)
	}
	status, _, _ = client.playerRequest(t, http.MethodPost, "/api/v1/player/discovery/content-selection", login.AccessToken, map[string]any{"result_token": token, "preview_token": preview.Token, "selected_entry_tokens": []string{preview.Files[0].Token}})
	if status != http.StatusOK {
		t.Fatalf("Player selection status=%d", status)
	}
	var count int64
	client.db.Model(&models.DownloadTask{}).Count(&count)
	if count != 0 {
		t.Fatal("read/selection API created an external download task")
	}
	t.Run("HTTP expected identity replays the accepted offline confirmation", func(t *testing.T) {
		previousTransport := http.DefaultTransport
		http.DefaultTransport = selectedIdentityMetadataTransport{}
		defer func() { http.DefaultTransport = previousTransport }()
		audit := services.NewAuditService(client.db)
		registry := downloadpkg.NewRegistry()
		caps := downloadpkg.Capabilities{NativeOffline: true, Cancel: true}
		if err := registry.Register(models.DownloaderTypeFake, caps, func(cfg downloadpkg.Config) (downloadpkg.Client, error) {
			return &offlineHTTPFixtureClient{FakeClient: downloadpkg.NewFakeClient(), namespace: cfg.OfflineAccountNamespace}, nil
		}); err != nil {
			t.Fatal(err)
		}
		connection := models.Connection{Name: "HTTP fixture account", NameNormalized: "http fixture account", Provider: "fixture-cloud", AccountID: "original-account", Enabled: true}
		if err := client.db.Create(&connection).Error; err != nil {
			t.Fatal(err)
		}
		storage := models.Storage{Name: "HTTP fixture source", NameNormalized: "http fixture source", Type: models.StorageTypeLocal, RootPath: t.TempDir(), ConnectionID: &connection.ID, Enabled: true}
		if err := client.db.Create(&storage).Error; err != nil {
			t.Fatal(err)
		}
		capsJSON, _ := json.Marshal(caps)
		downloader := models.Downloader{ID: "http-native", OwnerID: actor.User.ID, Name: "HTTP native fixture", NameNormalized: "http native fixture", Type: models.DownloaderTypeFake, StorageID: &storage.ID, ProviderDirectoryID: "original-root", Enabled: true, CapabilitiesJSON: string(capsJSON), MaxConcurrentDownloads: 2}
		if err := client.db.Create(&downloader).Error; err != nil {
			t.Fatal(err)
		}
		downloaderService := services.NewDownloaderService(client.db, audit, client.credentials, registry)
		downloads := services.NewDownloadService(client.db, audit, client.credentials, downloaderService, services.NewDownloadSettingsService(client.db, audit), client.queue, zerolog.Nop())
		sites := services.NewSiteServiceWithAdapters(client.db, audit, client.credentials, downloads, []sitepkg.Adapter{offlineHTTPFixtureAdapter{}}, zerolog.Nop())
		sites.SetMetadataSettings(services.NewMetadataSettingsService(client.db, audit, client.credentials, tmdb.Credential{Kind: tmdb.CredentialKindReadAccessToken, Value: "synthetic-metadata"}))
		publicSite, err := sites.Create(context.Background(), actor, services.SiteInput{Name: "Public HTTP fixture", Kind: "nyaa", BaseURL: "https://nyaa.si", Enabled: true, Priority: 1, TimeoutSeconds: 15, RateLimitPerMinute: 120}, services.RequestContext{})
		if err != nil {
			t.Fatal(err)
		}
		api := handlers.NewAPI(config.Config{}, nil, nil, audit, nil, nil, nil, zerolog.Nop())
		api.SetSiteService(sites)
		router := gin.New()
		for _, route := range []string{"/api/v1/discovery/downloads", "/api/v1/player/discovery/downloads"} {
			router.POST(route, middleware.NoStore(), func(c *gin.Context) { c.Set(middleware.ContextActor, actor) }, api.CreateDiscoveryDownload)
		}
		post := func(route string, body map[string]any) *httptest.ResponseRecorder {
			raw, _ := json.Marshal(body)
			req := httptest.NewRequest(http.MethodPost, route, bytes.NewReader(raw))
			req.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, req)
			if response.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("offline confirmation was cacheable")
			}
			return response
		}
		freshInput := func() map[string]any {
			result, err := sites.Search(context.Background(), actor, services.SiteSearchInput{Keyword: "Fixture Show", SiteID: &publicSite.ID, Page: 1})
			if err != nil || len(result) != 1 || len(result[0].Items) != 1 {
				t.Fatal("search fixture", err)
			}
			return map[string]any{"result_token": result[0].Items[0].Token, "downloader_id": downloader.ID, "content_mode": "all", "expected_tmdb_id": 100, "expected_media_type": "tv"}
		}
		first := post("/api/v1/player/discovery/downloads", freshInput())
		if first.Code != http.StatusCreated {
			t.Fatalf("first HTTP acceptance %d %s", first.Code, first.Body.String())
		}
		var original models.DownloadTask
		if err := client.db.First(&original, "downloader_id = ?", downloader.ID).Error; err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()
		history := models.OfflineFailure{TaskID: original.ID, OwnerID: actor.User.ID, AccountNamespace: original.OfflineAccountNamespace, ResourceDigest: original.OfflineResourceDigest, RequiredDigest: original.OfflineRequiredDigest, Reason: services.CodeOfflineDeadlineExceeded, StartedAt: now.Add(-31 * time.Minute), DeadlineAt: now.Add(-time.Minute), CreatedAt: now}
		if err := client.db.Create(&history).Error; err != nil {
			t.Fatal(err)
		}
		body := freshInput()
		warning := post("/api/v1/discovery/downloads", body)
		var warningEnvelope struct {
			Data struct {
				Warning services.OfflineFailureWarning `json:"offline_failure_warning"`
			} `json:"data"`
		}
		if warning.Code != http.StatusConflict || json.Unmarshal(warning.Body.Bytes(), &warningEnvelope) != nil || len(warningEnvelope.Data.Warning.ConfirmationToken) != 64 {
			t.Fatalf("offline warning %d %s", warning.Code, warning.Body.String())
		}
		body["offline_failure_override_token"] = warningEnvelope.Data.Warning.ConfirmationToken
		accepted := post("/api/v1/player/discovery/downloads", body)
		if accepted.Code != http.StatusCreated {
			t.Fatalf("confirmed HTTP acceptance %d %s", accepted.Code, accepted.Body.String())
		}
		// The Site claim is consumed by success. An identical expected-TMDB request
		// must read its accepted identity before attempting fresh claim binding.
		replayed := post("/api/v1/player/discovery/downloads", body)
		if replayed.Code != http.StatusCreated || !bytes.Equal(replayed.Body.Bytes(), accepted.Body.Bytes()) {
			t.Fatalf("lost ACK replay %d %s", replayed.Code, replayed.Body.String())
		}
		body["expected_tmdb_id"] = 200
		changed := post("/api/v1/player/discovery/downloads", body)
		if changed.Code != http.StatusConflict {
			t.Fatalf("changed expected identity reused confirmation: %d %s", changed.Code, changed.Body.String())
		}
		var tasks int64
		if err := client.db.Model(&models.DownloadTask{}).Where("downloader_id = ?", downloader.ID).Count(&tasks).Error; err != nil || tasks != 2 {
			t.Fatalf("HTTP confirmation created duplicate tasks: %d %v", tasks, err)
		}
	})
}
