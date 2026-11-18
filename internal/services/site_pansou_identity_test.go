package services

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/metadata/tmdb"
	sitepkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/site"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/site/pansou"
	"golang.org/x/time/rate"
)

func TestPanSouIdentitySnapshotFreezesAliasesAndPages(t *testing.T) {
	const fullTitle = "孤单又灿烂的神：鬼怪"
	const officialAlias = "鬼怪"
	const shortQuery = "孤单灿烂的神"
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
			items = append(items, map[string]any{"unique_id": "only-short-query", "title": shortQuery, "datetime": "2026-10-30T00:00:00Z", "links": []any{map[string]string{"type": "115", "url": "https://115.com/s/fixture?password=abcd"}}})
		} else {
			for i := 0; i < 61; i++ {
				items = append(items, map[string]any{"unique_id": fmt.Sprintf("fixture-%d", i), "title": fmt.Sprintf("%s.S01.1080p.%03d", officialAlias, i), "links": []any{map[string]string{"type": "magnet", "url": fmt.Sprintf("magnet:?xt=urn:btih:%040x", i+1)}}})
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
	share := first.Groups[0].Items[0]
	if share.SourceKind != "115_share" || share.Title != shortQuery || share.MatchedName != "" {
		t.Fatalf("short-query share missing from the first page or presented as verified: source=%s title=%q", share.SourceKind, share.Title)
	}
	claim, err := s.resolveAvailableClaim(share.Token, actor.User.ID)
	if err != nil || claim.SearchIdentity == nil || claim.SearchIdentity.TMDBID != 346 || claim.ManualTMDBID != nil ||
		!slices.Equal(claim.SearchIdentity.Names, first.QueryNames) || slices.ContainsFunc(first.QueryNames, func(name tmdb.SearchName) bool { return name.Value == shortQuery }) {
		t.Fatal("short query changed the trusted identity names or bypassed share inspection", err)
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
	if err != nil || len(second.Groups) != 1 || len(second.Groups[0].Items) != 12 || second.Groups[0].HasNext || second.Groups[0].QueryAttempted || len(searchKeywords) != requests {
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
	for _, item := range second.Groups[0].Items {
		if seen[item.Title] {
			t.Fatal("row repeated across pages", item.Title)
		}
		claim, err := s.resolveAvailableClaim(item.Token, actor.User.ID)
		if err != nil || claim.ManualTMDBID == nil || *claim.ManualTMDBID != 346 {
			t.Fatal("snapshot lost actor identity claim", err)
		}
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
