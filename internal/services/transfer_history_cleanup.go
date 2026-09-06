package services

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

// cleanupTransferHistoryDependencies removes only OhMyCine's history and
// ownership bookkeeping for a terminal transfer. It never touches provider or
// filesystem content. Reorganization jobs must be terminal so deleting a
// transfer cannot invalidate an active worker's ownership boundary.
func cleanupTransferHistoryDependencies(tx *gorm.DB, transferTaskID string) ([]models.Job, error) {
	var reorganizationTasks []models.MediaReorganizationTask
	if err := tx.Where("transfer_task_id = ?", transferTaskID).Limit(CatalogBatchRows + 1).Find(&reorganizationTasks).Error; err != nil {
		return nil, err
	}
	if len(reorganizationTasks) > CatalogBatchRows {
		return nil, appError(CodeQueueStateConflict, "重整历史较多，需要分批清理后再删除", nil)
	}
	reorganizationJobs := make([]models.Job, 0, len(reorganizationTasks))
	for _, task := range reorganizationTasks {
		var job models.Job
		if err := tx.First(&job, "id = ?", task.JobID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, appError(CodeQueueStateConflict, "重新整理历史不完整，不能删除媒体整理记录", nil)
			}
			return nil, err
		}
		if !isTerminalPipelineJobStatus(job.Status) {
			return nil, appError(CodeQueueStateConflict, "重新整理任务仍在执行，不能删除媒体整理记录", nil)
		}
		if jobHasActiveLease(job, time.Now().UTC()) {
			return nil, appError(CodeQueueStateConflict, "重新整理 worker 仍在收口，不能删除媒体整理记录", nil)
		}
		reorganizationJobs = append(reorganizationJobs, job)
	}
	// New snapshot consumers keep durable recovery evidence. Validate every
	// owner before any deletion, then detach only bounded terminal delta facts.
	if len(reorganizationTasks) > 0 {
		taskIDs, jobIDs := make([]string, 0, len(reorganizationTasks)), make([]string, 0, len(reorganizationTasks))
		for _, task := range reorganizationTasks {
			taskIDs = append(taskIDs, task.ID)
			jobIDs = append(jobIDs, task.JobID)
		}
		var snapshots []string
		var refs []models.CatalogSnapshotReference
		if err := tx.Model(&models.CatalogSnapshot{}).Where("job_id IN ?", jobIDs).Limit(CatalogBatchRows+1).Pluck("id", &snapshots).Error; err != nil {
			return nil, err
		}
		if err := tx.Where("owner_kind=? AND owner_id IN ?", "reorganization", taskIDs).Limit(CatalogBatchRows + 1).Find(&refs).Error; err != nil {
			return nil, err
		}
		if len(snapshots) > CatalogBatchRows || len(refs) > CatalogBatchRows {
			return nil, appError(CodeQueueStateConflict, "重整目录凭据较多，需要后台分批清理", nil)
		}
	}
	for i, task := range reorganizationTasks {
		if err := retireReorganizationHistoryTx(tx, task, reorganizationJobs[i]); err != nil {
			return nil, err
		}
	}
	if err := tx.Where("transfer_task_id = ?", transferTaskID).Delete(&models.MediaReorganizationPreview{}).Error; err != nil {
		return nil, err
	}
	if err := tx.Where("transfer_task_id = ?", transferTaskID).Delete(&models.MediaManagedItem{}).Error; err != nil {
		return nil, err
	}
	if err := tx.Where("transfer_task_id = ?", transferTaskID).Delete(&models.MediaReorganizationTask{}).Error; err != nil {
		return nil, err
	}
	if len(reorganizationJobs) > 0 {
		ids := make([]string, 0, len(reorganizationJobs))
		for _, job := range reorganizationJobs {
			ids = append(ids, job.ID)
		}
		if err := tx.Where("id IN ?", ids).Delete(&models.Job{}).Error; err != nil {
			return nil, err
		}
	}
	return reorganizationJobs, nil
}

func retireReorganizationHistoryTx(tx *gorm.DB, task models.MediaReorganizationTask, job models.Job) error {
	var state reorganizationState
	var plan reorganizationPlan
	if err := json.Unmarshal([]byte(task.StateJSON), &state); err != nil {
		return appError(CodeQueueStateConflict, "重整恢复信息不完整，请先处理任务后再清理历史", nil)
	}
	var refs []models.CatalogSnapshotReference
	if err := tx.Where("owner_kind=? AND owner_id=?", "reorganization", task.ID).Limit(CatalogBatchRows + 1).Find(&refs).Error; err != nil {
		return err
	}
	if len(refs) > CatalogBatchRows {
		return appError(CodeQueueStateConflict, "重整引用较多，需要后台分批清理", nil)
	}
	if state.Catalog != nil {
		if task.Phase != models.MediaReorganizationPhaseCompleted && (state.Catalog.ExecutionProofVersion != 1 || state.Catalog.ExecutionStarted || state.Catalog.Stage == "reconciling" || len(state.Completed) > 0) {
			return appError(CodeQueueStateConflict, "重整已经开始或仍待收口，请先恢复任务后再清理历史", nil)
		}
		if task.Phase == models.MediaReorganizationPhaseCompleted && state.Catalog.Stage != "completed" {
			return appError(CodeQueueStateConflict, "重整完成凭据不完整，请先恢复任务", nil)
		}
		if json.Unmarshal([]byte(task.PlanJSON), &plan) != nil || plan.CatalogFence == nil {
			return appError(CodeQueueStateConflict, "重整目录凭据不完整，请先处理任务", nil)
		}
		if len(refs) > 0 {
			if _, err := PinBoundCatalogTx(tx, state.Catalog.binding(*plan.CatalogFence), "reorganization", task.ID); err != nil {
				return err
			}
			if len(refs) != len(state.Catalog.Layers) {
				return ErrCatalogFence
			}
		}
	} else if len(refs) > 0 {
		return appError(CodeQueueStateConflict, "重整恢复凭据缺失，不能清理历史", nil)
	}
	var snapshots []models.CatalogSnapshot
	if err := tx.Where("job_id=?", job.ID).Limit(CatalogBatchRows + 1).Find(&snapshots).Error; err != nil {
		return err
	}
	if len(snapshots) > CatalogBatchRows {
		return appError(CodeQueueStateConflict, "重整目录快照较多，需要后台分批清理", nil)
	}
	if state.Catalog == nil && len(snapshots) > 0 {
		return appError(CodeQueueStateConflict, "旧重整缺少安全执行凭据，请先恢复或人工处理任务", nil)
	}
	ids := make([]string, 0, len(snapshots))
	for _, snapshot := range snapshots {
		if snapshot.Kind != "delta" || (snapshot.State != "published" && snapshot.State != "abandoned") || snapshot.LibraryID != task.LibraryID {
			return appError(CodeQueueStateConflict, "重整目录任务尚未安全结束，请先恢复或清理任务", nil)
		}
		ids = append(ids, snapshot.ID)
	}
	if len(ids) > 0 {
		var converters int64
		if err := tx.Model(&models.CatalogConversionManifest{}).Where("snapshot_id IN ?", ids).Count(&converters).Error; err != nil {
			return err
		}
		if converters > 0 {
			return appError(CodeQueueStateConflict, "目录转换凭据不能随重整历史删除", nil)
		}
		if err := NewAuditService(tx).Record(tx, &task.OwnerID, "media.reorganization.history.detach", "job", job.ID, "success", map[string]any{"library_id": task.LibraryID, "reorganization_task_id": task.ID, "snapshot_ids": ids}, RequestContext{}); err != nil {
			return err
		}
		if err := tx.Model(&models.CatalogSnapshot{}).Where("id IN ? AND job_id=? AND state IN ?", ids, job.ID, []string{"published", "abandoned"}).Updates(map[string]any{"job_id": nil, "job_lease_hash": ""}).Error; err != nil {
			return err
		}
	}
	if len(refs) > 0 {
		return ReleaseCatalogBindingTx(tx, state.Catalog.binding(*plan.CatalogFence), "reorganization", task.ID)
	}
	return nil
}

func isTerminalPipelineJobStatus(status string) bool {
	return status == models.JobStatusCompleted || status == models.JobStatusFailed || status == models.JobStatusCancelled
}
