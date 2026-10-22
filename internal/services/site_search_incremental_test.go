package services

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/metadata/tmdb"
	sitepkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/site"
	"golang.org/x/time/rate"
)

type searchFunctionAdapter struct {
	*stubSiteAdapter
	search func(context.Context, sitepkg.Config, sitepkg.Query) (sitepkg.Page, error)
}

func (a *searchFunctionAdapter) Search(ctx context.Context, c sitepkg.Config, q sitepkg.Query) (sitepkg.Page, error) {
	return a.search(ctx, c, q)
}

func TestSearchTimeoutPolicyPreservesLegacyProbeAndCanReturnToAutomatic(t *testing.T) {
	service, _, actor, _, _, _ := siteFixture(t)
	adapter := &stubSiteAdapter{kind: "torznab"}
	service.adapters["torznab"] = adapter
	input := validSiteInput("slow jackett", "http://jackett.example.test:9117")
	input.Kind, input.Cookie, input.Passkey, input.APIKey = "torznab", "", "", "synthetic-key"
	created, err := service.Create(context.Background(), actor, input, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if created.TimeoutSeconds != 12 || created.SearchTimeoutSeconds != nil || created.EffectiveSearchTimeoutSeconds != 120 || adapter.lastConfig.Timeout != 12*time.Second {
		t.Fatalf("legacy probe changed: %+v %+v", created, adapter.lastConfig)
	}
	explicit := 160
	updated, err := service.Update(context.Background(), actor, created.ID, SiteUpdateInput{Revision: created.Revision, SearchTimeoutSeconds: &explicit}, RequestContext{})
	if err != nil || updated.SearchTimeoutSeconds == nil || *updated.SearchTimeoutSeconds != 160 || updated.TimeoutSeconds != 12 || adapter.lastConfig.Timeout != 12*time.Second {
		t.Fatalf("update=%+v err=%v", updated, err)
	}
	zero := 0
	automatic, err := service.Update(context.Background(), actor, created.ID, SiteUpdateInput{Revision: updated.Revision, SearchTimeoutSeconds: &zero}, RequestContext{})
	if err != nil || automatic.SearchTimeoutSeconds != nil || automatic.EffectiveSearchTimeoutSeconds != 120 {
		t.Fatalf("automatic=%+v err=%v", automatic, err)
	}
	invalid := 181
	if _, err = service.Update(context.Background(), actor, created.ID, SiteUpdateInput{Revision: automatic.Revision, SearchTimeoutSeconds: &invalid}, RequestContext{}); ErrorCode(err) != CodeInvalidRequest {
		t.Fatal("invalid search policy accepted", err)
	}
}

func TestSearchQueryBudgetStartsAfterSharedRateWaitAndCancellationDoesNotRequest(t *testing.T) {
	service, stub, actor, _, _, _ := siteFixture(t)
	created, err := service.Create(context.Background(), actor, validSiteInput("rate", "https://rate.example.test"), RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	var record models.Site
	if err = service.db.First(&record, created.ID).Error; err != nil {
		t.Fatal(err)
	}
	policy := defaultSearchPolicy
	policy.Query = 100 * time.Millisecond
	policy.RateWait = 150 * time.Millisecond
	service.searchPolicy = &policy
	limiter := rate.NewLimiter(rate.Every(35*time.Millisecond), 1)
	limiter.Allow()
	service.limits[record.ID] = &siteLimiter{revision: record.Revision, limiter: limiter}
	var calls atomic.Int32
	service.adapters[record.Kind] = &searchFunctionAdapter{stubSiteAdapter: stub, search: func(ctx context.Context, c sitepkg.Config, q sitepkg.Query) (sitepkg.Page, error) {
		calls.Add(1)
		deadline, _ := ctx.Deadline()
		if time.Until(deadline) < 80*time.Millisecond || c.SearchTimeout != 100*time.Millisecond {
			t.Errorf("query inherited wait instead of fresh request budget: %v", time.Until(deadline))
		}
		return sitepkg.Page{Page: 1}, nil
	}}
	started := time.Now()
	group := service.searchSite(context.Background(), actor, record, SiteSearchInput{Keyword: "test", Page: 1})
	if group.Status != "success" || calls.Load() != 1 || time.Since(started) < 20*time.Millisecond {
		t.Fatalf("group=%+v calls=%d duration=%v", group, calls.Load(), time.Since(started))
	}
	policy.RateWait = 5 * time.Millisecond
	limiter = rate.NewLimiter(rate.Every(time.Hour), 1)
	limiter.Allow()
	service.limits[record.ID] = &siteLimiter{revision: record.Revision, limiter: limiter}
	group = service.searchSite(context.Background(), actor, record, SiteSearchInput{Keyword: "test", Page: 1})
	if group.ErrorCode != CodeSearchRateWait || group.QueryAttempted || calls.Load() != 1 {
		t.Fatalf("local refusal made request: %+v calls=%d", group, calls.Load())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	group = service.searchSite(ctx, actor, record, SiteSearchInput{Keyword: "test", Page: 1})
	if group.ErrorCode != CodeSearchCancelled || calls.Load() != 1 {
		t.Fatalf("cancel misclassified: %+v", group)
	}
}

func TestIdentityCumulativeSnapshotsArriveBeforeLateTimeoutAndKeepStableClaims(t *testing.T) {
	for _, detailed := range []bool{false, true} {
		t.Run(fmt.Sprint(detailed), func(t *testing.T) {
			service, stub, actor, _, _, _ := siteFixture(t)
			created, err := service.Create(context.Background(), actor, validSiteInput("aliases", "https://aliases.example.test"), RequestContext{})
			if err != nil {
				t.Fatal(err)
			}
			var record models.Site
			service.db.First(&record, created.ID)
			policy := defaultSearchPolicy
			policy.Query = 30 * time.Millisecond
			service.searchPolicy = &policy
			var calls atomic.Int32
			firstPublished := false
			service.adapters[record.Kind] = &searchFunctionAdapter{stubSiteAdapter: stub, search: func(ctx context.Context, c sitepkg.Config, q sitepkg.Query) (sitepkg.Page, error) {
				if calls.Add(1) == 1 {
					return sitepkg.Page{Page: 1, Items: []sitepkg.Result{{TorrentID: "private-source", Title: "Seven.Samurai.1954.1080p", Tags: []string{"initial"}}}}, nil
				}
				if detailed && !firstPublished {
					t.Error("second query started before first batch was published")
				}
				<-ctx.Done()
				return sitepkg.Page{}, ctx.Err()
			}}
			var snapshots []SiteSearchGroup
			var progress []SiteSearchProgress
			ctx := context.WithValue(context.Background(), detailedSearchKey{}, detailed)
			err = service.runSiteSearches(ctx, []models.Site{record}, func(ctx context.Context, r models.Site) SiteSearchGroup {
				return service.searchMediaIdentitySite(ctx, actor, MediaIdentitySearchInput{MediaType: "movie", TMDBID: 346, Page: 1}, tmdb.Match{Title: "七武士"}, []tmdb.SearchName{{Value: "七武士"}, {Value: "Seven Samurai"}}, r)
			}, func(g SiteSearchGroup) {
				if g.Complete != nil && !*g.Complete {
					firstPublished = true
				}
				snapshots = append(snapshots, g)
			}, func(p SiteSearchProgress) { progress = append(progress, p) })
			if err != nil {
				t.Fatal(err)
			}
			if !detailed && len(snapshots) != 1 {
				t.Fatalf("legacy received interim events: %d", len(snapshots))
			}
			if detailed && len(snapshots) < 2 {
				t.Fatalf("no interim snapshots: %+v", snapshots)
			}
			final := snapshots[len(snapshots)-1]
			if final.Status != "success" || !final.Partial || final.IncompleteReason != CodeSearchRequestTimeout || final.QueriesCompleted != 1 || final.QueriesFailed != 1 || final.QueriesTotal != 2 || len(final.Items) != 1 {
				t.Fatalf("final=%+v", final)
			}
			for _, g := range snapshots {
				if len(g.Items) > 0 && g.Items[0].Token != final.Items[0].Token {
					t.Fatal("snapshot reissued existing token")
				}
			}
			claim, err := service.resolveAvailableClaim(final.Items[0].Token, actor.User.ID)
			if err != nil || claim.TorrentID != "private-source" || !claim.Published {
				t.Fatalf("published claim lost: %+v %v", claim, err)
			}
			previousSequence := uint64(0)
			for _, p := range progress {
				if p.EventSequence <= previousSequence || p.Completed > 1 || p.Completed != p.Succeeded+p.Failed || p.Total != p.Pending+p.Running+p.Completed || p.ResultCount > 1 {
					t.Fatalf("counter/revision changed per batch: %+v", p)
				}
				previousSequence = p.EventSequence
			}
			encoded, _ := json.Marshal(final)
			if strings.Contains(string(encoded), "private-source") || strings.Contains(string(encoded), "resource_fingerprint") {
				t.Fatal("private identity leaked")
			}
		})
	}
}

func TestPublishedShareClaimSurvivesNewerPostAndSnapshotSorting(t *testing.T) {
	service, stub, actor, _, _, _ := siteFixture(t)
	created, err := service.Create(context.Background(), actor, validSiteInput("shares", "https://shares.example.test"), RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	var record models.Site
	service.db.First(&record, created.ID)
	firstTime := time.Now().Add(-time.Hour)
	nextTime := firstTime.Add(time.Minute)
	calls := 0
	service.adapters[record.Kind] = &searchFunctionAdapter{stubSiteAdapter: stub, search: func(ctx context.Context, c sitepkg.Config, q sitepkg.Query) (sitepkg.Page, error) {
		calls++
		published, identity := &firstTime, "old-private-share"
		if calls > 1 {
			published, identity = &nextTime, "new-private-share"
		}
		return sitepkg.Page{Page: 1, Items: []sitepkg.Result{{TorrentID: identity, Title: "Seven.Samurai.1954.1080p", SourceKind: "115_share", Fingerprint: "same-private-share", Published: published}}}, nil
	}}
	var oldToken, newToken string
	var originalExpiry time.Time
	ctx := context.WithValue(context.Background(), detailedSearchKey{}, true)
	err = service.runSiteSearches(ctx, []models.Site{record}, func(ctx context.Context, r models.Site) SiteSearchGroup {
		return service.searchMediaIdentitySite(ctx, actor, MediaIdentitySearchInput{MediaType: "movie", TMDBID: 346, Page: 1}, tmdb.Match{}, []tmdb.SearchName{{Value: "七武士"}, {Value: "Seven Samurai"}}, r)
	}, func(g SiteSearchGroup) {
		if len(g.Items) > 0 {
			if oldToken == "" {
				oldToken = g.Items[0].Token
				originalExpiry = g.Items[0].ExpiresAt
			}
			newToken = g.Items[0].Token
		}
	}, nil)
	if err != nil || oldToken == "" || newToken == oldToken {
		t.Fatalf("replacement did not happen: %s %s %v", oldToken, newToken, err)
	}
	old, err := service.resolveAvailableClaim(oldToken, actor.User.ID)
	if err != nil || old.TorrentID != "old-private-share" {
		t.Fatalf("open operation changed/expired: %+v %v", old, err)
	}
	latest, err := service.resolveAvailableClaim(newToken, actor.User.ID)
	if err != nil || latest.TorrentID != "new-private-share" || !old.ExpiresAt.Equal(originalExpiry) {
		t.Fatalf("new claim mutated TTL/source: %+v %v", latest, err)
	}
}

func TestSearchSnapshotBudgetRetainsPublishedRowsAndReservesFinalSpace(t *testing.T) {
	budget := newSearchSnapshotBudget()
	budget.eventLimit = 1600
	budget.finalLimit = 3000
	group := SiteSearchGroup{SiteID: 1, Status: "success", QueriesTotal: 2, Items: []SiteSearchResult{{Token: "one", Title: "first", Tags: []string{"safe"}}}}
	first, ok := budget.admit(group)
	if !ok {
		t.Fatal("first batch rejected")
	}
	group.Items = append(group.Items, SiteSearchResult{Token: "two", Title: strings.Repeat("x", 2000)})
	final, ok := budget.admit(group)
	if ok || !final.Partial || final.IncompleteReason != CodeSearchResultLimit || len(final.Items) != 1 || final.Items[0].Token != first.Items[0].Token {
		t.Fatalf("limit lost existing result: %+v", final)
	}
	first.Items[0].Tags[0] = "caller-mutation"
	if budget.latest[1].Items[0].Tags[0] != "safe" {
		t.Fatal("snapshot slices shared with writer")
	}
	encoded, _ := json.Marshal(final)
	if len(encoded) > budget.eventLimit || budget.reserved > budget.finalLimit {
		t.Fatal("byte limit exceeded")
	}
}

func TestClaimCapacityDoesNotEvictPublishedOrReservedResults(t *testing.T) {
	service := &SiteService{now: time.Now, vault: map[string]siteResultClaim{}}
	for i := 0; i < maxPTResultClaims; i++ {
		service.vault[fmt.Sprint(i)] = siteResultClaim{Published: true, ExpiresAt: time.Now().Add(time.Minute)}
	}
	if _, err := service.issueClaim(siteResultClaim{ExpiresAt: time.Now().Add(time.Minute)}); ErrorCode(err) != CodeSearchResultLimit {
		t.Fatalf("published claim evicted: %v", err)
	}
	if len(service.vault) != maxPTResultClaims {
		t.Fatal("vault capacity changed")
	}
}

func TestIdentityEmptyPartialAndAllFailedAreNotAuthoritativeNoMatch(t *testing.T) {
	for _, emptyFirst := range []bool{true, false} {
		t.Run(fmt.Sprint(emptyFirst), func(t *testing.T) {
			service, stub, actor, _, _, _ := siteFixture(t)
			created, err := service.Create(context.Background(), actor, validSiteInput("empty aliases", "https://empty.example.test"), RequestContext{})
			if err != nil {
				t.Fatal(err)
			}
			var record models.Site
			service.db.First(&record, created.ID)
			calls := 0
			service.adapters[record.Kind] = &searchFunctionAdapter{stubSiteAdapter: stub, search: func(ctx context.Context, c sitepkg.Config, q sitepkg.Query) (sitepkg.Page, error) {
				calls++
				if emptyFirst && calls == 1 {
					return sitepkg.Page{Page: 1}, nil
				}
				return sitepkg.Page{}, sitepkg.ErrInvalidReply
			}}
			group := service.searchMediaIdentitySite(context.Background(), actor, MediaIdentitySearchInput{MediaType: "movie", TMDBID: 346, Page: 1}, tmdb.Match{}, []tmdb.SearchName{{Value: "七武士"}, {Value: "Seven Samurai"}}, record)
			if len(group.Items) != 0 || group.QueriesTotal != 2 || group.IncompleteReason != CodeSiteResponseInvalid {
				t.Fatalf("failure/empty facts lost: %+v", group)
			}
			if emptyFirst {
				if group.Status != "success" || !group.Partial || group.QueriesCompleted != 1 || group.QueriesFailed != 1 {
					t.Fatalf("empty partial misclassified: %+v", group)
				}
			} else if group.Status != "error" || group.QueriesCompleted != 0 || group.QueriesFailed != 2 {
				t.Fatalf("all failures became zero-hit success: %+v", group)
			}
		})
	}
}
