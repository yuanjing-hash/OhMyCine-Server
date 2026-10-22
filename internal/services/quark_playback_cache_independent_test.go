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

// The real service/catalog/credential boundary is retained. Only the provider
// is synthetic; the blocked call deliberately can outlive invalidation.
type independentQuarkPlaybackDriver struct {
	*fakeCloudDriver
	reads  atomic.Int32
	lookup func(context.Context, int32) error
	link   func(context.Context, int32) (cloud.TemporaryURL, error)
}

func (d *independentQuarkPlaybackDriver) ResolveScopedItem(ctx context.Context, _, _ string, id string) (cloud.Item, error) {
	n := d.reads.Add(1)
	if d.lookup != nil {
		if err := d.lookup(ctx, n); err != nil {
			return cloud.Item{}, err
		}
	}
	return d.items[id], nil
}

func (d *independentQuarkPlaybackDriver) DirectURL(ctx context.Context, _ cloud.DirectURLRequest) (cloud.TemporaryURL, error) {
	n := d.directURLCalls.Add(1)
	if d.link != nil {
		return d.link(ctx, n)
	}
	return cloud.TemporaryURL{URL: "https://synthetic.invalid/original", ExpiresAt: time.Now().Add(time.Minute)}, nil
}

func independentQuarkPlaybackFixture(t *testing.T) (*SignedProxyService, *independentQuarkPlaybackDriver, Actor, uint) {
	t.Helper()
	s, original, actor, id := quarkPlaybackCheckFixture(t)
	d := &independentQuarkPlaybackDriver{fakeCloudDriver: original.fakeCloudDriver}
	s.connections.registry = cloud.NewRegistry()
	if err := s.connections.registry.Register(cloud.ProviderQuark, func(cloud.Config) (cloud.Driver, error) { return d, nil }); err != nil {
		t.Fatal(err)
	}
	return s, d, actor, id
}

type independentPlaybackResult struct {
	stream PlayerStreamResolution
	err    error
}

func independentPlaybackAwait(t *testing.T, result <-chan independentPlaybackResult) independentPlaybackResult {
	t.Helper()
	select {
	case value := <-result:
		return value
	case <-time.After(4 * time.Second):
		t.Fatal("bounded synthetic resolver did not return")
		return independentPlaybackResult{}
	}
}

func TestQuarkPlaybackCacheIndependentFollowerCancellation(t *testing.T) {
	s, d, actor, id := independentQuarkPlaybackFixture(t)
	started, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	d.lookup = func(_ context.Context, n int32) error {
		if n == 1 {
			close(started)
		}
		<-release
		return nil
	}
	leader := make(chan independentPlaybackResult, 1)
	go func() {
		result, err := s.ResolvePlayerEntry(context.Background(), actor, id, "", "127.0.0.1:10001")
		leader <- independentPlaybackResult{result, err}
	}()
	select {
	case <-started:
	case <-time.After(4 * time.Second):
		t.Fatal("leader did not enter provider")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	follower := make(chan independentPlaybackResult, 1)
	go func() {
		result, err := s.ResolvePlayerEntry(ctx, actor, id, "", "127.0.0.1:10002")
		follower <- independentPlaybackResult{result, err}
	}()
	// A cancelled follower must leave even while the original cloud call blocks.
	if result := independentPlaybackAwait(t, follower); result.err == nil || result.stream.RedirectURL != "" {
		t.Fatal("cancelled follower received success")
	}
	if d.reads.Load() != 1 {
		t.Fatal("cancelled follower started another provider read")
	}
	// Release exactly once, while retaining cleanup on early test failure.
	unblock()
	if result := independentPlaybackAwait(t, leader); result.err != nil || result.stream.RedirectURL == "" {
		t.Fatalf("follower cancellation affected leader: %v", result.err)
	}
	before := d.reads.Load()
	if _, err := s.ResolvePlayerEntry(context.Background(), actor, id, "", "127.0.0.1:10003"); err != nil || d.reads.Load() != before {
		t.Fatal("leader result was not available to later permitted stream")
	}
}

func TestQuarkPlaybackCacheIndependentFreshFailureFencesOldFlight(t *testing.T) {
	s, d, actor, id := independentQuarkPlaybackFixture(t)
	started, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	d.lookup = func(_ context.Context, n int32) error {
		if n == 1 {
			close(started)
			<-release
			return nil
		}
		return &quark.Failure{Code: quark.CodeAuth}
	}
	old := make(chan independentPlaybackResult, 1)
	go func() {
		result, err := s.ResolvePlayerEntry(context.Background(), actor, id, "", "127.0.0.1:10001")
		old <- independentPlaybackResult{result, err}
	}()
	select {
	case <-started:
	case <-time.After(4 * time.Second):
		t.Fatal("old flight did not enter provider")
	}
	freshContext, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := s.CheckPlayerEntryDetails(freshContext, actor, id, "", "127.0.0.1:10002"); ErrorCode(err) != quark.CodeAuth {
		t.Fatalf("explicit fresh check reused old flight: %v", err)
	}
	close(release)
	if result := independentPlaybackAwait(t, old); result.err == nil || result.stream.RedirectURL != "" {
		t.Fatal("old flight returned success after explicit failed revalidation")
	}
	before := d.reads.Load()
	if result, err := s.ResolvePlayerEntry(context.Background(), actor, id, "", "127.0.0.1:10003"); ErrorCode(err) != quark.CodeAuth || result.RedirectURL != "" || d.reads.Load() != before+1 {
		t.Fatal("old inflight completion repopulated usable result")
	}
}

func TestQuarkPlaybackCacheIndependentWarmLocalFences(t *testing.T) {
	for _, scenario := range []string{"library_disabled", "storage_disabled", "connection_disabled", "library_root", "storage_root", "connection_revision", "credential_rotation", "catalog_config", "catalog_removal"} {
		t.Run(scenario, func(t *testing.T) {
			s, d, actor, id := independentQuarkPlaybackFixture(t)
			ctx := context.Background()
			source, err := s.playerStreamSource(ctx, actor, id)
			if err != nil {
				t.Fatal(err)
			}
			if stream, err := s.ResolvePlayerEntry(ctx, actor, id, "", "127.0.0.1:10001"); err != nil || stream.RedirectURL == "" {
				t.Fatalf("warmup: %v", err)
			}
			if d.reads.Load() != 1 {
				t.Fatal("fixture warmup was not one resolution")
			}
			connectionID := *source.Storage.ConnectionID
			write := s.connections.db
			var changed error
			switch scenario {
			case "library_disabled":
				changed = write.Model(&models.MediaLibrary{}).Where("id=?", source.Library.ID).Update("enabled", false).Error
			case "storage_disabled":
				changed = write.Model(&models.Storage{}).Where("id=?", source.Storage.ID).Update("enabled", false).Error
			case "connection_disabled":
				changed = write.Model(&models.Connection{}).Where("id=?", connectionID).Update("enabled", false).Error
			case "library_root":
				changed = write.Model(&models.MediaLibrary{}).Where("id=?", source.Library.ID).Update("provider_root_id", "new-root").Error
			case "storage_root":
				changed = write.Model(&models.Storage{}).Where("id=?", source.Storage.ID).Update("root_path", "new-storage-root").Error
			case "connection_revision":
				changed = write.Model(&models.Connection{}).Where("id=?", connectionID).Update("revision", gorm.Expr("revision + 1")).Error
			case "credential_rotation":
				saveQuarkTestBundle(t, s.connections, connectionID, func(c *quark.Credentials) { c.TV.AccessToken = "synthetic-rotated"; c.TV.Generation++ })
			case "catalog_config":
				changed = write.Model(&models.CatalogHead{}).Where("library_id=?", source.Library.ID).Update("config_fingerprint", "next-config").Error
			case "catalog_removal":
				candidate, token := catalogCandidate(t, s.catalogStore, source.Library, "delta", source.Head.Revision)
				fact := CatalogEntryFromLegacy(source.Entry, nil)
				fact.Tombstone = true
				if err := s.catalogStore.AppendBatch(ctx, candidate.ID, token, CatalogFactBatch{Entries: []models.CatalogEntryFact{fact}}); err != nil {
					t.Fatal(err)
				}
				catalogPublish(t, s.catalogStore, candidate, token)
			}
			if changed != nil {
				t.Fatal(changed)
			}
			// Revalidation can either reject locally or reach this fresh provider
			// failure. It must never return the previously permitted cached URL.
			d.lookup = func(context.Context, int32) error { return &quark.Failure{Code: quark.CodeAuth} }
			if stream, err := s.ResolvePlayerEntry(ctx, actor, id, "", "127.0.0.1:10002"); err == nil || stream.RedirectURL != "" {
				t.Fatal("changed local fence reused old permitted result")
			}
			if scenario == "library_disabled" || scenario == "storage_disabled" || scenario == "connection_disabled" || scenario == "catalog_removal" {
				if d.reads.Load() != 1 {
					t.Fatal("local rejection unnecessarily contacted provider")
				}
			}
		})
	}
}

func TestQuarkPlaybackCacheIndependentURLExpiresBeforePublication(t *testing.T) {
	s, d, actor, id := independentQuarkPlaybackFixture(t)
	initial := time.Now().UTC()
	var postLinkChecks atomic.Int32
	s.now = func() time.Time {
		// The provider URL is valid at acquisition. Time then passes while the
		// service performs its final local checks before publishing a response.
		if d.directURLCalls.Load() > 0 && postLinkChecks.Add(1) > 1 {
			return initial.Add(2 * time.Minute)
		}
		return initial
	}
	stream, err := s.ResolvePlayerEntry(context.Background(), actor, id, "", "127.0.0.1:10001")
	if err == nil || stream.RedirectURL != "" {
		t.Fatal("URL expired after acquisition but was still published")
	}
	if d.directURLCalls.Load() != 1 {
		t.Fatal("expiry handling made another cloud call")
	}
}

func TestQuarkPlaybackMembershipIndependentInvalidationFencesWarmLink(t *testing.T) {
	for _, scenario := range []string{"fresh_check", "warm_link_failure", "cold_other_client_failure"} {
		t.Run(scenario, func(t *testing.T) {
			s, d, actor, id := independentQuarkPlaybackFixture(t)
			started, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			var failReads atomic.Bool
			d.lookup = func(context.Context, int32) error {
				if failReads.Load() {
					return &quark.Failure{Code: quark.CodeAuth}
				}
				return nil
			}
			d.link = func(_ context.Context, n int32) (cloud.TemporaryURL, error) {
				if n == 2 {
					close(started)
					<-release // Deliberately model a provider completion after invalidation.
				}
				if n == 3 {
					return cloud.TemporaryURL{}, &quark.Failure{Code: quark.CodeAuth}
				}
				return cloud.TemporaryURL{URL: fmt.Sprintf("https://synthetic.invalid/request-%d", n)}, nil
			}
			if _, err := s.ResolvePlayerEntry(context.Background(), actor, id, "A", "127.0.0.1:1"); err != nil {
				t.Fatal(err)
			}
			late := make(chan independentPlaybackResult, 1)
			go func() {
				stream, err := s.ResolvePlayerEntry(context.Background(), actor, id, "A", "127.0.0.1:2")
				late <- independentPlaybackResult{stream, err}
			}()
			select {
			case <-started:
			case <-time.After(4 * time.Second):
				t.Fatal("warm proof did not start its own link lookup")
			}
			if scenario == "fresh_check" {
				failReads.Store(true)
				if _, err := s.CheckPlayerEntryDetails(context.Background(), actor, id, "A", "127.0.0.1:3"); ErrorCode(err) != quark.CodeAuth {
					t.Fatalf("fresh check did not expose provider failure: %v", err)
				}
			} else {
				ua := "A"
				if scenario == "cold_other_client_failure" {
					ua = "B"
				}
				if stream, err := s.ResolvePlayerEntry(context.Background(), actor, id, ua, "127.0.0.1:3"); ErrorCode(err) != quark.CodeAuth || stream.RedirectURL != "" {
					t.Fatalf("link failure did not reach caller: %v", err)
				}
				failReads.Store(true)
			}
			unblock()
			if result := independentPlaybackAwait(t, late); result.err == nil || result.stream.RedirectURL != "" {
				t.Fatal("late warm link escaped a newer explicit provider failure")
			}
			reads, links := d.reads.Load(), d.directURLCalls.Load()
			if stream, err := s.ResolvePlayerEntry(context.Background(), actor, id, "A", "127.0.0.1:4"); ErrorCode(err) != quark.CodeAuth || stream.RedirectURL != "" || d.reads.Load() != reads+1 || d.directURLCalls.Load() != links {
				t.Fatal("invalidated membership was reused or another link was acquired")
			}
		})
	}
}

func TestQuarkPlaybackMembershipIndependentLocalFences(t *testing.T) {
	for _, scenario := range []string{"identical_metadata_head", "credential_rotation", "library_root"} {
		t.Run(scenario, func(t *testing.T) {
			s, d, actor, id := independentQuarkPlaybackFixture(t)
			d.link = func(_ context.Context, n int32) (cloud.TemporaryURL, error) {
				return cloud.TemporaryURL{URL: fmt.Sprintf("https://synthetic.invalid/fresh-%d", n)}, nil
			}
			ctx := context.Background()
			source, err := s.playerStreamSource(ctx, actor, id)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.ResolvePlayerEntry(ctx, actor, id, "", "127.0.0.1:1"); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "identical_metadata_head":
				candidate, token := catalogCandidate(t, s.catalogStore, source.Library, "delta", source.Head.Revision)
				if err := s.catalogStore.AppendBatch(ctx, candidate.ID, token, CatalogFactBatch{Entries: []models.CatalogEntryFact{CatalogEntryFromLegacy(source.Entry, nil)}}); err != nil {
					t.Fatal(err)
				}
				catalogPublish(t, s.catalogStore, candidate, token)
			case "credential_rotation":
				saveQuarkTestBundle(t, s.connections, *source.Storage.ConnectionID, func(c *quark.Credentials) { c.TV.AccessToken = "synthetic-membership-rotation"; c.TV.Generation++ })
			case "library_root":
				if err := s.connections.db.Model(&models.MediaLibrary{}).Where("id=?", source.Library.ID).Update("provider_root_id", "different-owned-root").Error; err != nil {
					t.Fatal(err)
				}
			}
			d.lookup = func(context.Context, int32) error { return &quark.Failure{Code: quark.CodeAuth} }
			if stream, err := s.ResolvePlayerEntry(ctx, actor, id, "", "127.0.0.1:2"); ErrorCode(err) != quark.CodeAuth || stream.RedirectURL != "" || d.reads.Load() != 2 || d.directURLCalls.Load() != 1 {
				t.Fatal("changed local identity reused membership proof")
			}
		})
	}
}

func TestQuarkPlaybackMembershipIndependentOldFailurePreservesNewProof(t *testing.T) {
	s, d, actor, id := independentQuarkPlaybackFixture(t)
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	d.link = func(_ context.Context, n int32) (cloud.TemporaryURL, error) {
		if n == 2 {
			close(started)
			<-release
			return cloud.TemporaryURL{}, &quark.Failure{Code: quark.CodeAuth}
		}
		return cloud.TemporaryURL{URL: fmt.Sprintf("https://synthetic.invalid/generation-%d", n)}, nil
	}
	ctx := context.Background()
	if _, err := s.ResolvePlayerEntry(ctx, actor, id, "A", "127.0.0.1:1"); err != nil {
		t.Fatal(err)
	}
	old := make(chan independentPlaybackResult, 1)
	go func() {
		stream, err := s.ResolvePlayerEntry(ctx, actor, id, "B", "127.0.0.1:2")
		old <- independentPlaybackResult{stream, err}
	}()
	select {
	case <-started:
	case <-time.After(4 * time.Second):
		t.Fatal("different client did not enter cold link request")
	}
	if _, err := s.CheckPlayerEntryDetails(ctx, actor, id, "A", "127.0.0.1:3"); err != nil {
		t.Fatal(err)
	}
	unblock()
	if result := independentPlaybackAwait(t, old); result.err == nil || result.stream.RedirectURL != "" {
		t.Fatal("old generation unexpectedly succeeded")
	}
	if stream, err := s.ResolvePlayerEntry(ctx, actor, id, "A", "127.0.0.1:4"); err != nil || stream.RedirectURL != "https://synthetic.invalid/generation-4" || d.reads.Load() != 3 || d.directURLCalls.Load() != 4 {
		t.Fatal("late old failure removed the newly verified membership or reused its URL")
	}
}

func TestQuarkPlaybackMembershipIndependentExpiryBoundaryRevalidation(t *testing.T) {
	s, d, actor, id := independentQuarkPlaybackFixture(t)
	var clock atomic.Int64
	initial := time.Now().UTC()
	clock.Store(initial.UnixNano())
	s.now = func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	var warmDeadline time.Time
	d.link = func(ctx context.Context, n int32) (cloud.TemporaryURL, error) {
		if n == 2 {
			warmDeadline, _ = ctx.Deadline()
			clock.Store(initial.Add(30200 * time.Millisecond).UnixNano())
		}
		if n == 3 {
			deadline, ok := ctx.Deadline()
			if !ok || warmDeadline.IsZero() || deadline.After(warmDeadline) {
				t.Fatal("proof-expiry recovery extended the original request deadline")
			}
		}
		return cloud.TemporaryURL{URL: fmt.Sprintf("https://synthetic.invalid/boundary-%d", n)}, nil
	}
	ctx := context.Background()
	if _, err := s.ResolvePlayerEntry(ctx, actor, id, "", "127.0.0.1:1"); err != nil {
		t.Fatal(err)
	}
	clock.Store(initial.Add(29800 * time.Millisecond).UnixNano())
	stream, err := s.ResolvePlayerEntry(ctx, actor, id, "", "127.0.0.1:2")
	if err != nil || stream.RedirectURL != "https://synthetic.invalid/boundary-3" || d.reads.Load() != 2 || d.directURLCalls.Load() != 3 {
		t.Fatalf("successful lookup crossing proof expiry was not revalidated once: reads=%d links=%d err=%v", d.reads.Load(), d.directURLCalls.Load(), err)
	}
	clock.Store(initial.Add(60100 * time.Millisecond).UnixNano())
	if stream, err = s.ResolvePlayerEntry(ctx, actor, id, "", "127.0.0.1:3"); err != nil || stream.RedirectURL != "https://synthetic.invalid/boundary-4" || d.reads.Load() != 2 {
		t.Fatal("revalidated proof did not retain its own bounded lifetime/fresh link")
	}
	clock.Store(initial.Add(60200 * time.Millisecond).UnixNano())
	if stream, err = s.ResolvePlayerEntry(ctx, actor, id, "", "127.0.0.1:4"); err != nil || stream.RedirectURL != "https://synthetic.invalid/boundary-5" || d.reads.Load() != 3 || d.directURLCalls.Load() != 5 {
		t.Fatal("revalidated proof slid beyond its new 30-second deadline")
	}
}

func TestQuarkPlaybackMembershipIndependentExpiryDoesNotRetryFailures(t *testing.T) {
	for _, scenario := range []string{"provider_failure", "cancelled", "credential_rotation", "forced_check_failure"} {
		t.Run(scenario, func(t *testing.T) {
			s, d, actor, id := independentQuarkPlaybackFixture(t)
			var clock atomic.Int64
			initial := time.Now().UTC()
			clock.Store(initial.UnixNano())
			s.now = func() time.Time { return time.Unix(0, clock.Load()).UTC() }
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			source, err := s.playerStreamSource(ctx, actor, id)
			if err != nil {
				t.Fatal(err)
			}
			d.lookup = func(_ context.Context, n int32) error {
				if scenario == "forced_check_failure" && n > 1 {
					return &quark.Failure{Code: quark.CodeAuth}
				}
				return nil
			}
			d.link = func(_ context.Context, n int32) (cloud.TemporaryURL, error) {
				if n == 2 {
					clock.Store(initial.Add(30200 * time.Millisecond).UnixNano())
					switch scenario {
					case "provider_failure":
						return cloud.TemporaryURL{}, &quark.Failure{Code: quark.CodeLimited, RetryAfter: time.Minute}
					case "cancelled":
						cancel()
					case "credential_rotation":
						saveQuarkTestBundle(t, s.connections, *source.Storage.ConnectionID, func(c *quark.Credentials) { c.TV.AccessToken = "synthetic-boundary-rotation"; c.TV.Generation++ })
					case "forced_check_failure":
						if _, err := s.CheckPlayerEntryDetails(context.Background(), actor, id, "", "127.0.0.1:3"); ErrorCode(err) != quark.CodeAuth {
							t.Fatal("fresh check failed to revoke the old generation")
						}
					}
				}
				return cloud.TemporaryURL{URL: fmt.Sprintf("https://synthetic.invalid/no-retry-%d", n)}, nil
			}
			if _, err := s.ResolvePlayerEntry(ctx, actor, id, "", "127.0.0.1:1"); err != nil {
				t.Fatal(err)
			}
			clock.Store(initial.Add(29800 * time.Millisecond).UnixNano())
			stream, err := s.ResolvePlayerEntry(ctx, actor, id, "", "127.0.0.1:2")
			wantReads := int32(1)
			if scenario == "forced_check_failure" {
				wantReads++
			}
			if err == nil || stream.RedirectURL != "" || d.directURLCalls.Load() != 2 || d.reads.Load() != wantReads {
				t.Fatal("proof expiry retried an error, cancellation, or changed local/explicit fence")
			}
			if scenario == "provider_failure" && ErrorCode(err) != quark.CodeLimited {
				t.Fatal("proof expiry masked the upstream cooldown error")
			}
		})
	}
}
