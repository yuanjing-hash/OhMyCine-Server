package services

import (
	"context"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestSourceRouteOrderPinsPTAndFallsThroughForPublicBT(t *testing.T) {
	_, _, actor, _, downloads, downloaders := siteFixture(t)
	actor.Permissions[authz.PermissionMediaLibrariesRead] = struct{}{}
	var profile models.MediaClassificationProfile
	if err := downloads.db.First(&profile, "code = ?", "default-v1").Error; err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	storage := models.Storage{Name: "Routing target", NameNormalized: "routing-target", Type: models.StorageTypeLocal, RootPath: root, RootPathNormalized: root, Enabled: true, Capabilities: `{}`}
	if err := downloads.db.Create(&storage).Error; err != nil {
		t.Fatal(err)
	}
	library := models.MediaLibrary{Name: "Routing library", NameNormalized: "routing-library", StorageID: storage.ID, ProfileID: profile.ID, ProfileRevision: profile.Revision, RelativeRoot: "/", SortOrder: 1, Enabled: true, Status: models.MediaLibraryStatusListening, TransferMode: models.MediaLibraryTransferMove, VideoExtensionsJSON: `[]`, STRMAssetExtraExtensionsJSON: `[]`, IgnorePatternsJSON: `[]`}
	if err := downloads.db.Create(&library).Error; err != nil {
		t.Fatal(err)
	}
	first, err := downloaders.Create(actor, DownloaderInput{Name: "A route", Type: models.DownloaderTypeQBittorrent, BaseURL: "http://first.example.test", Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := downloaders.Create(actor, DownloaderInput{Name: "B route", Type: models.DownloaderTypeQBittorrent, BaseURL: "http://second.example.test", Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	ordered, err := downloaders.Reorder(actor, []string{second.ID, first.ID}, RequestContext{})
	if err != nil || len(ordered) != 2 || ordered[0].ID != second.ID || ordered[0].SortOrder != 1 {
		t.Fatalf("reorder = %+v, %v", ordered, err)
	}
	pt := models.Site{Name: "PT route", NameNormalized: "pt-route", Kind: "pttime", BaseURL: "https://pt.example.test", Enabled: true, Revision: 1}
	bt := models.Site{Name: "BT route", NameNormalized: "bt-route", Kind: "mikan", BaseURL: "https://bt.example.test", Enabled: true, Revision: 1}
	for _, site := range []*models.Site{&pt, &bt} {
		if err := downloads.db.Create(site).Error; err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	ptRoute, err := downloads.RecommendSourceRoute(ctx, actor, pt.ID, &library.ID)
	if err != nil || ptRoute.Recommended == nil || ptRoute.Recommended.DownloaderID != second.ID {
		t.Fatalf("PT route = %+v, %v", ptRoute, err)
	}
	// The first qB becomes unusable for this target. PT must not silently use
	// the second qB, while public BT may continue to its next safe candidate.
	if err := downloads.db.Model(&models.Downloader{}).Where("id = ?", second.ID).Update("execution_location", models.NodeLocationRemote).Error; err != nil {
		t.Fatal(err)
	}
	ptRoute, err = downloads.RecommendSourceRoute(ctx, actor, pt.ID, &library.ID)
	if err != nil || ptRoute.Recommended != nil {
		t.Fatalf("PT silently fell back = %+v, %v", ptRoute, err)
	}
	btRoute, err := downloads.RecommendSourceRoute(ctx, actor, bt.ID, &library.ID)
	if err != nil || btRoute.Recommended == nil || btRoute.Recommended.DownloaderID != first.ID {
		t.Fatalf("public BT fallback = %+v, %v", btRoute, err)
	}
}

func TestOldPlayerFollowWriteRequiresUpgrade(t *testing.T) {
	legacy := FollowExecutionSnapshot{Version: 1, DownloaderID: "fixed"}
	if ErrorCode(requireSourceAwareFollowWrite(legacy)) != CodePlayerUpdateRequired {
		t.Fatal("old Player request did not receive explicit upgrade code")
	}
	current := FollowExecutionSnapshot{Version: 2, RoutingPolicy: "source_priority"}
	if err := requireSourceAwareFollowWrite(current); err != nil {
		t.Fatal(err)
	}
	if normalized := normalizeFollowExecutionSnapshot(legacy); normalized.Version != 2 || normalized.DownloaderID != "" || normalized.RoutingPolicy != "source_priority" {
		t.Fatalf("legacy subscription was not normalized: %+v", normalized)
	}
}

func TestPublicBTPrefersFirstUsableLibraryBeforeDownloader(t *testing.T) {
	libraries := []models.MediaLibrary{{ID: 10}, {ID: 20}}
	downloaders := []models.Downloader{{ID: "qbit-first", Type: models.DownloaderTypeQBittorrent}, {ID: "qbit-second", Type: models.DownloaderTypeQBittorrent}}
	choices := map[string]map[uint]SourceRouteChoice{
		"qbit-first":  {10: {Enabled: false}, 20: {Enabled: true}},
		"qbit-second": {10: {Enabled: true}, 20: {Enabled: true}},
	}
	selected := recommendedSourceRoute(RouteSourceBT, libraries, downloaders, choices)
	if selected == nil || selected.MediaLibraryID != 10 || selected.DownloaderID != "qbit-second" {
		t.Fatalf("public BT did not prefer the first usable library: %+v", selected)
	}
	selected = recommendedSourceRoute(RouteSourcePT, libraries, downloaders, choices)
	if selected == nil || selected.MediaLibraryID != 20 || selected.DownloaderID != "qbit-first" {
		t.Fatalf("PT changed downloader before trying its next library: %+v", selected)
	}
}

func TestFollowRuntimeDoesNotBlockOtherSitesForMissingRoute(t *testing.T) {
	queue, actor, _ := queueFixture(t)
	for _, permission := range []string{authz.PermissionDiscoveryRead, authz.PermissionDownloadsCreate, authz.PermissionMediaLibrariesRead} {
		actor.Permissions[permission] = struct{}{}
	}
	var profile models.MediaClassificationProfile
	if err := queue.db.First(&profile, "code = ?", "default-v1").Error; err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	storage := models.Storage{Name: "Route runtime", NameNormalized: "route-runtime", Type: models.StorageTypeLocal, RootPath: root, RootPathNormalized: root, Enabled: true, Capabilities: `{}`}
	if err := queue.db.Create(&storage).Error; err != nil {
		t.Fatal(err)
	}
	library := models.MediaLibrary{Name: "Route runtime", NameNormalized: "route-runtime", StorageID: storage.ID, ProfileID: profile.ID, RelativeRoot: "/", Enabled: true, Status: models.MediaLibraryStatusListening, VideoExtensionsJSON: `[]`, STRMAssetExtraExtensionsJSON: `[]`, IgnorePatternsJSON: `[]`}
	if err := queue.db.Create(&library).Error; err != nil {
		t.Fatal(err)
	}
	site := models.Site{Name: "Route PT", NameNormalized: "route-pt", Kind: "pttime", BaseURL: "https://pt.example.test", Enabled: true, Revision: 1}
	if err := queue.db.Create(&site).Error; err != nil {
		t.Fatal(err)
	}
	follows := NewFollowService(queue.db, queue.audit, queue, nil, nil)
	snapshot := FollowExecutionSnapshot{Version: 2, RoutingPolicy: "source_priority", Seasons: []int{1}, SiteIDs: []uint{site.ID}, MediaLibraryID: library.ID, Schedule: FollowSchedule{Kind: "interval", Minutes: 60}, MaxResourcesPerRun: 1}
	if _, _, err := follows.validateSnapshotWithRoutes(actor, 100, snapshot, true); err == nil {
		t.Fatal("new follow accepted without route")
	}
	if _, _, err := follows.validateSnapshotWithRoutes(actor, 100, snapshot, false); err != nil {
		t.Fatalf("worker blocked before candidate selection: %v", err)
	}
	other := site
	other.ID, other.Name, other.NameNormalized = 0, "Route restricted", "route-restricted"
	other.BaseURL = "https://restricted.example.test"
	if err := queue.db.Create(&other).Error; err != nil {
		t.Fatal(err)
	}
	snapshot.SiteIDs = []uint{other.ID, site.ID}
	actor.ResourceAccessPolicies = map[string]ResourceAccessPolicy{models.ResourceAccessScopeSiteSearch: {Scope: models.ResourceAccessScopeSiteSearch, Mode: models.ResourceAccessModeAllowlist, ResourceIDs: []string{uintID(site.ID)}}}
	filtered, _, err := follows.validateSnapshotWithRoutes(actor, 100, snapshot, false)
	if err != nil || len(filtered.SiteIDs) != 1 || filtered.SiteIDs[0] != site.ID || len(snapshot.SiteIDs) != 2 {
		t.Fatalf("site intersection=%+v err=%v", filtered.SiteIDs, err)
	}
	if _, _, err := follows.validateSnapshotWithRoutes(actor, 100, snapshot, true); ErrorCode(err) != CodePermissionDenied {
		t.Fatalf("new follow accepted unauthorized stored site: %v", err)
	}
	actor.ResourceAccessPolicies[models.ResourceAccessScopeSiteSearch] = ResourceAccessPolicy{Mode: models.ResourceAccessModeAllowlist}
	if _, _, err := follows.validateSnapshotWithRoutes(actor, 100, snapshot, false); ErrorCode(err) != CodePermissionDenied {
		t.Fatalf("empty authorized intersection=%v", err)
	}
}
