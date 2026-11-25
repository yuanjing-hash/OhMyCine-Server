package services

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/metadata/tmdb"
	sitepkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/site"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/site/pansou"
	"golang.org/x/time/rate"
)

func TestPanSouIdentityPreservesNumericTitlesAndMixedResources(t *testing.T) {
	const title = "寒战1994"
	const alias = "Cold War 1994"
	metadataServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/movie/1994" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, `{"id":1994,"title":%q,"original_title":%q,"original_language":"zh","release_date":"2026-05-01","alternative_titles":{"titles":[]},"translations":{"translations":[]}}`, title, alias)
	}))
	defer metadataServer.Close()
	releases := []string{title, title + ".2160p.WEB-DL.H265", title + " (2026)", title + ".2026.2160p.WEB-DL.H265", "Cold.War.1994.1080p.WEB-DL"}
	var keywords []string
	panSou := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Keyword string `json:"kw"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		keywords = append(keywords, request.Keyword)
		items := []any{}
		add := func(source, value, kind, resource string) {
			items = append(items, map[string]any{"source": "plugin:" + source, "title": value, "links": []any{map[string]string{"type": kind, "url": resource}}})
		}
		for i := range 2 {
			add("5266ys", title, "115", fmt.Sprintf("https://115.com/s/numeric%d?password=abcd", i+1))
		}
		for i, release := range releases {
			add("5266ys", release, "magnet", fmt.Sprintf("magnet:?xt=urn:btih:%040x", i+1))
		}
		// This is the same magnet from another plugin, not a sixth resource.
		add("dygang", releases[0], "magnet", fmt.Sprintf("magnet:?xt=urn:btih:%040x", 1))
		for i, wrong := range []string{title + " (2012)", title + "续集.2160p", "新" + title + ".2160p"} {
			add("dygang", wrong, "magnet", fmt.Sprintf("magnet:?xt=urn:btih:%040x", i+100))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"total": len(items), "results": items}})
	}))
	defer panSou.Close()
	s, _, actor, store, _, _ := siteFixture(t)
	_, _, _ = seedPanSouReceiver(t, s, models.ConnectionProviderPan115)
	metadata := NewMetadataSettingsService(s.db, s.audit, store, tmdb.Credential{Kind: tmdb.CredentialKindReadAccessToken, Value: "fixture"})
	metadata.clientFactory = func(tmdb.Credential, string, string) (*tmdb.Client, error) {
		return tmdb.NewForTest("fixture", metadataServer.URL, metadataServer.Client())
	}
	s.SetMetadataSettings(metadata)
	s.adapters[pansou.Kind] = pansou.NewForTest(panSou.Client())
	created, err := s.Create(context.Background(), actor, SiteInput{Name: "Numeric PanSou", Kind: pansou.Kind, BaseURL: panSou.URL, CloudConfig: &sitepkg.CloudConfig{SourceScope: "plugin"}, Enabled: true, Priority: 100, TimeoutSeconds: 10, RateLimitPerMinute: 120}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	keywords = nil
	result, err := s.SearchMediaIdentity(context.Background(), actor, MediaIdentitySearchInput{MediaType: "movie", TMDBID: 1994, SiteID: &created.ID, Page: 1, ClientCapabilities: []string{MixedDiscoveryCapability}, TypePolicy: "all"})
	if err != nil || len(result.Groups) != 1 || len(result.Groups[0].Items) != 7 {
		t.Fatalf("mixed detail search = %+v, err=%v", result, err)
	}
	if !slices.Equal(keywords, []string{title, alias}) {
		t.Fatalf("identity query names were widened: %v", keywords)
	}
	shares, magnets := 0, 0
	seen := map[string]bool{}
	for _, item := range result.Groups[0].Items {
		if item.MatchedName != title && item.MatchedName != alias || seen[item.ResourceFingerprint] {
			t.Fatalf("unmatched or duplicated result: %+v", item)
		}
		seen[item.ResourceFingerprint] = true
		claim, err := s.resolveAvailableClaim(item.Token, actor.User.ID)
		if err != nil || claim.SearchIdentity == nil || claim.SearchIdentity.TMDBID != 1994 {
			t.Fatalf("actor-bound detail identity lost: %+v, err=%v", claim.SearchIdentity, err)
		}
		switch item.SourceKind {
		case RouteSource115Share:
			shares++
			if claim.ManualTMDBID != nil {
				t.Fatal("numeric title matching bypassed actual share-file inspection")
			}
		case RouteSourceBT:
			magnets++
			if !slices.Contains(releases, item.Title) || claim.ManualTMDBID == nil || *claim.ManualTMDBID != 1994 {
				t.Fatalf("wrong release or missing recognition identity: %+v", item)
			}
			if item.Title == releases[0] && (len(item.Origins) != 2 || item.Origins[0].Name != "5266ys" || item.Origins[1].Name != "dygang") {
				t.Fatalf("duplicate plugin origins were not merged: %+v", item.Origins)
			}
		default:
			t.Fatalf("unexpected source kind %q", item.SourceKind)
		}
	}
	if shares != 2 || magnets != 5 {
		t.Fatalf("mixed detail results: shares=%d magnets=%d", shares, magnets)
	}
	public, err := json.Marshal(result)
	if err != nil || strings.Contains(string(public), "magnet:?") || strings.Contains(string(public), "115.com/s/") {
		t.Fatal("private resource links escaped in the search response", err)
	}
	keywords = nil
	rawQuery := "寒战1994 自定义关键词"
	raw, err := s.Search(context.Background(), actor, SiteSearchInput{Keyword: rawQuery, SiteID: &created.ID, Page: 1, ClientCapabilities: []string{MixedDiscoveryCapability}, TypePolicy: "all"})
	if err != nil || !slices.Equal(keywords, []string{rawQuery}) || len(raw) != 1 || len(raw[0].Items) != 10 {
		t.Fatalf("raw keyword search changed: queries=%v groups=%+v err=%v", keywords, raw, err)
	}
	for _, item := range raw[0].Items {
		claim, err := s.resolveAvailableClaim(item.Token, actor.User.ID)
		if err != nil || item.MatchedName != "" || claim.SearchIdentity != nil || claim.ManualTMDBID != nil {
			t.Fatal("raw keyword search fabricated a media identity", err)
		}
	}
}

func TestPanSouIdentitySnapshotFreezesAliasesAndPages(t *testing.T) {
	const fullTitle = "孤单又灿烂的神：鬼怪"
	const officialAlias = "鬼怪"
	const shortQuery = "孤单灿烂的神"
	const releaseFromShortQuery = fullTitle + ".S01E62.1080p.WEB-DL"
	metadataServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tv/346" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, `{"id":346,"name":%q,"original_name":"Goblin","original_language":"ko","first_air_date":"2016-12-02","alternative_titles":{"results":[{"iso_3166_1":"CN","title":%q},{"iso_3166_1":"TW","title":"孤單又燦爛的神"},{"iso_3166_1":"TW","title":"燦爛的鬼怪"},{"iso_3166_1":"HK","title":"孤單的神"},{"iso_3166_1":"HK","title":"孤單鬼怪"}]},"translations":{"translations":[]}}`, fullTitle, officialAlias)
	}))
	defer metadataServer.Close()
	var searchKeywords []string
	panSou := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Keyword string `json:"kw"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		searchKeywords = append(searchKeywords, request.Keyword)
		items := []any{}
		if request.Keyword == shortQuery {
			items = append(items,
				map[string]any{"unique_id": "only-short-query", "title": shortQuery, "datetime": "2026-11-18T00:00:00Z", "links": []any{map[string]string{"type": "115", "url": "https://115.com/s/fixture?password=abcd"}}},
				map[string]any{"unique_id": "fixture-short-query-release", "title": releaseFromShortQuery, "datetime": "2026-11-17T00:00:00Z", "links": []any{map[string]string{"type": "magnet", "url": fmt.Sprintf("magnet:?xt=urn:btih:%040x", 62)}}},
			)
		} else {
			for i := 0; i < 61; i++ {
				items = append(items, map[string]any{"unique_id": fmt.Sprintf("fixture-%d", i), "title": fmt.Sprintf("%s.S01E%02d.1080p.WEB-DL", officialAlias, i+1), "links": []any{map[string]string{"type": "magnet", "url": fmt.Sprintf("magnet:?xt=urn:btih:%040x", i+1)}}})
			}
			if request.Keyword == officialAlias {
				for i := range 6 {
					items = append(items, map[string]any{"unique_id": fmt.Sprintf("fixture-share-%d", i), "title": fmt.Sprintf("%s.S01E%02d.2160p.WEB-DL", officialAlias, i+1), "datetime": "2026-11-16T00:00:00Z", "links": []any{map[string]string{"type": "115", "url": fmt.Sprintf("https://115.com/s/episode%d?password=abcd", i+1)}}})
				}
				// Newer, unrelated magnets may fill an entire first page when a
				// short verified alias occurs inside a different work's title.
				unrelated := []string{"魔鬼怪医", "纯白鬼怪"}
				for i := range 52 {
					items = append(items, map[string]any{"unique_id": fmt.Sprintf("fixture-unrelated-%d", i), "title": fmt.Sprintf("%s.S01E%02d.1080p.WEB-DL", unrelated[i%len(unrelated)], i+1), "datetime": "2026-11-18T00:00:00Z", "links": []any{map[string]string{"type": "magnet", "url": fmt.Sprintf("magnet:?xt=urn:btih:%040x", i+1000)}}})
				}
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"total": len(items), "results": items}})
	}))
	defer panSou.Close()
	s, _, actor, store, _, _ := siteFixture(t)
	now := time.Now().UTC()
	s.now = func() time.Time { return now }
	receiver, _, _ := seedPanSouReceiver(t, s, models.ConnectionProviderPan115)
	metadata := NewMetadataSettingsService(s.db, s.audit, store, tmdb.Credential{Kind: tmdb.CredentialKindReadAccessToken, Value: "fixture"})
	metadata.clientFactory = func(tmdb.Credential, string, string) (*tmdb.Client, error) {
		return tmdb.NewForTest("fixture", metadataServer.URL, metadataServer.Client())
	}
	s.SetMetadataSettings(metadata)
	s.adapters[pansou.Kind] = pansou.NewForTest(panSou.Client())
	created, err := s.Create(context.Background(), actor, SiteInput{Name: "Alias PanSou", Kind: pansou.Kind, BaseURL: panSou.URL, CloudConfig: &sitepkg.CloudConfig{SourceScope: "plugin"}, Enabled: true, Priority: 100, TimeoutSeconds: 10, RateLimitPerMinute: 120}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	searchKeywords = nil
	input := MediaIdentitySearchInput{MediaType: "tv", TMDBID: 346, Page: 1, SiteID: &created.ID, ClientCapabilities: []string{MixedDiscoveryCapability}, TypePolicy: "all", ProgressVersion: 2}
	first := MediaIdentitySearchResult{}
	progressEvents := 0
	err = s.SearchMediaIdentityEachProgress(context.Background(), actor, input, func(metadata MediaIdentitySearchResult) {
		first = metadata
	}, func(group SiteSearchGroup) {
		// Internal alias/page collection is not the client's frozen first page.
		// Sending it would make the shared SSE controller reject a page mismatch.
		if group.Page != 1 || group.Complete == nil || !*group.Complete {
			t.Errorf("unfrozen internal page escaped: page=%d complete=%v", group.Page, group.Complete)
		}
		first.Groups = append(first.Groups, group)
	}, func(progress SiteSearchProgress) {
		progressEvents++
		if progress.ResultCount > 50 {
			t.Errorf("progress counted internal pages: %d", progress.ResultCount)
		}
	})
	if err != nil || len(first.Groups) != 1 || len(first.Groups[0].Items) != 50 || !first.Groups[0].HasNext || first.Groups[0].SnapshotID == "" {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	if progressEvents == 0 {
		t.Fatal("frozen PanSou search omitted detailed progress")
	}
	shares := 0
	shortQueryReleaseFound := false
	for _, item := range first.Groups[0].Items {
		if item.MatchedName != fullTitle && item.MatchedName != officialAlias {
			t.Fatalf("unrelated or unverified result displaced related first-page results: %q (matched %q)", item.Title, item.MatchedName)
		}
		if item.SourceKind == "115_share" {
			shares++
			claim, err := s.resolveAvailableClaim(item.Token, actor.User.ID)
			if err != nil || claim.ManualTMDBID != nil {
				t.Fatal("matching a share title bypassed actual file inspection", err)
			}
		}
		if item.Title == releaseFromShortQuery {
			shortQueryReleaseFound = item.MatchedName == fullTitle
		}
	}
	if shares != 6 || !shortQueryReleaseFound {
		t.Fatalf("first-page shares=%d, exact match from compact query=%t, top=%q", shares, shortQueryReleaseFound, first.Groups[0].Items[0].Title)
	}
	requests := len(searchKeywords)
	if requests != tmdb.DefaultIdentitySearchNameLimit || first.Groups[0].QueriesTotal != requests || first.Groups[0].QueriesCompleted != requests ||
		!slices.Equal(searchKeywords[:3], []string{fullTitle, officialAlias, shortQuery}) {
		t.Fatalf("query count or order changed: %v", searchKeywords)
	}
	// All subsequent pages are local projections. Even an exhausted shared
	// upstream allowance must not turn them into a timeout or a new search.
	var record models.Site
	if err := s.db.First(&record, created.ID).Error; err != nil {
		t.Fatal(err)
	}
	limiter := rate.NewLimiter(rate.Every(time.Hour), 1)
	limiter.Allow()
	s.limits[record.ID] = &siteLimiter{revision: record.Revision, limiter: limiter}
	now = now.Add(16 * time.Minute)
	input.Page = 2
	input.SnapshotID = first.Groups[0].SnapshotID
	second, err := s.SearchMediaIdentity(context.Background(), actor, input)
	if err != nil || len(second.Groups) != 1 || len(second.Groups[0].Items) != 19 || second.Groups[0].HasNext || second.Groups[0].QueryAttempted || len(searchKeywords) != requests {
		t.Fatalf("second=%+v searches=%d err=%v", second, len(searchKeywords), err)
	}
	input.Page = 1
	back, err := s.SearchMediaIdentity(context.Background(), actor, input)
	if err != nil || len(back.Groups) != 1 || back.Groups[0].SnapshotID != first.Groups[0].SnapshotID || len(searchKeywords) != requests {
		t.Fatal("return to first identity page refreshed upstream", err, len(searchKeywords))
	}
	input.Page = 2
	seen := map[string]bool{}
	for _, item := range first.Groups[0].Items {
		seen[item.Title] = true
	}
	unknownShareFound := false
	for _, item := range second.Groups[0].Items {
		if seen[item.Title] {
			t.Fatal("row repeated across pages", item.Title)
		}
		claim, err := s.resolveAvailableClaim(item.Token, actor.User.ID)
		if item.Title == shortQuery {
			if err != nil || item.SourceKind != "115_share" || item.MatchedName != "" || claim.ManualTMDBID != nil || claim.SearchIdentity == nil || claim.SearchIdentity.TMDBID != 346 ||
				!slices.Equal(claim.SearchIdentity.Names, first.QueryNames) || slices.ContainsFunc(first.QueryNames, func(name tmdb.SearchName) bool { return name.Value == shortQuery }) {
				t.Fatal("short query changed trusted identity names or bypassed share inspection", err)
			}
			unknownShareFound = true
			continue
		}
		if err != nil || claim.ManualTMDBID == nil || *claim.ManualTMDBID != 346 {
			t.Fatal("snapshot lost actor identity claim", err)
		}
	}
	if !unknownShareFound {
		t.Fatal("unverified share disappeared instead of remaining available to inspect")
	}
	input.TypePolicy = "manual"
	input.CloudTypes = []string{"magnet"}
	changed, err := s.SearchMediaIdentity(context.Background(), actor, input)
	if err != nil || len(changed.Groups) != 1 || changed.Groups[0].ErrorCode != CodeSiteResultExpired || len(searchKeywords) != requests {
		t.Fatalf("scope drift accepted: %+v err=%v", changed, err)
	}
	input.TypePolicy, input.CloudTypes = "all", nil
	if err := s.db.Model(&receiver).Update("enabled", false).Error; err != nil {
		t.Fatal(err)
	}
	changed, err = s.SearchMediaIdentity(context.Background(), actor, input)
	if err != nil || len(changed.Groups) != 1 || changed.Groups[0].ErrorCode != CodeSiteResultExpired || len(searchKeywords) != requests {
		t.Fatalf("receiver change reused identity snapshot: %+v err=%v", changed, err)
	}
}
