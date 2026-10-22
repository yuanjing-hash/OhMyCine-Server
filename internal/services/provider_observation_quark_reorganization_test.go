package services

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"gorm.io/gorm"
)

func observationTestReorganizationHold(t *testing.T, f observationFixture) models.MediaReorganizationTask {
	t.Helper()
	plan, _ := json.Marshal(reorganizationPlan{Version: 1, LibraryID: f.scope.LibraryID, StorageType: models.StorageTypeQuark})
	state, _ := json.Marshal(reorganizationState{Version: 1, Catalog: &reorganizationCatalogState{ObservationHoldVersion: 1, ExecutionProofVersion: 1, ExecutionStarted: true}})
	row := models.MediaReorganizationTask{ID: "held-reorganization", JobID: "held-job", LibraryID: f.scope.LibraryID, PlanJSON: string(plan), StateJSON: string(state), Phase: models.MediaReorganizationPhaseFailed}
	if err := f.db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	return row
}

func TestProviderObservationQuarkHoldSurvivesFailureAndProtectsBoundaries(t *testing.T) {
	f := newObservationFixture(t)
	f.reader.tree["root"] = []ObservationFact{{ID: "file", ParentID: "root", Name: "old.mkv", Size: 123}}
	f.step(t)
	dir := f.item(t, "root")
	f.advance(time.Second)
	batch, err := f.s.beginBatch(context.Background(), f.scope, dir)
	if err != nil {
		t.Fatal(err)
	}
	hold := observationTestReorganizationHold(t, f)
	reads := len(f.reader.calls)
	if _, err := f.s.ObserveNext(context.Background(), f.scope.ID, f.reader); !errors.Is(err, ErrObservationDeferred) || len(f.reader.calls) != reads {
		t.Fatalf("held read reached provider: %v", err)
	}
	if err := f.s.acceptPage(context.Background(), f.scope, dir, &batch, ObservationDirectoryPage{Items: []ObservationFact{{ID: "file", ParentID: "root", Name: "intermediate.mkv", Size: 123}}}); !errors.Is(err, ErrObservationDeferred) || f.item(t, "file").Name != "old.mkv" {
		t.Fatalf("held response accepted: %v", err)
	}
	if _, err := f.s.PublishPending(context.Background(), f.scope.ID, 100); !errors.Is(err, ErrObservationDeferred) || f.publisher.calls != 0 {
		t.Fatalf("held publisher started: %v", err)
	}
	// A fresh service and a terminal domain phase do not erase the durable
	// owner. This check intentionally does not rely on a running queue lease.
	if err := observationQuarkReorganizationHoldTx(f.db, f.scope); !errors.Is(err, ErrObservationDeferred) {
		t.Fatal(err)
	}
	other := f.scope
	other.LibraryID++
	if err := observationQuarkReorganizationHoldTx(f.db, other); err != nil {
		t.Fatalf("other library blocked: %v", err)
	}
	other = f.scope
	other.Provider = models.ConnectionProviderPan115
	if err := observationQuarkReorganizationHoldTx(f.db, other); err != nil {
		t.Fatalf("other provider blocked: %v", err)
	}
	if err := f.db.Model(&hold).Update("plan_json", "{broken").Error; err != nil {
		t.Fatal(err)
	}
	if err := observationQuarkReorganizationHoldTx(f.db, f.scope); !errors.Is(err, ErrObservationDeferred) {
		t.Fatalf("corrupt plan erased an existing hold: %v", err)
	}
	if err := f.db.Model(&hold).Update("state_json", "{broken").Error; err != nil {
		t.Fatal(err)
	}
	if err := observationQuarkReorganizationHoldTx(f.db, f.scope); !errors.Is(err, ErrObservationDeferred) {
		t.Fatalf("unknown persistent state unlocked the observer: %v", err)
	}
	if err := f.db.Model(&hold).Update("state_json", `{"version":1,"catalog":{"observation_hold_version":0}}`).Error; err != nil {
		t.Fatal(err)
	}
	f.publish(t)
	if f.publisher.calls != 1 {
		t.Fatal("explicitly released owner did not unblock")
	}
}

func TestProviderObservationQuarkHoldStartsDuringPublication(t *testing.T) {
	f := newObservationFixture(t)
	f.reader.tree["root"] = []ObservationFact{{ID: "file", ParentID: "root", Name: "old.mkv"}}
	f.step(t)
	f.publisher.before = func() { observationTestReorganizationHold(t, f) }
	if _, err := f.s.PublishPending(context.Background(), f.scope.ID, 100); !errors.Is(err, ErrObservationDeferred) {
		t.Fatal(err)
	}
	var count int64
	if err := f.db.Model(&observationTestPublication{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("publication escaped commit fence: count=%d err=%v", count, err)
	}
}

func observationTestConfirmedRename(f observationFixture, name string) (reorganizationPlan, reorganizationState, []cloud.Item) {
	plan := reorganizationPlan{Items: []reorganizationPlanItem{{ManagedItemID: 1, ProviderItemID: "file", ProviderParentID: "root", OldRelativePath: "old.mkv", NewRelativePath: name, Size: 123}}}
	state := reorganizationState{Completed: map[uint]bool{1: true}, Catalog: &reorganizationCatalogState{Directories: map[string]string{".": "root"}}}
	facts := []cloud.Item{{ID: "file", ParentID: "root", Name: name, Size: 123}}
	return plan, state, facts
}

func TestProviderObservationConfirmedBaselineIsExactAtomicAndNotPublication(t *testing.T) {
	f := newObservationFixture(t)
	f.reader.tree["root"] = []ObservationFact{{ID: "file", ParentID: "root", Name: "old.mkv", Size: 123}}
	f.step(t)
	before := f.item(t, "file")
	var scopeBefore models.ProviderObservationScope
	if err := f.db.First(&scopeBefore, "id = ?", f.scope.ID).Error; err != nil {
		t.Fatal(err)
	}
	f.advance(time.Second)
	batch, err := f.s.beginBatch(context.Background(), f.scope, f.item(t, "root"))
	if err != nil {
		t.Fatal(err)
	}
	plan, state, facts := observationTestConfirmedRename(f, "new.mkv")
	f.advance(time.Second)
	rollback := errors.New("fixture rollback after baseline")
	if err := f.db.Transaction(func(tx *gorm.DB) error {
		if err := confirmReorganizationObservationFactsTx(tx, f.scope, plan, state, facts, *f.clock); err != nil {
			return err
		}
		return rollback
	}); !errors.Is(err, rollback) {
		t.Fatal(err)
	}
	if f.item(t, "file").Name != "old.mkv" || f.item(t, "root").ActiveBatchID != batch.ID {
		t.Fatal("rolled-back baseline escaped its transaction")
	}
	if err := f.db.Transaction(func(tx *gorm.DB) error {
		return confirmReorganizationObservationFactsTx(tx, f.scope, plan, state, facts, *f.clock)
	}); err != nil {
		t.Fatal(err)
	}
	got := f.item(t, "file")
	if got.Name != "new.mkv" || got.RelativePath != "/new.mkv" || got.LastSeenRound != before.LastSeenRound {
		t.Fatalf("incorrect exact baseline: %+v", got)
	}
	var stopped models.ProviderObservationBatch
	if err := f.db.First(&stopped, "id = ?", batch.ID).Error; err != nil || stopped.State != "incomplete" || stopped.IncompleteReason != "reorganization_catalog_confirmed" {
		t.Fatalf("late page remains eligible: %+v %v", stopped, err)
	}
	if err := f.s.acceptPage(context.Background(), f.scope, f.item(t, "root"), &batch, ObservationDirectoryPage{}); !errors.Is(err, ErrObservationFence) {
		t.Fatalf("old empty listing could infer absence: %v", err)
	}
	f.publish(t)
	if f.publisher.calls != 0 {
		t.Fatal("acknowledgement caused a second Catalog publication")
	}
	var scopeAfter models.ProviderObservationScope
	if err := f.db.First(&scopeAfter, "id = ?", f.scope.ID).Error; err != nil || scopeAfter.CoverageRound != scopeBefore.CoverageRound || scopeAfter.LastCompleteAt == nil || !scopeAfter.LastCompleteAt.Equal(*scopeBefore.LastCompleteAt) {
		t.Fatal("baseline invented coverage evidence")
	}
	f.reader.tree["root"] = []ObservationFact{{ID: "file", ParentID: "root", Name: "new.mkv", Size: 123}}
	f.advance(6 * time.Second)
	f.step(t)
	f.publish(t)
	if f.publisher.calls != 0 {
		t.Fatal("unchanged final readback republished the task")
	}
}

func TestProviderObservationConfirmedIdenticalFactRevokesCapturedDelta(t *testing.T) {
	f := newObservationFixture(t)
	f.reader.tree["root"] = []ObservationFact{{ID: "file", ParentID: "root", Name: "old.mkv", Size: 123}}
	f.step(t)
	plan, state, facts := observationTestConfirmedRename(f, "old.mkv")
	f.publisher.before = func() {
		f.advance(time.Second)
		if err := f.db.Transaction(func(tx *gorm.DB) error {
			return confirmReorganizationObservationFactsTx(tx, f.scope, plan, state, facts, *f.clock)
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.s.PublishPending(context.Background(), f.scope.ID, 100); !errors.Is(err, ErrObservationDeferred) {
		t.Fatalf("captured identical fact retained publication ownership: %v", err)
	}
	var count int64
	if err := f.db.Model(&observationTestPublication{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("captured delta committed: %d %v", count, err)
	}
}

func TestProviderObservationLateSameParentRenameRestartsCompleteness(t *testing.T) {
	f := newObservationFixture(t)
	f.reader.tree["root"] = []ObservationFact{{ID: "file", ParentID: "root", Name: "old.mkv", Size: 123}}
	f.step(t)
	f.advance(time.Second)
	dir := f.item(t, "root")
	batch, err := f.s.beginBatch(context.Background(), f.scope, dir)
	if err != nil {
		t.Fatal(err)
	}
	f.advance(time.Second)
	row := f.item(t, "file")
	row.Name, row.RelativePath, row.SeenAt = "new.mkv", "/new.mkv", *f.clock
	row.Fingerprint = observationFingerprint(observationFact(row))
	if err := observationUpsert(f.db, &row); err != nil {
		t.Fatal(err)
	}
	if err := f.s.acceptPage(context.Background(), f.scope, dir, &batch, ObservationDirectoryPage{Items: f.reader.tree["root"]}); err != nil {
		t.Fatal(err)
	}
	if batch.State != "incomplete" || batch.IncompleteReason != "observation_fact_superseded" || f.item(t, "file").Name != "new.mkv" || f.item(t, "root").ActiveBatchID != "" {
		t.Fatal("late same-parent rename overwrote new facts or retained completeness")
	}
	f.reader.tree["root"] = []ObservationFact{{ID: "file", ParentID: "root", Name: "new.mkv", Size: 123}}
	f.advance(time.Minute)
	f.step(t)
	if f.reader.offsets[len(f.reader.offsets)-1] != 0 || f.item(t, "file").Name != "new.mkv" {
		t.Fatal("new complete pass did not restart at page zero")
	}
}

func TestProviderObservationConfirmedNewParentsRemainIncompleteUntilObserved(t *testing.T) {
	f := newObservationFixture(t)
	f.reader.tree["root"] = []ObservationFact{{ID: "file", ParentID: "root", Name: "old.mkv", Size: 123}}
	f.step(t)
	plan, state, _ := observationTestConfirmedRename(f, "Show/Season/new.mkv")
	state.Catalog.Directories["Show"], state.Catalog.Directories["Show/Season"] = "show", "season"
	facts := []cloud.Item{{ID: "root", IsDir: true}, {ID: "show", ParentID: "root", Name: "Show", IsDir: true}, {ID: "season", ParentID: "show", Name: "Season", IsDir: true}, {ID: "file", ParentID: "season", Name: "new.mkv", Size: 123}}
	f.advance(time.Second)
	bad := append([]cloud.Item(nil), facts...)
	bad[len(bad)-1].ParentID = "unrelated"
	if err := f.db.Transaction(func(tx *gorm.DB) error {
		return confirmReorganizationObservationFactsTx(tx, f.scope, plan, state, bad, *f.clock)
	}); !errors.Is(err, ErrObservationFence) || f.item(t, "file").ParentID != "root" {
		t.Fatalf("unfrozen parent accepted: %v", err)
	}
	if err := f.db.Transaction(func(tx *gorm.DB) error {
		return confirmReorganizationObservationFactsTx(tx, f.scope, plan, state, facts, *f.clock)
	}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"show", "season"} {
		item := f.item(t, id)
		if item.CompletedRound != 0 || item.LastCompleteAt != nil || item.LastSeenRound != 0 || item.QueueRound != f.scope.CoverageRound {
			t.Fatalf("exact directory Stat invented listing coverage: %+v", item)
		}
	}
	if f.item(t, "file").RelativePath != "/Show/Season/new.mkv" {
		t.Fatal("new ancestor graph did not yield the confirmed path")
	}
	f.reader.tree["root"] = []ObservationFact{{ID: "show", ParentID: "root", Name: "Show", IsDir: true}}
	f.reader.tree["show"] = []ObservationFact{{ID: "season", ParentID: "show", Name: "Season", IsDir: true}}
	f.reader.tree["season"] = []ObservationFact{{ID: "file", ParentID: "season", Name: "new.mkv", Size: 123}}
	f.round(t)
	f.publish(t)
	if f.publisher.calls != 0 {
		t.Fatal("confirmed directory graph produced a second publication")
	}
}
