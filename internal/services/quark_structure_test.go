package services

import (
	"context"
	"encoding/json"
	"errors"
	"path"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
	"gorm.io/gorm"
)

type quarkStructureFixture struct {
	s       *MediaLibraryStructureService
	library models.MediaLibrary
	owner   uint
	driver  *quarkTransferFake
	plan    StructurePlan
	entries []models.CatalogEntryFact
}

func newQuarkStructureFixture(t *testing.T) quarkStructureFixture {
	t.Helper()
	s, library, owner := quarkStructureGapFixture(t, false)
	var storage models.Storage
	if err := s.db.First(&storage, library.StorageID).Error; err != nil {
		t.Fatal(err)
	}
	driver := makeQuarkTransferFake(newFakeMutationCloudDriver())
	for _, i := range []cloud.Item{{ID: "0", Name: "root", IsDir: true}, {ID: "synthetic-storage", ParentID: "0", Name: "storage", IsDir: true}, {ID: library.ProviderRootID, ParentID: "synthetic-storage", Name: "library", IsDir: true}, {ID: "show", ParentID: library.ProviderRootID, Name: "Show", IsDir: true}, {ID: "season", ParentID: "show", Name: "Season 02", IsDir: true}} {
		driver.items[i.ID] = i
	}
	var entries []models.CatalogEntryFact
	if err := s.catalogStore.Read(context.Background(), []uint{library.ID}, func(r *CatalogReader) error { return r.RawEntryFacts().Order("id").Find(&entries).Error }); err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		driver.items[e.ProviderID] = cloud.Item{ID: e.ProviderID, ParentID: "season", Name: path.Base(e.RelativePath), Size: e.Size, ModifiedAt: e.ModifiedAt}
	}
	s.connections = &ConnectionService{db: s.db, drivers: map[uint]cloud.Driver{*storage.ConnectionID: driver}}
	plan, _, err := s.buildPlan(context.Background(), library.ID, "series:tmdb:42")
	if err != nil || len(plan.Items) == 0 {
		t.Fatalf("plan: %v", err)
	}
	return quarkStructureFixture{s: s, library: library, owner: owner, driver: driver, plan: plan, entries: entries}
}
func (f quarkStructureFixture) run() error {
	return f.s.EnsureWorkLayout(context.Background(), f.owner, f.library.ID, 42, "tv")
}
func (f quarkStructureFixture) stored(t *testing.T) (models.MediaLibraryStructureRepair, structureCatalogRepairState) {
	t.Helper()
	var r models.MediaLibraryStructureRepair
	if err := f.s.db.Order("created_at DESC").First(&r).Error; err != nil {
		t.Fatal(err)
	}
	var state structureCatalogRepairState
	if err := json.Unmarshal([]byte(r.StateJSON), &state); err != nil {
		t.Fatal(err)
	}
	return r, state
}
func (f quarkStructureFixture) head(t *testing.T) uint64 {
	t.Helper()
	var h models.CatalogHead
	if err := f.s.db.First(&h, "library_id = ?", f.library.ID).Error; err != nil {
		t.Fatal(err)
	}
	return h.Revision
}
func (f quarkStructureFixture) assertCompleted(t *testing.T, before uint64) {
	t.Helper()
	r, state := f.stored(t)
	if r.Phase != "completed" || state.ObservationHoldVersion != 0 || state.QuarkMutation != nil || f.head(t) != before+1 {
		t.Fatalf("repair not uniquely published: phase=%s stage=%s hold=%d head=%d before=%d", r.Phase, state.Stage, state.ObservationHoldVersion, f.head(t), before)
	}
	var after []models.CatalogEntryFact
	if err := f.s.catalogStore.Read(context.Background(), []uint{f.library.ID}, func(r *CatalogReader) error { return r.RawEntryFacts().Order("id").Find(&after).Error }); err != nil {
		t.Fatal(err)
	}
	if len(after) != len(f.entries) {
		t.Fatal("entry count changed")
	}
	for i, e := range after {
		original := f.entries[i]
		if e.ID != original.ID || !e.CreatedAt.Equal(original.CreatedAt) || e.RecognitionID == nil || !reflect.DeepEqual(e.RecognitionID, original.RecognitionID) || e.WorkKey != original.WorkKey || e.SharedOverrideMask != original.SharedOverrideMask {
			t.Fatal("stable/manual identity changed")
		}
		found := false
		for _, p := range f.plan.Items {
			if p.ProviderID == e.ProviderID {
				found = true
				if e.RelativePath != "/"+p.TargetRelative || path.Base(p.TargetRelative) != f.driver.items[e.ProviderID].Name {
					t.Fatal("published path does not describe TV output")
				}
			}
		}
		if !found {
			t.Fatal("unknown published entry")
		}
	}
}
func TestQuarkStructureEnsureWorkLayoutAndNoop(t *testing.T) {
	f := newQuarkStructureFixture(t)
	before := f.head(t)
	if err := f.run(); err != nil {
		t.Fatal(err)
	}
	f.assertCompleted(t, before)
	calls := [3]int{f.driver.nextID, f.driver.moveCalls, f.driver.renameCalls}
	if err := f.run(); err != nil {
		t.Fatal(err)
	}
	if f.head(t) != before+1 || calls != [3]int{f.driver.nextID, f.driver.moveCalls, f.driver.renameCalls} {
		t.Fatal("completed layout repeated writes/publication")
	}
}

func TestQuarkStructureAsyncWaitKeepsOriginalOperationAndHold(t *testing.T) {
	for _, kind := range []string{"move", "rename", "mkdir"} {
		for _, code := range []string{quark.CodePending, cloud.CodeMutationUnknown, quark.CodeAuth} {
			t.Run(kind+"/"+code, func(t *testing.T) {
				f := newQuarkStructureFixture(t)
				var storage models.Storage
				f.s.db.First(&storage, f.library.StorageID)
				async := &quarkStructureReceiptFake{quarkTransferFake: f.driver, kind: kind, code: code, receipts: map[string]cloud.Item{}}
				f.s.connections.drivers[*storage.ConnectionID] = async
				before := f.head(t)
				for attempt := 0; attempt < 2; attempt++ {
					err := f.run()
					var wait *structureRepairWaitError
					if !errors.As(err, &wait) {
						t.Fatalf("async result lost: %v", err)
					}
					if code == quark.CodePending && (wait.result.RetryAt == nil || retryWaitConsumesFailureBudget(wait.result.ErrorCode)) {
						t.Fatal("pending consumes failure budget")
					}
					if code != quark.CodePending && wait.result.Wait == nil {
						t.Fatal("unknown/auth is not actionable")
					}
					_, state := f.stored(t)
					if state.ObservationHoldVersion != 1 || state.QuarkMutation == nil || state.QuarkMutation.Kind != kind || f.head(t) != before {
						t.Fatal("lost hold/cursor or published unknown")
					}
					if err := observationQuarkStructureHoldTx(f.s.db, models.ProviderObservationScope{Provider: models.ConnectionProviderQuark, LibraryID: f.library.ID}); !errors.Is(err, ErrObservationDeferred) {
						t.Fatal("observer did not remain held")
					}
				}
				if async.accepts != 1 {
					t.Fatalf("native mutation repeated: %d", async.accepts)
				}
				async.settle = true
				if err := f.run(); err != nil {
					t.Fatal(err)
				}
				f.assertCompleted(t, before)
			})
		}
	}
}

// This synthetic receipt is keyed by the fixed mutation arguments. Native
// OperationStore tests separately verify the production operation-ID contract.
type quarkStructureReceiptFake struct {
	*quarkTransferFake
	kind, code string
	settle     bool
	accepts    int
	receipts   map[string]cloud.Item
}

func (f *quarkStructureReceiptFake) operation(ctx context.Context, kind, key string, apply func() (cloud.Item, error)) (cloud.Item, error) {
	if kind != f.kind {
		return apply()
	}
	id := kind + ":" + key
	out, ok := f.receipts[id]
	if !ok {
		var err error
		out, err = apply()
		if err != nil {
			return out, err
		}
		f.receipts[id] = out
		f.accepts++
	}
	if !f.settle {
		return cloud.Item{}, cloud.Error(f.code, true, nil)
	}
	return out, nil
}
func (f *quarkStructureReceiptFake) CreateDirectory(ctx context.Context, p, n string) (cloud.Item, error) {
	return f.operation(ctx, "mkdir", p+":"+n, func() (cloud.Item, error) { return f.quarkTransferFake.CreateDirectory(ctx, p, n) })
}
func (f *quarkStructureReceiptFake) Move(ctx context.Context, id, p string) error {
	_, err := f.operation(ctx, "move", id+":"+p, func() (cloud.Item, error) { return cloud.Item{}, f.quarkTransferFake.Move(ctx, id, p) })
	return err
}
func (f *quarkStructureReceiptFake) Rename(ctx context.Context, id, n string) error {
	_, err := f.operation(ctx, "rename", id+":"+n, func() (cloud.Item, error) { return cloud.Item{}, f.quarkTransferFake.Rename(ctx, id, n) })
	return err
}

func TestQuarkStructurePublicationRollbackResumesWithoutPhysicalReplay(t *testing.T) {
	f := newQuarkStructureFixture(t)
	before := f.head(t)
	if err := f.s.db.Exec("CREATE TRIGGER quark_structure_fail_publish BEFORE UPDATE OF phase ON media_library_structure_repairs WHEN NEW.phase='reconciling' BEGIN SELECT RAISE(ABORT,'synthetic publish failure'); END").Error; err != nil {
		t.Fatal(err)
	}
	if err := f.run(); err == nil {
		t.Fatal("trigger did not fail")
	}
	_, state := f.stored(t)
	if state.ObservationHoldVersion != 1 || state.QuarkMutation != nil || state.Stage != "physical_completed" || f.head(t) != before {
		t.Fatal("publication rollback released original ownership")
	}
	calls := [3]int{f.driver.nextID, f.driver.moveCalls, f.driver.renameCalls}
	if err := f.s.db.Exec("DROP TRIGGER quark_structure_fail_publish").Error; err != nil {
		t.Fatal(err)
	}
	if err := f.run(); err != nil {
		t.Fatal(err)
	}
	f.assertCompleted(t, before)
	if calls != [3]int{f.driver.nextID, f.driver.moveCalls, f.driver.renameCalls} {
		t.Fatal("publication recovery replayed cloud writes")
	}
}

type quarkStructureObservation struct {
	f     quarkStructureFixture
	s     *ProviderObservationService
	scope models.ProviderObservationScope
	clock time.Time
}

func newQuarkStructureObservation(t *testing.T) *quarkStructureObservation {
	t.Helper()
	f := newQuarkStructureFixture(t)
	libraries := NewMediaLibraryService(f.s.db, f.s.audit, zerolog.Nop())
	libraries.SetCatalogSnapshotStore(f.s.catalogStore)
	libraries.SetConnectionService(f.s.connections)
	libraries.SetCatalogScanCommit(func(*gorm.DB, CatalogScanPublication) error { return nil })
	x := &quarkStructureObservation{f: f, clock: time.Now().UTC().Add(-time.Hour)}
	x.s = NewProviderObservationService(f.s.db)
	x.s.now = func() time.Time { return x.clock }
	libraries.SetProviderObservationService(x.s)
	var err error
	x.scope, err = libraries.ensureQuarkObservation(context.Background(), f.library.ID)
	if err != nil {
		t.Fatal(err)
	}
	x.round(t)
	if err := f.s.db.Model(&models.ProviderObservationBatch{}).Where("scope_id = ?", x.scope.ID).Update("published_revision", gorm.Expr("revision")).Error; err != nil {
		t.Fatal(err)
	}
	return x
}
func (x *quarkStructureObservation) round(t *testing.T) {
	t.Helper()
	if err := x.s.RequestFullSync(context.Background(), x.scope.ID); err != nil {
		t.Fatal(err)
	}
	for range 60 {
		x.clock = x.clock.Add(2 * time.Second)
		if _, err := x.s.ObserveNext(context.Background(), x.scope.ID, quarkObservationReader{driver: x.f.driver}); err != nil {
			t.Fatal(err)
		}
		var scope models.ProviderObservationScope
		x.f.s.db.First(&scope, "id = ?", x.scope.ID)
		if scope.LastCompleteAt != nil && !scope.LastCompleteAt.Before(scope.CoverageStartedAt) {
			return
		}
	}
	t.Fatal("coverage did not complete")
}
func TestQuarkStructureObservationCommitAndReplayAreAtomic(t *testing.T) {
	x := newQuarkStructureObservation(t)
	f := x.f
	var storage models.Storage
	f.s.db.First(&storage, f.library.StorageID)
	f.s.connections.drivers[*storage.ConnectionID] = &quarkStructureChangingTimes{quarkTransferFake: f.driver}
	before := f.head(t)
	var old models.ProviderObservationItem
	if err := observationCurrent(f.s.db, x.scope.ID).First(&old, "resource_id = ?", f.plan.Items[0].ProviderID).Error; err != nil {
		t.Fatal(err)
	}
	stale := models.ProviderObservationBatch{ID: "structure-stale-positive", ScopeID: x.scope.ID, DirectoryID: old.ParentID, Fence: x.scope.Fence, RuntimeID: x.s.runtimeID, State: "completed", CoverageRound: x.scope.CoverageRound, Revision: 1, StartedAt: x.clock, UpdatedAt: x.clock}
	pending := old
	pending.BatchID, pending.Revision, pending.Change = stale.ID, 1, "updated"
	if err := f.s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&stale).Error; err != nil {
			return err
		}
		return tx.Create(&pending).Error
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.db.Exec("CREATE TRIGGER quark_structure_observation_rollback BEFORE UPDATE OF phase ON media_library_structure_repairs WHEN NEW.phase='reconciling' BEGIN SELECT RAISE(ABORT,'rollback after baseline'); END").Error; err != nil {
		t.Fatal(err)
	}
	if err := f.run(); err == nil {
		t.Fatal("expected publication rollback")
	}
	var current models.ProviderObservationItem
	observationCurrent(f.s.db, x.scope.ID).First(&current, "resource_id = ?", old.ResourceID)
	if current.RelativePath != old.RelativePath || f.head(t) != before {
		t.Fatal("observer/catalog transaction split")
	}
	if _, err := x.s.ObserveNext(context.Background(), x.scope.ID, quarkObservationReader{driver: f.driver}); !errors.Is(err, ErrObservationDeferred) {
		t.Fatal("held observer reached provider")
	}
	if _, err := x.s.PublishPending(context.Background(), x.scope.ID, 100); !errors.Is(err, ErrObservationDeferred) {
		t.Fatal("held publisher escaped")
	}
	if err := f.s.db.Exec("DROP TRIGGER quark_structure_observation_rollback").Error; err != nil {
		t.Fatal(err)
	}
	if err := f.run(); err != nil {
		t.Fatal(err)
	}
	f.assertCompleted(t, before)
	for _, item := range f.plan.Items {
		var row models.ProviderObservationItem
		if err := observationCurrent(f.s.db, x.scope.ID).First(&row, "resource_id = ?", item.ProviderID).Error; err != nil {
			t.Fatal(err)
		}
		if row.RelativePath != "/"+item.TargetRelative || row.Change != "" {
			t.Fatal("baseline did not match committed TV facts")
		}
	}
	x.round(t)
	for range 20 {
		n, err := x.s.PublishPending(context.Background(), x.scope.ID, 100)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
	}
	if f.head(t) != before+1 {
		t.Fatal("post-repair full observation republished catalog")
	}
}

// Native management may update file and parent-directory times. The next real
// observer round must still find no additional media/catalog publication.
type quarkStructureChangingTimes struct{ *quarkTransferFake }

func (f *quarkStructureChangingTimes) touch(ids ...string) {
	for _, id := range ids {
		item := f.items[id]
		item.ModifiedAt = time.Now().UTC()
		f.items[id] = item
	}
}
func (f *quarkStructureChangingTimes) Move(ctx context.Context, id, parent string) error {
	tv, _ := agentTestID(id)
	target, _ := agentTestID(parent)
	old := f.items[tv].ParentID
	if err := f.quarkTransferFake.Move(ctx, id, parent); err != nil {
		return err
	}
	f.touch(tv, old, target)
	return nil
}
func (f *quarkStructureChangingTimes) Rename(ctx context.Context, id, name string) error {
	tv, _ := agentTestID(id)
	if err := f.quarkTransferFake.Rename(ctx, id, name); err != nil {
		return err
	}
	f.touch(tv, f.items[tv].ParentID)
	return nil
}

func (f quarkStructureFixture) queued(t *testing.T) (models.MediaLibraryStructureRepair, ClaimedJob) {
	t.Helper()
	s := f.s
	s.queue = NewQueueService(s.db, s.audit)
	id := uuid.NewString()
	job, err := s.queue.Enqueue(EnqueueJobInput{OwnerID: f.owner, JobType: JobTypeMediaLibraryRepair, DisplayName: "isolated quark structure", Payload: mediaLibraryRepairJobPayload{RepairID: id}})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(f.plan)
	repair := models.MediaLibraryStructureRepair{ID: id, JobID: &job.ID, OwnerID: f.owner, LibraryID: f.library.ID, Scope: models.MediaLibraryStructureScopeWork, WorkKey: "series:tmdb:42", RuleFingerprint: f.plan.RuleFingerprint, Generation: f.plan.Generation, PlanJSON: string(raw), StateJSON: "{}", Phase: "queued", TotalItems: len(f.plan.Items), CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := s.structureCatalogWriteTx(context.Background(), func(tx *gorm.DB) error {
		if err := s.freezeCatalogStructureRepairTx(tx, &repair, f.plan); err != nil {
			return err
		}
		return tx.Create(&repair).Error
	}); err != nil {
		t.Fatal(err)
	}
	claim, err := s.queue.Claim([]string{JobTypeMediaLibraryRepair})
	if err != nil || claim == nil {
		t.Fatalf("claim: %v", err)
	}
	return repair, *claim
}
func TestQuarkStructureQueuedLeaseLossBeforeWrite(t *testing.T) {
	f := newQuarkStructureFixture(t)
	repair, claim := f.queued(t)
	before := f.head(t)
	nextID := f.driver.nextID
	claim.LeaseToken = "wrong lease"
	r := f.s.runRepair(context.Background(), nil, repair.ID, &claim)
	if r.ErrorCode == "" || f.driver.nextID != nextID || f.driver.moveCalls != 0 || f.driver.renameCalls != 0 || f.head(t) != before {
		t.Fatal("lost lease permitted cloud work")
	}
	var saved models.MediaLibraryStructureRepair
	f.s.db.First(&saved, "id = ?", repair.ID)
	var state structureCatalogRepairState
	json.Unmarshal([]byte(saved.StateJSON), &state)
	if state.ObservationHoldVersion != 0 {
		t.Fatal("unentered failed preflight held observer")
	}
}
