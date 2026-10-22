package services

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	sitepkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/site"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/site/pansou"
)

func TestPanSouAutomaticTypesUseOrdinaryResourceRights(t *testing.T) {
	s, _, admin, _, _, _ := siteFixture(t)
	ids := map[string]string{}
	for _, provider := range []string{models.ConnectionProviderPan115, models.ConnectionProviderQuark} {
		row, _, _ := seedPanSouReceiver(t, s, provider)
		ids[provider] = row.ID
	}
	for _, tt := range []struct {
		name      string
		providers []string
		want      string
	}{{"none", nil, "magnet"}, {"115", []string{models.ConnectionProviderPan115}, "115,magnet"}, {"quark", []string{models.ConnectionProviderQuark}, "magnet,quark"}, {"both", []string{models.ConnectionProviderPan115, models.ConnectionProviderQuark}, "115,magnet,quark"}} {
		t.Run(tt.name, func(t *testing.T) {
			ordinary := Actor{User: admin.User, Permissions: map[string]struct{}{authz.PermissionDiscoveryRead: {}, authz.PermissionDownloadsCreate: {}}, ResourceAccessPolicies: map[string]ResourceAccessPolicy{models.ResourceAccessScopeDownloaderUse: {Scope: models.ResourceAccessScopeDownloaderUse, Mode: models.ResourceAccessModeAllowlist, ResourceIDs: []string{}}}}
			policy := ordinary.ResourceAccessPolicies[models.ResourceAccessScopeDownloaderUse]
			for _, provider := range tt.providers {
				policy.ResourceIDs = append(policy.ResourceIDs, ids[provider])
			}
			ordinary.ResourceAccessPolicies[models.ResourceAccessScopeDownloaderUse] = policy
			types, err := s.panSouAutomaticTypes(ordinary)
			if err != nil || strings.Join(types, ",") != tt.want || ordinary.Can(authz.PermissionConnectionsRead) {
				t.Fatalf("types=%v err=%v", types, err)
			}
		})
	}
}

func TestPanSouMixedClaimsLegacyAndPublicMagnetRoutes(t *testing.T) {
	s, _, actor, _, _, downloaders := siteFixture(t)
	var requested []string
	const magnet = "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Types []string `json:"cloud_types"`
		}
		_ = json.NewDecoder(r.Body).Decode(&input)
		requested = input.Types
		fmt.Fprintf(w, `{"code":0,"data":{"total":4,"results":[{"unique_id":"fixture-1","title":"Sample.Show","links":[{"type":"115","url":"https://115.com/s/fixture?password=abcd"},{"type":"quark","url":"https://pan.quark.cn/s/fixture"},{"type":"magnet","url":%q},{"type":"baidu","url":"https://pan.baidu.com/s/fixture","password":"abcd"}]}]}}`, magnet)
	}))
	defer server.Close()
	s.adapters[pansou.Kind] = pansou.NewForTest(server.Client())
	created, err := s.Create(context.Background(), actor, SiteInput{Name: "Mixed", Kind: pansou.Kind, BaseURL: server.URL, CloudConfig: &sitepkg.CloudConfig{SourceScope: "plugin"}, Enabled: true, Priority: 100, TimeoutSeconds: 10, RateLimitPerMinute: 120}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := s.Search(context.Background(), actor, SiteSearchInput{Keyword: "Sample", Page: 1, SiteID: &created.ID})
	if err != nil || len(legacy) != 1 || len(legacy[0].Items) != 1 || legacy[0].Items[0].SourceKind != RouteSource115Share || strings.Join(requested, ",") != "115" {
		t.Fatalf("legacy=%+v types=%v err=%v", legacy, requested, err)
	}
	groups, err := s.Search(context.Background(), actor, SiteSearchInput{Keyword: "Sample", Page: 1, SiteID: &created.ID, ClientCapabilities: []string{MixedDiscoveryCapability}, TypePolicy: "all"})
	if err != nil || len(groups) != 1 || len(groups[0].Items) != 4 {
		t.Fatalf("groups=%+v err=%v", groups, err)
	}
	byType := map[string]SiteSearchResult{}
	for _, item := range groups[0].Items {
		byType[item.ResultType] = item
	}
	if byType["baidu"].AcquisitionAvailable == nil || *byType["baidu"].AcquisitionAvailable || byType["baidu"].AcquisitionReason == "" {
		t.Fatal("view-only type did not block acquisition")
	}
	qbit, err := downloaders.Create(actor, DownloaderInput{Name: "Mixed qBit", Type: models.DownloaderTypeQBittorrent, BaseURL: "http://qbit.example.test", Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	for kind, want := range map[string]string{"115": RouteSource115Share, "quark": RouteSourceQuarkShare, "magnet": RouteSourceBT} {
		recommendation, err := s.RecommendSiteRoute(context.Background(), actor, SourceRouteRecommendationInput{ResultToken: byType[kind].Token})
		if err != nil || recommendation.SourceKind != want {
			t.Fatalf("%s recommendation=%+v err=%v", kind, recommendation, err)
		}
	}
	if _, err := s.RecommendSiteRoute(context.Background(), actor, SourceRouteRecommendationInput{ResultToken: byType["baidu"].Token}); ErrorCode(err) != CodeDownloadSourceInvalid {
		t.Fatal("view-only recommended", err)
	}
	if _, err := s.Download(context.Background(), actor, SiteDownloadInput{ResultToken: byType["baidu"].Token, DownloaderID: qbit.ID}, RequestContext{}); ErrorCode(err) != CodeDownloadSourceInvalid {
		t.Fatal("view-only enqueued", err)
	}
	link, err := s.ResultLink(context.Background(), actor, byType["baidu"].Token)
	if err != nil || link.ResultType != "baidu" || !strings.Contains(link.URL, "pwd=abcd") {
		t.Fatalf("link=%+v err=%v", link, err)
	}
	foreign := actor
	foreign.User.ID++
	if _, err := s.ResultLink(context.Background(), foreign, byType["baidu"].Token); err == nil {
		t.Fatal("cross-user public-link claim accepted")
	}
	if _, err := s.Download(context.Background(), actor, SiteDownloadInput{ResultToken: byType["quark"].Token, DownloaderID: qbit.ID}, RequestContext{}); ErrorCode(err) != CodeDownloadSourceInvalid {
		t.Fatal("quark sent to BT", err)
	}
	if _, err := s.Download(context.Background(), actor, SiteDownloadInput{ResultToken: byType["magnet"].Token, DownloaderID: qbit.ID, Priority: 10}, RequestContext{}); err != nil {
		t.Fatal("public magnet failed", err)
	}
	if err := s.db.Model(&models.Site{}).Where("id = ?", created.ID).Update("revision", 2).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResultLink(context.Background(), actor, byType["baidu"].Token); ErrorCode(err) != CodeSiteResultExpired {
		t.Fatal("obsolete link claim survived", err)
	}
}

func TestPanSouDraftScopeDoesNotForwardSavedSecretsToChangedURL(t *testing.T) {
	s, _, actor, _, _, _ := siteFixture(t)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		switch r.URL.Path {
		case "/api/auth/login":
			fmt.Fprint(w, `{"token":"fixture"}`)
		case "/api/health":
			fmt.Fprint(w, `{"channels":["movies"]}`)
		default:
			fmt.Fprint(w, `{"code":0,"data":{"total":0,"results":[]}}`)
		}
	}))
	defer server.Close()
	s.adapters[pansou.Kind] = pansou.NewForTest(server.Client())
	cfg := &sitepkg.CloudConfig{AuthEnabled: true, SourceScope: "all", ChannelMode: "defaults_plus_custom"}
	created, err := s.Create(context.Background(), actor, SiteInput{Name: "AuthMixed", Kind: pansou.Kind, BaseURL: server.URL, CloudConfig: cfg, Username: "fixture", Password: "private-fixture-password", Enabled: true, Priority: 100, TimeoutSeconds: 10, RateLimitPerMinute: 120}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	scope, err := s.SavedPanSouScope(context.Background(), actor, created.ID, true)
	if err != nil || scope.DefaultCount != 1 {
		t.Fatalf("scope=%+v err=%v", scope, err)
	}
	before := requests
	if _, err := s.PanSouScope(context.Background(), actor, PanSouScopeInput{SiteID: &created.ID, Revision: created.Revision, BaseURL: "http://different.invalid:9090", CloudConfig: cfg}); ErrorCode(err) != CodeSiteCredentialInvalid {
		t.Fatal("draft reused secret for changed URL", err)
	}
	if requests != before {
		t.Fatal("changed candidate performed request with implicit credential")
	}
	if _, err := s.PanSouScope(context.Background(), actor, PanSouScopeInput{SiteID: &created.ID, Revision: created.Revision + 1, CloudConfig: cfg}); ErrorCode(err) != CodeConflict {
		t.Fatal("obsolete candidate revision accepted", err)
	}
	raw, _ := json.Marshal(scope)
	if strings.Contains(string(raw), "private-fixture") || strings.Contains(string(raw), "token") {
		t.Fatal("scope exposed auth", string(raw))
	}
	if _, err := normalizeSiteBaseURL(pansou.Kind, "ftp://pansou:8888"); ErrorCode(err) != CodeSiteURLInvalid || !strings.Contains(safeErrorMessage(err, ""), "HTTP或HTTPS") {
		t.Fatal("misleading PanSou URL hint", err)
	}
}

func TestPanSouPreviewKeepsOtherCloudAdaptersAndQuarkOfflineClosed(t *testing.T) {
	s, _, _, _, downloads, _ := siteFixture(t)
	now := time.Now().UTC()
	for _, kind := range []string{pansou.Kind, "tg_cloud"} {
		site := models.Site{Name: kind, NameNormalized: kind, Kind: kind, BaseURL: "https://example.test", Enabled: true, Revision: 1, Priority: 100, TimeoutSeconds: 10, RateLimitPerMinute: 10, CreatedAt: now, UpdatedAt: now}
		if kind != "tg_cloud" {
			if err := s.db.Create(&site).Error; err != nil {
				t.Fatal(err)
			}
			if err := downloads.validatePreviewSource(models.Downloader{Type: models.DownloaderTypePan115Offline}, "url", &site.ID); err != nil {
				t.Fatal("PanSou magnet preview rejected", err)
			}
			if err := downloads.validatePreviewSource(models.Downloader{Type: models.DownloaderTypePan115Offline}, "torrent", &site.ID); ErrorCode(err) != CodeDownloadSourceInvalid {
				t.Fatal("PanSou raw torrent accepted", err)
			}
		}
	}
	if routeDownloaderApplicable(RouteSourceBT, models.Downloader{Type: models.DownloaderTypeQuarkShare}) || routeDownloaderApplicable(RouteSourcePT, models.Downloader{Type: models.DownloaderTypeQuarkShare}) {
		t.Fatal("Quark became offline/PT candidate")
	}
}
