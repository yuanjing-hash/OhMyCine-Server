package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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
