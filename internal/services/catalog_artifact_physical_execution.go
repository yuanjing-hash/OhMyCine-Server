package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

type artifactPhysicalExecution struct {
	ctx    context.Context
	permit CatalogPhysicalWritePermit
	policy mediaArtifactPolicy
}

func artifactPhysicalRecoveryPending(code string) WorkerResult {
	next := time.Now().UTC().Add(time.Minute)
	return WorkerResult{RetryAt: &next, ErrorCode: code, ErrorMessage: "媒体产物文件操作等待原任务恢复确认，保留文件与清单后自动重试"}
}

func (s *MediaArtifactService) recoverSupersededArtifactExecution(ctx context.Context, claim ClaimedJob, run models.MediaArtifactRun, policy mediaArtifactPolicy) WorkerResult {
	var existing models.CatalogPhysicalWrite
	err := s.db.WithContext(ctx).Where("owner_kind = ? AND owner_id = ?", CatalogPhysicalArtifact, run.ID).First(&existing).Error
	if errors.Is(err, gorm.ErrRecordNotFound) && run.Status == models.MediaArtifactStatusCompleted && run.FinishedAt != nil && (run.CleanupStatus == models.MediaArtifactCleanupCompleted || run.CleanupStatus == models.MediaArtifactCleanupSkipped) {
		// Respect actual historical completion without manufacturing coverage for
		// unknown old writes. No new generation or physical operation is started.
		return WorkerResult{}
	}
	if err != nil {
		return artifactPhysicalRecoveryPending("artifact_legacy_reconciliation_required")
	}
	if existing.State == "settled" {
		return WorkerResult{}
	}
	if existing.State == "admitted" {
		err := s.catalogArtifactWriteTx(ctx, func(tx *gorm.DB) error {
			if _, err := s.queue.verifyLease(tx, claim.Job.ID, claim.LeaseToken); err != nil {
				return err
			}
			var current models.CatalogPhysicalWrite
			if err := tx.First(&current, existing.ID).Error; err != nil {
				return err
			}
			if current.State != "admitted" || current.Revision != existing.Revision || current.JobID != claim.Job.ID {
				return ErrCatalogFence
			}
			now := time.Now().UTC()
			return tx.Model(&models.MediaArtifactRun{}).Where("id = ? AND job_id = ? AND policy_json = ?", run.ID, claim.Job.ID, run.PolicyJSON).Updates(map[string]any{"status": models.MediaArtifactStatusSuperseded, "cleanup_status": models.MediaArtifactCleanupSkipped, "cleanup_at": now, "finished_at": now, "updated_at": now}).Error
		})
		if err != nil {
			return artifactPhysicalRecoveryPending("artifact_superseded_reconciliation_pending")
		}
		return WorkerResult{}
	}
	permit, err := enterCatalogPhysicalWrite(ctx, s.db, CatalogPhysicalWriteInput{LibraryID: run.LibraryID, OwnerKind: CatalogPhysicalArtifact, OwnerID: run.ID, Job: &claim, ArtifactReceiptVersion: 1})
	if err != nil {
		return artifactPhysicalRecoveryPending("artifact_superseded_reconciliation_pending")
	}
	defer quiesceCatalogPhysicalWrite(s.db, permit, s.log)
	if err := s.settleSupersededArtifactExecution(permit, policy); err != nil {
		return artifactPhysicalRecoveryPending("artifact_superseded_reconciliation_pending")
	}
	return WorkerResult{}
}

func (s *MediaArtifactService) prepareArtifactPhysicalWrite(root string, before, after models.MediaArtifact, content []byte, execution artifactPhysicalExecution) (models.CatalogArtifactWriteReceipt, error) {
	var receipt models.CatalogArtifactWriteReceipt
	if int64(len(content)) > catalogArtifactReceiptMaxBytes {
		return receipt, ErrCatalogBudget
	}
	observed, err := inspectArtifactReceiptFile(execution.ctx, root, execution.policy.ProjectionRootIdentity, before.RelativePath)
	if err != nil {
		return receipt, err
	}
	// Incremental work must fail closed if bytes changed outside the managed
	// writer. A full scope exists specifically to audit and repair a damaged
	// managed projection, so its receipt records the actually observed bytes as
	// the rollback side while retaining the current manifest metadata.
	if observed.Exists && before.Status == models.MediaArtifactStatusCompleted && before.ContentFingerprint != "" && observed.Fingerprint != before.ContentFingerprint && !artifactFullAudit(execution.policy) {
		return receipt, ErrCatalogFence
	}
	hash := sha256.Sum256(content)
	input := CatalogArtifactWriteInput{BeforeArtifact: before, AfterArtifact: after, RootIdentity: execution.policy.ProjectionRootIdentity, BeforeExists: observed.Exists, BeforeFingerprint: observed.Fingerprint, BeforeSize: observed.Size, AfterFingerprint: hex.EncodeToString(hash[:]), AfterSize: int64(len(content))}
	err = s.catalogArtifactWriteTx(execution.ctx, func(tx *gorm.DB) error {
		var err error
		receipt, err = PrepareCatalogArtifactWriteTx(tx, execution.permit, input)
		return err
	})
	return receipt, err
}

func (s *MediaArtifactService) reconcileArtifactPhysicalWrite(root string, receipt models.CatalogArtifactWriteReceipt, execution artifactPhysicalExecution) error {
	observed, err := inspectArtifactReceiptFile(execution.ctx, root, execution.policy.ProjectionRootIdentity, receipt.RelativePath)
	if err != nil {
		return err
	}
	conflict := false
	err = s.catalogArtifactWriteTx(execution.ctx, func(tx *gorm.DB) error {
		if err := ReconcileCatalogArtifactWriteTx(tx, execution.permit, receipt, CatalogArtifactObservation{RootIdentity: execution.policy.ProjectionRootIdentity, Exists: observed.Exists, Fingerprint: observed.Fingerprint, Size: observed.Size}); err != nil {
			return err
		}
		var phase string
		if err := tx.Model(&models.CatalogArtifactWriteReceipt{}).Select("phase").Where("id = ?", receipt.ID).Scan(&phase).Error; err != nil {
			return err
		}
		conflict = phase == "conflict"
		return nil
	})
	if err != nil {
		return err
	}
	if conflict {
		return ErrCatalogFence
	}
	return nil
}

// Resume only previously recorded exact physical outcomes. This never renders
// an obsolete generation, downloads an old asset, removes a file, or announces
// the obsolete generation as ready. Batches bound both DB reads and writes.
func (s *MediaArtifactService) reconcileArtifactExecution(ctx context.Context, permit CatalogPhysicalWritePermit, policy mediaArtifactPolicy) error {
	var after uint64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var rows []models.CatalogArtifactWriteReceipt
		if err := s.db.WithContext(ctx).Where("physical_write_id = ? AND id > ? AND phase IN ?", permit.evidence.ID, after, []string{"prepared", "conflict"}).Order("id").Limit(CatalogBatchRows).Find(&rows).Error; err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		for _, row := range rows {
			if err := s.reconcileArtifactPhysicalWrite(policy.ProjectionRoot, row, artifactPhysicalExecution{ctx: ctx, permit: permit, policy: policy}); err != nil {
				return err
			}
			after = row.ID
		}
	}
}

func (s *MediaArtifactService) settleSupersededArtifactExecution(permit CatalogPhysicalWritePermit, policy mediaArtifactPolicy) error {
	return s.settleStoppedArtifactExecution(permit, policy, false)
}

// Called only after the synchronous worker stack (including cleanup) returns.
// Pause, shutdown and plain context cancellation are not user cancellation:
// they retain resumable quiescent state. A durable cancelled job, however, will
// not be revived by binding recovery, so its original runtime must reconcile.
func (s *MediaArtifactService) finishArtifactExecutionOnExit(permit CatalogPhysicalWritePermit, policy mediaArtifactPolicy) {
	quiesceCatalogPhysicalWrite(s.db, permit, s.log)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var job models.Job
	if err := s.db.WithContext(ctx).Select("status,interrupt_status,cancellation_asked").First(&job, "id = ?", permit.evidence.JobID).Error; err != nil {
		return
	}
	if !artifactJobCancelled(job) {
		return
	}
	var state string
	if err := s.db.WithContext(ctx).Model(&models.CatalogPhysicalWrite{}).Select("state").Where("id = ? AND revision = ?", permit.evidence.ID, permit.evidence.Revision).Scan(&state).Error; err != nil || state != "quiescent" {
		return
	}
	if err := s.settleStoppedArtifactExecution(permit, policy, true); err != nil {
		s.log.Warn().Uint("library_id", permit.evidence.LibraryID).Str("error_code", "artifact_cancelled_reconciliation_pending").Msg("已取消任务的文件操作确认未完成，保留恢复证据")
	}
}

func artifactJobCancelled(job models.Job) bool {
	return job.Status == models.JobStatusCancelled || (job.CancellationAsked && job.InterruptStatus == models.JobStatusCancelled)
}

func (s *MediaArtifactService) settleStoppedArtifactExecution(permit CatalogPhysicalWritePermit, policy mediaArtifactPolicy, cancelled bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return s.settleStoppedArtifactExecutionContext(ctx, permit, policy, cancelled)
}

func (s *MediaArtifactService) settleStoppedArtifactExecutionContext(ctx context.Context, permit CatalogPhysicalWritePermit, policy mediaArtifactPolicy, cancelled bool) error {
	// All generator calls are synchronous and have returned before this helper.
	if err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error { return QuiesceCatalogPhysicalWriteTx(tx, permit) }); err != nil {
		return err
	}
	if err := s.reconcileArtifactExecution(ctx, permit, policy); err != nil {
		return err
	}
	if recovery, ok := s.cleanup.(mediaArtifactCleanupRecovery); ok {
		if err := recovery.ReconcileSupersededCleanup(ctx, permit); err != nil {
			return err
		}
	}
	var abandoned int64
	err := s.catalogArtifactWriteTx(ctx, func(tx *gorm.DB) error {
		proof, run, err := catalogArtifactPermitTx(tx, permit)
		if err != nil {
			return err
		}
		if cancelled {
			var job models.Job
			if err := tx.First(&job, "id = ?", proof.JobID).Error; err != nil {
				return err
			}
			if !artifactJobCancelled(job) {
				return ErrCatalogFence
			}
		}
		now := time.Now().UTC()
		updates := map[string]any{"status": models.MediaArtifactStatusSuperseded, "cleanup_status": models.MediaArtifactCleanupSkipped, "cleanup_at": now, "finished_at": now, "updated_at": now}
		if cancelled {
			updates["status"], updates["error_code"] = models.MediaArtifactStatusFailed, "artifact_cancelled"
		}
		result := tx.Model(&models.MediaArtifactRun{}).Where("id = ? AND policy_json = ? AND job_id = ?", run.ID, run.PolicyJSON, proof.JobID).Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrCatalogFence
		}
		// Only the original, joined/quiescent owner may abandon its optional
		// cloud-directory intent. Keep the record and never report a recycle
		// success or resume provider mutations on this stopped execution.
		if proof.State != "quiescent" {
			return ErrCatalogFence
		}
		result = tx.Model(&models.CatalogCloudCleanupClaim{}).Where("physical_write_id=? AND run_id=? AND library_id=? AND source_fingerprint=? AND status='prepared'", proof.ID, run.ID, proof.LibraryID, proof.SourceFingerprint).Updates(map[string]any{"status": "abandoned", "updated_at": now})
		if result.Error != nil {
			return result.Error
		}
		abandoned = result.RowsAffected
		return SettleSupersededCatalogArtifactTx(tx, permit)
	})
	if err == nil && abandoned > 0 {
		s.log.Info().Uint("library_id", permit.evidence.LibraryID).Str("task_id", permit.evidence.OwnerID).Int64("abandoned", abandoned).Msg("【云端空目录】任务已停止，未确认的回收结果保留在记录中，不再执行云端操作")
	}
	return err
}
