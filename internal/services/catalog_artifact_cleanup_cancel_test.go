package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

func TestArtifactWorkerCancellationReconcilesCleanupWithoutReplayingPhysicalCalls(t *testing.T) {
	for _, scenario := range []string{"cancel-missing", "cancel-matching", "cancel-changed", "pause", "shutdown"} {
		t.Run(scenario, func(t *testing.T) {
			cleanup, queue, actor, library, root := strmManagementFixture(t)
			run, artifact, target := createAutoCleanupScenario(t, cleanup, library, root, "full", false, models.MediaArtifactStatusCompleted, nil)
			content := []byte("stale\n")
			digest := sha256.Sum256(content)
			if err := cleanup.db.Model(&artifact).Update("content_fingerprint", hex.EncodeToString(digest[:])).Error; err != nil {
				t.Fatal(err)
			}
			claimed, err := queue.Claim([]string{JobTypeMediaArtifact})
			if err != nil || claimed == nil {
				t.Fatalf("claim=%v %v", claimed, err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			queue.SetInterrupt(func(string, string) { cancel() })
			actor.Permissions[authz.PermissionJobsControlAll] = struct{}{}
			actor.Permissions[authz.PermissionJobsReadAll] = struct{}{}
			calls := 0
			cleanup.removeFile = func(path string) error {
				calls++
				if calls != 1 {
					t.Fatal("cancelled recovery attempted another physical delete")
				}
				if scenario != "cancel-matching" && scenario != "cancel-changed" {
					if err := os.Remove(path); err != nil {
						return err
					}
				} else if scenario == "cancel-changed" {
					content = []byte("new external contents")
					if err := os.WriteFile(path, content, 0o600); err != nil {
						return err
					}
				}
				if scenario == "shutdown" {
					cancel()
				} else {
					action := "cancel"
					if scenario == "pause" {
						action = "pause"
					}
					if _, err := queue.Control(actor, claimed.Job.ID, action, RequestContext{}); err != nil {
						t.Fatal(err)
					}
				}
				return errors.New("physical call returned after interruption")
			}
			cleanup.removeDir = func(string) error { t.Fatal("cancelled recovery attempted directory pruning"); return nil }
			service := NewMediaArtifactService(cleanup.db, queue, nil, zerolog.Nop())
			service.cleanup = cleanup
			result := NewMediaArtifactWorker(service).Run(ctx, workerRuntime{queue: queue, job: *claimed}, *claimed)
			if calls != 1 || result.ErrorCode == "" {
				t.Fatalf("worker did not reach interrupted cleanup: calls=%d result=%+v", calls, result)
			}
			var proof models.CatalogPhysicalWrite
			if err := cleanup.db.Where("owner_kind = ? AND owner_id = ?", CatalogPhysicalArtifact, run.ID).First(&proof).Error; err != nil {
				t.Fatal(err)
			}
			settled := scenario == "cancel-missing" || scenario == "cancel-matching"
			if (proof.State == "settled") != settled {
				t.Fatalf("proof state=%s", proof.State)
			}
			if err := cleanup.db.First(&run, "id = ?", run.ID).Error; err != nil {
				t.Fatal(err)
			}
			if settled {
				if run.Status != models.MediaArtifactStatusFailed || run.ErrorCode != "artifact_cancelled" || run.CleanupStatus != models.MediaArtifactCleanupSkipped {
					t.Fatalf("cancelled domain=%+v", run)
				}
			} else if run.CleanupStatus == models.MediaArtifactCleanupSkipped || run.ErrorCode == "artifact_cancelled" {
				t.Fatalf("unreconciled/pause/shutdown falsely completed: %+v", run)
			}
			var count int64
			if err := cleanup.db.Model(&models.CatalogArtifactCleanupClaim{}).Where("artifact_id = ?", artifact.ID).Count(&count).Error; err != nil {
				t.Fatal(err)
			}
			if (count == 0) != settled {
				t.Fatalf("claim count=%d settled=%v", count, settled)
			}
			if scenario == "cancel-matching" || scenario == "cancel-changed" {
				actual, err := os.ReadFile(target)
				if err != nil || string(actual) != string(content) {
					t.Fatal("cancel recovery rewrote preserved contents")
				}
			}
			if scenario != "shutdown" {
				// The scheduler acknowledges only after Run returns; cancellation
				// was not revived or converted into a successful queue completion.
				if err := queue.AcknowledgeInterrupt(claimed.Job.ID, claimed.LeaseToken); err != nil {
					t.Fatal(err)
				}
				var job models.Job
				if err := cleanup.db.First(&job, "id = ?", claimed.Job.ID).Error; err != nil {
					t.Fatal(err)
				}
				want := models.JobStatusCancelled
				if scenario == "pause" {
					want = models.JobStatusPaused
				}
				if job.Status != want {
					t.Fatalf("job status=%s want=%s", job.Status, want)
				}
			}
		})
	}
}

func TestCleanupRecoveryAfterLeaseRevocationRejectsForeignPermit(t *testing.T) {
	service, _, _, library, root := strmManagementFixture(t)
	var permit CatalogPhysicalWritePermit
	run, artifact, target := createAutoCleanupScenario(t, service, library, root, "full", false, models.MediaArtifactStatusCompleted, &permit)
	plan, err := service.buildCleanupPlan(library.ID, run.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	plan.PhysicalPermit = &permit
	if err := service.db.First(&artifact, artifact.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := service.claimCleanupArtifact(plan, artifact); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := service.db.Transaction(func(tx *gorm.DB) error { return QuiesceCatalogPhysicalWriteTx(tx, permit) }); err != nil {
		t.Fatal(err)
	}
	if err := service.db.Model(&models.Job{}).Where("id = ?", permit.evidence.JobID).Updates(map[string]any{"lease_token_hash": "", "lease_expires_at": time.Now().UTC().Add(-time.Minute)}).Error; err != nil {
		t.Fatal(err)
	}
	foreign := permit
	foreign.evidence.OwnerID = "unrelated-owner"
	for _, invalid := range []CatalogPhysicalWritePermit{{}, foreign} {
		if err := service.ReconcileSupersededCleanup(context.Background(), invalid); err == nil {
			t.Fatal("foreign/empty opaque proof accepted")
		}
	}
	service.removeFile = func(string) error { t.Fatal("revoked lease performed deletion"); return nil }
	service.removeDir = func(string) error { t.Fatal("revoked lease pruned directory"); return nil }
	if err := service.ReconcileSupersededCleanup(context.Background(), permit); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := service.db.Model(&models.CatalogArtifactCleanupClaim{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("valid original proof stranded claims=%d %v", count, err)
	}
}

func TestStoppedArtifactSettlementRechecksCancellationInFinalTransaction(t *testing.T) {
	db, permit, _, _ := catalogArtifactReceiptFixture(t, true)
	var run models.MediaArtifactRun
	if err := db.First(&run, "id = ?", permit.evidence.OwnerID).Error; err != nil {
		t.Fatal(err)
	}
	var policy mediaArtifactPolicy
	if err := json.Unmarshal([]byte(run.PolicyJSON), &policy); err != nil {
		t.Fatal(err)
	}
	service := NewMediaArtifactService(db, nil, nil, zerolog.Nop())
	// An earlier cancellation observation cannot authorize marking a currently
	// running/retried job failed. Only the final writer's current state counts.
	if err := service.settleStoppedArtifactExecution(permit, policy, true); err == nil {
		t.Fatal("non-cancelled job was settled as cancelled")
	}
	if err := db.First(&run, "id = ?", run.ID).Error; err != nil {
		t.Fatal(err)
	}
	if run.ErrorCode == "artifact_cancelled" || run.CleanupStatus == models.MediaArtifactCleanupSkipped {
		t.Fatal("rejected cancellation committed terminal domain state")
	}
	if err := db.Model(&models.Job{}).Where("id = ?", permit.evidence.JobID).Updates(map[string]any{"status": models.JobStatusCancelled, "lease_token_hash": "", "lease_expires_at": nil}).Error; err != nil {
		t.Fatal(err)
	}
	if err := service.settleStoppedArtifactExecution(permit, policy, true); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&run, "id = ?", run.ID).Error; err != nil || run.Status != models.MediaArtifactStatusFailed || run.ErrorCode != "artifact_cancelled" {
		t.Fatalf("run=%+v error=%v", run, err)
	}
}
