package services

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
	"gorm.io/gorm"
)

type quarkPlayerCacheDriver struct {
	*fakeCloudDriver
	reads  atomic.Int32
	lookup func(context.Context, int32) error
	expiry func() time.Time
	link   func(context.Context, int32) (cloud.TemporaryURL, error)
}

func (d *quarkPlayerCacheDriver) ResolveScopedItem(ctx context.Context, _, _ string, id string) (cloud.Item, error) {
	n := d.reads.Add(1)
	if d.lookup != nil {
		if err := d.lookup(ctx, n); err != nil {
			return cloud.Item{}, err
		}
	}
	return d.items[id], nil
}

func (d *quarkPlayerCacheDriver) DirectURL(ctx context.Context, _ cloud.DirectURLRequest) (cloud.TemporaryURL, error) {
	n := d.directURLCalls.Add(1)
	if d.link != nil {
		return d.link(ctx, n)
	}
	return cloud.TemporaryURL{URL: "https://synthetic.invalid/original", ExpiresAt: d.expiry()}, nil
}

func quarkPlayerCacheFixture(t *testing.T) (*SignedProxyService, *quarkPlayerCacheDriver, Actor, uint, *atomic.Int64) {
	t.Helper()
	s, original, actor, id := quarkPlaybackCheckFixture(t)
	clock := &atomic.Int64{}
	clock.Store(time.Now().UTC().UnixNano())
	s.now = func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	d := &quarkPlayerCacheDriver{fakeCloudDriver: original.fakeCloudDriver, expiry: func() time.Time { return s.now().Add(time.Hour) }}
	s.connections.registry = cloud.NewRegistry()
	if err := s.connections.registry.Register(cloud.ProviderQuark, func(cloud.Config) (cloud.Driver, error) { return d, nil }); err != nil {
		t.Fatal(err)
	}
	return s, d, actor, id, clock
}

func TestQuarkPlayerCacheConcurrentFirstAndContinuousRanges(t *testing.T) {
	s, d, actor, id, _ := quarkPlayerCacheFixture(t)
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	d.lookup = func(ctx context.Context, n int32) error {
		if n == 1 {
			close(started)
		}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	results := make(chan error, 12)
	for i := 0; i < 12; i++ {
		go func(port int) {
			result, err := s.ResolvePlayerEntry(context.Background(), actor, id, "", fmt.Sprintf("127.0.0.1:%d", 10000+port))
			if err == nil && result.RedirectURL == "" {
				err = fmt.Errorf("missing redirect")
			}
			results <- err
		}(i)
	}
	select {
	case <-started:
	case <-time.After(4 * time.Second):
		t.Fatal("no provider call")
	}
	// Keep the synthetic cloud call in flight while concurrent requests enter.
	time.Sleep(50 * time.Millisecond)
	unblock()
	for i := 0; i < 12; i++ {
		select {
		case err := <-results:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(4 * time.Second):
			t.Fatal("resolver did not finish")
		}
	}
	for i := 0; i < 20; i++ {
		if _, err := s.ResolvePlayerEntry(context.Background(), actor, id, "", fmt.Sprintf("127.0.0.1:%d", 20000+i)); err != nil {
			t.Fatal(err)
		}
	}
	if d.reads.Load() != 1 || d.directURLCalls.Load() != 1 {
		t.Fatalf("metadata=%d link=%d", d.reads.Load(), d.directURLCalls.Load())
	}
	if len(s.cache) != 0 {
		t.Fatal("Player populated the independent STRM cache")
	}
}

func TestQuarkPlayerCacheNonSlidingExpiry(t *testing.T) {
	for _, tc := range []struct {
		name     string
		urlTTL   time.Duration
		cacheTTL time.Duration
	}{
		{"hard_30_seconds", time.Hour, 30 * time.Second},
		{"url_safety_margin", 45 * time.Second, 15 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, d, actor, id, clock := quarkPlayerCacheFixture(t)
			initial := s.now()
			d.expiry = func() time.Time { return s.now().Add(tc.urlTTL) }
			for _, elapsed := range []time.Duration{0, tc.cacheTTL / 3, tc.cacheTTL - 1} {
				clock.Store(initial.Add(elapsed).UnixNano())
				if _, err := s.ResolvePlayerEntry(context.Background(), actor, id, "", "127.0.0.1:1"); err != nil {
					t.Fatal(err)
				}
			}
			if d.reads.Load() != 1 || d.directURLCalls.Load() != 1 {
				t.Fatal("premature cache miss")
			}
			clock.Store(initial.Add(tc.cacheTTL).UnixNano())
			if _, err := s.ResolvePlayerEntry(context.Background(), actor, id, "", "127.0.0.1:2"); err != nil {
				t.Fatal(err)
			}
			if d.reads.Load() != 2 || d.directURLCalls.Load() != 2 {
				t.Fatal("cache slid or exceeded expiry margin")
			}
		})
	}
}

func TestQuarkPlayerCacheUnknownExpiryAndSlowProofNotCached(t *testing.T) {
	for _, scenario := range []string{"unknown_expiry", "slow_proof"} {
		t.Run(scenario, func(t *testing.T) {
			s, d, actor, id, clock := quarkPlayerCacheFixture(t)
			if scenario == "unknown_expiry" {
				d.expiry = func() time.Time { return time.Time{} }
			} else {
				d.lookup = func(context.Context, int32) error { clock.Add(int64(31 * time.Second)); return nil }
			}
			for i := 0; i < 2; i++ {
				if _, err := s.ResolvePlayerEntry(context.Background(), actor, id, "", "127.0.0.1:1"); err != nil {
					t.Fatal(err)
				}
			}
			wantReads := int32(2)
			if scenario == "unknown_expiry" {
				wantReads = 1
			}
			if d.reads.Load() != wantReads || d.directURLCalls.Load() != 2 {
				t.Fatal("URL was reused or membership proof exceeded its lifetime")
			}
		})
	}
}

func TestQuarkPlayerMembershipUnknownConcurrentFreshLinks(t *testing.T) {
	s, d, actor, id, clock := quarkPlayerCacheFixture(t)
	initial := s.now()
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	d.lookup = func(ctx context.Context, n int32) error {
		if n == 1 {
			close(started)
		}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	d.link = func(_ context.Context, n int32) (cloud.TemporaryURL, error) {
		return cloud.TemporaryURL{URL: fmt.Sprintf("https://synthetic.invalid/original/%d", n)}, nil
	}
	results := make(chan independentPlaybackResult, 12)
	for i := 0; i < 12; i++ {
		go func(port int) {
			r, e := s.ResolvePlayerEntry(context.Background(), actor, id, "", fmt.Sprintf("127.0.0.1:%d", 10000+port))
			results <- independentPlaybackResult{r, e}
		}(i)
	}
	select {
	case <-started:
	case <-time.After(4 * time.Second):
		t.Fatal("directory was not requested")
	}
	time.Sleep(50 * time.Millisecond)
	unblock()
	links := map[string]bool{}
	for i := 0; i < 12; i++ {
		select {
		case r := <-results:
			if r.err != nil || r.stream.RedirectURL == "" || links[r.stream.RedirectURL] {
				t.Fatalf("request did not acquire its own fresh link: %v", r.err)
			}
			links[r.stream.RedirectURL] = true
		case <-time.After(4 * time.Second):
			t.Fatal("bounded parallel requests stuck")
		}
	}
	if d.reads.Load() != 1 || d.directURLCalls.Load() != 12 {
		t.Fatalf("unknown parallel reads/links=%d/%d", d.reads.Load(), d.directURLCalls.Load())
	}
	for _, seconds := range []int{10, 20, 29} {
		clock.Store(initial.Add(time.Duration(seconds) * time.Second).UnixNano())
		if _, err := s.ResolvePlayerEntry(context.Background(), actor, id, "", "127.0.0.1:20000"); err != nil {
			t.Fatal(err)
		}
	}
	if d.reads.Load() != 1 || d.directURLCalls.Load() != 15 {
		t.Fatal("unknown warm request reused URL or reread directory")
	}
	for _, scope := range s.quarkPlayer.scopes {
		if scope.entry != nil {
			t.Fatal("unknown URL entered reusable cache")
		}
	}
	clock.Store(initial.Add(30 * time.Second).UnixNano())
	if _, err := s.ResolvePlayerEntry(context.Background(), actor, id, "", "127.0.0.1:20001"); err != nil {
		t.Fatal(err)
	}
	if d.reads.Load() != 2 || d.directURLCalls.Load() != 16 {
		t.Fatal("membership lifetime slid past 30 seconds")
	}
}

func TestQuarkPlayerMembershipLinkCancellationAndRateLimit(t *testing.T) {
	for _, scenario := range []string{"cancelled_link", "rate_limited_link"} {
		t.Run(scenario, func(t *testing.T) {
			s, d, actor, id, _ := quarkPlayerCacheFixture(t)
			d.expiry = func() time.Time { return time.Time{} }
			if _, err := s.ResolvePlayerEntry(context.Background(), actor, id, "", "127.0.0.1:1"); err != nil {
				t.Fatal(err)
			}
			if scenario == "cancelled_link" {
				started := make(chan struct{})
				d.link = func(ctx context.Context, n int32) (cloud.TemporaryURL, error) {
					if n == 2 {
						close(started)
						<-ctx.Done()
						return cloud.TemporaryURL{}, ctx.Err()
					}
					return cloud.TemporaryURL{URL: "https://synthetic.invalid/new"}, nil
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				result := make(chan error, 1)
				go func() { _, e := s.ResolvePlayerEntry(ctx, actor, id, "", "127.0.0.1:2"); result <- e }()
				select {
				case <-started:
				case <-time.After(4 * time.Second):
					t.Fatal("fresh link not requested")
				}
				cancel()
				select {
				case err := <-result:
					if err == nil {
						t.Fatal("cancelled link succeeded")
					}
				case <-time.After(4 * time.Second):
					t.Fatal("cancelled link stuck")
				}
				if _, err := s.ResolvePlayerEntry(context.Background(), actor, id, "", "127.0.0.1:3"); err != nil {
					t.Fatal(err)
				}
				if d.reads.Load() != 1 || d.directURLCalls.Load() != 3 {
					t.Fatal("cancellation poisoned a valid proof or reused URL")
				}
			} else {
				d.link = func(context.Context, int32) (cloud.TemporaryURL, error) {
					return cloud.TemporaryURL{}, &quark.Failure{Code: quark.CodeLimited, RetryAfter: time.Minute}
				}
				out, err := s.ResolvePlayerEntry(context.Background(), actor, id, "", "127.0.0.1:2")
				if ErrorCode(err) != quark.CodeLimited || out.RedirectURL != "" || d.reads.Load() != 1 || d.directURLCalls.Load() != 2 {
					t.Fatal("rate limit was hidden by proof/old URL")
				}
				// Model the shared account cooldown rejecting the next directory
				// operation. The service must not fallback to the former link.
				d.lookup = func(context.Context, int32) error {
					return &quark.Failure{Code: quark.CodeLimited, RetryAfter: time.Minute}
				}
				out, err = s.ResolvePlayerEntry(context.Background(), actor, id, "", "127.0.0.1:3")
				if ErrorCode(err) != quark.CodeLimited || out.RedirectURL != "" || d.reads.Load() != 2 || d.directURLCalls.Load() != 2 {
					t.Fatal("cooldown was bypassed after failed link")
				}
			}
			if s.quarkPlayer.active != 0 {
				t.Fatal("link capacity was not released")
			}
		})
	}
}

func TestQuarkPlayerMembershipExpiresDuringLinkRechecksOnce(t *testing.T) {
	s, d, actor, id, clock := quarkPlayerCacheFixture(t)
	initial := s.now()
	d.expiry = func() time.Time { return time.Time{} }
	if _, err := s.ResolvePlayerEntry(context.Background(), actor, id, "", "127.0.0.1:1"); err != nil {
		t.Fatal(err)
	}
	clock.Store(initial.Add(29800 * time.Millisecond).UnixNano())
	var firstDeadline time.Time
	d.link = func(ctx context.Context, n int32) (cloud.TemporaryURL, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("bounded link deadline missing")
		}
		if n == 2 {
			firstDeadline = deadline
			clock.Store(initial.Add(30200 * time.Millisecond).UnixNano())
		}
		if n == 3 && !deadline.Equal(firstDeadline) {
			t.Fatal("expiry recheck extended original deadline")
		}
		return cloud.TemporaryURL{URL: fmt.Sprintf("https://synthetic.invalid/fresh/%d", n)}, nil
	}
	result, err := s.ResolvePlayerEntry(context.Background(), actor, id, "", "127.0.0.1:2")
	if err != nil || result.RedirectURL != "https://synthetic.invalid/fresh/3" || d.reads.Load() != 2 || d.directURLCalls.Load() != 3 {
		t.Fatalf("successful link crossing proof deadline was not rechecked: err=%v scoped=%d links=%d", err, d.reads.Load(), d.directURLCalls.Load())
	}
}

func TestQuarkPlayerCacheCancelledLeaderDoesNotPoisonNextRequest(t *testing.T) {
	s, d, actor, id, _ := quarkPlayerCacheFixture(t)
	started := make(chan struct{})
	d.lookup = func(ctx context.Context, n int32) error {
		if n == 1 {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}
	leaderCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	leader := make(chan error, 1)
	go func() { _, err := s.ResolvePlayerEntry(leaderCtx, actor, id, "", "127.0.0.1:1"); leader <- err }()
	select {
	case <-started:
	case <-time.After(4 * time.Second):
		t.Fatal("leader did not start")
	}
	follower := make(chan error, 1)
	go func() {
		_, err := s.ResolvePlayerEntry(context.Background(), actor, id, "", "127.0.0.1:2")
		follower <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-leader:
		if err == nil {
			t.Fatal("cancelled leader succeeded")
		}
	case <-time.After(4 * time.Second):
		t.Fatal("cancelled leader stuck")
	}
	select {
	case err := <-follower:
		if err != nil {
			t.Fatalf("live follower inherited cancellation: %v", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("follower stuck")
	}
	if d.reads.Load() != 2 || d.directURLCalls.Load() != 1 {
		t.Fatalf("read/link=%d/%d", d.reads.Load(), d.directURLCalls.Load())
	}
	if _, err := s.ResolvePlayerEntry(context.Background(), actor, id, "", "127.0.0.1:3"); err != nil || d.reads.Load() != 2 {
		t.Fatal("fresh follower result not cached")
	}
}

func TestQuarkPlayerCacheCapacityBound(t *testing.T) {
	c := quarkPlayerPlaybackCache{}
	for i := 0; i < quarkPlayerCacheMaximum+10; i++ {
		if c.scope(fmt.Sprint(i)) == nil {
			t.Fatal("idle eviction failed")
		}
	}
	if len(c.scopes) != quarkPlayerCacheMaximum {
		t.Fatal("scope capacity exceeded")
	}
	for _, scope := range c.scopes {
		scope.flights["busy"] = &quarkPlayerFlight{}
	}
	if c.scope("all-busy") != nil || len(c.scopes) != quarkPlayerCacheMaximum {
		t.Fatal("active generation evicted or capacity exceeded")
	}
	for _, scope := range c.scopes {
		delete(scope.flights, "busy")
		scope.activeLinks = 1
	}
	if c.scope("all-links-busy") != nil || len(c.scopes) != quarkPlayerCacheMaximum {
		t.Fatal("scope with an active independent link lookup was evicted")
	}
	s, d, actor, id, _ := quarkPlayerCacheFixture(t)
	s.quarkPlayer.active = quarkPlayerCacheMaximum
	if result, err := s.ResolvePlayerEntry(context.Background(), actor, id, "", ""); err == nil || result.RedirectURL != "" || d.reads.Load() != 0 {
		t.Fatal("active work budget exceeded")
	}
}

func TestQuarkPlayerCachePublishedVersionAndAccountEpochFence(t *testing.T) {
	for _, scenario := range []string{"source_epoch", "account_identity", "published_content_version", "published_identical_metadata"} {
		t.Run(scenario, func(t *testing.T) {
			s, d, actor, id, _ := quarkPlayerCacheFixture(t)
			ctx := context.Background()
			source, err := s.playerStreamSource(ctx, actor, id)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.ResolvePlayerEntry(ctx, actor, id, "", "127.0.0.1:1"); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "source_epoch":
				if err := s.db.Model(&models.CatalogHead{}).Where("library_id=?", source.Library.ID).Update("source_epoch", gorm.Expr("source_epoch + 1")).Error; err != nil {
					t.Fatal(err)
				}
			case "account_identity":
				if err := s.db.Model(&models.Connection{}).Where("id=?", *source.Storage.ConnectionID).Update("account_id", "synthetic-new-account").Error; err != nil {
					t.Fatal(err)
				}
			case "published_content_version", "published_identical_metadata":
				entry := source.Entry
				if scenario == "published_content_version" {
					entry.Size++
					entry.ModifiedAt = entry.ModifiedAt.Add(time.Second)
				}
				candidate, token := catalogCandidate(t, s.catalogStore, source.Library, "delta", source.Head.Revision)
				if err := s.catalogStore.AppendBatch(ctx, candidate.ID, token, CatalogFactBatch{Entries: []models.CatalogEntryFact{CatalogEntryFromLegacy(entry, nil)}}); err != nil {
					t.Fatal(err)
				}
				catalogPublish(t, s.catalogStore, candidate, token)
				current, err := s.playerStreamSource(ctx, actor, id)
				if err != nil || current.Head.Revision != source.Head.Revision+1 {
					t.Fatal("fixture did not publish a new Catalog head")
				}
				if scenario == "published_identical_metadata" && (current.Entry.ID != source.Entry.ID || current.Entry.ProviderID != source.Entry.ProviderID || current.Entry.RelativePath != source.Entry.RelativePath || current.Entry.Size != source.Entry.Size || !current.Entry.ModifiedAt.Equal(source.Entry.ModifiedAt)) {
					t.Fatal("fixture changed visible metadata instead of only publication revision")
				}
			}
			d.lookup = func(context.Context, int32) error { return &quark.Failure{Code: quark.CodeAuth} }
			if result, err := s.ResolvePlayerEntry(ctx, actor, id, "", "127.0.0.1:2"); err == nil || result.RedirectURL != "" {
				t.Fatal("changed current identity/version reused prior evidence")
			}
			if scenario == "published_identical_metadata" && d.reads.Load() != 2 {
				t.Fatal("new Catalog publication did not require fresh provider evidence")
			}
		})
	}
}
