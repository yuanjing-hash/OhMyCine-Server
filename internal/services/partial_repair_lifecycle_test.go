package services

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/database"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	cloudpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

func TestStructureCloudSameDirectoryRenameObservation(t *testing.T) {
	for _, changed := range []bool{false, true} {
		driver := newFakeMutationCloudDriver()
		size := int64(5)
		if changed {
			size = 17
		}
		driver.items["media"] = cloudpkg.Item{ID: "media", ParentID: "root", Name: "new.mkv", Size: size}
		connectionID := uint(1)
		backend := pan115MediaLibraryStructureBackend{driver: func(uint) (cloudpkg.Driver, error) { return driver, nil }}
		observe, err := structureFileObserver(context.Background(), StructureBoundary{Library: models.MediaLibrary{ProviderRootID: "root"}, Storage: models.Storage{Type: models.StorageTypePan115, ConnectionID: &connectionID}}, backend)
		if err != nil {
			t.Fatal(err)
		}
		atSource, _, err := observe("old.mkv", "media", 5, 0)
		if err != nil || atSource {
			t.Fatalf("renamed source: %v %v", atSource, err)
		}
		atTarget, _, err := observe("new.mkv", "media", 5, 0)
		if err != nil || atTarget == changed {
			t.Fatalf("exact target=%v changed=%v err=%v", atTarget, changed, err)
		}
		if driver.renameCalls != 0 || driver.moveCalls != 0 || driver.statCalls != 0 || driver.listCalls != 1 {
			t.Fatalf("observation replay/call amplification: %+v", driver)
		}
	}
}

type interruptAfterMoveBackend struct {
	localMediaLibraryStructureBackend
	after func()
}

func (b *interruptAfterMoveBackend) Apply(ctx context.Context, boundary StructureBoundary, items []StructurePlanItem, progress StructureProgress) error {
	err := b.localMediaLibraryStructureBackend.Apply(ctx, boundary, items, progress)
	if err == nil && b.after != nil {
		after := b.after
		b.after = nil
		after()
	}
	return err
}

func TestCancelledRepairObservesUncheckpointedMoveWithoutReplay(t *testing.T) {
	for _, mode := range []string{"running_cancel", "pause_then_cancel", "changed_after_cancel", "replaced_catalog"} {
		t.Run(mode, func(t *testing.T) {
			s, actor, library, diagnostics := prepareLegacyStructureRepair(t)
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
			runtime, err := database.AcquireExclusiveRuntime(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = runtime.Close() })
			bound, err := runtime.Bind(s.db)
			if err != nil {
				t.Fatal(err)
			}
			s.db, s.queue.db = bound, bound
			actor.Permissions[authz.PermissionJobsControlAll] = struct{}{}
			repair := enqueueLegacyStructureRepair(t, s, actor, library, diagnostics, "full")
			claim, err := s.queue.Claim([]string{JobTypeMediaLibraryRepair})
			if err != nil || claim == nil {
				t.Fatalf("claim: %+v %v", claim, err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			backend := &interruptAfterMoveBackend{after: func() {
				action := "cancel"
				if mode == "pause_then_cancel" || mode == "replaced_catalog" {
					action = "pause"
				}
				if _, err := s.queue.Control(actor, claim.Job.ID, action, RequestContext{}); err != nil {
					t.Fatal(err)
				}
				cancel()
				if mode == "changed_after_cancel" {
					var plan StructurePlan
					if err := json.Unmarshal([]byte(repair.PlanJSON), &plan); err != nil {
						t.Fatal(err)
					}
					var storage models.Storage
					if err := s.db.First(&storage, library.StorageID).Error; err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(storage.RootPath, filepath.FromSlash(plan.Items[0].TargetRelative)), []byte("changed unrelated bytes"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}}
			s.backends.Register(backend)
			_ = NewMediaLibraryRepairWorker(s).Run(ctx, fastScanTestRuntime{}, *claim)
			if err := s.queue.AcknowledgeInterrupt(claim.Job.ID, claim.LeaseToken); err != nil {
				t.Fatal(err)
			}
			if mode == "changed_after_cancel" {
				var proof models.CatalogPhysicalWrite
				if err := s.db.First(&proof, "owner_kind=? AND owner_id=?", CatalogPhysicalRepair, repair.ID).Error; err != nil || proof.State != "quiescent" {
					t.Fatalf("unknown result settled: %+v %v", proof, err)
				}
				return
			}
			if mode == "pause_then_cancel" || mode == "replaced_catalog" {
				var proof models.CatalogPhysicalWrite
				if err := s.db.First(&proof, "owner_kind=? AND owner_id=?", CatalogPhysicalRepair, repair.ID).Error; err != nil || proof.State != "quiescent" {
					t.Fatalf("pause was settled: %+v %v", proof, err)
				}
				if _, err := s.queue.Control(actor, claim.Job.ID, "cancel", RequestContext{}); err != nil {
					t.Fatal(err)
				}
				if mode == "replaced_catalog" {
					var plan StructurePlan
					if err := json.Unmarshal([]byte(repair.PlanJSON), &plan); err != nil {
						t.Fatal(err)
					}
					if err := s.db.Model(&models.MediaLibraryEntry{}).Where("library_id=? AND relative_path=?", library.ID, "/"+plan.Items[0].SourceRelative).Update("provider_id", "replacement").Error; err != nil {
						t.Fatal(err)
					}
				}
				if _, err := NewMediaLibraryRepairWorker(s).RecoverStoppedWork(context.Background(), 0); err != nil {
					t.Fatal(err)
				}
				if mode == "replaced_catalog" {
					if err := s.db.First(&proof, "owner_kind=? AND owner_id=?", CatalogPhysicalRepair, repair.ID).Error; err != nil || proof.State != "quiescent" {
						t.Fatalf("replacement falsely reconciled: %+v %v", proof, err)
					}
					return
				}
			}
			var proof models.CatalogPhysicalWrite
			if err := s.db.First(&proof, "owner_kind=? AND owner_id=?", CatalogPhysicalRepair, repair.ID).Error; err != nil || proof.State != "settled" {
				t.Fatalf("cancel still blocked: %+v %v", proof, err)
			}
			var stored models.MediaLibraryStructureRepair
			if err := s.db.First(&stored, "id=?", repair.ID).Error; err != nil || stored.SucceededItems != 1 || stored.LastErrorCode != "structure_cancelled" {
				t.Fatalf("cancel outcome: %+v %v", stored, err)
			}
			var job models.Job
			if err := s.db.First(&job, "id=?", claim.Job.ID).Error; err != nil || job.Status != models.JobStatusCancelled {
				t.Fatalf("job revived: %+v %v", job, err)
			}
			if _, err := s.Diagnose(context.Background(), library.ID, ""); err != nil {
				t.Fatal(err)
			}
			diagnosis, err := s.queue.Claim([]string{JobTypeMediaLibraryStructureDiagnosis})
			if err != nil || diagnosis == nil {
				t.Fatalf("diagnosis blocked: %+v %v", diagnosis, err)
			}
			if result := NewMediaLibraryStructureDiagnosisWorker(s).Run(context.Background(), fastScanTestRuntime{}, *diagnosis); result.ErrorCode != "" {
				t.Fatalf("diagnosis: %+v", result)
			}
			if err := s.queue.Complete(diagnosis.Job.ID, diagnosis.LeaseToken); err != nil {
				t.Fatal(err)
			}
			diagnostics, err = s.Diagnostics(context.Background(), actor, library.ID)
			if err != nil {
				t.Fatal(err)
			}
			next := enqueueLegacyStructureRepair(t, s, actor, library, diagnostics, "full")
			if next.ID == repair.ID {
				t.Fatal("cancelled owner reused")
			}
			completeLegacyStructureRepairForTest(t, s, next)
		})
	}
}

func TestPartialRepairSettlesVerifiedResultsAndAllowsFreshDiagnosis(t *testing.T) {
	testPartialRepairLifecycle(t, false)
}

func TestPartialRepairRetryExecutesOnlyRemainingItems(t *testing.T) {
	testPartialRepairLifecycle(t, true)
}

func TestCancelledRepairBeforeFirstCheckpointReleasesAdmission(t *testing.T) {
	s, actor, library, diagnostics := prepareLegacyStructureRepair(t)
	actor.Permissions[authz.PermissionJobsControlAll] = struct{}{}
	repair := enqueueLegacyStructureRepair(t, s, actor, library, diagnostics, "full")
	claim, err := s.queue.Claim([]string{JobTypeMediaLibraryRepair})
	if err != nil || claim == nil {
		t.Fatalf("claim: %+v %v", claim, err)
	}
	permit, err := enterCatalogPhysicalWrite(context.Background(), s.db, CatalogPhysicalWriteInput{LibraryID: library.ID, OwnerKind: CatalogPhysicalRepair, OwnerID: repair.ID, Job: claim, ActorID: actor.User.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.queue.Control(actor, claim.Job.ID, "cancel", RequestContext{}); err != nil {
		t.Fatal(err)
	}
	var plan StructurePlan
	if err := json.Unmarshal([]byte(repair.PlanJSON), &plan); err != nil {
		t.Fatal(err)
	}
	var storage models.Storage
	if err := s.db.First(&storage, library.StorageID).Error; err != nil {
		t.Fatal(err)
	}
	s.finishCancelledStructureRepair(permit, repair, plan, StructureBoundary{Library: library, Storage: storage}, localMediaLibraryStructureBackend{})
	if err := s.queue.AcknowledgeInterrupt(claim.Job.ID, claim.LeaseToken); err != nil {
		t.Fatal(err)
	}
	var proof models.CatalogPhysicalWrite
	if err := s.db.First(&proof, permit.evidence.ID).Error; err != nil || proof.State != "settled" {
		t.Fatalf("before checkpoint remains blocked: %+v %v", proof, err)
	}
}

func testPartialRepairLifecycle(t *testing.T, retry bool) {
	t.Helper()
	s, actor, library, diagnostics := prepareLegacyStructureRepair(t)
	actor.Permissions[authz.PermissionJobsControlAll] = struct{}{}
	preview, err := s.PreviewSelectionRepair(context.Background(), actor, library.ID, MediaLibraryStructureSelectionInput{Revision: diagnostics.Revision, IncludeAutomaticRepairs: true})
	if err != nil {
		t.Fatal(err)
	}
	repair, err := s.EnqueueSelectionRepair(context.Background(), actor, library.ID, preview.ConfirmationToken, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	var plan StructurePlan
	if err := json.Unmarshal([]byte(repair.PlanJSON), &plan); err != nil || len(plan.Items) != 2 {
		t.Fatalf("plan: %+v %v", plan, err)
	}
	var storage models.Storage
	if err := s.db.First(&storage, library.StorageID).Error; err != nil {
		t.Fatal(err)
	}
	// A real target appears after confirmation. The first move must succeed,
	// the second must fail unchanged, and final publication must not roll back.
	conflict := filepath.Join(storage.RootPath, filepath.FromSlash(plan.Items[1].TargetRelative))
	if err := os.MkdirAll(filepath.Dir(conflict), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(conflict, []byte("unrelated target"), 0600); err != nil {
		t.Fatal(err)
	}
	claim, err := s.queue.Claim([]string{JobTypeMediaLibraryRepair})
	if err != nil || claim == nil {
		t.Fatalf("claim: %+v %v", claim, err)
	}
	result := NewMediaLibraryRepairWorker(s).Run(context.Background(), fastScanTestRuntime{}, *claim)
	if result.ErrorCode != CodeMediaLibraryStructureApplyFailed {
		t.Fatalf("result: %+v", result)
	}
	if err := s.queue.Fail(claim.Job.ID, claim.LeaseToken, result.ErrorCode, result.ErrorMessage); err != nil {
		t.Fatal(err)
	}
	var stored models.MediaLibraryStructureRepair
	if err := s.db.First(&stored, "id=?", repair.ID).Error; err != nil || stored.Phase != "failed" || stored.SucceededItems != 1 || stored.FailedItems != 1 || stored.FinishedAt == nil {
		t.Fatalf("result not committed: %+v %v", stored, err)
	}
	var proof models.CatalogPhysicalWrite
	if err := s.db.First(&proof, "owner_kind=? AND owner_id=?", CatalogPhysicalRepair, repair.ID).Error; err != nil || proof.State != "settled" {
		t.Fatalf("partial proof: %+v %v", proof, err)
	}
	if retry {
		// A fresh diagnosis replaces the selection revision while the settled
		// failed attempt remains visible. It must not invalidate its retry.
		if _, err := s.Diagnose(context.Background(), library.ID, ""); err != nil {
			t.Fatal(err)
		}
		diagnosis, err := s.queue.Claim([]string{JobTypeMediaLibraryStructureDiagnosis})
		if err != nil || diagnosis == nil {
			t.Fatalf("diagnosis blocked: %v", err)
		}
		if result := NewMediaLibraryStructureDiagnosisWorker(s).Run(context.Background(), fastScanTestRuntime{}, *diagnosis); result.ErrorCode != "" {
			t.Fatalf("diagnosis: %+v", result)
		}
		if err := s.queue.Complete(diagnosis.Job.ID, diagnosis.LeaseToken); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(conflict); err != nil {
			t.Fatal(err)
		}
		if _, err := s.queue.Control(actor, claim.Job.ID, "retry", RequestContext{}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.queue.Control(actor, claim.Job.ID, "retry", RequestContext{}); ErrorCode(err) != CodeQueueStateConflict {
			t.Fatalf("duplicate retry accepted: %v", err)
		}
		completeLegacyStructureRepairForTest(t, s, repair)
		var checkpoints []models.MediaLibraryStructureRepairItem
		if err := s.db.Where("repair_id=?", repair.ID).Order("ordinal").Find(&checkpoints).Error; err != nil {
			t.Fatal(err)
		}
		if len(checkpoints) != 2 || checkpoints[0].AttemptCount != 1 || checkpoints[1].AttemptCount != 2 {
			t.Fatalf("retry repeated success: %+v", checkpoints)
		}
		return
	}
	if _, err := s.queue.Control(actor, claim.Job.ID, "cancel", RequestContext{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Diagnose(context.Background(), library.ID, ""); err != nil {
		t.Fatal(err)
	}
	diagnosis, err := s.queue.Claim([]string{JobTypeMediaLibraryStructureDiagnosis})
	if err != nil || diagnosis == nil {
		t.Fatalf("diagnosis still blocked: %+v %v", diagnosis, err)
	}
	if result := NewMediaLibraryStructureDiagnosisWorker(s).Run(context.Background(), fastScanTestRuntime{}, *diagnosis); result.ErrorCode != "" {
		t.Fatalf("diagnosis: %+v", result)
	}
	if err := s.queue.Complete(diagnosis.Job.ID, diagnosis.LeaseToken); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(conflict); err != nil {
		t.Fatal(err)
	}
	diagnostics, err = s.Diagnostics(context.Background(), actor, library.ID)
	if err != nil {
		t.Fatal(err)
	}
	next := enqueueLegacyStructureRepair(t, s, actor, library, diagnostics, "full")
	if next.ID == repair.ID {
		t.Fatal("reused cancelled partial repair")
	}
	completeLegacyStructureRepairForTest(t, s, next)
	var nextPlan StructurePlan
	if err := json.Unmarshal([]byte(next.PlanJSON), &nextPlan); err != nil || len(nextPlan.Items) != 1 || nextPlan.Items[0].SourceRelative != plan.Items[1].SourceRelative {
		t.Fatalf("successful move replayed: %+v %v", nextPlan, err)
	}
}

// A provider can return an error after committing its operation.
type lostAckStructureBackend struct {
	localMediaLibraryStructureBackend
	calls int
	hide  bool
}

func (b *lostAckStructureBackend) Apply(ctx context.Context, boundary StructureBoundary, items []StructurePlanItem, progress StructureProgress) error {
	b.calls++
	if err := b.localMediaLibraryStructureBackend.Apply(ctx, boundary, items, progress); err != nil {
		return err
	}
	if b.hide {
		target := filepath.Join(boundary.Storage.RootPath, filepath.FromSlash(items[0].TargetRelative))
		if err := os.Rename(target, target+".unknown"); err != nil {
			return err
		}
	}
	return errors.New("provider acknowledgement lost")
}
func TestStructureLostAcknowledgementSettlesOrRecoversAfterDiagnosisChanges(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(map[bool]string{false: "exact_target", true: "unknown_then_retry"}[unknown], func(t *testing.T) {
			s, actor, library, diagnostics := prepareLegacyStructureRepair(t)
			actor.Permissions[authz.PermissionJobsControlAll] = struct{}{}
			preview, err := s.PreviewSelectionRepair(context.Background(), actor, library.ID, MediaLibraryStructureSelectionInput{Revision: diagnostics.Revision, IncludeAutomaticRepairs: true})
			if err != nil {
				t.Fatal(err)
			}
			repair, err := s.EnqueueSelectionRepair(context.Background(), actor, library.ID, preview.ConfirmationToken, RequestContext{})
			if err != nil {
				t.Fatal(err)
			}
			backend := &lostAckStructureBackend{hide: unknown}
			s.backends.Register(backend)
			claim, err := s.queue.Claim([]string{JobTypeMediaLibraryRepair})
			if err != nil || claim == nil {
				t.Fatalf("claim: %v", err)
			}
			result := NewMediaLibraryRepairWorker(s).Run(context.Background(), fastScanTestRuntime{}, *claim)
			var proof models.CatalogPhysicalWrite
			if err := s.db.Where("owner_kind=? AND owner_id=?", CatalogPhysicalRepair, repair.ID).First(&proof).Error; err != nil {
				t.Fatal(err)
			}
			if !unknown {
				if result.ErrorCode != "" || proof.State != "settled" {
					t.Fatalf("lost ack left a blocker: %+v %+v", result, proof)
				}
				var stored models.MediaLibraryStructureRepair
				if err := s.db.First(&stored, "id=?", repair.ID).Error; err != nil || stored.SucceededItems != 2 || stored.FailedItems != 0 {
					t.Fatalf("outcome: %+v %v", stored, err)
				}
				return
			}
			if result.ErrorCode == "" || proof.State != "quiescent" {
				t.Fatalf("unknown result released: %+v %+v", result, proof)
			}
			if err := s.queue.Fail(claim.Job.ID, claim.LeaseToken, result.ErrorCode, result.ErrorMessage); err != nil {
				t.Fatal(err)
			}
			// Enqueueing diagnosis replaces the diagnostic Job ID even while waiting.
			if _, err := s.Diagnose(context.Background(), library.ID, ""); err != nil {
				t.Fatal(err)
			}
			var plan StructurePlan
			if err := json.Unmarshal([]byte(repair.PlanJSON), &plan); err != nil {
				t.Fatal(err)
			}
			var storage models.Storage
			if err := s.db.First(&storage, library.StorageID).Error; err != nil {
				t.Fatal(err)
			}
			for _, item := range plan.Items {
				target := filepath.Join(storage.RootPath, filepath.FromSlash(item.TargetRelative))
				if err := os.Rename(target+".unknown", target); err != nil {
					t.Fatal(err)
				}
			}
			s.backends.Register(localMediaLibraryStructureBackend{})
			if _, err := s.queue.Control(actor, claim.Job.ID, "retry", RequestContext{}); err != nil {
				t.Fatal(err)
			}
			completeLegacyStructureRepairForTest(t, s, repair)
			if err := s.db.First(&proof, proof.ID).Error; err != nil || proof.State != "settled" {
				t.Fatalf("retry retained blocker: %+v %v", proof, err)
			}
			diagnosis, err := s.queue.Claim([]string{JobTypeMediaLibraryStructureDiagnosis})
			if err != nil || diagnosis == nil {
				t.Fatalf("diagnosis remained blocked: %v", err)
			}
		})
	}
}

func TestStructurePreflightFailureDoesNotBlockFreshDiagnosis(t *testing.T) {
	s, actor, library, diagnostics := prepareLegacyStructureRepair(t)
	preview, err := s.PreviewSelectionRepair(context.Background(), actor, library.ID, MediaLibraryStructureSelectionInput{Revision: diagnostics.Revision, IncludeAutomaticRepairs: true})
	if err != nil {
		t.Fatal(err)
	}
	repair, err := s.EnqueueSelectionRepair(context.Background(), actor, library.ID, preview.ConfirmationToken, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Diagnose(context.Background(), library.ID, ""); err != nil {
		t.Fatal(err)
	}
	claim, err := s.queue.Claim([]string{JobTypeMediaLibraryRepair})
	if err != nil || claim == nil {
		t.Fatalf("claim: %v", err)
	}
	result := NewMediaLibraryRepairWorker(s).Run(context.Background(), fastScanTestRuntime{}, *claim)
	if result.ErrorCode != CodeMediaLibraryStructureBoundaryChanged {
		t.Fatalf("stale initial selection accepted: %+v", result)
	}
	if err := s.queue.Fail(claim.Job.ID, claim.LeaseToken, result.ErrorCode, result.ErrorMessage); err != nil {
		t.Fatal(err)
	}
	var proof models.CatalogPhysicalWrite
	if err := s.db.Where("owner_kind=? AND owner_id=?", CatalogPhysicalRepair, repair.ID).First(&proof).Error; err != nil || proof.State != "admitted" {
		t.Fatalf("preflight entered I/O: %+v %v", proof, err)
	}
	diagnosis, err := s.queue.Claim([]string{JobTypeMediaLibraryStructureDiagnosis})
	if err != nil || diagnosis == nil {
		t.Fatalf("failed preflight blocked diagnosis: %v", err)
	}
}

func TestStructureCloudFailureOutcomeUsesExactIdentity(t *testing.T) {
	for _, position := range []string{"source", "target", "replacement"} {
		t.Run(position, func(t *testing.T) {
			s, repair, _, _, _ := catalogStructureRepairFixture(t)
			plan := StructurePlan{Version: 1, LibraryID: repair.LibraryID, Items: []StructurePlanItem{{Kind: "video", SourceRelative: "old.mkv", TargetRelative: "new.mkv", ProviderID: "media", Size: 5}}}
			raw, _ := json.Marshal(plan)
			repair.PlanJSON = string(raw)
			if err := s.db.Model(&models.MediaLibraryStructureRepair{}).Where("id=?", repair.ID).Updates(map[string]any{"plan_json": repair.PlanJSON, "total_items": 1}).Error; err != nil {
				t.Fatal(err)
			}
			execution := s.executeStructureRepairItems(context.Background(), fastScanTestRuntime{}, repair, plan, StructureBoundary{}, &checkpointStructureBackend{fail: map[string]bool{"old.mkv": true}}, nil)
			if execution.GlobalErr != nil || execution.Failed != 1 {
				t.Fatalf("execution: %+v", execution)
			}
			driver := newFakeMutationCloudDriver()
			name := "new.mkv"
			if position == "source" {
				name = "old.mkv"
			}
			id := "media"
			if position == "replacement" {
				id = "other"
			}
			driver.items[id] = cloudpkg.Item{ID: id, ParentID: "root", Name: name, Size: 5}
			connectionID := uint(1)
			boundary := StructureBoundary{Library: models.MediaLibrary{ProviderRootID: "root"}, Storage: models.Storage{Type: models.StorageTypePan115, ConnectionID: &connectionID}}
			backend := pan115MediaLibraryStructureBackend{driver: func(uint) (cloudpkg.Driver, error) { return driver, nil }}
			err := s.reconcileStructureFailureOutcomes(context.Background(), repair, plan, boundary, backend, nil, &execution)
			if position == "replacement" {
				if err == nil {
					t.Fatal("replacement released unknown result")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if position == "source" && execution.Failed != 1 {
				t.Fatal("unchanged failure lost")
			}
			if position == "target" && (execution.Failed != 0 || execution.Succeeded != 1 || len(execution.Plan.Items) != 1) {
				t.Fatalf("target not reconciled: %+v", execution)
			}
			if driver.renameCalls != 0 || driver.moveCalls != 0 {
				t.Fatal("observation repeated mutation")
			}
		})
	}
}
