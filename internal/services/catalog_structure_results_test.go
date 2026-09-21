package services

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/database"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

func TestCatalogStructureRepairCancelPublishesObservedMoveWithoutReplay(t *testing.T) {
	for _, action := range []string{"cancel", "pause", "pause_then_cancel", "later_item_unavailable"} {
		t.Run(action, func(t *testing.T) {
			s, repair, plan, entries, root := catalogStructureRepairFixture(t)
			var databases []struct{ Name, File string }
			if err := s.db.Raw("PRAGMA database_list").Scan(&databases).Error; err != nil {
				t.Fatal(err)
			}
			var dbPath string
			for _, db := range databases {
				if db.Name == "main" {
					dbPath = db.File
				}
			}
			rt, err := database.AcquireExclusiveRuntime(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = rt.Close() })
			bound, err := rt.Bind(s.db)
			if err != nil {
				t.Fatal(err)
			}
			s.db = bound
			s.catalogStore.writeDB = bound
			boundRead, err := rt.Bind(s.catalogStore.readDB)
			if err != nil {
				t.Fatal(err)
			}
			s.catalogStore.readDB = boundRead
			plan.Items = append(plan.Items, StructurePlanItem{Kind: "video", SourceRelative: entries[1].RelativePath, TargetRelative: "Show/second.mkv", ProviderID: entries[1].ProviderID})
			raw, _ := json.Marshal(plan)
			repair.PlanJSON = string(raw)
			if err := s.db.Model(&repair).Updates(map[string]any{"plan_json": repair.PlanJSON, "total_items": 2}).Error; err != nil {
				t.Fatal(err)
			}
			s.queue = NewQueueService(s.db, s.audit)
			job, err := s.queue.Enqueue(EnqueueJobInput{OwnerID: repair.OwnerID, JobType: JobTypeMediaLibraryRepair, DisplayName: "cancel result", Payload: mediaLibraryRepairJobPayload{RepairID: repair.ID}})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.db.Model(&repair).Update("job_id", job.ID).Error; err != nil {
				t.Fatal(err)
			}
			claim, err := s.queue.Claim([]string{JobTypeMediaLibraryRepair})
			if err != nil || claim == nil {
				t.Fatalf("claim=%v %v", claim, err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			backend := &catalogStructureResultBackend{}
			backend.after = func() {
				actor := Actor{User: models.User{ID: repair.OwnerID}, Permissions: map[string]struct{}{authz.PermissionJobsControlAll: {}, authz.PermissionJobsReadAll: {}}}
				control := action
				if action == "later_item_unavailable" {
					control = "cancel"
				}
				if action == "pause_then_cancel" {
					control = "pause"
				}
				if _, err := s.queue.Control(actor, job.ID, control, RequestContext{}); err != nil {
					t.Fatal(err)
				}
				cancel()
				if action == "later_item_unavailable" {
					source := filepath.Join(root, filepath.FromSlash(plan.Items[1].SourceRelative))
					if err := os.Rename(source, source+".hold"); err != nil {
						t.Fatal(err)
					}
				}
			}
			s.backends.Register(backend)
			var before models.CatalogHead
			s.db.First(&before, "library_id=?", repair.LibraryID)
			_ = NewMediaLibraryRepairWorker(s).Run(ctx, fastScanTestRuntime{}, *claim)
			if len(backend.calls) != 1 {
				t.Fatalf("post-cancel mutation=%v", backend.calls)
			}
			if err := s.queue.AcknowledgeInterrupt(job.ID, claim.LeaseToken); err != nil {
				t.Fatal(err)
			}
			if action == "later_item_unavailable" {
				assertPhysicalOwnerState(t, s.db, CatalogPhysicalRepair, repair.ID, "quiescent")
				var stored models.MediaLibraryStructureRepair
				if err := s.db.First(&stored, "id=?", repair.ID).Error; err != nil {
					t.Fatal(err)
				}
				var state structureCatalogRepairState
				if err := json.Unmarshal([]byte(stored.StateJSON), &state); err != nil || state.CancelVerified != 1 {
					t.Fatalf("verified prefix lost: %+v %v", state, err)
				}
				source := filepath.Join(root, filepath.FromSlash(plan.Items[1].SourceRelative))
				if err := os.Rename(source+".hold", source); err != nil {
					t.Fatal(err)
				}
				if _, err := NewMediaLibraryRepairWorker(s).RecoverStoppedWork(context.Background(), 0); err != nil {
					t.Fatal(err)
				}
			}
			if action == "pause" {
				assertPhysicalOwnerState(t, s.db, CatalogPhysicalRepair, repair.ID, "quiescent")
				return
			}
			if action == "pause_then_cancel" {
				actor := Actor{User: models.User{ID: repair.OwnerID}, Permissions: map[string]struct{}{authz.PermissionJobsControlAll: {}, authz.PermissionJobsReadAll: {}}}
				if _, err := s.queue.Control(actor, job.ID, "cancel", RequestContext{}); err != nil {
					t.Fatal(err)
				}
				if _, err := NewMediaLibraryRepairWorker(s).RecoverStoppedWork(context.Background(), 0); err != nil {
					t.Fatal(err)
				}
			}
			assertPhysicalOwnerState(t, s.db, CatalogPhysicalRepair, repair.ID, "settled")
			var current models.MediaLibraryStructureRepair
			if err := s.db.First(&current, "id=?", repair.ID).Error; err != nil {
				t.Fatal(err)
			}
			if current.LastErrorCode != "structure_cancelled" || current.SucceededItems != 1 || current.FailedItems != 1 {
				t.Fatalf("result=%+v", current)
			}
			var rows []models.CatalogEntryFact
			if err := s.catalogStore.Read(context.Background(), []uint{repair.LibraryID}, func(r *CatalogReader) error { return r.RawEntryFacts().Order("id").Find(&rows).Error }); err != nil {
				t.Fatal(err)
			}
			if rows[0].RelativePath != "/"+plan.Items[0].TargetRelative || rows[1].RelativePath != entries[1].RelativePath {
				t.Fatalf("facts=%+v", rows)
			}
			// Global candidate/Job checks still reject the cancelled lease.
			if err := s.db.Transaction(func(tx *gorm.DB) error {
				return catalogCheckJob(tx, models.CatalogSnapshot{JobID: &job.ID, JobLeaseHash: leaseHash(claim.LeaseToken)}, time.Now().UTC())
			}); err == nil {
				t.Fatal("cancelled lease regained write authority")
			}
		})
	}
}

type catalogStructureResultBackend struct {
	localMediaLibraryStructureBackend
	fail  string
	calls []string
	after func()
}

func (b *catalogStructureResultBackend) Apply(ctx context.Context, boundary StructureBoundary, items []StructurePlanItem, progress StructureProgress) error {
	for _, item := range items {
		b.calls = append(b.calls, item.SourceRelative)
		if item.SourceRelative == b.fail {
			return errors.New("file locked")
		}
	}
	err := b.localMediaLibraryStructureBackend.Apply(ctx, boundary, items, progress)
	if b.after != nil {
		b.after()
	}
	return err
}

func TestCatalogStructureRepairPartialUnknownSourceRetainsEvidence(t *testing.T) {
	s, repair, plan, entries, root := catalogStructureRepairFixture(t)
	plan.Items = append(plan.Items, StructurePlanItem{Kind: "video", SourceRelative: entries[1].RelativePath, TargetRelative: "Show/second.mkv", ProviderID: entries[1].ProviderID})
	raw, _ := json.Marshal(plan)
	if err := s.db.Model(&repair).Updates(map[string]any{"plan_json": string(raw), "total_items": 2}).Error; err != nil {
		t.Fatal(err)
	}
	backend := &catalogStructureResultBackend{fail: entries[1].RelativePath}
	backend.after = func() {
		if err := os.Remove(filepath.Join(root, filepath.FromSlash(entries[1].RelativePath))); err != nil {
			t.Fatal(err)
		}
	}
	s.backends.Register(backend)
	var before, after models.CatalogHead
	s.db.First(&before, "library_id=?", repair.LibraryID)
	if result := s.runRepair(context.Background(), fastScanTestRuntime{}, repair.ID); result.ErrorCode == "" {
		t.Fatal("accepted unknown source")
	}
	s.db.First(&after, "library_id=?", repair.LibraryID)
	if before.Revision != after.Revision {
		t.Fatal("published unknown result")
	}
	assertPhysicalOwnerState(t, s.db, CatalogPhysicalRepair, repair.ID, "quiescent")
}

func TestCatalogStructureRepairPartialBookkeepingResumesSubset(t *testing.T) {
	s, repair, plan, entries, _ := catalogStructureRepairFixture(t)
	plan.Items = append(plan.Items, StructurePlanItem{Kind: "video", SourceRelative: entries[1].RelativePath, TargetRelative: "Show/second.mkv", ProviderID: entries[1].ProviderID})
	raw, _ := json.Marshal(plan)
	if err := s.db.Model(&repair).Updates(map[string]any{"plan_json": string(raw), "total_items": 2}).Error; err != nil {
		t.Fatal(err)
	}
	s.backends.Register(&catalogStructureResultBackend{fail: entries[1].RelativePath})
	callback := "test:partial-bookkeeping"
	if err := s.db.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "media_managed_items" {
			_ = tx.AddError(errors.New("interrupted"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	result := s.runRepair(context.Background(), fastScanTestRuntime{}, repair.ID)
	_ = s.db.Callback().Query().Remove(callback)
	if result.RetryAt == nil {
		t.Fatalf("result=%+v", result)
	}
	s.backends.Register(catalogStructureNoPhysicalBackend{t})
	result = s.runRepair(context.Background(), fastScanTestRuntime{}, repair.ID)
	if result.ErrorCode != CodeMediaLibraryStructureApplyFailed || result.RetryAt != nil {
		t.Fatalf("resume=%+v", result)
	}
	assertPhysicalOwnerState(t, s.db, CatalogPhysicalRepair, repair.ID, "settled")
	var rows []models.CatalogEntryFact
	if err := s.catalogStore.Read(context.Background(), []uint{repair.LibraryID}, func(r *CatalogReader) error { return r.RawEntryFacts().Order("id").Find(&rows).Error }); err != nil {
		t.Fatal(err)
	}
	if rows[1].RelativePath != entries[1].RelativePath {
		t.Fatalf("failed target published=%s", rows[1].RelativePath)
	}
}

func TestCatalogStructurePartialOriginalJobRetryDoesNotReplayPublishedMove(t *testing.T) {
	testCatalogStructurePartialRetry(t, false)
}

func TestCatalogStructurePartialRetryCancelDoesNotRepublishPreviousSuccess(t *testing.T) {
	testCatalogStructurePartialRetry(t, true)
}

func TestCatalogStructurePartialRetryBookkeepingResumesOnlyNewSuccess(t *testing.T) {
	testCatalogStructurePartialRetry(t, false, true)
}

func testCatalogStructurePartialRetry(t *testing.T, cancelRetry bool, interruptFinalization ...bool) {
	t.Helper()
	s, repair, plan, entries, _ := catalogStructureRepairFixture(t)
	plan.Items = append(plan.Items, StructurePlanItem{Kind: "video", SourceRelative: entries[1].RelativePath, TargetRelative: "Show/second.mkv", ProviderID: entries[1].ProviderID})
	raw, _ := json.Marshal(plan)
	repair.PlanJSON = string(raw)
	if err := s.db.Model(&repair).Updates(map[string]any{"plan_json": repair.PlanJSON, "total_items": 2}).Error; err != nil {
		t.Fatal(err)
	}
	s.queue = NewQueueService(s.db, s.audit)
	job, err := s.queue.Enqueue(EnqueueJobInput{OwnerID: repair.OwnerID, JobType: JobTypeMediaLibraryRepair, DisplayName: "partial retry", ResourceKey: "library:" + uintID(repair.LibraryID), Payload: mediaLibraryRepairJobPayload{RepairID: repair.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.db.Model(&repair).Update("job_id", job.ID).Error; err != nil {
		t.Fatal(err)
	}
	first, err := s.queue.Claim([]string{JobTypeMediaLibraryRepair})
	if err != nil || first == nil {
		t.Fatalf("claim: %+v %v", first, err)
	}
	backend := &catalogStructureResultBackend{fail: entries[1].RelativePath}
	s.backends.Register(backend)
	result := NewMediaLibraryRepairWorker(s).Run(context.Background(), fastScanTestRuntime{}, *first)
	if result.ErrorCode != CodeMediaLibraryStructureApplyFailed {
		t.Fatalf("first: %+v", result)
	}
	if err := s.queue.Fail(job.ID, first.LeaseToken, result.ErrorCode, result.ErrorMessage); err != nil {
		t.Fatal(err)
	}
	actor := Actor{User: models.User{ID: repair.OwnerID}, Permissions: map[string]struct{}{authz.PermissionJobsControlAll: {}, authz.PermissionJobsReadAll: {}}}
	if _, err := s.queue.Control(actor, job.ID, "retry", RequestContext{}); err != nil {
		t.Fatal(err)
	}
	second, err := s.queue.Claim([]string{JobTypeMediaLibraryRepair})
	if err != nil || second == nil {
		t.Fatalf("retry claim: %+v %v", second, err)
	}
	backend.fail = ""
	backend.calls = nil
	interrupt := len(interruptFinalization) > 0 && interruptFinalization[0]
	callback := "test:retry-subset-finalization"
	if interrupt {
		if err := s.db.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
			if tx.Statement.Table == "media_managed_items" {
				_ = tx.AddError(errors.New("interrupted"))
			}
		}); err != nil {
			t.Fatal(err)
		}
	}
	if cancelRetry {
		backend.after = func() {
			if _, err := s.queue.Control(actor, job.ID, "cancel", RequestContext{}); err != nil {
				t.Fatal(err)
			}
		}
	}
	result = NewMediaLibraryRepairWorker(s).Run(context.Background(), fastScanTestRuntime{}, *second)
	if interrupt {
		_ = s.db.Callback().Query().Remove(callback)
		if result.RetryAt == nil {
			t.Fatalf("did not suspend bookkeeping: %+v", result)
		}
		s.backends.Register(catalogStructureNoPhysicalBackend{t})
		result = NewMediaLibraryRepairWorker(s).Run(context.Background(), fastScanTestRuntime{}, *second)
	}
	if !cancelRetry && result.ErrorCode != "" {
		t.Fatalf("retry: %+v", result)
	}
	if len(backend.calls) != 1 || backend.calls[0] != entries[1].RelativePath {
		t.Fatalf("replayed success: %v", backend.calls)
	}
	if cancelRetry {
		if err := s.queue.AcknowledgeInterrupt(job.ID, second.LeaseToken); err != nil {
			t.Fatal(err)
		}
	} else {
		if err := s.queue.Complete(job.ID, second.LeaseToken); err != nil {
			t.Fatal(err)
		}
	}
	var stored models.MediaLibraryStructureRepair
	phase := "completed"
	if cancelRetry {
		phase = "failed"
	}
	if err := s.db.First(&stored, "id=?", repair.ID).Error; err != nil || stored.SucceededItems != 2 || stored.Phase != phase {
		t.Fatalf("stored: %+v %v", stored, err)
	}
	assertPhysicalOwnerState(t, s.db, CatalogPhysicalRepair, repair.ID, "settled")
}
