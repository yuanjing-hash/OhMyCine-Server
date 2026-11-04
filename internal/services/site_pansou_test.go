package services

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/resourcecontent"
	sitepkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/site"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/site/btapi"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/site/bthtml"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/site/btrss"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/site/pansou"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/site/torznab"
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
	requests := 0
	const magnet = "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
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
	for kind, want := range map[string]sitepkg.PayloadKind{"115": sitepkg.PayloadShare, "quark": sitepkg.PayloadShare, "magnet": sitepkg.PayloadMagnet, "baidu": sitepkg.PayloadShare} {
		if byType[kind].PayloadKind != want {
			t.Fatalf("%s safe payload projection=%q want=%q", kind, byType[kind].PayloadKind, want)
		}
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
		previewKind := want
		if kind == "magnet" {
			previewKind = "url"
		}
		if recommendation.PreviewSourceKind != previewKind {
			t.Fatalf("%s lost the validated preview kind: %+v", kind, recommendation)
		}
		if kind == "magnet" {
			preview, err := s.downloads.PreviewRoutes(context.Background(), actor, DownloadRoutePreviewInput{DownloaderID: qbit.ID, SourceKind: recommendation.PreviewSourceKind, SiteID: &created.ID})
			if err != nil || preview.SourceKind != recommendation.PreviewSourceKind {
				t.Fatalf("magnet recommendation cannot be previewed: %+v, %v", preview, err)
			}
		}
	}
	if _, err := s.RecommendSiteRoute(context.Background(), actor, SourceRouteRecommendationInput{ResultToken: byType["baidu"].Token}); ErrorCode(err) != CodeDownloadSourceInvalid {
		t.Fatal("view-only recommended", err)
	}
	if _, err := s.Download(context.Background(), actor, SiteDownloadInput{ResultToken: byType["baidu"].Token, DownloaderID: qbit.ID}, RequestContext{}); ErrorCode(err) != CodeDownloadSourceInvalid {
		t.Fatal("view-only enqueued", err)
	}
	beforeCopy := requests
	link, err := s.ResultLink(context.Background(), actor, byType["baidu"].Token)
	if err != nil || link.ResultType != "baidu" || !strings.Contains(link.URL, "pwd=abcd") {
		t.Fatalf("link=%+v err=%v", link, err)
	}
	magnetLink, err := s.ResultLink(context.Background(), actor, byType["magnet"].Token)
	if err != nil || magnetLink.ResultType != "magnet" || magnetLink.URL != magnet || requests != beforeCopy {
		t.Fatalf("copy did not reuse the actual public magnet without an upstream request: %+v err=%v requests=%d", magnetLink, err, requests-beforeCopy)
	}
	// Native payloads stay private to adapters. Copy supports only their local
	// magnet capability; detail/torrent/unknown identities cannot cause a fetch.
	fullMagnet := magnet + "&tr=https%3A%2F%2Ftracker.example.test%2Fannounce&dn=Copy.Fixture"
	largeHints := url.Values{"xt": {"urn:btih:0123456789abcdef0123456789abcdef01234567"}, "dn": {"Native.Full.Hints"}}
	for index := 0; index < 6; index++ {
		largeHints.Add("tr", fmt.Sprintf("https://tracker%d.example.test/%s", index, strings.Repeat("a", 1500)))
	}
	largeMagnet := "magnet:?" + largeHints.Encode()
	s.adapters["mikan"] = btrss.NewForProfile("mikan", btrss.MikanProfile())
	s.adapters["yts"] = btapi.NewForProfile("yts", btapi.YTSProfile())
	s.adapters["thepiratebay"] = bthtml.NewForProfile("thepiratebay", bthtml.PirateBayProfile())
	s.adapters["torznab"] = torznab.New()
	for index, scenario := range []struct {
		kind, source, identity, expected string
	}{
		{"mikan", RouteSourceBT, magnet, magnet},
		{"mikan", RouteSourceBT, fullMagnet, fullMagnet},
		{"mikan", RouteSourceBT, btrss.EncodeIdentity("magnet", fullMagnet), fullMagnet},
		{"mikan", RouteSourceBT, largeMagnet, largeMagnet},
		{"mikan", RouteSourceBT, btrss.EncodeIdentity("magnet", largeMagnet), largeMagnet},
		{pansou.Kind, RouteSourceBT, largeMagnet, ""},
		{"yts", RouteSourceBT, btrss.EncodeIdentity("magnet", fullMagnet), fullMagnet},
		{"thepiratebay", RouteSourceBT, btrss.EncodeIdentity("magnet", fullMagnet), fullMagnet},
		{"torznab", RouteSourceBT, btrss.EncodeIdentity("magnet", fullMagnet), fullMagnet},
		{"mikan", RouteSourceBT, "opaque-adapter-id", ""},
		{"mikan", RouteSourceBT, "https://t.me/fixture/123", ""},
		{"thepiratebay", RouteSourceBT, btrss.EncodeIdentity("detail", "https://copy.example.test/detail"), ""},
		{"mikan", RouteSourceBT, btrss.EncodeIdentity("torrent", "https://copy.example.test/file.torrent"), ""},
		{"pttime", RouteSourcePT, fullMagnet, ""},
	} {
		record := models.Site{Name: fmt.Sprintf("Copy %d", index), NameNormalized: fmt.Sprintf("copy-%d", index), Kind: scenario.kind, Enabled: true, Revision: 1, BaseURL: "https://copy.example.test"}
		if err := s.db.Create(&record).Error; err != nil {
			t.Fatal(err)
		}
		copyToken, err := s.issueClaim(siteResultClaim{ActorID: actor.User.ID, SiteID: record.ID, SiteRevision: record.Revision, SourceKind: scenario.source, PayloadKind: sitepkg.PayloadMagnet, TorrentID: scenario.identity, ExpiresAt: s.now().Add(time.Minute)})
		if err != nil {
			t.Fatal(err)
		}
		copied, err := s.ResultLink(context.Background(), actor, copyToken)
		if scenario.expected != "" {
			actual, parseErr := url.Parse(copied.URL)
			expected, _ := url.Parse(scenario.expected)
			if err != nil || parseErr != nil || actual.Scheme != "magnet" || copied.ResultType != "magnet" || actual.Query().Encode() != expected.Query().Encode() {
				t.Fatalf("public native magnet unavailable: %+v err=%v", copied, err)
			}
		} else if ErrorCode(err) != CodeDownloadSourceInvalid || copied.URL != "" {
			t.Fatalf("opaque, post or private source gained public-copy authority: kind=%s error=%v", scenario.kind, err)
		}
	}
	// Actual verified private metainfo remains private after presentation/cache
	// retirement and an ordinary claim reserve/release. Copy adds no read.
	publicRaw := inspectionTorrentWithSize(32<<20, "Fixture.Show.S01E13.mkv")
	privateRaw := append(append(append([]byte(nil), publicRaw[:len(publicRaw)-2]...), []byte("7:privatei1e")...), 'e', 'e')
	privateManifest, err := resourcecontent.ParseTorrent(privateRaw, resourcecontent.Limits{})
	if err != nil || !privateManifest.Private {
		t.Fatalf("invalid private fixture: %v", err)
	}
	privateMagnet := "magnet:?xt=urn:btih:" + privateManifest.Identity.V1
	nativeAdapter := s.adapters["mikan"]
	s.adapters["mikan"] = &stubSiteAdapter{kind: "mikan"}
	privateSite, err := s.Create(context.Background(), actor, SiteInput{Name: "Known private", Kind: "mikan", BaseURL: "https://mikanani.me", Enabled: true, Priority: 100, TimeoutSeconds: 12, RateLimitPerMinute: 120}, RequestContext{})
	s.adapters["mikan"] = nativeAdapter
	if err != nil {
		t.Fatal(err)
	}
	privateToken, err := s.issueClaim(siteResultClaim{ActorID: actor.User.ID, SiteID: privateSite.ID, SiteRevision: privateSite.Revision, SourceKind: RouteSourceBT, PayloadKind: sitepkg.PayloadMagnet, TorrentID: btrss.EncodeIdentity("magnet", privateMagnet), Title: "Fixture.Show.S01E13", ExpiresAt: s.now().Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	metadataReads := 0
	s.SetResourceContentResolver(contentResolverFunc(func(_ context.Context, input resourcecontent.ResolveRequest) (resourcecontent.Manifest, error) {
		metadataReads++
		if input.URI != privateMagnet {
			return resourcecontent.Manifest{}, sitepkg.ErrInvalidReply
		}
		return privateManifest, nil
	}))
	preview, err := s.PreviewContent(context.Background(), actor, ContentPreviewInput{ResultToken: privateToken})
	if err != nil || !preview.Complete || metadataReads != 1 {
		t.Fatalf("private metadata was not inspected: %+v %v", preview, err)
	}
	s.discardContentPreview(actor, preview.Token)
	state := s.contents()
	state.mu.Lock()
	state.cache = map[string]inspectedContent{}
	state.mu.Unlock()
	if _, err := s.reserveClaim(privateToken, actor.User.ID); err != nil {
		t.Fatal(err)
	}
	s.finishClaim(privateToken, false)
	if copied, err := s.ResultLink(context.Background(), actor, privateToken); ErrorCode(err) != CodeDownloadSourceInvalid || copied.URL != "" || metadataReads != 1 {
		t.Fatalf("known private fact was lost or copy initiated metadata: %+v %v reads=%d", copied, err, metadataReads)
	}
	var copiedTasks int64
	if err := s.db.Model(&models.DownloadTask{}).Count(&copiedTasks).Error; err != nil || copiedTasks != 0 || requests != beforeCopy {
		t.Fatalf("copy performed download/upstream work: tasks=%d requests=%d error=%v", copiedTasks, requests-beforeCopy, err)
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
