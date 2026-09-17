package services

import (
	"context"
	"encoding/json"
	"math"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/database"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

// RecoverStoppedWork is scheduler maintenance, not another Job. Cancellation
// after Run returned (or after restart) has no worker exit callback left to
// reconcile it. Keep the cancelled Job terminal and observe its exact receipts.
func (w *MediaArtifactWorker) RecoverStoppedWork(ctx context.Context, after uint64) (uint64, error) {
	var rows []models.CatalogPhysicalWrite
	err := w.service.db.WithContext(ctx).Model(&models.CatalogPhysicalWrite{}).
		Joins("JOIN jobs j ON j.id=catalog_physical_writes.job_id").
		Where(`catalog_physical_writes.id>? AND owner_kind=? AND state='quiescent' AND runtime_id<>''
			AND ((artifact_receipt_version=1 AND j.status=?) OR (artifact_receipt_version=0 AND j.status IN ?))
			AND j.finished_at IS NOT NULL AND j.lease_token_hash='' AND j.lease_expires_at IS NULL AND j.interrupt_status=''`, after, CatalogPhysicalArtifact, models.JobStatusCancelled, historyTerminalStatuses()).
		Select("catalog_physical_writes.*").Order("catalog_physical_writes.id").Limit(8).Find(&rows).Error
	if err != nil || len(rows) == 0 {
		return 0, err
	}
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return after, err
		}
		err := w.service.recoverCancelledArtifact(ctx, row.ID)
		after = row.ID // A conflict must not starve later libraries.
		if ctx.Err() != nil {
			return after, ctx.Err()
		}
		if row.ArtifactReceiptVersion == 0 && err != nil {
			w.service.recordCancelledArtifactRecovery(ctx, row.JobID, "artifact_legacy_no_io_reconciliation_pending", "旧任务已结束；现有记录无法证明未写入文件，系统已保留恢复证据")
		} else if row.ArtifactReceiptVersion == 0 {
			w.service.recordCancelledArtifactRecovery(ctx, row.JobID, "artifact_legacy_no_io_reconciled", "旧任务已结束；已确认没有写入文件并清理未执行占位")
			w.service.queue.wake()
		} else if err != nil {
			w.service.recordCancelledArtifactRecovery(ctx, row.JobID, "artifact_cancelled_reconciliation_pending", "已取消；文件结果仍需核验。请检查存储连接及文件是否被外部修改，系统会自动再次核验")
		} else {
			w.service.recordCancelledArtifactRecovery(ctx, row.JobID, "artifact_cancelled_reconciled", "已取消；文件操作恢复记录已处理，未继续整理或删除。可选云端清理的未确认结果保留在原记录中")
			w.service.queue.wake()
		}
	}
	return after, nil
}

func (s *MediaArtifactService) recoverCancelledArtifact(ctx context.Context, id uint64) error {
	var version uint
	if err := s.db.WithContext(ctx).Model(&models.CatalogPhysicalWrite{}).Select("artifact_receipt_version").Where("id=?", id).Scan(&version).Error; err != nil {
		return err
	}
	if version == 0 {
		return s.recoverLegacyCancelledArtifactNoIO(ctx, id)
	}
	if version != 1 {
		return ErrCatalogFence
	}
	var permit CatalogPhysicalWritePermit
	var policy mediaArtifactPolicy
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		runtimeID, err := database.ExclusiveRuntimeID(tx)
		if err != nil || runtimeID == "" {
			return ErrCatalogFence
		}
		var proof models.CatalogPhysicalWrite
		if err := tx.First(&proof, id).Error; err != nil {
			return err
		}
		if proof.State != "quiescent" || proof.RuntimeID == "" || proof.OwnerKind != CatalogPhysicalArtifact || proof.ArtifactReceiptVersion != 1 || proof.Revision >= math.MaxInt64 {
			return ErrCatalogFence
		}
		var job models.Job
		if err := tx.First(&job, "id=?", proof.JobID).Error; err != nil {
			return err
		}
		var payload mediaArtifactJobPayload
		if job.Status != models.JobStatusCancelled || job.LeaseTokenHash != "" || job.LeaseExpiresAt != nil || job.JobType != JobTypeMediaArtifact || json.Unmarshal([]byte(job.PayloadJSON), &payload) != nil || payload.ArtifactRunID != proof.OwnerID {
			return ErrCatalogFence
		}
		// Durable quiescence proves the external stack exited; the OS-held DB
		// runtime proves a previous process cannot return. Rebind only observation
		// authority, staying quiescent (never Enter). Revision fences old callbacks.
		result := tx.Model(&models.CatalogPhysicalWrite{}).Where("id=? AND revision=? AND state='quiescent' AND runtime_id=?", proof.ID, proof.Revision, proof.RuntimeID).
			Updates(map[string]any{"runtime_id": runtimeID, "revision": proof.Revision + 1, "updated_at": time.Now().UTC()})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrCatalogFence
		}
		proof.RuntimeID, proof.Revision = runtimeID, proof.Revision+1
		permit = CatalogPhysicalWritePermit{evidence: proof}
		_, run, err := catalogArtifactPermitTx(tx, permit)
		if err != nil {
			return err
		}
		if json.Unmarshal([]byte(run.PolicyJSON), &policy) != nil || policy.LibraryID != run.LibraryID || policy.Generation != run.Generation {
			return ErrCatalogFence
		}
		return nil
	})
	if err != nil {
		return err
	}
	return s.settleStoppedArtifactExecutionContext(ctx, permit, policy, true)
}

func (s *MediaArtifactService) recoverLegacyCancelledArtifactNoIO(ctx context.Context, id uint64) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		settled, err := database.ReconcileLegacyArtifactNoIOPhysicalWriteTx(tx, id)
		if err != nil {
			return err
		}
		if !settled {
			return ErrCatalogFence
		}
		return nil
	})
}

func (s *MediaArtifactService) recordCancelledArtifactRecovery(parent context.Context, jobID, code, message string) {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	_ = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&models.Job{}).Where("id=? AND status=? AND last_error_code<>?", jobID, models.JobStatusCancelled, code).
			Updates(map[string]any{"last_error_code": code, "last_error_message": message, "updated_at": time.Now().UTC()})
		if result.Error != nil || result.RowsAffected == 0 {
			return result.Error
		}
		return recordJobEvent(tx, jobID, "cancel.reconciliation", models.JobStatusCancelled, models.JobStatusCancelled, nil, message, time.Now().UTC())
	})
}
