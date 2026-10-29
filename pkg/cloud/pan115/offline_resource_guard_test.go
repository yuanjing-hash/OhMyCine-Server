package pan115

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

type guardedOfflineSDK struct {
	*selectedTestSDK
	adds                  int
	addErr, errorDelete   error
	applyAdd, applyDelete bool
	returned              string
}

func (s *guardedOfflineSDK) AddOfflineTaskURIs(uris []string, dir string, _ ...sdk.OfflineOption) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.adds++
	hash, _ := offlineInfoHash(uris[0])
	if s.returned != "" {
		hash = s.returned
	}
	if s.applyAdd {
		s.task = &sdk.OfflineTask{InfoHash: hash, DirId: dir, Status: 1}
	}
	return []string{hash}, s.addErr
}
func (s *guardedOfflineSDK) DeleteOfflineTasks(hashes []string, remove bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancels++
	if s.applyDelete {
		s.task = nil
	}
	return s.errorDelete
}
func guardedFixture(t *testing.T) (*Client, *guardedOfflineSDK, *selectedMemoryStore, cloud.SelectiveOfflineRequest, string) {
	c, state, store, r := selectedFixture(t)
	wire := &guardedOfflineSDK{selectedTestSDK: state, applyAdd: true, applyDelete: true}
	c.sdk = wire
	return c, wire, store, r, "magnet:?xt=urn:btih:" + r.Authority.Selection.Identity.V1
}
func ordinaryTestContext(task string) context.Context {
	return cloud.WithOfflineOperationScope(context.Background(), "fixture:"+task)
}
func TestOrdinaryOfflineCannotAdoptOrCancelSelectedOwner(t *testing.T) {
	c, s, _, r, uri := guardedFixture(t)
	if _, err := c.SubmitSelectedOffline(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SubmitOffline(ordinaryTestContext("ordinary"), uri, "ordinary"); err == nil {
		t.Fatal("ordinary adopted selected")
	}
	if _, err := c.GetOffline(context.Background(), r.Authority.Selection.Identity.V1); err == nil {
		t.Fatal("ordinary read adopted selected")
	}
	if err := c.CancelOffline(ordinaryTestContext("ordinary"), r.Authority.Selection.Identity.V1, false); err == nil {
		t.Fatal("ordinary cancelled selected")
	}
	if s.adds != 0 || s.cancels != 0 || s.task == nil {
		t.Fatal("ordinary touched selected", s.adds, s.cancels)
	}
}
func TestOrdinaryOfflineReleasesConfirmedGuardButLateTerminalCannotTouchSuccessor(t *testing.T) {
	c, s, store, r, uri := guardedFixture(t)
	ctx := ordinaryTestContext("old")
	result, err := c.SubmitOffline(ctx, uri, "ordinary")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.ResourceClaim(ctx, offlineResourceKey(result.ID)); !errors.Is(err, cloud.ErrOperationNotFound) {
		t.Fatal("confirmed add retained mode", err)
	}
	if err = c.CancelOffline(ctx, result.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err = c.SubmitSelectedOffline(context.Background(), r); err != nil {
		t.Fatal("ordinary terminal blocked future selected", err)
	}
	if _, err = c.SubmitOffline(ctx, uri, "ordinary"); err == nil {
		t.Fatal("old submit returned successor")
	}
	if err = c.CancelOffline(ctx, result.ID, false); err == nil {
		t.Fatal("old terminal cancel adopted successor")
	}
	if s.adds != 1 || s.cancels != 1 || s.submits != 1 || s.task == nil {
		t.Fatal("late terminal mutated successor")
	}
}
func TestOrdinaryOfflineUnknownNeverReplaysOrReleasesOnAbsence(t *testing.T) {
	for _, kind := range []string{"submit-absent", "cancel-absent", "cancel-present"} {
		t.Run(kind, func(t *testing.T) {
			c, s, store, r, uri := guardedFixture(t)
			ctx := ordinaryTestContext(kind)
			if kind == "submit-absent" {
				s.applyAdd, s.addErr = false, errors.New("lost ACK")
				if _, err := c.SubmitOffline(ctx, uri, "ordinary"); err == nil {
					t.Fatal("unknown add accepted")
				}
				_, _ = c.SubmitOffline(ctx, uri, "ordinary")
			} else {
				if _, err := c.SubmitOffline(ctx, uri, "ordinary"); err != nil {
					t.Fatal(err)
				}
				s.errorDelete = errors.New("lost ACK")
				s.applyDelete = kind == "cancel-absent"
				if err := c.CancelOffline(ctx, r.Authority.Selection.Identity.V1, false); err == nil {
					t.Fatal("unknown cancel accepted")
				}
				_ = c.CancelOffline(ctx, r.Authority.Selection.Identity.V1, false)
			}
			if _, err := store.ResourceClaim(ctx, offlineResourceKey(r.Authority.Selection.Identity.V1)); err != nil {
				t.Fatal("absence released unknown owner", err)
			}
			if _, err := c.SubmitSelectedOffline(context.Background(), r); err == nil {
				t.Fatal("selected overlapped unknown ordinary")
			}
			if s.adds > 1 || s.cancels > 1 || s.submits != 0 {
				t.Fatal("unknown request replayed")
			}
		})
	}
}
func TestOrdinaryOfflineKnownDuplicateAndPositiveOwnDirectoryRecovery(t *testing.T) {
	for _, kind := range []string{"duplicate", "lost-ack-owned"} {
		t.Run(kind, func(t *testing.T) {
			c, s, _, r, uri := guardedFixture(t)
			if kind == "duplicate" {
				s.task = &sdk.OfflineTask{InfoHash: r.Authority.Selection.Identity.V1, DirId: "legacy-user", Status: 1}
				s.applyAdd, s.addErr = false, sdk.ErrOfflineTaskExisted
			} else {
				s.addErr = errors.New("lost ACK")
			}
			ctx := ordinaryTestContext(kind)
			if _, err := c.SubmitOffline(ctx, uri, "ordinary"); err != nil {
				t.Fatal(err)
			}
			if _, err := c.SubmitOffline(ctx, uri, "ordinary"); err != nil || s.adds != 1 {
				t.Fatal("ordinary recovery replayed", err)
			}
		})
	}
}
func TestOrdinaryOfflineConcurrentSelectedAndWholeAreMutuallyExclusive(t *testing.T) {
	for i := 0; i < 12; i++ {
		c, s, _, r, uri := guardedFixture(t)
		var done sync.WaitGroup
		done.Add(2)
		go func() { defer done.Done(); _, _ = c.SubmitOffline(ordinaryTestContext("whole"), uri, "ordinary") }()
		go func() { defer done.Done(); _, _ = c.SubmitSelectedOffline(context.Background(), r) }()
		done.Wait()
		if s.adds+s.submits != 1 {
			t.Fatal("both modes dispatched", s.adds, s.submits)
		}
	}
}
func TestOrdinaryOfflineRejectsConflictingMagnetTopicsAndGuardsUnknownURIResult(t *testing.T) {
	c, s, _, r, uri := guardedFixture(t)
	if _, err := c.SubmitOffline(ordinaryTestContext("ambiguous"), uri+"&xt=urn:btih:"+strings.Repeat("f", 40), "ordinary"); err == nil || s.adds != 0 {
		t.Fatal("ambiguous magnet bypassed identity guard")
	}
	if _, err := c.SubmitSelectedOffline(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	s.returned = r.Authority.Selection.Identity.V1
	s.applyAdd = false
	if _, err := c.SubmitOffline(ordinaryTestContext("http"), "https://download.example.test/movie", "ordinary"); err == nil {
		t.Fatal("opaque URI adopted selected result")
	}
	if s.adds != 1 || s.cancels != 0 {
		t.Fatal("unknown URI semantics changed or cancel leaked")
	}
}
func TestRemoteOrdinaryGuardHoldsUntilDurableCompleteAndNeverReopensSelected(t *testing.T) {
	c, _, store, r, uri := guardedFixture(t)
	access := cloud.OfflineAccess{OperationID: "node:original", URI: uri, Remote: true}
	if err := c.BeginOfflineAccess(context.Background(), access); err != nil {
		t.Fatal(err)
	}
	if err := c.BeginOfflineAccess(context.Background(), access); err != nil {
		t.Fatal("renew changed operation", err)
	}
	if _, err := c.SubmitSelectedOffline(context.Background(), r); err == nil {
		t.Fatal("pending Node did not hold resource")
	}
	if err := c.CompleteOfflineAccess(context.Background(), access); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ResourceClaim(context.Background(), offlineResourceKey(r.Authority.Selection.Identity.V1)); !errors.Is(err, cloud.ErrOperationNotFound) {
		t.Fatal(err)
	}
	if _, err := c.SubmitSelectedOffline(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if err := c.BeginOfflineAccess(context.Background(), access); err == nil {
		t.Fatal("late Node grant reused selected owner")
	}
	if err := c.CompleteOfflineAccess(context.Background(), access); err == nil {
		t.Fatal("late Node completion released selected owner")
	}
}

func TestOrdinaryOfflineUndispatchedFailureReleasesAndOnlyOriginalInputCanRearm(t *testing.T) {
	c, s, store, r, uri := guardedFixture(t)
	ctx := ordinaryTestContext("auth-retry")
	c.beforeCall = func(context.Context) error { return cloud.Error(cloud.CodeAuthExpired, false, nil) }
	if _, err := c.SubmitOffline(ctx, uri, "ordinary"); err == nil || s.adds != 0 {
		t.Fatal("predispatch failure wrote")
	}
	if _, err := store.ResourceClaim(context.Background(), offlineResourceKey(r.Authority.Selection.Identity.V1)); !errors.Is(err, cloud.ErrOperationNotFound) {
		t.Fatal("undispatched guard stayed pinned", err)
	}
	cold := selectedClient(s.selectedTestSDK, store)
	cold.sdk = s
	if _, err := cold.SubmitOffline(ctx, uri, "changed-directory"); err == nil || s.adds != 0 {
		t.Fatal("rearm changed frozen input")
	}
	if _, err := cold.SubmitOffline(ctx, uri, "ordinary"); err != nil || s.adds != 1 {
		t.Fatal("credential refresh could not rearm original", err)
	}
}

type releaseFailureStore struct {
	cloud.ResourceOperationStore
	before, after bool
}

func (s *releaseFailureStore) ReleaseResourceClaim(ctx context.Context, key string, revision uint64, r cloud.OperationReceipt) error {
	if s.before {
		s.before = false
		return errors.New("checkpoint unavailable")
	}
	if err := s.ResourceOperationStore.ReleaseResourceClaim(ctx, key, revision, r); err != nil {
		return err
	}
	if s.after {
		s.after = false
		return errors.New("commit ACK lost")
	}
	return nil
}
func TestOrdinaryOfflineLostReceiptACKDoesNotResubmitOrRecancel(t *testing.T) {
	for _, kind := range []string{"submit-after-release", "cancel-before-release"} {
		t.Run(kind, func(t *testing.T) {
			c, s, store, r, uri := guardedFixture(t)
			ctx := ordinaryTestContext(kind)
			wrapper := &releaseFailureStore{ResourceOperationStore: store}
			c.selective.store = wrapper
			if kind == "submit-after-release" {
				wrapper.after = true
				if _, err := c.SubmitOffline(ctx, uri, "ordinary"); err == nil {
					t.Fatal("lost receipt ACK concealed")
				}
				if _, err := c.SubmitOffline(ctx, uri, "ordinary"); err != nil || s.adds != 1 {
					t.Fatal("replayed known submit", err)
				}
			} else {
				if _, err := c.SubmitOffline(ctx, uri, "ordinary"); err != nil {
					t.Fatal(err)
				}
				wrapper.before = true
				if err := c.CancelOffline(ctx, r.Authority.Selection.Identity.V1, false); err == nil {
					t.Fatal("missing checkpoint concealed")
				}
				if err := c.CancelOffline(ctx, r.Authority.Selection.Identity.V1, false); err != nil || s.cancels != 1 {
					t.Fatal("replayed known cancel", err)
				}
			}
		})
	}
}

func TestCredentialResourceAccountRejectsAmbiguityAndIsNotAProbe(t *testing.T) {
	for _, uid := range []string{"0", "bad", "+100", "100_", "9223372036854775808"} {
		if _, err := CredentialResourceAccount("UID=" + uid + "; CID=c; SEID=s"); err == nil {
			t.Fatal("invalid credential namespace", uid)
		}
	}
	if account, err := CredentialResourceAccount("UID=00100_A1_timestamp; CID=c; SEID=s"); err != nil || account != "100" {
		t.Fatal("numeric namespace not canonical", account, err)
	}
	if _, err := CredentialResourceAccount("UID=100; UID=100_A1; CID=c; SEID=s"); err == nil {
		t.Fatal("duplicate identity accepted")
	}
}

func TestOrdinaryOfflineTypedRejectionReleasesAndCredentialRefreshCanRetry(t *testing.T) {
	for _, rejection := range []error{sdk.ErrNotLogin, sdk.ErrBadCookie, sdk.ErrOfflineNoTimes, sdk.ErrOfflineInvalidLink} {
		t.Run(rejection.Error(), func(t *testing.T) {
			c, s, store, r, uri := guardedFixture(t)
			ctx := ordinaryTestContext("rejected")
			s.applyAdd, s.addErr = false, fmt.Errorf("typed SDK rejection: %w", rejection)
			if _, err := c.SubmitOffline(ctx, uri, "ordinary"); !errors.Is(err, rejection) {
				t.Fatal("SDK rejection classification lost", err)
			}
			if _, err := store.ResourceClaim(context.Background(), offlineResourceKey(r.Authority.Selection.Identity.V1)); !errors.Is(err, cloud.ErrOperationNotFound) {
				t.Fatal("typed refusal pinned resource", err)
			}
			s.applyAdd, s.addErr = true, nil
			cold := selectedClient(s.selectedTestSDK, store)
			cold.sdk = s
			if _, err := cold.SubmitOffline(ctx, uri, "ordinary"); err != nil || s.adds != 2 {
				t.Fatal("original rejected operation could not retry", err, s.adds)
			}
		})
	}
}

func TestOrdinaryCancelTypedAuthenticationRejectionIsNotAnUnknownDelete(t *testing.T) {
	c, s, store, r, uri := guardedFixture(t)
	ctx := ordinaryTestContext("cancel-rejected")
	if _, err := c.SubmitOffline(ctx, uri, "ordinary"); err != nil {
		t.Fatal(err)
	}
	s.applyDelete, s.errorDelete = false, sdk.ErrNotLogin
	if err := c.CancelOffline(ctx, r.Authority.Selection.Identity.V1, false); !errors.Is(err, sdk.ErrNotLogin) {
		t.Fatal(err)
	}
	if _, err := store.ResourceClaim(context.Background(), offlineResourceKey(r.Authority.Selection.Identity.V1)); !errors.Is(err, cloud.ErrOperationNotFound) || s.task == nil {
		t.Fatal("refused cancellation changed ownership or data", err)
	}
	s.applyDelete, s.errorDelete = true, nil
	cold := selectedClient(s.selectedTestSDK, store)
	cold.sdk = s
	if err := cold.CancelOffline(ctx, r.Authority.Selection.Identity.V1, false); err != nil || s.cancels != 2 || s.task != nil {
		t.Fatal("fresh credentials could not retry exact cancellation", err)
	}
}

func TestOrdinaryOfflineDoesNotGuessDefiniteRejectionFromErrorText(t *testing.T) {
	c, s, store, r, uri := guardedFixture(t)
	s.applyAdd, s.addErr = false, errors.New("user not login")
	ctx := ordinaryTestContext("opaque-error")
	if _, err := c.SubmitOffline(ctx, uri, "ordinary"); err == nil {
		t.Fatal("opaque error accepted")
	}
	if _, err := store.ResourceClaim(context.Background(), offlineResourceKey(r.Authority.Selection.Identity.V1)); err != nil {
		t.Fatal("string guessed as explicit rejection", err)
	}
	_, _ = c.SubmitOffline(ctx, uri, "ordinary")
	if s.adds != 1 {
		t.Fatal("opaque response replayed")
	}
}

type pendingOrdinaryACKSDK struct {
	*guardedOfflineSDK
	visible, release chan struct{}
	blockedHash      string
	otherAdds        int
}

func (s *pendingOrdinaryACKSDK) AddOfflineTaskURIs(uris []string, dir string, options ...sdk.OfflineOption) ([]string, error) {
	if hash, _ := offlineInfoHash(uris[0]); hash != s.blockedHash {
		s.mu.Lock()
		s.otherAdds++
		s.mu.Unlock()
		return nil, sdk.ErrOfflineInvalidLink
	}
	result, err := s.guardedOfflineSDK.AddOfflineTaskURIs(uris, dir, options...)
	close(s.visible)
	<-s.release
	return result, err
}
func TestOrdinaryOfflinePendingSDKCallKeepsResourceUntilPhysicalReturn(t *testing.T) {
	c, s, store, r, uri := guardedFixture(t)
	wire := &pendingOrdinaryACKSDK{guardedOfflineSDK: s, visible: make(chan struct{}), release: make(chan struct{}), blockedHash: r.Authority.Selection.Identity.V1}
	c.sdk = wire
	ctx := ordinaryTestContext("physical-owner")
	first := make(chan error, 1)
	go func() { _, err := c.SubmitOffline(ctx, uri, "ordinary"); first <- err }()
	<-wire.visible
	cold := selectedClient(s.selectedTestSDK, store)
	cold.sdk = wire
	waiting, cancel := context.WithTimeout(ctx, 75*time.Millisecond)
	_, waitErr := cold.SubmitOffline(waiting, uri, "ordinary")
	cancel()
	interrupt, cancelInterrupt := context.WithTimeout(ctx, 75*time.Millisecond)
	cancelErr := cold.CancelOffline(interrupt, r.Authority.Selection.Identity.V1, false)
	cancelInterrupt()
	other := selectedClient(s.selectedTestSDK, store)
	other.sdk = wire
	independent, cancelIndependent := context.WithTimeout(ordinaryTestContext("different-resource"), 200*time.Millisecond)
	_, otherErr := other.SubmitOffline(independent, "magnet:?xt=urn:btih:"+strings.Repeat("f", 40), "other")
	cancelIndependent()
	_, ownerErr := store.ResourceClaim(context.Background(), offlineResourceKey(r.Authority.Selection.Identity.V1))
	close(wire.release)
	firstErr := <-first
	if waitErr == nil || cancelErr == nil || ownerErr != nil || firstErr != nil {
		t.Fatalf("pending SDK owner was released by another waiter: waiter=%v owner=%v original=%v", waitErr, ownerErr, firstErr)
	}
	if !errors.Is(otherErr, sdk.ErrOfflineInvalidLink) || wire.otherAdds != 1 || s.cancels != 0 {
		t.Fatal("lock crossed resources or canceled a physical owner", otherErr)
	}
	if s.adds != 1 {
		t.Fatal("pending operation submitted twice")
	}
}
