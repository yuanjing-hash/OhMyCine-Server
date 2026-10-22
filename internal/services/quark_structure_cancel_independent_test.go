package services

import (
	"context"
	"errors"
	"path"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/database"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"gorm.io/gorm"
)

// This fixture applies the physical rename once, then interrupts before the
// local cursor can be cleared. Recovery sees the original native receipt only
// through QueryOperation; a second mutation or BeginOperation is a test failure.
type quarkStructureCancelReceipt struct {
	*quarkTransferFake
	afterRename func()
	afterMove   func()
	beforeMove  func()
	operationID string
	queryKind   string
	confirmed   bool
	queryErr    error
	queryCalls  int
	beginCalls  int
}

func (f *quarkStructureCancelReceipt) Move(ctx context.Context, id, parent string) error {
	if f.beforeMove != nil {
		before := f.beforeMove
		f.beforeMove = nil
		before()
		return ctx.Err()
	}
	if err := f.quarkTransferFake.Move(ctx, id, parent); err != nil {
		return err
	}
	if f.afterMove != nil {
		after := f.afterMove
		f.afterMove = nil
		after()
	}
	return nil
}

func (f *quarkStructureCancelReceipt) Rename(ctx context.Context, id, name string) error {
	if err := f.quarkTransferFake.Rename(ctx, id, name); err != nil {
		return err
	}
	if f.afterRename != nil {
		after := f.afterRename
		f.afterRename = nil
		after()
	}
	return nil
}

func (f *quarkStructureCancelReceipt) BeginOperation(context.Context, cloud.OperationRequest) (cloud.OperationResult, error) {
	f.beginCalls++
	return cloud.OperationResult{}, errors.New("unexpected mutation submission during cancellation")
}

func (f *quarkStructureCancelReceipt) QueryOperation(_ context.Context, id string) (cloud.OperationResult, error) {
	f.queryCalls++
	if id != f.operationID {
		return cloud.OperationResult{}, errors.New("unexpected original operation identity")
	}
	if f.queryErr != nil {
		return cloud.OperationResult{}, f.queryErr
	}
	if !f.confirmed {
		return cloud.OperationResult{}, cloud.Error(cloud.CodeMutationUnknown, true, nil)
	}
	kind := f.queryKind
	if kind == "" {
		kind = "rename"
	}
	return cloud.OperationResult{Complete: true, Receipt: cloud.OperationReceipt{
		ID: id, Kind: kind, Provider: cloud.ProviderQuark, State: "confirmed", Revision: 2,
	}}, nil
}

func TestQuarkStructureIndependentCancelledNoReceiptIsDistinctFromUnreadableStore(t *testing.T) {
	for _, absent := range []bool{true, false} {
		name := "unreadable_store_keeps_hold"
		if absent {
			name = "exact_absence_settles_without_cloud_write"
		}
		t.Run(name, func(t *testing.T) {
			f := newQuarkStructureFixture(t)
			bindQuarkStructureCancelRuntime(t, f.s)
			repair, claim := f.queued(t)
			before := f.head(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			fake := &quarkStructureCancelReceipt{quarkTransferFake: f.driver}
			fake.beforeMove = func() {
				_, state := f.stored(t)
				if state.QuarkMutation == nil || state.QuarkMutation.Kind != "move" || state.ObservationHoldVersion != 1 {
					t.Fatal("move cursor was not persisted before dispatch")
				}
				cursor := state.QuarkMutation
				fake.operationID = quarkTransferOperationID("structure-repair:"+repair.ID, "move", cursor.ItemID, cursor.ParentID)
				actor := Actor{User: models.User{ID: repair.OwnerID}, Permissions: map[string]struct{}{authz.PermissionJobsControlAll: {}, authz.PermissionJobsReadAll: {}}}
				if _, err := f.s.queue.Control(actor, claim.Job.ID, "cancel", RequestContext{}); err != nil {
					t.Fatal(err)
				}
				cancel()
			}
			var storage models.Storage
			if err := f.s.db.First(&storage, f.library.StorageID).Error; err != nil {
				t.Fatal(err)
			}
			f.s.connections.drivers[*storage.ConnectionID] = fake
			_ = f.s.runRepair(ctx, nil, repair.ID, &claim)
			if fake.operationID == "" || f.driver.moveCalls != 0 || f.driver.renameCalls != 0 {
				t.Fatal("fixture dispatched a file mutation")
			}
			if err := f.s.queue.AcknowledgeInterrupt(claim.Job.ID, claim.LeaseToken); err != nil {
				t.Fatal(err)
			}
			fake.queryErr = errors.New("synthetic unreadable operation store")
			if absent {
				fake.queryErr = cloud.ErrOperationNotFound
			}
			writes := [3]int{f.driver.nextID, f.driver.moveCalls, f.driver.renameCalls}
			queries := fake.queryCalls
			for range 2 {
				if _, err := NewMediaLibraryRepairWorker(f.s).RecoverStoppedWork(context.Background(), 0); err != nil {
					t.Fatal(err)
				}
			}
			current, state := f.stored(t)
			if fake.queryCalls <= queries || fake.beginCalls != 0 || writes != [3]int{f.driver.nextID, f.driver.moveCalls, f.driver.renameCalls} || f.head(t) != before {
				t.Fatal("receipt-only recovery wrote files or published an empty catalog delta")
			}
			if absent {
				if state.QuarkMutation != nil || state.ObservationHoldVersion != 0 || state.Stage != "cancelled_finalized" || current.SucceededItems != 0 || current.FailedItems != len(f.plan.Items) {
					t.Fatalf("positive absence not settled: stage=%s hold=%d", state.Stage, state.ObservationHoldVersion)
				}
				assertPhysicalOwnerState(t, f.s.db, CatalogPhysicalRepair, repair.ID, "settled")
			} else {
				if state.QuarkMutation == nil || state.ObservationHoldVersion != 1 || state.Stage == "cancelled_finalized" {
					t.Fatal("unreadable operation store was treated as absent")
				}
				assertPhysicalOwnerState(t, f.s.db, CatalogPhysicalRepair, repair.ID, "quiescent")
			}
		})
	}
}

func TestQuarkStructureIndependentCancelRecoversOnlyOriginalConfirmedReceipt(t *testing.T) {
	for _, scenario := range []struct {
		name       string
		confirmed  bool
		sameParent bool
		moveOnly   bool
	}{
		{"confirmed_receipt_recovers_local_cursor", true, false, false},
		{"unknown_retains_hold_without_resubmit", false, false, false},
		{"same_parent_rename_recovers_local_cursor", true, true, false},
		{"confirmed_move_intermediate_name_remains_held", true, false, true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			f := newQuarkStructureFixture(t)
			bindQuarkStructureCancelRuntime(t, f.s)
			if scenario.sameParent {
				first := &f.plan.Items[0]
				first.TargetRelative = path.Join(path.Dir(first.SourceRelative), "confirmed-renamed"+path.Ext(first.SourceRelative))
			}
			repair, claim := f.queued(t)
			before := f.head(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			fake := &quarkStructureCancelReceipt{quarkTransferFake: f.driver}
			cancelAtBoundary := func() {
				_, state := f.stored(t)
				kind := "rename"
				if scenario.moveOnly {
					kind = "move"
				}
				if state.QuarkMutation == nil || state.QuarkMutation.Kind != kind || state.ObservationHoldVersion != 1 {
					t.Fatal("mutation did not persist its cursor and hold before dispatch")
				}
				cursor := state.QuarkMutation
				part := cursor.Name
				if scenario.moveOnly {
					part = cursor.ParentID
				}
				fake.operationID = quarkTransferOperationID("structure-repair:"+repair.ID, cursor.Kind, cursor.ItemID, part)
				actor := Actor{User: models.User{ID: repair.OwnerID}, Permissions: map[string]struct{}{
					authz.PermissionJobsControlAll: {}, authz.PermissionJobsReadAll: {},
				}}
				if _, err := f.s.queue.Control(actor, claim.Job.ID, "cancel", RequestContext{}); err != nil {
					t.Fatal(err)
				}
				cancel()
			}
			if scenario.moveOnly {
				fake.afterMove, fake.queryKind = cancelAtBoundary, "move"
			} else {
				fake.afterRename = cancelAtBoundary
			}
			var storage models.Storage
			if err := f.s.db.First(&storage, f.library.StorageID).Error; err != nil {
				t.Fatal(err)
			}
			f.s.connections.drivers[*storage.ConnectionID] = fake
			_ = f.s.runRepair(ctx, nil, repair.ID, &claim)
			if fake.operationID == "" || !scenario.moveOnly && f.driver.renameCalls != 1 || scenario.moveOnly && (f.driver.moveCalls != 1 || f.driver.renameCalls != 0) {
				t.Fatal("fixture did not interrupt the first completed physical mutation")
			}
			_, pending := f.stored(t)
			if pending.QuarkMutation == nil || pending.ObservationHoldVersion != 1 || f.head(t) != before {
				t.Fatal("unconfirmed cancelled cursor was released or published")
			}
			if err := f.s.queue.AcknowledgeInterrupt(claim.Job.ID, claim.LeaseToken); err != nil {
				t.Fatal(err)
			}
			assertPhysicalOwnerState(t, f.s.db, CatalogPhysicalRepair, repair.ID, "quiescent")
			writes := [3]int{f.driver.nextID, f.driver.moveCalls, f.driver.renameCalls}
			queryBefore := fake.queryCalls
			fake.confirmed = scenario.confirmed
			for range 2 {
				if _, err := NewMediaLibraryRepairWorker(f.s).RecoverStoppedWork(context.Background(), 0); err != nil {
					t.Fatal(err)
				}
			}
			current, state := f.stored(t)
			if writes != [3]int{f.driver.nextID, f.driver.moveCalls, f.driver.renameCalls} || fake.beginCalls != 0 {
				t.Fatal("cancel recovery dispatched a new physical operation")
			}
			if fake.queryCalls <= queryBefore {
				t.Fatalf("cancel recovery did not query original receipt: stage=%s phase=%s", state.Stage, current.Phase)
			}
			if !scenario.confirmed || scenario.moveOnly {
				if !scenario.confirmed && (state.QuarkMutation == nil || state.QuarkMutation.Kind != "rename") || state.ObservationHoldVersion != 1 || f.head(t) != before {
					t.Fatal("unresolved result was treated as a settled final destination")
				}
				if scenario.moveOnly && (f.driver.renameCalls != 0 || state.Stage == "cancelled_finalized") {
					t.Fatal("cancelled intermediate-name file was completed by an unapproved rename")
				}
				assertPhysicalOwnerState(t, f.s.db, CatalogPhysicalRepair, repair.ID, "quiescent")
				if err := observationQuarkStructureHoldTx(f.s.db, models.ProviderObservationScope{Provider: cloud.ProviderQuark, LibraryID: f.library.ID}); !errors.Is(err, ErrObservationDeferred) {
					t.Fatal("observer escaped unresolved cancelled operation")
				}
				return
			}
			if state.QuarkMutation != nil || state.ObservationHoldVersion != 0 || state.Stage != "cancelled_finalized" || current.LastErrorCode != "structure_cancelled" || current.SucceededItems != 1 || current.FailedItems != len(f.plan.Items)-1 || f.head(t) != before+1 {
				t.Fatalf("confirmed cancellation not finalized exactly once: stage=%s hold=%d succeeded=%d failed=%d head=%d", state.Stage, state.ObservationHoldVersion, current.SucceededItems, current.FailedItems, f.head(t))
			}
			assertPhysicalOwnerState(t, f.s.db, CatalogPhysicalRepair, repair.ID, "settled")
			var rows []models.CatalogEntryFact
			if err := f.s.catalogStore.Read(context.Background(), []uint{repair.LibraryID}, func(r *CatalogReader) error { return r.RawEntryFacts().Order("id").Find(&rows).Error }); err != nil {
				t.Fatal(err)
			}
			for _, row := range rows {
				want := ""
				for _, original := range f.entries {
					if original.ID == row.ID {
						want = original.RelativePath
						if !row.CreatedAt.Equal(original.CreatedAt) || row.WorkKey != original.WorkKey {
							t.Fatal("cancel publication lost stable media identity")
						}
					}
				}
				if row.ProviderID == pending.QuarkMutation.ItemID {
					want = "/" + pending.QuarkMutation.TargetRelative
				}
				if want == "" || row.RelativePath != want {
					t.Fatal("cancel publication does not describe exact settled subset")
				}
			}
			if err := f.s.db.Transaction(func(tx *gorm.DB) error {
				return catalogCheckJob(tx, models.CatalogSnapshot{JobID: &claim.Job.ID, JobLeaseHash: leaseHash(claim.LeaseToken)}, time.Now().UTC())
			}); err == nil {
				t.Fatal("cancelled job regained normal execution authority")
			}
		})
	}
}

func bindQuarkStructureCancelRuntime(t *testing.T, s *MediaLibraryStructureService) {
	t.Helper()
	var location struct{ File string }
	if err := s.db.Raw("PRAGMA database_list").Scan(&location).Error; err != nil {
		t.Fatal(err)
	}
	runtime, err := database.AcquireExclusiveRuntime(location.File)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	bound, err := runtime.Bind(s.db)
	if err != nil {
		t.Fatal(err)
	}
	s.db, s.catalogStore.writeDB = bound, bound
	s.catalogStore.readDB, err = runtime.Bind(s.catalogStore.readDB)
	if err != nil {
		t.Fatal(err)
	}
}
