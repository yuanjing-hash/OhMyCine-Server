package services

import (
	"context"
	"errors"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

// These tests use a namespace-checking synthetic cloud and the real observer,
// catalog writer, queue lease and SQLite transactions. They do not establish
// real provider acknowledgement, performance or client playback acceptance.
type quarkReorganizationObservationFixture struct {
	f     versionedReorganizationFixture
	s     *ProviderObservationService
	scope models.ProviderObservationScope
	clock time.Time
}

func newQuarkReorganizationObservationFixture(t *testing.T) *quarkReorganizationObservationFixture {
	t.Helper()
	f := newQuarkReorganizationFixture(t, false)
	// Catalog fixture file metadata must describe the synthetic provider output;
	// otherwise a later observation would correctly detect an unrelated update.
	for _, entry := range f.entries {
		item := f.quarkDriver.items[entry.ProviderID]
		item.ModifiedAt = entry.ModifiedAt
		f.quarkDriver.items[entry.ProviderID] = item
	}
	x := &quarkReorganizationObservationFixture{f: f, clock: time.Now().UTC().Add(-time.Hour)}
	x.attach(t)
	x.round(t)
	// The catalog fixture already contains these exact file facts. Mark this
	// synthetic initial baseline observed, without adding another catalog writer.
	if err := f.store.writeDB.Model(&models.ProviderObservationBatch{}).Where("scope_id = ?", x.scope.ID).Update("published_revision", gorm.Expr("revision")).Error; err != nil {
		t.Fatal(err)
	}
	return x
}

func (x *quarkReorganizationObservationFixture) attach(t *testing.T) {
	t.Helper()
	libraries := x.f.worker.service.libraries
	libraries.SetConnectionService(x.f.worker.service.connections)
	x.s = NewProviderObservationService(x.f.store.writeDB)
	x.s.now = func() time.Time { return x.clock }
	libraries.SetProviderObservationService(x.s)
	var err error
	x.scope, err = libraries.ensureQuarkObservation(context.Background(), x.f.library.ID)
	if err != nil {
		t.Fatal(err)
	}
}

func (x *quarkReorganizationObservationFixture) round(t *testing.T) {
	t.Helper()
	if err := x.s.RequestFullSync(context.Background(), x.scope.ID); err != nil {
		t.Fatal(err)
	}
	for range 50 {
		x.clock = x.clock.Add(2 * time.Second)
		if _, err := x.s.ObserveNext(context.Background(), x.scope.ID, quarkObservationReader{driver: x.f.quarkDriver}); err != nil {
			t.Fatal(err)
		}
		var scope models.ProviderObservationScope
		if err := x.f.store.writeDB.First(&scope, "id = ?", x.scope.ID).Error; err != nil {
			t.Fatal(err)
		}
		if scope.LastCompleteAt != nil && !scope.LastCompleteAt.Before(scope.CoverageStartedAt) {
			return
		}
	}
	t.Fatal("synthetic observation coverage did not finish")
}

func (x *quarkReorganizationObservationFixture) stalePending(t *testing.T) {
	t.Helper()
	var row models.ProviderObservationItem
	if err := observationCurrent(x.f.store.writeDB, x.scope.ID).First(&row, "resource_id = ?", x.f.entries[0].ProviderID).Error; err != nil {
		t.Fatal(err)
	}
	batch := models.ProviderObservationBatch{ID: "reorganization-stale-observation", ScopeID: x.scope.ID, DirectoryID: row.ParentID, Fence: x.scope.Fence, RuntimeID: x.s.runtimeID, State: "completed", CoverageRound: x.scope.CoverageRound, Revision: 1, StartedAt: x.clock.Add(-time.Minute), UpdatedAt: x.clock}
	row.BatchID, row.Revision, row.Change = batch.ID, 1, "updated"
	if err := x.f.store.writeDB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&batch).Error; err != nil {
			return err
		}
		return tx.Create(&row).Error
	}); err != nil {
		t.Fatal(err)
	}
}

func quarkReorganizationHead(t *testing.T, f versionedReorganizationFixture) models.CatalogHead {
	t.Helper()
	var head models.CatalogHead
	if err := f.store.writeDB.First(&head, "library_id = ?", f.library.ID).Error; err != nil {
		t.Fatal(err)
	}
	return head
}

func TestQuarkReorganizationIntegrationRollbackKeepsObservationHoldAndSinglePublication(t *testing.T) {
	for _, boundary := range []string{"confirmed_output_checkpoint", "catalog_finalize"} {
		t.Run(boundary, func(t *testing.T) { testQuarkReorganizationRollback(t, boundary) })
	}
}

func testQuarkReorganizationRollback(t *testing.T, boundary string) {
	x := newQuarkReorganizationObservationFixture(t)
	f := &x.f
	x.stalePending(t)
	before := quarkReorganizationHead(t, *f)
	trigger := "CREATE TRIGGER integration_reject_reorganization_finalize BEFORE UPDATE OF phase ON media_reorganization_tasks WHEN NEW.phase='reconciling' BEGIN SELECT RAISE(ABORT,'isolated-finalize-failure'); END"
	if boundary == "confirmed_output_checkpoint" {
		trigger = "CREATE TRIGGER integration_reject_reorganization_finalize BEFORE UPDATE OF state_json ON media_reorganization_tasks WHEN NEW.phase='executing' AND EXISTS (SELECT 1 FROM json_each(NEW.state_json, '$.completed')) BEGIN SELECT RAISE(ABORT,'isolated-completion-checkpoint-failure'); END"
	}
	if err := f.store.writeDB.Exec(trigger).Error; err != nil {
		t.Fatal(err)
	}
	if r := f.run(); r.ErrorCode == "" && r.RetryAt == nil && r.Wait == nil {
		t.Fatal("finalize trigger did not stop the transaction")
	}
	item := f.quarkDriver.items[f.entries[0].ProviderID]
	if item.Name != path.Base(f.plan.Items[0].NewRelativePath) || item.ParentID == "old-parent" || f.quarkDriver.moveCalls != 1 || f.quarkDriver.renameCalls != 1 {
		t.Fatal("fixture did not reach confirmed cloud output before local failure")
	}
	if boundary == "confirmed_output_checkpoint" {
		var task models.MediaReorganizationTask
		var state reorganizationState
		if err := f.store.writeDB.First(&task, "id = ?", f.task.ID).Error; err != nil {
			t.Fatal(err)
		}
		if decodeStrictJSON(task.StateJSON, &state) != nil || len(state.Completed) != 0 {
			t.Fatal("failure did not leave the cloud-confirmed output without a local completion checkpoint")
		}
	}
	if head := quarkReorganizationHead(t, *f); head.Revision != before.Revision {
		t.Fatal("rolled-back finalize published a catalog generation")
	}
	var original models.ProviderObservationItem
	if err := observationCurrent(f.store.writeDB, x.scope.ID).First(&original, "resource_id = ?", item.ID).Error; err != nil {
		t.Fatal(err)
	}
	if original.ParentID != "old-parent" || original.Name == item.Name {
		t.Fatal("observation baseline escaped the failed finalize transaction")
	}
	// A new observer has no process-local knowledge of the failed worker.
	// Its actual production source fence must still see the persisted hold.
	x.attach(t)
	x.clock = time.Now().UTC().Add(time.Minute)
	reads := f.quarkDriver.listCalls
	if _, err := x.s.ObserveNext(context.Background(), x.scope.ID, quarkObservationReader{driver: f.quarkDriver}); !errors.Is(err, ErrObservationDeferred) {
		t.Fatalf("read during unresolved finalization: %v", err)
	}
	if _, err := x.s.PublishPending(context.Background(), x.scope.ID, 100); !errors.Is(err, ErrObservationDeferred) {
		t.Fatalf("pending publication during unresolved finalization: %v", err)
	}
	if f.quarkDriver.listCalls != reads || quarkReorganizationHead(t, *f).Revision != before.Revision {
		t.Fatal("held observer read or published the intermediate operation")
	}
	if err := f.store.writeDB.Exec("DROP TRIGGER integration_reject_reorganization_finalize").Error; err != nil {
		t.Fatal(err)
	}
	f.worker = NewMediaReorganizationWorker(f.worker.service)
	if r := f.run(); r.ErrorCode != "" || r.RetryAt != nil || r.Wait != nil {
		t.Fatalf("recovery: %+v", r)
	}
	if f.quarkDriver.moveCalls != 1 || f.quarkDriver.renameCalls != 1 {
		t.Fatal("recovery repeated an already-confirmed cloud mutation")
	}
	after := quarkReorganizationHead(t, *f)
	if after.Revision != before.Revision+1 {
		t.Fatal("reorganization did not publish exactly one catalog revision")
	}
	assertQuarkReorganizationManualIdentity(t, *f)
	var baseline models.ProviderObservationItem
	if err := observationCurrent(f.store.writeDB, x.scope.ID).First(&baseline, "resource_id = ?", item.ID).Error; err != nil {
		t.Fatal(err)
	}
	if baseline.ParentID != item.ParentID || baseline.Name != item.Name || strings.TrimPrefix(baseline.RelativePath, "/") != f.plan.Items[0].NewRelativePath {
		t.Fatal("finalize did not atomically establish the confirmed observation baseline")
	}
	if _, err := x.s.PublishPending(context.Background(), x.scope.ID, 100); err != nil {
		t.Fatal(err)
	}
	if quarkReorganizationHead(t, *f).Revision != after.Revision {
		t.Fatal("stale pending observation published a second catalog revision")
	}
	x.round(t)
	if _, err := x.s.PublishPending(context.Background(), x.scope.ID, 100); err != nil {
		t.Fatal(err)
	}
	if quarkReorganizationHead(t, *f).Revision != after.Revision {
		t.Fatal("identical provider readback republished corrected media")
	}
	assertQuarkReorganizationManualIdentity(t, *f)
}

func assertQuarkReorganizationManualIdentity(t *testing.T, f versionedReorganizationFixture) {
	t.Helper()
	rows := catalogReadEntries(t, f.store, f.library.ID, "id = ?", f.entries[0].ID)
	if len(rows) != 1 || rows[0].ProviderID != f.entries[0].ProviderID || rows[0].TMDBID == nil || *rows[0].TMDBID != 999 || strings.TrimPrefix(rows[0].RelativePath, "/") != f.plan.Items[0].NewRelativePath || !rows[0].CreatedAt.Equal(f.entries[0].CreatedAt) {
		t.Fatal("stable entry, ingestion time or explicit manual identity changed")
	}
	var download models.DownloadTask
	if err := f.store.writeDB.First(&download, "id = ?", f.download.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !download.IdentityLocked || download.IdentitySource != mediaIdentitySourceManual || download.IdentityRevision != f.task.TargetIdentityRevision {
		t.Fatal("manual download identity was not retained")
	}
	var managed models.MediaManagedItem
	if err := f.store.writeDB.First(&managed, f.plan.Items[0].ManagedItemID).Error; err != nil {
		t.Fatal(err)
	}
	if managed.ProviderItemID != f.entries[0].ProviderID || managed.RelativePath != f.plan.Items[0].NewRelativePath || strings.HasPrefix(managed.ProviderParentID, "agent:") {
		t.Fatal("managed manifest lost canonical TV identity")
	}
}

func TestQuarkReorganizationIntegrationPublishedRecoveryDoesNotAccessCloud(t *testing.T) {
	f := newQuarkReorganizationFixture(t, false)
	if err := f.store.writeDB.Exec("CREATE TRIGGER integration_reject_managed_reconcile BEFORE UPDATE OF relative_path ON media_managed_items BEGIN SELECT RAISE(ABORT,'isolated-managed-failure'); END").Error; err != nil {
		t.Fatal(err)
	}
	if r := f.run(); r.ErrorCode == "" && r.RetryAt == nil && r.Wait == nil {
		t.Fatal("managed reconciliation trigger did not stop recovery")
	}
	var current models.MediaReorganizationTask
	if err := f.store.writeDB.First(&current, "id = ?", f.task.ID).Error; err != nil {
		t.Fatal(err)
	}
	var state reorganizationState
	if err := decodeStrictJSON(current.StateJSON, &state); err != nil || state.Catalog == nil || state.Catalog.Stage != "reconciling" {
		t.Fatalf("published recovery checkpoint missing: %v", err)
	}
	head := quarkReorganizationHead(t, f)
	list, stat, moves, renames := f.quarkDriver.listCalls, f.quarkDriver.statCalls, f.quarkDriver.moveCalls, f.quarkDriver.renameCalls
	f.quarkDriver.denyProof = true
	for id := range f.quarkDriver.items {
		f.quarkDriver.statErrors[id] = errors.New("post-publication cloud access forbidden by fixture")
	}
	if err := f.store.writeDB.Exec("DROP TRIGGER integration_reject_managed_reconcile").Error; err != nil {
		t.Fatal(err)
	}
	f.worker = NewMediaReorganizationWorker(f.worker.service)
	if r := f.run(); r.ErrorCode != "" || r.RetryAt != nil || r.Wait != nil {
		t.Fatalf("local reconciliation depended on cloud: %+v", r)
	}
	if f.quarkDriver.listCalls != list || f.quarkDriver.statCalls != stat || f.quarkDriver.moveCalls != moves || f.quarkDriver.renameCalls != renames {
		t.Fatal("published recovery returned to cloud I/O")
	}
	if quarkReorganizationHead(t, f).Revision != head.Revision {
		t.Fatal("published recovery repeated catalog publication")
	}
	assertQuarkReorganizationManualIdentity(t, f)
	if err := f.store.writeDB.First(&current, "id = ?", f.task.ID).Error; err != nil || current.Phase != models.MediaReorganizationPhaseCompleted {
		t.Fatalf("recovery did not complete: %v", err)
	}
}
