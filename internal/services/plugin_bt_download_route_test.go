package services

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	cloudpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/metadata/tmdb"
	"gorm.io/gorm"
)

type pluginBTRouteFixture struct {
	sites      *SiteService
	downloads  *DownloadService
	actor      Actor
	site       models.Site
	library    models.MediaLibrary
	downloader models.Downloader
	driver     *fakeMutationCloudDriver
	bridge     *pluginBTRouteBridge
}

type pluginBTRouteBridge struct {
	*stubPluginResourceBridge
	resolveCalls int
	resolveHook  func()
}

func (b *pluginBTRouteBridge) ResolveResource(ctx context.Context, input PluginResourceResolveInput) (PluginResourceResolveResult, error) {
	b.resolveCalls++
	if b.resolveHook != nil {
		b.resolveHook()
	}
	return b.stubPluginResourceBridge.ResolveResource(ctx, input)
}

func newPluginBTRouteFixture(t *testing.T) pluginBTRouteFixture {
	t.Helper()
	sites, _, actor, _, downloads, downloaders := siteFixture(t)
	actor.Permissions[authz.PermissionFollowsCreate] = struct{}{}
	persistFixtureAuthority(t, sites.db, actor)
	queue, now := downloads.queue, sites.now()
	connection := createRouteConnection(t, queue, "plugin-bt", now)
	driver := newFakeMutationCloudDriver()
	for _, item := range []cloudpkg.Item{{ID: "0", IsDir: true}, {ID: "root", ParentID: "0", IsDir: true}, {ID: "downloads", ParentID: "root", IsDir: true}, {ID: "library", ParentID: "root", IsDir: true}} {
		driver.items[item.ID] = item
	}
	downloaders.SetConnectionService(&ConnectionService{db: queue.db, drivers: map[uint]cloudpkg.Driver{connection.ID: driver}})
	storage := createRouteStorage(t, queue, "plugin-bt", connection.ID, "root", now)
	var profile models.MediaClassificationProfile
	if err := sites.db.First(&profile, "code = ?", "default-v1").Error; err != nil {
		t.Fatal(err)
	}
	library := createRouteLibrary(t, queue, "plugin-bt", storage.ID, profile.ID, "library", now)
	downloader := models.Downloader{ID: uuid.NewString(), Name: "115 fixture", NameNormalized: "plugin-bt-115", Type: models.DownloaderTypePan115Offline, StorageID: &storage.ID, ProviderDirectoryID: "downloads", Enabled: true, SortOrder: 1}
	if err := sites.db.Create(&downloader).Error; err != nil {
		t.Fatal(err)
	}
	bridge := &pluginBTRouteBridge{stubPluginResourceBridge: &stubPluginResourceBridge{pluginID: "org.ohmycine.fixture", version: "1.2.3", magnet: "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567"}}
	sites.SetPluginResourceBridge(bridge)
	pluginPackage := models.PluginPackage{PluginID: bridge.pluginID, Version: bridge.version, RepositoryOwner: "fixture", RepositoryRepo: "plugins", RegistryCommit: strings.Repeat("a", 40), RegistryEntryJSON: `{}`, ManifestURL: "https://example.test/manifest.json", PackageURL: "https://example.test/plugin.omcp", PackageSHA256: strings.Repeat("b", 64), ExtractedTreeSHA256: strings.Repeat("c", 64), ManifestJSON: `{}`, PackagePath: t.TempDir(), VerifiedAt: now, CreatedAt: now}
	if err := sites.db.Create(&pluginPackage).Error; err != nil {
		t.Fatal(err)
	}
	installation := models.PluginInstallation{PluginID: bridge.pluginID, ActivePackageID: pluginPackage.ID, Status: models.PluginInstallationEnabled, Revision: 1, InstalledAt: now, UpdatedAt: now}
	if err := sites.db.Create(&installation).Error; err != nil {
		t.Fatal(err)
	}
	pluginConnection := models.PluginConnection{ID: uuid.NewString(), PluginID: bridge.pluginID, Name: "Resource fixture", ConfigJSON: `{}`, CredentialMode: models.PluginCredentialModeCookie, ResourceType: "bt_resource", EntryOrigin: "https://resource.example.test", Enabled: true, Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := sites.db.Create(&pluginConnection).Error; err != nil {
		t.Fatal(err)
	}
	site := models.Site{Name: "Public plugin fixture", NameNormalized: "plugin-bt-fixture", Kind: pluginResourceKind, SourceType: "plugin", PluginID: bridge.pluginID, PluginConnectionID: pluginConnection.ID, BaseURL: pluginConnection.EntryOrigin, Enabled: true, LastHealthStatus: "healthy", Revision: 7, Priority: 100, TimeoutSeconds: 12, RateLimitPerMinute: 120}
	if err := sites.db.Create(&site).Error; err != nil {
		t.Fatal(err)
	}
	return pluginBTRouteFixture{sites: sites, downloads: downloads, actor: actor, site: site, library: library, downloader: downloader, driver: driver, bridge: bridge}
}

func (f pluginBTRouteFixture) claim(t *testing.T, site models.Site) (string, models.PluginResourceClaim) {
	t.Helper()
	now := f.sites.now()
	claimID, resourceID := uuid.NewString(), "fixture-resource"
	token, err := f.sites.issueClaim(siteResultClaim{ActorID: f.actor.User.ID, SiteID: site.ID, SiteRevision: site.Revision, PluginClaimID: claimID, TorrentID: resourceID, Title: "Fixture.Resource.2026", ExpiresAt: now.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	claim := models.PluginResourceClaim{ID: claimID, TokenHash: tokenDigest(token), OwnerID: f.actor.User.ID, SiteID: site.ID, PluginID: f.bridge.pluginID, PluginVersion: f.bridge.version, PluginConnectionID: f.site.PluginConnectionID, ResourceID: resourceID, Title: "Fixture.Resource.2026", ExpiresAt: now.Add(time.Minute), CreatedAt: now}
	if err := f.sites.db.Create(&claim).Error; err != nil {
		t.Fatal(err)
	}
	return token, claim
}

func TestPluginBT115PreviewRecommendationFollowAndHandoffAgree(t *testing.T) {
	f, ctx := newPluginBTRouteFixture(t), context.Background()
	token, claim := f.claim(t, f.site)
	preview, err := f.downloads.PreviewRoutes(ctx, f.actor, DownloadRoutePreviewInput{DownloaderID: f.downloader.ID, SourceKind: downloadpkg.SourceURL, SiteID: &f.site.ID})
	if err != nil || len(preview.Options) != 1 || !preview.Options[0].Enabled {
		t.Fatalf("trusted plugin BT preview = %+v, %v", preview, err)
	}
	recommendation, err := f.sites.RecommendSiteRoute(ctx, f.actor, SourceRouteRecommendationInput{ResultToken: token, MediaLibraryID: &f.library.ID})
	if err != nil || recommendation.SourceKind != RouteSourceBT || recommendation.Recommended == nil || recommendation.Recommended.DownloaderID != f.downloader.ID {
		t.Fatalf("plugin recommendation = %+v, %v", recommendation, err)
	}
	follows := NewFollowService(f.sites.db, f.sites.audit, f.downloads.queue, nil, nil)
	follows.downloads = f.downloads
	followPreview, err := follows.PreviewSourceRoutes(ctx, f.actor, FollowRoutePreviewInput{SiteIDs: []uint{f.site.ID}, MediaLibraryID: f.library.ID})
	if err != nil || !followPreview.Available || len(followPreview.Routes) != 1 || followPreview.Routes[0].SourceKind != RouteSourceBT {
		t.Fatalf("plugin follow preview = %+v, %v", followPreview, err)
	}
	snapshot := FollowExecutionSnapshot{Version: 2, RoutingPolicy: "source_priority", Seasons: []int{1}, SiteIDs: []uint{f.site.ID}, MediaLibraryID: f.library.ID, Schedule: FollowSchedule{Kind: "interval", Minutes: 60}, MaxResourcesPerRun: 1}
	if _, _, err := follows.validateSnapshotWithRoutes(f.actor, 100, snapshot, true); err != nil {
		t.Fatalf("plugin production follow route: %v", err)
	}
	follows.downloads = nil
	if _, _, err := follows.validateSnapshotWithRoutes(f.actor, 100, snapshot, true); err != nil {
		t.Fatalf("plugin isolated follow route: %v", err)
	}
	if f.bridge.resolveCalls != 0 || f.driver.statCalls != 0 {
		t.Fatal("preview/recommendation invoked resource resolve or provider")
	}
	result, err := f.sites.Download(ctx, f.actor, SiteDownloadInput{ResultToken: token, DownloaderID: f.downloader.ID, MediaLibraryID: &f.library.ID}, RequestContext{})
	if err != nil {
		t.Fatalf("plugin BT handoff: %v", err)
	}
	var task models.DownloadTask
	if err := f.sites.db.First(&task, "id = ?", result.ID).Error; err != nil {
		t.Fatal(err)
	}
	if f.bridge.resolveCalls != 1 || task.PluginID != f.site.PluginID || task.PluginConnectionID != f.site.PluginConnectionID || task.PluginResourceClaimID != claim.ID || task.TargetLibraryID == nil || *task.TargetLibraryID != f.library.ID || task.Phase != models.DownloadTaskStatusQueued {
		t.Fatal("handoff lost trusted plugin provenance, library or queue state")
	}
	plaintext, err := f.downloads.credentials.Decrypt(downloadSourcePurpose(task.ID), task.SourceCiphertext)
	var source downloadSourceEnvelope
	if err != nil || json.Unmarshal([]byte(plaintext), &source) != nil || source.Kind != downloadpkg.SourceURL || source.URL != f.bridge.magnet {
		t.Fatal("plugin magnet was not preserved in encrypted source")
	}
	if err := f.sites.db.First(&claim, "id = ?", claim.ID).Error; err != nil || claim.ConsumedAt == nil {
		t.Fatal("plugin claim was not atomically consumed")
	}
	if _, err := f.sites.Download(ctx, f.actor, SiteDownloadInput{ResultToken: token, DownloaderID: f.downloader.ID}, RequestContext{}); ErrorCode(err) != CodeSiteResultExpired || f.bridge.resolveCalls != 1 {
		t.Fatalf("replayed claim reached resolve: %v", err)
	}
}

func TestPluginBT115PreviewRejectsPrivateUnknownAndForgedOrigins(t *testing.T) {
	f, ctx := newPluginBTRouteFixture(t), context.Background()
	for index, site := range []models.Site{
		{Name: "BT_RESOURCE label on private site", Kind: "nexusphp"},
		{Name: "Unknown source", Kind: "unknown"},
		{Name: "Untrusted plugin kind", Kind: pluginResourceKind, SourceType: "builtin"},
		{Name: "Forged plugin source on PT", Kind: "nexusphp", SourceType: "plugin"},
		{Name: "Missing plugin identity", Kind: pluginResourceKind, SourceType: "plugin", PluginConnectionID: uuid.NewString()},
		{Name: "Missing connection identity", Kind: pluginResourceKind, SourceType: "plugin", PluginID: f.site.PluginID},
		{Name: "Plugin identity with built-in BT kind", Kind: "mikan", SourceType: "plugin", PluginID: f.site.PluginID, PluginConnectionID: uuid.NewString()},
	} {
		site.NameNormalized, site.BaseURL, site.Enabled = "denied-"+uuid.NewString(), "https://fixture.example.test", true
		if err := f.sites.db.Create(&site).Error; err != nil {
			t.Fatal(err)
		}
		for _, kind := range []string{downloadpkg.SourceURL, downloadpkg.SourceTorrent, downloadpkg.SourcePan115Share} {
			if _, err := f.downloads.PreviewRoutes(ctx, f.actor, DownloadRoutePreviewInput{DownloaderID: f.downloader.ID, SiteID: &site.ID, SourceKind: kind}); ErrorCode(err) != CodeDownloadSourceInvalid {
				t.Fatalf("untrusted site %d source %s accepted: %v", index, kind, err)
			}
		}
	}
	if _, err := f.downloads.PreviewRoutes(ctx, f.actor, DownloadRoutePreviewInput{DownloaderID: f.downloader.ID, SourceKind: "bt_resource", SiteID: &f.site.ID}); ErrorCode(err) != CodeInvalidRequest {
		t.Fatalf("forged client source kind accepted: %v", err)
	}
	if _, err := f.downloads.PreviewRoutes(ctx, f.actor, DownloadRoutePreviewInput{DownloaderID: f.downloader.ID, SourceKind: downloadpkg.SourceTorrent}); ErrorCode(err) != CodeDownloadSourceInvalid {
		t.Fatalf("unbound torrent accepted: %v", err)
	}
	var count int64
	if err := f.sites.db.Model(&models.DownloadTask{}).Count(&count).Error; err != nil || count != 0 || f.bridge.resolveCalls != 0 || f.driver.statCalls != 0 {
		t.Fatal("rejected preview caused resolve/provider/task side effects")
	}
}

func TestPluginBT115SubmitAndHandoffRejectUntrustedSitesBeforeSideEffects(t *testing.T) {
	f, ctx := newPluginBTRouteFixture(t), context.Background()
	for _, kind := range []string{"nexusphp", "unknown", pluginResourceKind} {
		site := models.Site{Name: "BT_RESOURCE forged label", NameNormalized: uuid.NewString(), Kind: kind, SourceType: "builtin", BaseURL: "https://fixture.example.test", Enabled: true, Revision: 1}
		if err := f.sites.db.Create(&site).Error; err != nil {
			t.Fatal(err)
		}
		token, claim := f.claim(t, site)
		if _, err := f.sites.Download(ctx, f.actor, SiteDownloadInput{ResultToken: token, DownloaderID: f.downloader.ID, MediaLibraryID: &f.library.ID}, RequestContext{}); ErrorCode(err) != CodeDownloadSourceInvalid {
			t.Fatalf("untrusted %s handoff accepted: %v", kind, err)
		}
		if _, err := f.downloads.Submit(ctx, f.actor, SubmitDownloadInput{SourceSiteID: site.ID, DownloaderID: f.downloader.ID, MediaLibraryID: &f.library.ID, Source: DownloadSourceInput{Kind: downloadpkg.SourceURL, URL: f.bridge.magnet}}, RequestContext{}); ErrorCode(err) != CodeDownloadSourceInvalid {
			t.Fatalf("untrusted %s internal Submit accepted: %v", kind, err)
		}
		if err := f.sites.db.First(&claim, "id = ?", claim.ID).Error; err != nil || claim.ConsumedAt != nil {
			t.Fatal("rejected handoff consumed plugin claim")
		}
	}
	for _, source := range []DownloadSourceInput{{Kind: "bt_resource", URL: f.bridge.magnet}, {Kind: downloadpkg.SourceTorrent, Torrent: []byte("d4:infod4:name4:testee"), Filename: "public-label.torrent"}} {
		if _, err := f.downloads.Submit(ctx, f.actor, SubmitDownloadInput{SourceSiteID: f.site.ID, DownloaderID: f.downloader.ID, Source: source}, RequestContext{}); ErrorCode(err) != CodeDownloadSourceInvalid {
			t.Fatalf("client kind or raw torrent bypassed115 guard: %v", err)
		}
	}
	var taskCount, jobCount int64
	if err := f.sites.db.Model(&models.DownloadTask{}).Count(&taskCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.sites.db.Model(&models.Job{}).Where("job_type = ?", "download").Count(&jobCount).Error; err != nil || taskCount != 0 || jobCount != 0 || f.bridge.resolveCalls != 0 || f.driver.statCalls != 0 {
		t.Fatal("denied source performed resolve/provider/enqueue work")
	}
}

func TestPluginBT115ClaimBindingsAndResourceScopesRemainRequired(t *testing.T) {
	f, ctx := newPluginBTRouteFixture(t), context.Background()
	for _, scope := range []string{models.ResourceAccessScopeSiteSearch, models.ResourceAccessScopeDownloaderUse, models.ResourceAccessScopeLibraryIngest} {
		token, claim := f.claim(t, f.site)
		actor := f.actor
		actor.ResourceAccessPolicies = map[string]ResourceAccessPolicy{scope: {Mode: models.ResourceAccessModeAllowlist}}
		if _, err := f.sites.RecommendSiteRoute(ctx, actor, SourceRouteRecommendationInput{ResultToken: token, MediaLibraryID: &f.library.ID}); ErrorCode(err) != CodePermissionDenied {
			t.Fatalf("recommendation ignored %s policy: %v", scope, err)
		}
		if _, err := f.sites.Download(ctx, actor, SiteDownloadInput{ResultToken: token, DownloaderID: f.downloader.ID, MediaLibraryID: &f.library.ID}, RequestContext{}); ErrorCode(err) != CodePermissionDenied {
			t.Fatalf("handoff ignored %s policy: %v", scope, err)
		}
		if err := f.sites.db.First(&claim, "id = ?", claim.ID).Error; err != nil || claim.ConsumedAt != nil {
			t.Fatal("permission rejection consumed claim")
		}
	}
	for _, drift := range []string{"owner", "version", "resource", "connection", "site_plugin", "revision", "unavailable"} {
		t.Run(drift, func(t *testing.T) {
			token, claim := f.claim(t, f.site)
			switch drift {
			case "owner":
				f.sites.vaultMu.Lock()
				cached := f.sites.vault[token]
				cached.ActorID++
				f.sites.vault[token] = cached
				f.sites.vaultMu.Unlock()
			case "version":
				if err := f.sites.db.Model(&claim).Update("plugin_version", "9.9.9").Error; err != nil {
					t.Fatal(err)
				}
			case "resource":
				f.sites.vaultMu.Lock()
				cached := f.sites.vault[token]
				cached.TorrentID = "different-resource"
				f.sites.vault[token] = cached
				f.sites.vaultMu.Unlock()
			case "connection":
				var connection models.PluginConnection
				if err := f.sites.db.First(&connection, "id = ?", f.site.PluginConnectionID).Error; err != nil {
					t.Fatal(err)
				}
				connection.ID, connection.Name = uuid.NewString(), "Other resource connection"
				if err := f.sites.db.Create(&connection).Error; err != nil {
					t.Fatal(err)
				}
				if err := f.sites.db.Model(&claim).Update("plugin_connection_id", connection.ID).Error; err != nil {
					t.Fatal(err)
				}
			case "revision":
				original := f.site.Revision
				if err := f.sites.db.Model(&models.Site{}).Where("id = ?", f.site.ID).Update("revision", original+1).Error; err != nil {
					t.Fatal(err)
				}
				defer func() {
					if err := f.sites.db.Model(&models.Site{}).Where("id = ?", f.site.ID).Update("revision", original).Error; err != nil {
						t.Error(err)
					}
				}()
			case "site_plugin":
				if err := f.sites.db.Model(&models.Site{}).Where("id = ?", f.site.ID).Update("plugin_id", "org.ohmycine.other").Error; err != nil {
					t.Fatal(err)
				}
				defer func() {
					if err := f.sites.db.Model(&models.Site{}).Where("id = ?", f.site.ID).Update("plugin_id", f.site.PluginID).Error; err != nil {
						t.Error(err)
					}
				}()
			case "unavailable":
				f.bridge.provenanceErr = appError(CodePluginRuntimeUnavailable, "插件当前不可用", nil)
				defer func() { f.bridge.provenanceErr = nil }()
			}
			_, err := f.sites.Download(ctx, f.actor, SiteDownloadInput{ResultToken: token, DownloaderID: f.downloader.ID, MediaLibraryID: &f.library.ID}, RequestContext{})
			want := CodeSiteResultExpired
			if drift == "unavailable" {
				want = CodePluginRuntimeUnavailable
			}
			if ErrorCode(err) != want || f.bridge.resolveCalls != 0 {
				t.Fatalf("claim drift %s reached resolve: %v", drift, err)
			}
		})
	}
}

func TestPluginBT115RechecksClassificationAfterResolveAndAtPersist(t *testing.T) {
	for _, phase := range []string{"resolve", "persist"} {
		t.Run(phase, func(t *testing.T) {
			f, ctx := newPluginBTRouteFixture(t), context.Background()
			token, claim := f.claim(t, f.site)
			input := SiteDownloadInput{ResultToken: token, DownloaderID: f.downloader.ID, MediaLibraryID: &f.library.ID}
			mutate := func(db *gorm.DB) error {
				return db.Model(&models.Site{}).Where("id = ?", f.site.ID).Updates(map[string]any{"kind": "nexusphp", "source_type": "builtin"}).Error
			}
			if phase == "resolve" {
				f.bridge.resolveHook = func() {
					if err := mutate(f.sites.db); err != nil {
						t.Fatal(err)
					}
				}
			} else {
				input.BeforePersist = mutate
			}
			expectedCode := CodeDownloadSourceInvalid
			if phase == "persist" {
				// The final plugin lifecycle fence detects the changed source before
				// the general downloader classification check in the same transaction.
				expectedCode = CodeSiteResultExpired
			}
			if _, err := f.sites.Download(ctx, f.actor, input, RequestContext{}); ErrorCode(err) != expectedCode {
				t.Fatalf("source drift %s expected=%s code=%s err=%v", phase, expectedCode, ErrorCode(err), err)
			}
			var count int64
			if err := f.sites.db.Model(&models.DownloadTask{}).Count(&count).Error; err != nil || count != 0 {
				t.Fatal("drifted source created a task")
			}
			if err := f.sites.db.Model(&models.Job{}).Where("job_type = ?", "download").Count(&count).Error; err != nil || count != 0 {
				t.Fatal("drifted source left an enqueued job")
			}
			if err := f.sites.db.First(&claim, "id = ?", claim.ID).Error; err != nil || claim.ConsumedAt != nil {
				t.Fatal("source drift consumed claim instead of rolling back")
			}
		})
	}
}

func TestPluginBT115FollowDefaultsRequireCurrentLifecycleAndScope(t *testing.T) {
	f, ctx := newPluginBTRouteFixture(t), context.Background()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":100,"name":"Fixture","first_air_date":"2025-01-01","seasons":[]}`))
	}))
	defer upstream.Close()
	metadata := NewMetadataSettingsService(f.sites.db, f.sites.audit, f.downloads.credentials, tmdb.Credential{Kind: tmdb.CredentialKindReadAccessToken, Value: "fixture-token"})
	metadata.clientFactory = func(tmdb.Credential, string, string) (*tmdb.Client, error) {
		return tmdb.NewForTest("fixture-token", upstream.URL, upstream.Client())
	}
	follows := NewFollowService(f.sites.db, f.sites.audit, f.downloads.queue, NewMediaCoverageService(f.sites.db, metadata), nil)
	follows.SetDownloadService(f.downloads)
	defaults, err := follows.Defaults(ctx, f.actor, 100)
	if err != nil || len(defaults.Sites) != 1 || defaults.Sites[0].SiteType != "bt_resource" {
		t.Fatalf("healthy plugin excluded from Follow defaults: %+v, %v", defaults.Sites, err)
	}
	actor := f.actor
	actor.ResourceAccessPolicies = map[string]ResourceAccessPolicy{models.ResourceAccessScopeSiteSearch: {Mode: models.ResourceAccessModeAllowlist}}
	defaults, err = follows.Defaults(ctx, actor, 100)
	if err != nil || len(defaults.Sites) != 0 {
		t.Fatal("Follow defaults exposed denied plugin site")
	}
	if err := f.sites.db.Model(&models.Site{}).Where("id = ?", f.site.ID).Update("plugin_id", "org.ohmycine.other").Error; err != nil {
		t.Fatal(err)
	}
	defaults, err = follows.Defaults(ctx, f.actor, 100)
	if err != nil || len(defaults.Sites) != 0 {
		t.Fatal("Follow defaults exposed mismatched plugin ownership")
	}
	if err := f.sites.db.Model(&models.Site{}).Where("id = ?", f.site.ID).Update("plugin_id", f.site.PluginID).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.sites.db.Model(&models.PluginConnection{}).Where("id = ?", f.site.PluginConnectionID).Update("enabled", false).Error; err != nil {
		t.Fatal(err)
	}
	defaults, err = follows.Defaults(ctx, f.actor, 100)
	if err != nil || len(defaults.Sites) != 0 {
		t.Fatal("Follow defaults exposed disabled connection")
	}
	if err := f.sites.db.Model(&models.PluginConnection{}).Where("id = ?", f.site.PluginConnectionID).Update("enabled", true).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.sites.db.Model(&models.PluginInstallation{}).Where("plugin_id = ?", f.site.PluginID).Update("status", models.PluginInstallationDisabled).Error; err != nil {
		t.Fatal(err)
	}
	defaults, err = follows.Defaults(ctx, f.actor, 100)
	if err != nil || len(defaults.Sites) != 0 {
		t.Fatal("Follow defaults exposed disabled installation")
	}
}

func TestPluginBTAcceptedWorkerKeepsFrozenSourceAfterPluginDisabled(t *testing.T) {
	f, ctx := newPluginBTRouteFixture(t), context.Background()
	qbit, err := f.downloads.downloader.Create(f.actor, DownloaderInput{Name: "Accepted plugin fixture", Type: models.DownloaderTypeQBittorrent, BaseURL: "http://qbit.example.test", Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	token, _ := f.claim(t, f.site)
	result, err := f.sites.Download(ctx, f.actor, SiteDownloadInput{ResultToken: token, DownloaderID: qbit.ID}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	var task models.DownloadTask
	if err := f.sites.db.First(&task, "id = ?", result.ID).Error; err != nil {
		t.Fatal(err)
	}
	var job models.Job
	if err := f.sites.db.First(&job, "id = ?", task.JobID).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.sites.db.Model(&models.PluginInstallation{}).Where("plugin_id = ?", f.site.PluginID).Update("status", models.PluginInstallationDisabled).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.sites.db.Model(&models.Site{}).Where("id = ?", f.site.ID).Update("kind", "nexusphp").Error; err != nil {
		t.Fatal(err)
	}
	loaded, _, _, source, _, err := NewDownloadWorker(f.downloads).load(ctx, ClaimedJob{Job: job})
	if err != nil || source.URL != f.bridge.magnet || source.Kind != downloadpkg.SourceURL || loaded.PluginID != f.site.PluginID || loaded.PluginVersion != f.bridge.version || f.bridge.resolveCalls != 1 {
		t.Fatalf("accepted worker did not retain its frozen encrypted source: %v", err)
	}
}
