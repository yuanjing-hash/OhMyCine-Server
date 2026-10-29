package services

import (
	"context"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"time"
)

func (w *SeedingWorker) runSelectionGroupSeeding(ctx context.Context, task models.SeedingTask, download models.DownloadTask) WorkerResult {
	var group models.BTSelectionGroup
	if err := w.service.db.First(&group, "id = ?", download.BTSelectionGroupID).Error; err != nil {
		return w.retry(task, "shared_source_unavailable", "共享源状态暂不可用", ctx)
	}
	if group.CleanupState == "completed" {
		if err := w.service.finish(task, nil, "shared_source_completed", RequestContext{}); err != nil {
			return w.retry(task, "seeding_state_persist_failed", "共享源策略结果保存失败", ctx)
		}
		return WorkerResult{}
	}
	client, err := w.service.downloaders.clientForDownloadTask(ctx, download)
	if err != nil {
		return w.retry(task, "shared_source_unavailable", "共享源状态暂不可用", ctx)
	}
	if group.CleanupState == "" {
		provider, err := client.Get(ctx, task.ProviderTaskID)
		if err != nil {
			code, _ := downloadpkg.ErrorInfo(err)
			return w.retry(task, code, "正在核对共享源的原任务", ctx)
		}
		now := time.Now().UTC()
		task.Ratio, task.SeededSeconds, task.UploadedBytes = provider.Ratio, provider.SeededSeconds, provider.UploadedBytes
		phase := models.SeedingTaskStatusSeeding
		if !task.CleanupEnabled && task.TransferMode != models.MediaLibraryTransferMove {
			phase = models.SeedingTaskStatusRetained
		}
		if err = w.service.db.Model(&models.SeedingTask{}).Where("id = ?", task.ID).Updates(map[string]any{"phase": phase, "ratio": provider.Ratio, "seeded_seconds": provider.SeededSeconds, "uploaded_bytes": provider.UploadedBytes, "last_sampled_at": now, "updated_at": now}).Error; err != nil {
			return w.retry(task, "seeding_state_persist_failed", "共享源采样保存失败", ctx)
		}
		ready := task.TransferMode == models.MediaLibraryTransferMove && !task.CleanupEnabled || task.CleanupEnabled && seedingThresholdReached(task)
		if !ready {
			next := now.Add(seedingSampleInterval)
			return WorkerResult{RetryAt: &next}
		}
		var transfer models.TransferTask
		if err = w.service.db.First(&transfer, "download_task_id = ?", download.ID).Error; err != nil || transfer.Phase != models.TransferTaskStatusCompleted || transfer.CatalogPublishedAt == nil {
			return w.retry(task, "shared_source_consumer_pending", "等待本任务完成整理与目录发布，继续保留共享源", ctx)
		}
	}
	cleaner, ok := client.(interface {
		retireAndCleanup(context.Context) (bool, error)
	})
	if !ok {
		return w.retry(task, "shared_source_cleanup_unavailable", "共享源清理能力暂不可用", ctx)
	}
	done, err := cleaner.retireAndCleanup(ctx)
	if err != nil {
		code, _ := downloadpkg.ErrorInfo(err)
		if code == "" {
			code = "shared_source_cleanup_pending"
		}
		return w.retry(task, code, "共享源清理结果尚未确认，将核对原操作", ctx)
	}
	if !done {
		return w.retry(task, "shared_source_retained", "其他引用仍在下载、整理或做种，继续保留共享源", ctx)
	}
	if err = w.service.finish(task, nil, "shared_source_completed", RequestContext{}); err != nil {
		return w.retry(task, "seeding_state_persist_failed", "共享源策略结果保存失败", ctx)
	}
	return WorkerResult{}
}
