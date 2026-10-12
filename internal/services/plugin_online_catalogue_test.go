package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/plugins/contract"
	"gorm.io/gorm"
)

type catalogueTestRuntime struct {
	mu      sync.Mutex
	calls   map[string]int
	handler func(context.Context, string, []byte) ([]byte, error)
}

func (*catalogueTestRuntime) Validate(context.Context, string) error              { return nil }
func (*catalogueTestRuntime) Start(context.Context, string, string, uint64) error { return nil }
func (*catalogueTestRuntime) Stop(string) error                                   { return nil }
func (*catalogueTestRuntime) Close(context.Context) error                         { return nil }
func (r *catalogueTestRuntime) Invoke(ctx context.Context, _ string, op string, input []byte) ([]byte, error) {
	r.mu.Lock()
	r.calls[op]++
	handler := r.handler
	r.mu.Unlock()
	return handler(ctx, op, input)
}
func (r *catalogueTestRuntime) count(op string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[op]
}
func (r *catalogueTestRuntime) set(handler func(context.Context, string, []byte) ([]byte, error)) {
	r.mu.Lock()
	r.handler = handler
	r.mu.Unlock()
}
func catalogueResponse(_ context.Context, op string, input []byte) ([]byte, error) {
	switch op {
	case "site.navigation":
		var request struct {
			Parent string `json:"parentNodeKey"`
		}
		_ = json.Unmarshal(input, &request)
		if request.Parent != "" {
			return []byte(`{"version":2,"mode":"hierarchical","nodes":[{"id":"recommended","title":"最新","kind":"feed","routeKey":"catalog:2"}]}`), nil
		}
		return []byte(`{"version":2,"mode":"hierarchical","nodes":[{"id":"category:2","title":"电视剧","kind":"branch","nodeKey":"category:2","hasChildren":true}]}`), nil
	case "site.feed":
		return []byte(`[{"id":"feed","title":"最新","layout":"poster-grid","refreshable":true,"homeEligible":true,"items":[{"work":{"id":"work:1","title":"官方作品","kind":"series","posterUrl":"https://login.example.test/poster.jpg","identity":{"scheme":"fixture","value":"work:1"}}}]}]`), nil
	case "site.detail":
		return []byte(`{"id":"work:1","title":"官方作品","kind":"series","posterUrl":"https://login.example.test/poster.jpg","identity":{"scheme":"fixture","value":"work:1"},"defaultSegmentId":"special:1","segments":[{"id":"main:1","title":"第1期","kind":"episode","episodeNumber":1,"versions":[{"id":"full","label":"正片","variants":[]}]},{"id":"special:1","title":"第1期彩蛋","kind":"episode","versions":[{"id":"bonus","label":"彩蛋","variants":[]}]}]}`), nil
	case "media.playback":
		var in struct {
			Work    string `json:"itemId"`
			Segment string `json:"segmentId"`
			Version string `json:"versionId"`
			Variant string `json:"variantId"`
		}
		_ = json.Unmarshal(input, &in)
		return []byte(fmt.Sprintf(`{"workId":%q,"segmentId":%q,"versionId":%q,"variantId":%q,"variants":[],"assets":[{"kind":"progressive","urlRef":"11111111-1111-4111-8111-111111111111"}],"delivery":"server-gateway"}`, in.Work, in.Segment, in.Version, in.Variant)), nil
	case "playback.progress_sync":
		return nil, errors.New("upstream progress unavailable")
	}
	return nil, errors.New("unexpected operation")
}
func catalogueFixture(t *testing.T) (*PluginRepositoryService, Actor, string, *catalogueTestRuntime) {
	t.Helper()
	s, actor, _ := pluginRepositoryFixture(t)
	actor.Permissions[authz.PermissionMediaLibrariesRead] = struct{}{}
	now := time.Now().UTC()
	pluginID := "org.ohmycine.catalogue-test"
	manifest := fmt.Sprintf(`{"schemaVersion":1,"id":%q,"name":"目录测试","description":"fixture","version":"0.1.0","apiVersion":"1","minServerVersion":"0.1.0","runtime":"wasm","entry":"plugin.wasm","navigationMode":"hierarchical","capabilities":["site.navigation","site.feed","site.detail","media.playback","feed.refresh"],"permissions":[{"kind":"network.http","domains":["login.example.test"]}],"configSchema":{"type":"object"},"author":"test","license":"MIT","homepage":"https://example.test/plugin","source":"https://github.com/example/plugin","packageSha256":%q}`, pluginID, strings.Repeat("a", 64))
	pkg := models.PluginPackage{PluginID: pluginID, Version: "0.1.0", ManifestJSON: manifest, PackageSHA256: strings.Repeat("a", 64), CreatedAt: now, VerifiedAt: now}
	mustCatalogue(t, s.db.Create(&pkg).Error)
	mustCatalogue(t, s.db.Create(&models.PluginInstallation{PluginID: pluginID, ActivePackageID: pkg.ID, Status: models.PluginInstallationEnabled, RuntimeGeneration: 1, Revision: 1, InstalledAt: now, UpdatedAt: now}).Error)
	id := uuid.NewString()
	mustCatalogue(t, s.db.Create(&models.PluginConnection{ID: id, PluginID: pluginID, Name: "官方目录", ConfigJSON: `{}`, CredentialMode: models.PluginCredentialModeNone, Enabled: true, Revision: 1, CreatedAt: now, UpdatedAt: now}).Error)
	mustCatalogue(t, s.db.Create(&models.PluginOnlineLibrary{ID: id, PluginID: pluginID, ConnectionID: id, ExternalKey: "default", Name: "官方目录", HomeContributionsJSON: `[]`, Enabled: true, Revision: 1, CreatedAt: now, UpdatedAt: now}).Error)
	r := &catalogueTestRuntime{calls: map[string]int{}, handler: catalogueResponse}
	s.runtime = r
	s.artwork = &testOnlineArtworkGateway{}
	return s, actor, id, r
}
func mustCatalogue(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func catalogueNode(t *testing.T, raw []byte) pluginNavigationNode {
	t.Helper()
	var response pluginNavigationResponse
	mustCatalogue(t, json.Unmarshal(raw, &response))
	if len(response.Nodes) != 1 {
		t.Fatal("wrong node count")
	}
	return response.Nodes[0]
}

func TestOnlineCataloguePersistentRestartScopeAndProtectedProjection(t *testing.T) {
	s, actor, id, r := catalogueFixture(t)
	ctx := context.Background()
	root, err := s.OnlineNavigation(ctx, actor, id)
	mustCatalogue(t, err)
	node := catalogueNode(t, root)
	if node.ID != "category:2" || node.Title != "电视剧" || node.Kind != "branch" || node.NodeToken == "" {
		t.Fatal("official directory schema lost")
	}
	_, err = s.OnlineNavigationChildren(ctx, actor, id, node.NodeToken)
	mustCatalogue(t, err)
	first, err := s.OnlineFeed(ctx, actor, id, "catalog:2", "", "")
	mustCatalogue(t, err)
	for i := 0; i < 3; i++ {
		_, err = s.OnlineNavigation(ctx, actor, id)
		mustCatalogue(t, err)
		_, err = s.OnlineNavigationChildren(ctx, actor, id, node.NodeToken)
		mustCatalogue(t, err)
		hit, e := s.OnlineFeed(ctx, actor, id, "catalog:2", "", "")
		mustCatalogue(t, e)
		if !strings.Contains(string(hit), "/api/v1/player/artwork/") {
			t.Fatal("cached image was not re-registered")
		}
	}
	if r.count("site.navigation") != 2 || r.count("site.feed") != 1 || !strings.Contains(string(first), "/api/v1/player/artwork/") {
		t.Fatal("cache hit invoked upstream or lost artwork")
	}
	var rows []models.PluginCatalogueSnapshot
	mustCatalogue(t, s.db.Find(&rows).Error)
	for _, row := range rows {
		if strings.Contains(row.ResponseJSON, "nodeToken") || strings.Contains(row.ResponseJSON, "/api/v1/") || strings.Contains(row.ResponseJSON, "token=") {
			t.Fatal("process ticket/private URL persisted")
		}
	}
	mustCatalogue(t, s.db.Model(&models.PluginInstallation{}).Where("plugin_id = ?", "org.ohmycine.catalogue-test").Update("runtime_generation", 2).Error)
	restarted := NewPluginRepositoryService(s.db, s.audit, s.fetcher, s.log)
	restarted.runtime = r
	restarted.artwork = &testOnlineArtworkGateway{}
	root, err = restarted.OnlineNavigation(ctx, actor, id)
	mustCatalogue(t, err)
	fresh := catalogueNode(t, root)
	if fresh.NodeToken == node.NodeToken || r.count("site.navigation") != 2 {
		t.Fatal("restart failed to reuse snapshot and mint process-owned token")
	}
	if _, err = restarted.OnlineNavigationChildren(ctx, actor, id, node.NodeToken); ErrorCode(err) != CodeInvalidRequest {
		t.Fatal("previous process token accepted")
	}
	_, err = restarted.OnlineNavigationChildren(ctx, actor, id, fresh.NodeToken)
	mustCatalogue(t, err)
	mustCatalogue(t, s.db.Model(&models.PluginConnection{}).Where("id = ?", id).Updates(map[string]any{"revision": 99, "last_health_status": "healthy"}).Error)
	_, err = restarted.OnlineNavigation(ctx, actor, id)
	mustCatalogue(t, err)
	if r.count("site.navigation") != 2 {
		t.Fatal("health revision invalidated catalogue")
	}
	mustCatalogue(t, s.db.Model(&models.PluginConnection{}).Where("id = ?", id).Update("credential_version", 1).Error)
	_, err = restarted.OnlineNavigation(ctx, actor, id)
	mustCatalogue(t, err)
	if r.count("site.navigation") != 3 {
		t.Fatal("credential change reused old scope")
	}
	mustCatalogue(t, s.db.Model(&models.PluginConnection{}).Where("id = ?", id).Update("config_json", `{"region":"new"}`).Error)
	_, err = restarted.OnlineNavigation(ctx, actor, id)
	mustCatalogue(t, err)
	if r.count("site.navigation") != 4 {
		t.Fatal("config change reused old scope")
	}
	if _, err = restarted.OnlineNavigation(ctx, Actor{}, id); ErrorCode(err) != CodePermissionDenied {
		t.Fatal("warm cache bypassed authorization")
	}
	mustCatalogue(t, s.db.Model(&models.PluginOnlineLibrary{}).Where("id = ?", id).Update("enabled", false).Error)
	if _, err = restarted.OnlineNavigation(ctx, actor, id); ErrorCode(err) != CodeNotFound {
		t.Fatal("disabled library reused cache")
	}
}

func TestOnlineCatalogueStaleFailureBackoffHardExpiryAndFirstFailure(t *testing.T) {
	s, actor, id, r := catalogueFixture(t)
	now := time.Now().UTC()
	s.catalogueState().now = func() time.Time { return now }
	ctx := context.Background()
	_, err := s.OnlineNavigation(ctx, actor, id)
	mustCatalogue(t, err)
	scope, err := s.catalogueScope(id)
	mustCatalogue(t, err)
	now = now.Add(13 * time.Hour)
	r.set(func(context.Context, string, []byte) ([]byte, error) {
		return []byte(`{"pluginError":{"code":"upstream-unavailable","message":"secret-provider-url"}}`), nil
	})
	_, err = s.OnlineNavigation(ctx, actor, id)
	mustCatalogue(t, err)
	if r.count("site.navigation") != 1 {
		t.Fatal("stale read blocked on failed upstream")
	}
	_, _, err = s.refreshDueCatalogue(ctx, scope, "navigation", "", 0, nil)
	if ErrorCode(err) != CodePluginOnlineLibraryUnavailable {
		t.Fatal("refresh failure lost stable error")
	}
	_, _, err = s.refreshDueCatalogue(ctx, scope, "navigation", "", 0, nil)
	if r.count("site.navigation") != 2 || ErrorCode(err) != CodePluginOnlineRateLimited {
		t.Fatal("failed refresh ignored backoff")
	}
	_, err = s.OnlineNavigation(ctx, actor, id)
	mustCatalogue(t, err)
	now = now.Add(12 * time.Hour)
	if _, err = s.OnlineNavigation(ctx, actor, id); ErrorCode(err) != CodePluginOnlineLibraryUnavailable {
		t.Fatal("hard-expired snapshot was served")
	}
	before := r.count("site.navigation")
	_, _, _ = s.refreshDueCatalogue(ctx, scope, "navigation", "uncached-category", 1, []string{"uncached-category"})
	_, _, _ = s.refreshDueCatalogue(ctx, scope, "navigation", "uncached-category", 1, []string{"uncached-category"})
	if r.count("site.navigation") != before+1 {
		t.Fatal("first-ever failure hammered provider")
	}
	var row models.PluginCatalogueSnapshot
	mustCatalogue(t, s.db.First(&row, "id = ?", catalogueID(scope, "navigation", "", "")).Error)
	if strings.Contains(row.ResponseJSON, "pluginError") {
		t.Fatal("failure replaced last good snapshot")
	}
}

func TestOnlineCatalogueCoalescesCancelsFencesAndStopsWarmup(t *testing.T) {
	s, actor, id, r := catalogueFixture(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	r.set(func(ctx context.Context, op string, input []byte) ([]byte, error) {
		select {
		case <-entered:
		default:
			close(entered)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
			return catalogueResponse(ctx, op, input)
		}
	})
	done := make(chan error, 1)
	go func() { _, err := s.OnlineNavigation(context.Background(), actor, id); done <- err }()
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.OnlineNavigation(ctx, actor, id); !errors.Is(err, context.Canceled) {
		t.Fatal("coalesced waiter ignored cancellation")
	}
	mustCatalogue(t, s.db.Model(&models.PluginConnection{}).Where("id = ?", id).Update("credential_version", 1).Error)
	close(release)
	if err := <-done; err == nil {
		t.Fatal("stale invocation committed after credential change")
	}
	var count int64
	mustCatalogue(t, s.db.Model(&models.PluginCatalogueSnapshot{}).Count(&count).Error)
	if count != 0 {
		t.Fatal("stale cache write survived generation fence")
	}
	stopped := make(chan struct{})
	entered = make(chan struct{})
	r.set(func(ctx context.Context, _ string, _ []byte) ([]byte, error) {
		close(entered)
		<-ctx.Done()
		close(stopped)
		return nil, ctx.Err()
	})
	start := time.Now()
	s.StartOnlineCatalogue(context.Background())
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("startup waited for provider")
	}
	<-entered
	s.CloseOnlineCatalogue()
	select {
	case <-stopped:
	default:
		t.Fatal("shutdown did not join provider cancellation")
	}
	if len(s.catalogueState().flights) != 0 {
		t.Fatal("stopped generation retained flights")
	}
}

func TestOnlineCatalogueWarmupFeedsAndForegroundPriority(t *testing.T) {
	s, _, id, r := catalogueFixture(t)
	s.warmOnlineCatalogue(context.Background())
	if r.count("site.navigation") != 2 || r.count("site.feed") != 1 {
		t.Fatal("warmup failed to populate root/category/first feed")
	}
	scope, err := s.catalogueScope(id)
	mustCatalogue(t, err)
	release := s.foregroundOnline()
	_, _, err = s.refreshDueCatalogue(context.Background(), scope, "feed", "new-route", 0, nil)
	release()
	if ErrorCode(err) != CodePluginOnlineRateLimited || r.count("site.feed") != 1 {
		t.Fatal("prefetch competed with foreground")
	}
}

func TestOnlineCatalogueWarmsFlatAndHierarchicalRootFeeds(t *testing.T) {
	for _, mode := range []string{"flat", "hierarchical"} {
		t.Run(mode, func(t *testing.T) {
			s, _, _, r := catalogueFixture(t)
			if mode == "flat" {
				var pkg models.PluginPackage
				mustCatalogue(t, s.db.First(&pkg).Error)
				mustCatalogue(t, s.db.Model(&pkg).Update("manifest_json", strings.Replace(pkg.ManifestJSON, `"navigationMode":"hierarchical"`, `"navigationMode":"flat"`, 1)).Error)
			}
			r.set(func(ctx context.Context, op string, in []byte) ([]byte, error) {
				if op == "site.navigation" {
					if mode == "flat" {
						return []byte(`[{"id":"latest","title":"最新","pageType":"feed","routeKey":"catalog:2"}]`), nil
					}
					return []byte(`{"version":2,"mode":"hierarchical","nodes":[{"id":"latest","title":"最新","kind":"feed","routeKey":"catalog:2"}]}`), nil
				}
				return catalogueResponse(ctx, op, in)
			})
			s.warmOnlineCatalogue(context.Background())
			if r.count("site.navigation") != 1 || r.count("site.feed") != 1 {
				t.Fatal("default root feed not prewarmed")
			}
		})
	}
}

func TestOnlineCatalogueDurableCursorAndCombinedByteBounds(t *testing.T) {
	s, _, id, _ := catalogueFixture(t)
	scope, err := s.catalogueScope(id)
	mustCatalogue(t, err)
	for _, cursor := range []string{"page:2", "https://provider.invalid?token=secret"} {
		raw, _ := catalogueResponse(context.Background(), "site.feed", nil)
		var sections []contract.FeedSection
		mustCatalogue(t, json.Unmarshal(raw, &sections))
		sections[0].Cursor = cursor
		raw, _ = json.Marshal(sections)
		persisted, e := s.sanitizeCatalogue(scope, "feed", raw, "session", 0, nil)
		mustCatalogue(t, e)
		sections = nil // omitted cursor must not inherit the test input's old field
		mustCatalogue(t, json.Unmarshal(persisted, &sections))
		if cursor == "page:2" && sections[0].Cursor != cursor || cursor != "page:2" && sections[0].Cursor != "" {
			t.Fatal("unsafe cursor persisted or stable paging lost")
		}
	}
	now := time.Now().UTC()
	body := strings.Repeat("x", maxPluginCatalogueBytes)
	rows := make([]models.PluginCatalogueSnapshot, 0, 128)
	pages := make([]models.PluginFeedCache, 0, 128)
	for i := 0; i < 128; i++ {
		rows = append(rows, models.PluginCatalogueSnapshot{ID: fmt.Sprintf("%064d", i), LibraryID: id, ScopeKey: scope.key, Kind: "feed", ResponseJSON: body, StaleUntil: now.Add(time.Hour), UpdatedAt: now.Add(time.Duration(i) * time.Second)})
		pages = append(pages, models.PluginFeedCache{LibraryID: id, RouteKey: fmt.Sprint(i), CursorKey: "cursor", RefreshSession: "session", ResponseJSON: body, ExpiresAt: now.Add(time.Hour), UpdatedAt: now.Add(time.Duration(i) * time.Second)})
	}
	mustCatalogue(t, s.db.CreateInBatches(rows, 16).Error)
	mustCatalogue(t, s.db.CreateInBatches(pages, 16).Error)
	mustCatalogue(t, s.db.Transaction(func(tx *gorm.DB) error { return pruneCatalogueTx(tx, id, now) }))
	var snapshotBytes, feedBytes int64
	mustCatalogue(t, s.db.Model(&models.PluginCatalogueSnapshot{}).Select("COALESCE(SUM(length(response_json)),0)").Scan(&snapshotBytes).Error)
	mustCatalogue(t, s.db.Model(&models.PluginFeedCache{}).Select("COALESCE(SUM(length(response_json)),0)").Scan(&feedBytes).Error)
	if snapshotBytes > 48<<20 || feedBytes > 16<<20 || snapshotBytes+feedBytes > 64<<20 {
		t.Fatal("combined catalogue cache exceeded byte quota")
	}
}

func TestOnlineCatalogueQuotasAndUnsafeDescriptors(t *testing.T) {
	s, _, id, _ := catalogueFixture(t)
	scope, err := s.catalogueScope(id)
	mustCatalogue(t, err)
	now := time.Now().UTC()
	for _, value := range []string{"https://login.example.test/poster.jpg?token=secret", "https://u:p@login.example.test/poster.jpg", "http://login.example.test/poster.jpg", "/api/v1/player/artwork/lease"} {
		if durableCatalogueArtwork(value) != "" {
			t.Fatal("unsafe descriptor persisted")
		}
	}
	rows := make([]models.PluginCatalogueSnapshot, 0, 130)
	for i := 0; i < 130; i++ {
		rows = append(rows, models.PluginCatalogueSnapshot{ID: fmt.Sprintf("%064d", i), LibraryID: id, ScopeKey: scope.key, Kind: "feed", Selector: fmt.Sprint(i), ResponseJSON: `[]`, StaleUntil: now.Add(time.Hour), UpdatedAt: now.Add(time.Duration(i) * time.Second)})
	}
	mustCatalogue(t, s.db.Create(&rows).Error)
	pages := make([]models.PluginFeedCache, 0, 130)
	for i := 0; i < 130; i++ {
		pages = append(pages, models.PluginFeedCache{LibraryID: id, RouteKey: fmt.Sprint(i), CursorKey: "cursor", RefreshSession: "session", ScopeKey: scope.key, ResponseJSON: `[]`, ExpiresAt: now.Add(time.Hour), UpdatedAt: now.Add(time.Duration(i) * time.Second)})
	}
	mustCatalogue(t, s.db.Create(&pages).Error)
	mustCatalogue(t, s.db.Transaction(func(tx *gorm.DB) error { return pruneCatalogueTx(tx, id, now) }))
	var snapshots, feeds int64
	mustCatalogue(t, s.db.Model(&models.PluginCatalogueSnapshot{}).Count(&snapshots).Error)
	mustCatalogue(t, s.db.Model(&models.PluginFeedCache{}).Count(&feeds).Error)
	if snapshots != 128 || feeds != 128 {
		t.Fatal("library quota failed")
	}
	mustCatalogue(t, s.db.Model(&models.PluginOnlineLibrary{}).Where("id = ?", id).Update("enabled", false).Error)
	s.catalogueState().now = func() time.Time { return now.Add(2 * time.Hour) }
	s.warmOnlineCatalogue(context.Background())
	mustCatalogue(t, s.db.Model(&models.PluginCatalogueSnapshot{}).Count(&snapshots).Error)
	mustCatalogue(t, s.db.Model(&models.PluginFeedCache{}).Count(&feeds).Error)
	if snapshots != 0 || feeds != 0 {
		t.Fatal("disabled-only installation retained expired directory data")
	}
}
