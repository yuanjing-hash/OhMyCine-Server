package services

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	cloud "github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
)

// Simulates durable provider receipts separately from the task checkpoint.
// These tests exercise worker recovery, not actual provider acknowledgement.
type quarkReorganizationAsyncFake struct {
	*quarkTransferFake
	kind     string
	code     string
	checks   int
	accepted bool
	settle   bool
}

func (f *quarkReorganizationAsyncFake) CreateDirectory(ctx context.Context, parent, name string) (cloud.Item, error) {
	if f.kind != "mkdir" {
		return f.quarkTransferFake.CreateDirectory(ctx, parent, name)
	}
	f.checks++
	if !f.accepted {
		f.accepted = true
		if _, err := f.quarkTransferFake.CreateDirectory(ctx, parent, name); err != nil {
			return cloud.Item{}, err
		}
	}
	// No result identity was received: never adopt the visible same-name folder.
	return cloud.Item{}, cloud.Error(f.code, true, nil)
}
func (f *quarkReorganizationAsyncFake) Move(ctx context.Context, id, parent string) error {
	if f.kind != "move" {
		return f.quarkTransferFake.Move(ctx, id, parent)
	}
	f.checks++
	if !f.accepted {
		f.accepted = true
		if err := f.quarkTransferFake.Move(ctx, id, parent); err != nil {
			return err
		}
	}
	if !f.settle {
		return cloud.Error(f.code, true, nil)
	}
	return nil
}
func (f *quarkReorganizationAsyncFake) Rename(ctx context.Context, id, name string) error {
	if f.kind != "rename" {
		return f.quarkTransferFake.Rename(ctx, id, name)
	}
	f.checks++
	if !f.accepted {
		f.accepted = true
		if err := f.quarkTransferFake.Rename(ctx, id, name); err != nil {
			return err
		}
	}
	if !f.settle {
		return cloud.Error(f.code, true, nil)
	}
	return nil
}

func quarkReorganizationStoredState(t *testing.T, f versionedReorganizationFixture) (models.MediaReorganizationTask, reorganizationState) {
	t.Helper()
	var task models.MediaReorganizationTask
	if err := f.store.writeDB.First(&task, "id=?", f.task.ID).Error; err != nil {
		t.Fatal(err)
	}
	var state reorganizationState
	if err := json.Unmarshal([]byte(task.StateJSON), &state); err != nil {
		t.Fatal(err)
	}
	return task, state
}

func TestQuarkReorganizationPendingResumesKnownOperationAfterVisibleMutation(t *testing.T) {
	for _, kind := range []string{"move", "rename"} {
		t.Run(kind, func(t *testing.T) {
			f := newQuarkReorganizationFixture(t, false)
			fake := &quarkReorganizationAsyncFake{quarkTransferFake: f.quarkDriver, kind: kind, code: quark.CodePending}
			f.worker.service.connections.drivers[*f.storage.ConnectionID] = fake
			before := quarkReorganizationHead(t, f)
			result := f.run()
			if result.RetryAt == nil || result.Wait != nil || result.ErrorCode != quarkReorganizationPending {
				t.Fatalf("pending %+v", result)
			}
			task, state := quarkReorganizationStoredState(t, f)
			if task.Phase != models.MediaReorganizationPhaseExecuting || state.Catalog.QuarkMutation == nil || state.Catalog.QuarkMutation.Kind != kind || state.Catalog.ObservationHoldVersion != 1 {
				t.Fatal("pending lost recovery cursor/hold")
			}
			if quarkReorganizationHead(t, f).Revision != before.Revision {
				t.Fatal("pending published")
			}
			q := f.worker.service.queue
			if err := q.RetryLater(f.claim.Job.ID, f.claim.LeaseToken, result.ErrorCode, result.ErrorMessage, *result.RetryAt); err != nil {
				t.Fatal(err)
			}
			var job models.Job
			if err := q.db.First(&job, "id=?", f.claim.Job.ID).Error; err != nil {
				t.Fatal(err)
			}
			if job.FailureRetryCount != 0 {
				t.Fatal("accepted wait consumed failure budget")
			}
			q.clock = &fakeQueueClock{now: result.RetryAt.Add(time.Second)}
			if err := q.PromoteDueRetries(); err != nil {
				t.Fatal(err)
			}
			claim, err := q.Claim([]string{JobTypeMediaReorganization})
			if err != nil || claim == nil {
				t.Fatalf("claim %v", err)
			}
			f.claim = *claim
			f.worker = NewMediaReorganizationWorker(f.worker.service)
			fake.settle = true
			if result = f.run(); result.ErrorCode != "" || result.Wait != nil || result.RetryAt != nil {
				t.Fatalf("resume %+v", result)
			}
			if fake.checks != 2 || fake.moveCalls != 1 || fake.renameCalls != 1 {
				t.Fatalf("checks=%d writes=%d/%d", fake.checks, fake.moveCalls, fake.renameCalls)
			}
			_, state = quarkReorganizationStoredState(t, f)
			if state.Catalog.ObservationHoldVersion != 0 || state.Catalog.QuarkMutation != nil {
				t.Fatal("completed retained logical hold")
			}
			assertQuarkReorganizationManualIdentity(t, f)
		})
	}
}

func TestQuarkReorganizationUnknownMkdirNeverAdoptsSameName(t *testing.T) {
	f := newQuarkReorganizationFixture(t, false)
	fake := &quarkReorganizationAsyncFake{quarkTransferFake: f.quarkDriver, kind: "mkdir", code: cloud.CodeMutationUnknown}
	f.worker.service.connections.drivers[*f.storage.ConnectionID] = fake
	for round := 0; round < 2; round++ {
		result := f.run()
		if result.Wait == nil || result.RetryAt != nil || result.ErrorCode != cloud.CodeMutationUnknown {
			t.Fatalf("unknown %+v", result)
		}
		if len(result.Wait.Options) != 1 || result.Wait.Options[0] != quarkReorganizationRecheck {
			t.Fatal("unsafe unknown action")
		}
		task, state := quarkReorganizationStoredState(t, f)
		if task.Phase != models.MediaReorganizationPhaseExecuting || state.Catalog.ObservationHoldVersion != 1 || state.Catalog.QuarkMutation == nil || state.Catalog.Directories["Correct Show"] != "" {
			t.Fatal("unknown creation adopted or discarded")
		}
		f.worker = NewMediaReorganizationWorker(f.worker.service)
	}
	if fake.checks != 2 || fake.moveCalls != 0 || fake.renameCalls != 0 {
		t.Fatal("unknown creation continued cloud work")
	}
	count := 0
	for _, item := range fake.items {
		if item.Name == "Correct Show" {
			count++
		}
	}
	if count != 1 {
		t.Fatal("unknown creation was resubmitted")
	}
}

func TestQuarkReorganizationReadFailureDoesNotBecomeBoundaryFailure(t *testing.T) {
	f := newQuarkReorganizationFixture(t, false)
	f.quarkDriver.statErrors[f.entries[0].ProviderID] = cloud.Error(quark.CodeAuth, false, nil)
	result := f.run()
	if result.Wait == nil || result.ErrorCode != quark.CodeAuth || result.RetryAt != nil {
		t.Fatalf("auth %+v", result)
	}
	if f.driver.moveCalls != 0 || f.driver.renameCalls != 0 {
		t.Fatal("auth failure allowed cloud mutation")
	}
}

func TestQuarkReorganizationPreviewRejectsMissingSharedSpaceAndNamespace(t *testing.T) {
	f := newQuarkReorganizationFixture(t, false)
	f.quarkDriver.denyProof = true
	if _, err := f.worker.service.reorganizationTargetExists(context.Background(), f.library, f.storage, "new/file.mkv"); err == nil {
		t.Fatal("missing proof accepted")
	}
	f.quarkDriver.denyProof = false
	if err := f.store.writeDB.Model(&models.Connection{}).Where("id=?", *f.storage.ConnectionID).Update("provider", cloud.ProviderPan115).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := f.worker.service.reorganizationTargetExists(context.Background(), f.library, f.storage, "new/file.mkv"); err == nil {
		t.Fatal("mismatched provider accepted")
	}
	if f.driver.moveCalls != 0 || f.driver.renameCalls != 0 {
		t.Fatal("preview wrote cloud")
	}
}

func TestQuarkReorganizationWaitClassificationIsNarrow(t *testing.T) {
	f := newQuarkReorganizationFixture(t, false)
	for _, err := range []error{ErrCatalogFence, errors.New("fixture private provider message"), cloud.Error(quark.CodeIdentity, false, nil), cloud.Error(quark.CodePending, true, nil)} {
		if _, ok := f.worker.quarkReorganizationWait(context.Background(), f.task, f.plan, f.state, f.claim, err); ok {
			t.Fatal("unsafe untyped/fence/no-operation wait")
		}
	}
	if retryWaitConsumesFailureBudget(quarkReorganizationPending) || !retryWaitConsumesFailureBudget(quark.CodePending) || !retryWaitConsumesFailureBudget(quark.CodeUnavailable) || !retryWaitConsumesFailureBudget(cloud.CodeMutationUnknown) {
		t.Fatal("retry exemption too broad")
	}
}

func TestQuarkReorganizationPreflightFailureDoesNotFreezeObservation(t *testing.T) {
	f := newQuarkReorganizationFixture(t, false)
	f.quarkDriver.statErrors[f.library.ProviderRootID] = cloud.Error(quark.CodeAuth, false, nil)
	result := f.run()
	if result.Wait == nil || result.ErrorCode != quark.CodeAuth {
		t.Fatalf("auth %+v", result)
	}
	_, state := quarkReorganizationStoredState(t, f)
	if state.Catalog.ObservationHoldVersion != 0 || state.Catalog.QuarkMutation != nil {
		t.Fatal("read-only failure left a durable write hold")
	}
	if err := observationQuarkReorganizationHoldTx(f.store.writeDB, models.ProviderObservationScope{LibraryID: f.library.ID, Provider: models.ConnectionProviderQuark}); err != nil {
		t.Fatal("read-only failure froze observer")
	}
	if f.driver.moveCalls != 0 || f.driver.renameCalls != 0 {
		t.Fatal("preflight wrote cloud")
	}
}

func TestQuarkReorganizationLostLeaseKeepsUnknownReceiptAndCannotWrite(t *testing.T) {
	f := newQuarkReorganizationFixture(t, false)
	fake := &quarkReorganizationAsyncFake{quarkTransferFake: f.quarkDriver, kind: "mkdir", code: cloud.CodeMutationUnknown}
	f.worker.service.connections.drivers[*f.storage.ConnectionID] = fake
	if result := f.run(); result.Wait == nil {
		t.Fatalf("unknown %+v", result)
	}
	before, state := quarkReorganizationStoredState(t, f)
	if state.Catalog.ObservationHoldVersion != 1 {
		t.Fatal("unknown hold missing")
	}
	checks := fake.checks
	f.claim.LeaseToken = "obsolete-worker"
	if result := f.run(); result.ErrorCode == "" {
		t.Fatal("stale lease accepted")
	}
	after, _ := quarkReorganizationStoredState(t, f)
	if before.StateJSON != after.StateJSON || before.Phase != after.Phase || fake.checks != checks {
		t.Fatal("stale worker wrote cloud or replaced recovery evidence")
	}
}

func TestQuarkReorganizationCooldownKeepsProviderDelayAndSafeMessage(t *testing.T) {
	f := newQuarkReorganizationFixture(t, false)
	f.quarkDriver.statErrors[f.library.ProviderRootID] = &quark.Failure{Code: quark.CodeLimited, RetryAfter: 3 * time.Minute}
	before := time.Now().UTC()
	result := f.run()
	if result.RetryAt == nil || result.RetryAt.Before(before.Add(3*time.Minute)) || result.ErrorCode != quark.CodeLimited {
		t.Fatalf("cooldown %+v", result)
	}
	if !retryWaitConsumesFailureBudget(result.ErrorCode) {
		t.Fatal("network/cooldown accidentally exempted failure budget")
	}
}
