package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/database"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestCancelledArtifactAfterWorkerExitRecoversOriginalJob(t *testing.T) {
	for _, scenario := range []string{"retry_wait", "paused", "restart", "changed", "not_cancelled", "entered", "legacy_runtime", "legacy_receipts", "source_drift"} {
		t.Run(scenario, func(t *testing.T) {
			cleanup, queue, actor, library, root := strmManagementFixture(t)
			var databases []struct{ Name, File string }
			if err := cleanup.db.Raw("PRAGMA database_list").Scan(&databases).Error; err != nil {
				t.Fatal(err)
			}
			path := ""
			for _, db := range databases {
				if db.Name == "main" {
					path = db.File
				}
			}
			bind := func() *database.ExclusiveRuntime {
				runtime, err := database.AcquireExclusiveRuntime(path)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = runtime.Close() })
				bound, err := runtime.Bind(cleanup.db)
				if err != nil {
					t.Fatal(err)
				}
				cleanup.db, queue.db, cleanup.libraries.db = bound, bound, bound
				return runtime
			}
			runtime := bind()
			run, artifact, target := createAutoCleanupScenario(t, cleanup, library, root, "full", false, models.MediaArtifactStatusCompleted, nil)
			digest := sha256.Sum256([]byte("stale\n"))
			if err := cleanup.db.Model(&artifact).Update("content_fingerprint", hex.EncodeToString(digest[:])).Error; err != nil {
				t.Fatal(err)
			}
			claim, err := queue.Claim([]string{JobTypeMediaArtifact})
			if err != nil || claim == nil {
				t.Fatalf("claim=%v err=%v", claim, err)
			}
			calls := 0
			cleanup.removeFile = func(path string) error {
				calls++
				if scenario == "changed" {
					if err := os.WriteFile(path, []byte("external changes"), 0600); err != nil {
						return err
					}
				} else if err := os.Remove(path); err != nil {
					return err
				}
				return errors.New("lost acknowledgement")
			}
			cleanup.removeDir = func(string) error { t.Fatal("recovery pruned directory"); return nil }
			service := NewMediaArtifactService(cleanup.db, queue, nil, zerolog.Nop())
			service.cleanup = cleanup
			worker := NewMediaArtifactWorker(service)
			result := worker.Run(context.Background(), workerRuntime{queue: queue, job: *claim}, *claim)
			if calls != 1 || result.ErrorCode == "" {
				t.Fatalf("calls=%d result=%+v", calls, result)
			}
			if err := queue.RetryLater(claim.Job.ID, claim.LeaseToken, result.ErrorCode, "等待确认", time.Now().Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			actor.Permissions[authz.PermissionJobsControlAll] = struct{}{}
			actor.Permissions[authz.PermissionJobsReadAll] = struct{}{}
			if scenario == "paused" {
				if _, err := queue.Control(actor, claim.Job.ID, "pause", RequestContext{}); err != nil {
					t.Fatal(err)
				}
			}
			if scenario != "not_cancelled" {
				if _, err := queue.Control(actor, claim.Job.ID, "cancel", RequestContext{}); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "restart" {
				if err := runtime.Close(); err != nil {
					t.Fatal(err)
				}
				bind()
				service.db = cleanup.db
				if _, err := RecoverCatalogPhysicalRuntimeBatch(context.Background(), cleanup.db); err != nil {
					t.Fatal(err)
				}
			}
			// Missing historic evidence, live physical calls, and source drift
			// must never gain observation authority just from a cancelled label.
			updates := map[string]any{}
			switch scenario {
			case "entered":
				updates["state"] = "entered"
			case "legacy_runtime":
				updates["runtime_id"] = ""
			case "legacy_receipts":
				updates["artifact_receipt_version"] = 0
			case "source_drift":
				updates["source_fingerprint"] = "different-source"
			}
			if len(updates) > 0 {
				if err := cleanup.db.Model(&models.CatalogPhysicalWrite{}).Where("owner_id=?", run.ID).Updates(updates).Error; err != nil {
					t.Fatal(err)
				}
			}
			diagnosis, err := queue.Enqueue(EnqueueJobInput{System: true, JobType: JobTypeMediaLibraryStructureDiagnosis, DisplayName: "目录诊断", Provider: "media_library", ResourceKey: mediaArtifactResourceKey(library.ID), CoalescingKey: "diagnosis", Payload: map[string]any{"library_id": library.ID}})
			if err != nil {
				t.Fatal(err)
			}
			if got, err := queue.Claim([]string{JobTypeMediaLibraryStructureDiagnosis}); err != nil || got != nil {
				t.Fatalf("unsafe diagnosis claim=%v err=%v", got, err)
			}
			other := library
			other.ID, other.Name, other.NameNormalized = 0, "Independent", "independent"
			if err := cleanup.db.Create(&other).Error; err != nil {
				t.Fatal(err)
			}
			otherJob, err := queue.Enqueue(EnqueueJobInput{System: true, JobType: JobTypeMediaLibraryStructureDiagnosis, DisplayName: "Independent diagnosis", ResourceKey: mediaArtifactResourceKey(other.ID), Payload: map[string]any{"library_id": other.ID}})
			if err != nil {
				t.Fatal(err)
			}
			otherClaim, err := queue.Claim([]string{JobTypeMediaLibraryStructureDiagnosis})
			if err != nil || otherClaim == nil || otherClaim.Job.ID != otherJob.ID {
				t.Fatalf("unrelated library starved: %v %v", otherClaim, err)
			}
			if err := queue.Complete(otherClaim.Job.ID, otherClaim.LeaseToken); err != nil {
				t.Fatal(err)
			}
			if scenario == "retry_wait" {
				registry := NewWorkerRegistry()
				if err := registry.Register(JobTypeMediaArtifact, worker); err != nil {
					t.Fatal(err)
				}
				scheduler := NewScheduler(queue, registry, zerolog.Nop())
				if err := scheduler.Start(context.Background()); err != nil {
					t.Fatal(err)
				}
				deadline := time.Now().Add(5 * time.Second)
				for time.Now().Before(deadline) {
					var state string
					if err := cleanup.db.Model(&models.CatalogPhysicalWrite{}).Select("state").Where("owner_id=?", run.ID).Scan(&state).Error; err != nil {
						t.Fatal(err)
					}
					if state == "settled" {
						break
					}
					time.Sleep(10 * time.Millisecond)
				}
				scheduler.Close()
			} else if _, err := worker.RecoverStoppedWork(context.Background(), 0); err != nil {
				t.Fatal(err)
			}
			var proof models.CatalogPhysicalWrite
			if err := cleanup.db.Where("owner_kind=? AND owner_id=?", CatalogPhysicalArtifact, run.ID).First(&proof).Error; err != nil {
				t.Fatal(err)
			}
			settled := scenario == "retry_wait" || scenario == "paused" || scenario == "restart"
			if (proof.State == "settled") != settled {
				t.Fatalf("state=%s", proof.State)
			}
			if calls != 1 {
				t.Fatal("cancelled work replayed physical mutation")
			}
			if got, err := queue.Claim([]string{JobTypeMediaLibraryStructureDiagnosis}); err != nil || (got != nil) != settled {
				t.Fatalf("diagnosis=%s claim=%v err=%v settled=%v", diagnosis.ID, got, err, settled)
			}
			var job models.Job
			if err := cleanup.db.First(&job, "id=?", claim.Job.ID).Error; err != nil {
				t.Fatal(err)
			}
			if scenario != "not_cancelled" && job.Status != models.JobStatusCancelled {
				t.Fatal("cancelled job revived")
			}
			if scenario == "changed" {
				bytes, err := os.ReadFile(target)
				if err != nil || string(bytes) != "external changes" || job.LastErrorCode != "artifact_cancelled_reconciliation_pending" {
					t.Fatalf("lost conflict %s %v %+v", bytes, err, job)
				}
			}
			var count int64
			if err := cleanup.db.Model(&models.Job{}).Where("job_type=?", JobTypeMediaArtifact).Count(&count).Error; err != nil || count != 1 {
				t.Fatalf("replacement jobs=%d %v", count, err)
			}
		})
	}
}

func TestCancelledLegacyArtifactWithPositiveNoIOEvidenceUnblocksLibraryAndHistory(t *testing.T) {
	queue, actor, _ := queueFixture(t)
	actor = historyAdmin(actor)
	library := historyLibrary(t, queue, "legacy-artifact-no-io")
	if err := queue.db.Model(&library).Updates(map[string]any{
		"enabled": true, "baseline_generation": 1, "dirty_generation": 1,
		"status": models.MediaLibraryStatusListening,
	}).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	runID := "cancelled-legacy-no-io-run"
	job := models.Job{
		ID: "cancelled-legacy-no-io-job", CreatedByKind: "system", JobType: JobTypeMediaArtifact,
		Status: models.JobStatusCancelled, Revision: 2, DisplayName: "legacy artifact",
		PayloadJSON: fmt.Sprintf(`{"artifact_run_id":%q}`, runID), CheckpointJSON: `{}`,
		CancellationAsked: true, FinishedAt: &now, CreatedAt: now, UpdatedAt: now,
	}
	if err := queue.db.Create(&job).Error; err != nil {
		t.Fatal(err)
	}
	run := models.MediaArtifactRun{
		ID: runID, LibraryID: library.ID, Generation: 1, JobID: &job.ID, PolicyJSON: `{}`,
		Status: models.MediaArtifactStatusFailed, ExpectedCount: 12144, ProcessedCount: 12144,
		SkippedCount: 24115, FailedCount: 173, ErrorCode: "historic failure",
		CleanupStatus: models.MediaArtifactCleanupSkipped, CleanupAt: &now, FinishedAt: &now,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := queue.db.Create(&run).Error; err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 3; index++ {
		artifact := models.MediaArtifact{
			OpaqueID: fmt.Sprintf("cancelled-legacy-placeholder-%d", index), RunID: run.ID, LibraryID: library.ID,
			Kind: models.MediaArtifactKindNFO, TargetKind: models.MediaArtifactTargetLocalProjection,
			RelativePath: fmt.Sprintf("/pending-%d.nfo", index), Managed: true, Active: true,
			Status: models.MediaArtifactStatusQueued, CreatedAt: now, UpdatedAt: now,
		}
		if err := queue.db.Create(&artifact).Error; err != nil {
			t.Fatal(err)
		}
	}
	physical := models.CatalogPhysicalWrite{
		LibraryID: library.ID, OwnerKind: CatalogPhysicalArtifact, OwnerID: run.ID,
		Revision: 2, State: "quiescent", JobID: job.ID, JobLeaseHash: "historic-lease",
		RuntimeID: "historic-runtime", OwnerDigest: "owner", SourceFingerprint: "source",
		ConfigFingerprint: "config", ArtifactReceiptVersion: 0, EnteredAt: now, UpdatedAt: now,
	}
	if err := queue.db.Create(&physical).Error; err != nil {
		t.Fatal(err)
	}

	other := historyLibrary(t, queue, "legacy-artifact-other-library")
	otherPhysical := models.CatalogPhysicalWrite{
		LibraryID: other.ID, OwnerKind: CatalogPhysicalArtifact, OwnerID: "other-unsafe-run",
		Revision: 1, State: "quiescent", RuntimeID: "other-runtime", OwnerDigest: "other",
		SourceFingerprint: "other-source", ConfigFingerprint: "other-config", EnteredAt: now, UpdatedAt: now,
	}
	if err := queue.db.Create(&otherPhysical).Error; err != nil {
		t.Fatal(err)
	}

	diagnosis, err := queue.Enqueue(EnqueueJobInput{
		System: true, JobType: JobTypeMediaLibraryStructureDiagnosis, DisplayName: "目录诊断",
		ResourceKey:   "structure-diagnosis-library:" + uintID(library.ID),
		CoalescingKey: "manual", Payload: map[string]any{"library_id": library.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := queue.Claim([]string{JobTypeMediaLibraryStructureDiagnosis}); err != nil || got != nil {
		t.Fatalf("diagnosis was not initially blocked: claim=%v err=%v", got, err)
	}

	service := NewMediaArtifactService(queue.db, queue, nil, zerolog.Nop())
	worker := NewMediaArtifactWorker(service)
	if _, err := worker.RecoverStoppedWork(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if err := queue.db.First(&physical, physical.ID).Error; err != nil {
		t.Fatal(err)
	}
	if physical.State != "settled" || physical.SettledAt == nil {
		t.Fatalf("legacy no-I/O physical proof was not settled: %+v", physical)
	}
	var placeholders int64
	if err := queue.db.Model(&models.MediaArtifact{}).Where("run_id=?", run.ID).Count(&placeholders).Error; err != nil || placeholders != 0 {
		t.Fatalf("legacy placeholders=%d err=%v", placeholders, err)
	}
	readiness, err := libraryReadiness(queue.db, library.ID)
	if err != nil || !readiness.Ready || readiness.ReadinessStatus != "ready" {
		t.Fatalf("readiness=%+v err=%v", readiness, err)
	}
	var untouched models.CatalogPhysicalWrite
	if err := queue.db.First(&untouched, otherPhysical.ID).Error; err != nil || untouched.State != "quiescent" {
		t.Fatalf("other library evidence changed: %+v err=%v", untouched, err)
	}

	claim, err := queue.Claim([]string{JobTypeMediaLibraryStructureDiagnosis})
	if err != nil || claim == nil || claim.Job.ID != diagnosis.ID {
		t.Fatalf("diagnosis did not resume: claim=%v err=%v", claim, err)
	}
	if err := queue.Complete(claim.Job.ID, claim.LeaseToken); err != nil {
		t.Fatal(err)
	}
	result, err := queue.PurgeHistory(actor, HistoryPurgeInput{Scope: HistoryScopeTasks}, RequestContext{})
	if err != nil || result.Deleted < 1 {
		t.Fatalf("history purge=%+v err=%v", result, err)
	}
	if err := queue.db.First(&job, "id=?", job.ID).Error; err != nil {
		t.Fatal(err)
	}
	if job.HistoryClearedAt == nil {
		t.Fatal("reconciled cancelled artifact job remained visible in history")
	}
}

func TestLegacyArtifactEnteredAtMigrationRecoversAfterStartupQuiescence(t *testing.T) {
	queue, _, _ := queueFixture(t)
	var databases []struct{ Name, File string }
	if err := queue.db.Raw("PRAGMA database_list").Scan(&databases).Error; err != nil {
		t.Fatal(err)
	}
	path := ""
	for _, item := range databases {
		if item.Name == "main" {
			path = item.File
			break
		}
	}
	runtime, err := database.AcquireExclusiveRuntime(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	bound, err := runtime.Bind(queue.db)
	if err != nil {
		t.Fatal(err)
	}
	queue.db = bound

	// Re-run the numbered upgrade around production startup ordering: v101 is
	// applied before the held runtime changes a previous process' entered row
	// to quiescent. Runtime stopped-work recovery must finish that safe row.
	if err := bound.Exec("DELETE FROM schema_migrations WHERE version=101").Error; err != nil {
		t.Fatal(err)
	}
	library := historyLibrary(t, queue, "legacy-entered-startup")
	now := time.Now().UTC()
	runID := "legacy-entered-startup-run"
	job := models.Job{
		ID: "legacy-entered-startup-job", CreatedByKind: "system", JobType: JobTypeMediaArtifact,
		Status: models.JobStatusFailed, Revision: 2, DisplayName: "legacy entered artifact",
		PayloadJSON: fmt.Sprintf(`{"artifact_run_id":%q}`, runID), CheckpointJSON: `{}`,
		FinishedAt: &now, CreatedAt: now, UpdatedAt: now,
	}
	if err := bound.Create(&job).Error; err != nil {
		t.Fatal(err)
	}
	run := models.MediaArtifactRun{
		ID: runID, LibraryID: library.ID, Generation: 1, JobID: &job.ID, PolicyJSON: `{}`,
		Status: models.MediaArtifactStatusFailed, ExpectedCount: 1, ProcessedCount: 1, FailedCount: 1,
		CleanupStatus: models.MediaArtifactCleanupSkipped, CleanupAt: &now, FinishedAt: &now,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := bound.Create(&run).Error; err != nil {
		t.Fatal(err)
	}
	artifact := models.MediaArtifact{
		OpaqueID: "legacy-entered-startup-placeholder", RunID: run.ID, LibraryID: library.ID,
		Kind: models.MediaArtifactKindNFO, TargetKind: models.MediaArtifactTargetLocalProjection,
		RelativePath: "/pending.nfo", Managed: true, Active: true,
		Status: models.MediaArtifactStatusQueued, CreatedAt: now, UpdatedAt: now,
	}
	if err := bound.Create(&artifact).Error; err != nil {
		t.Fatal(err)
	}
	physical := models.CatalogPhysicalWrite{
		LibraryID: library.ID, OwnerKind: CatalogPhysicalArtifact, OwnerID: run.ID,
		Revision: 2, State: "entered", JobID: job.ID, JobLeaseHash: "historic-lease",
		RuntimeID: "previous-runtime", OwnerDigest: "owner", SourceFingerprint: "source",
		ConfigFingerprint: "config", ArtifactReceiptVersion: 0, EnteredAt: now, UpdatedAt: now,
	}
	if err := bound.Create(&physical).Error; err != nil {
		t.Fatal(err)
	}

	if err := database.Migrate(bound); err != nil {
		t.Fatal(err)
	}
	if err := bound.First(&physical, physical.ID).Error; err != nil || physical.State != "entered" {
		t.Fatalf("v101 must not settle a possibly live entered row: %+v err=%v", physical, err)
	}
	if recovered, err := RecoverCatalogPhysicalRuntimeBatch(context.Background(), bound); err != nil || recovered != 1 {
		t.Fatalf("startup quiescence recovered=%d err=%v", recovered, err)
	}
	service := NewMediaArtifactService(bound, queue, nil, zerolog.Nop())
	if _, err := NewMediaArtifactWorker(service).RecoverStoppedWork(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if err := bound.First(&physical, physical.ID).Error; err != nil || physical.State != "settled" || physical.SettledAt == nil {
		t.Fatalf("runtime recovery did not settle safe v0 row: %+v err=%v", physical, err)
	}
	var placeholders int64
	if err := bound.Model(&models.MediaArtifact{}).Where("run_id=?", run.ID).Count(&placeholders).Error; err != nil || placeholders != 0 {
		t.Fatalf("legacy placeholders=%d err=%v", placeholders, err)
	}
}
