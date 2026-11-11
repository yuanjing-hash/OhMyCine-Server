package services

import (
	"context"
	"encoding/json"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"gorm.io/gorm"
)

// Keep list scope, counters and bounded recovery on the same positive evidence.
// The aliases match transferReadScope. No terminal result is inferred from bytes.
const transferUploadRecoverySQL = `(
	jobs.status='completed' AND jobs.job_type='transfer'
	AND jobs.lease_token_hash='' AND jobs.lease_expires_at IS NULL
	AND jobs.interrupt_status='' AND jobs.cancellation_asked=0
	AND jobs.history_cleared_at IS NULL AND transfer.history_cleared_at IS NULL
	AND download.history_cleared_at IS NULL AND transfer.finished_at IS NULL
	AND transfer.phase NOT IN ('completed','failed') AND transfer.catalog_published_at IS NULL
	AND transfer.execution_location='node' AND download.execution_location='node'
	AND transfer.node_id=download.node_id AND transfer.owner_id=download.owner_id
	AND jobs.owner_id=transfer.owner_id AND download.phase<>'cancelled'
	AND transfer.library_id=download.target_library_id
	AND transfer.route_version=1 AND transfer.route_version=download.transfer_route_version
	AND json_valid(download.source_data_source_json) AND download.source_data_source_json<>'{}'
	AND json_valid(download.target_data_source_json) AND download.target_data_source_json<>'{}'
	AND transfer.source_data_source_json=download.source_data_source_json
	AND transfer.target_data_source_json=download.target_data_source_json
	AND CASE WHEN json_valid(jobs.payload_json) THEN json_extract(jobs.payload_json,'$.transfer_task_id') END=transfer.id
	AND EXISTS(SELECT 1 FROM remote_upload_files f WHERE f.transfer_task_id=transfer.id)
	AND EXISTS(SELECT 1 FROM remote_operations o WHERE o.task_id=download.id AND o.node_id=transfer.node_id AND o.status IN ('pending','running','paused','completed'))
)`

func transferUploadRecoveryPendingTx(db *gorm.DB, transferID string) (bool, error) {
	var count int64
	err := db.Table("transfer_tasks AS transfer").Joins("JOIN download_tasks AS download ON download.id=transfer.download_task_id").Joins("JOIN jobs ON jobs.id=transfer.job_id").Where("transfer.id=? AND "+transferUploadRecoverySQL, transferID).Count(&count).Error
	return count == 1, err
}

// RecoverStoppedWork repairs a completed queue ACK with an unfinished original
// upload. Domain and Job snapshots are rechecked in the same CAS transaction.
func (w *TransferWorker) RecoverStoppedWork(ctx context.Context, after uint64) (uint64, error) {
	if w.service == nil || w.service.queue == nil {
		return 0, nil
	}
	type candidate struct {
		RowID uint64
		ID    string
	}
	var candidates []candidate
	err := w.service.db.WithContext(ctx).Table("transfer_tasks AS transfer").Select("transfer.rowid AS row_id,transfer.id").
		Joins("JOIN download_tasks AS download ON download.id=transfer.download_task_id").
		Joins("JOIN jobs ON jobs.id=transfer.job_id").
		Where("transfer.rowid>? AND ("+transferUploadRecoverySQL+" OR (transfer.execution_location='node' AND jobs.lease_token_hash='' AND jobs.lease_expires_at IS NULL AND jobs.interrupt_status='' AND jobs.status IN ('completed','cancelled','paused','failed') AND EXISTS(SELECT 1 FROM catalog_physical_writes p WHERE p.owner_kind='transfer' AND p.owner_id=transfer.id AND p.job_id=jobs.id AND p.state='entered')))", after).Order("transfer.rowid").Limit(20).Scan(&candidates).Error
	if err != nil || len(candidates) == 0 {
		return 0, err
	}
	for _, candidate := range candidates {
		// A stopped original owner may still have an asynchronous Node writer.
		// Joining it never resumes the Job or starts intake, including after restart.
		var stopped models.TransferTask
		var stoppedDownload models.DownloadTask
		if err := w.service.db.WithContext(ctx).First(&stopped, "id=?", candidate.ID).Error; err != nil {
			return after, err
		}
		if err := w.service.db.WithContext(ctx).First(&stoppedDownload, "id=?", stopped.DownloadTaskID).Error; err != nil {
			return after, err
		}
		var entered int64
		if err := w.service.db.WithContext(ctx).Model(&models.CatalogPhysicalWrite{}).Where("owner_kind=? AND owner_id=? AND state='entered'", CatalogPhysicalTransfer, stopped.ID).Count(&entered).Error; err != nil {
			return after, err
		}
		var stopErr error
		if entered > 0 {
			stopCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			stopErr = w.interruptRemoteUpload(stopCtx, stopped, stoppedDownload, nil)
			cancel()
		}
		if stopErr != nil {
			w.transferEvent(ctx, stopped, "node.upload.stop_pending", "upload", "waiting", "原节点执行尚未确认退出，保留原文件及恢复记录", map[string]any{"error_code": "node_execution_stop_pending"})
		}
		err := w.service.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			var task models.TransferTask
			var job models.Job
			var download models.DownloadTask
			if err := tx.First(&task, "id=?", candidate.ID).Error; err != nil {
				return err
			}
			if err := tx.First(&job, "id=?", task.JobID).Error; err != nil {
				return err
			}
			if err := tx.First(&download, "id=?", task.DownloadTaskID).Error; err != nil {
				return err
			}
			var eligible int64
			if err := tx.Table("transfer_tasks AS transfer").Joins("JOIN download_tasks AS download ON download.id=transfer.download_task_id").Joins("JOIN jobs ON jobs.id=transfer.job_id").Where("transfer.id=? AND "+transferUploadRecoverySQL, task.ID).Count(&eligible).Error; err != nil || eligible != 1 {
				return err
			}
			if err := validateTransferRouteSnapshot(download); err != nil {
				return nil
			}
			var rows []models.RemoteUploadFile
			var storage models.Storage
			var manifest downloadpkg.Manifest
			if tx.Where("transfer_task_id=?", task.ID).Order("ordinal").Find(&rows).Error != nil || len(rows) == 0 || tx.First(&storage, download.TargetStorageID).Error != nil || json.Unmarshal([]byte(task.ManifestJSON), &manifest) != nil {
				return nil
			}
			batches, err := buildRemoteUploadBatches(rows, storage, download, manifest)
			if err != nil {
				return nil
			}
			for _, batch := range batches {
				if _, err := remoteUploadBatchRecordTx(tx, task, download, batch); err != nil {
					return nil
				}
			}
			owner, err := NewAuthorizationService(tx).Resolve(task.OwnerID)
			if err != nil || authorizeJobContinuationTx(tx, owner, job) != nil {
				return nil
			}
			var payload transferJobPayload
			if json.Unmarshal([]byte(job.PayloadJSON), &payload) != nil || payload.TransferTaskID != task.ID {
				return nil
			}
			now := time.Now().UTC()
			updated := tx.Model(&models.Job{}).Where("id=? AND revision=? AND generation=? AND owner_id=? AND status=? AND interrupt_status='' AND cancellation_asked=0 AND history_cleared_at IS NULL", job.ID, job.Revision, job.Generation, job.OwnerID, models.JobStatusCompleted).
				Where("EXISTS(SELECT 1 FROM transfer_tasks AS transfer JOIN download_tasks AS download ON download.id=transfer.download_task_id WHERE transfer.id=? AND transfer.updated_at=? AND download.updated_at=? AND "+transferUploadRecoverySQL+")", task.ID, task.UpdatedAt, download.UpdatedAt).
				Updates(map[string]any{"status": models.JobStatusQueued, "revision": job.Revision + 1, "finished_at": nil, "next_attempt_at": nil, "failure_retry_count": 0, "last_error_code": "transfer_reconciliation_pending", "last_error_message": "原上传尚未完成，将继续核对同一操作", "updated_at": now})
			if updated.Error != nil || updated.RowsAffected != 1 {
				return updated.Error
			}
			return recordJobEvent(tx, job.ID, "original_upload_recovery", job.Status, models.JobStatusQueued, nil, "transfer_reconciliation_pending", now)
		})
		if err != nil {
			return after, err
		}
		after = candidate.RowID
	}
	return after, nil
}
