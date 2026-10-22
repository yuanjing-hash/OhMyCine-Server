package services

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/metadata/tmdb"
	sitepkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/site"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/site/pansou"
)

func TestPanSouIdentitySnapshotFreezesAliasesAndPages(t *testing.T) {
	metadataServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/movie/346" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"id":346,"title":"七武士","original_title":"Seven Samurai","original_language":"ja","release_date":"1954-04-26","alternative_titles":{"titles":[{"iso_3166_1":"US","title":"Seven Samurai"}]},"translations":{"translations":[]}}`)
	}))
	defer metadataServer.Close()
	searches := 0
	panSou := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		searches++
		items := []any{}
		for i := 0; i < 61; i++ {
			items = append(items, map[string]any{"unique_id": fmt.Sprintf("fixture-%d", i), "title": fmt.Sprintf("Seven.Samurai.1954.1080p.%03d", i), "links": []any{map[string]string{"type": "magnet", "url": fmt.Sprintf("magnet:?xt=urn:btih:%040x", i+1)}}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"total": len(items), "results": items}})
	}))
	defer panSou.Close()
	s, _, actor, store, _, _ := siteFixture(t)
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
	searches = 0
	input := MediaIdentitySearchInput{MediaType: "movie", TMDBID: 346, Page: 1, SiteID: &created.ID, ClientCapabilities: []string{MixedDiscoveryCapability}, TypePolicy: "all"}
	first, err := s.SearchMediaIdentity(context.Background(), actor, input)
	if err != nil || len(first.Groups) != 1 || len(first.Groups[0].Items) != 50 || !first.Groups[0].HasNext || first.Groups[0].SnapshotID == "" {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	requests := searches
	if requests != len(first.QueryNames) {
		t.Fatalf("search requests=%d aliases=%d", requests, len(first.QueryNames))
	}
	input.Page = 2
	input.SnapshotID = first.Groups[0].SnapshotID
	second, err := s.SearchMediaIdentity(context.Background(), actor, input)
	if err != nil || len(second.Groups) != 1 || len(second.Groups[0].Items) != 11 || second.Groups[0].HasNext || searches != requests {
		t.Fatalf("second=%+v searches=%d err=%v", second, searches, err)
	}
	input.Page = 1
	back, err := s.SearchMediaIdentity(context.Background(), actor, input)
	if err != nil || len(back.Groups) != 1 || back.Groups[0].SnapshotID != first.Groups[0].SnapshotID || searches != requests {
		t.Fatal("return to first identity page refreshed upstream", err, searches)
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
	if err != nil || len(changed.Groups) != 1 || changed.Groups[0].ErrorCode != CodeSiteResultExpired || searches != requests {
		t.Fatalf("scope drift accepted: %+v err=%v", changed, err)
	}
}
