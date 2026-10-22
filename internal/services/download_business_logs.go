package services

import (
	"context"
	"fmt"
	"time"

	serverlog "github.com/yuanjing-hash/OhMyCine-Server/internal/logging"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func downloadBusinessFields(task models.DownloadTask) map[string]any {
	fields := map[string]any{"task_id": task.ID, "user_id": task.OwnerID, "resource_name": task.DisplayName, "downloader_name": task.DownloaderName, "source_type": task.ProviderType}
	if task.DownloaderID != nil {
		fields["downloader_id"] = *task.DownloaderID
	}
	if task.TargetLibraryID != nil {
		fields["library_id"], fields["library_name"] = *task.TargetLibraryID, task.TargetLibraryName
	}
	if task.FollowSubscriptionID != "" {
		fields["follow_id"] = task.FollowSubscriptionID
	}
	return fields
}

func (s *DownloadService) observedSubmission(ctx context.Context, task models.DownloadTask, status, outcome string, siteID uint) DownloadTaskSummary {
	if stats := followStats(ctx); stats != nil {
		stats.mu.Lock()
		if outcome == "created" {
			stats.created++
		} else if outcome == "reused" {
			stats.reused++
		}
		stats.mu.Unlock()
	}
	fields := downloadBusinessFields(task)
	fields["related_job_id"], fields["related_task_id"], fields["action"] = task.JobID, task.ID, outcome
	if siteID > 0 {
		fields["site_id"] = siteID
	}
	message := fmt.Sprintf("已创建下载任务：%s；下载器：%s；这仅确认任务已持久保存，尚未确认下载器接受或媒体入库", serverlog.BusinessDisplayName(task.DisplayName), serverlog.BusinessDisplayName(task.DownloaderName))
	eventOutcome := "accepted"
	if outcome == "reused" {
		message = fmt.Sprintf("已复用原下载任务：%s；下载器：%s；没有重复创建或提交到下载器", serverlog.BusinessDisplayName(task.DisplayName), serverlog.BusinessDisplayName(task.DownloaderName))
		eventOutcome = "reused"
	}
	serverlog.RecordBusiness(ctx, s.log, downloadOperation(task.ProviderType, task.SourceOrigin), serverlog.BusinessEvent{Code: "download.task." + outcome, Stage: "enqueue", Outcome: eventOutcome, Message: message, Fields: fields})
	result := downloadTaskSummary(task, status)
	result.submissionOutcome = outcome
	return result
}

func (w *DownloadWorker) recordDownload(ctx context.Context, task models.DownloadTask, code, stage, outcome, message string, additional map[string]any) {
	fields := downloadBusinessFields(task)
	fields["job_id"] = task.JobID
	for key, value := range additional {
		fields[key] = value
	}
	serverlog.RecordBusiness(ctx, w.service.log, downloadOperation(task.ProviderType, task.SourceOrigin), serverlog.BusinessEvent{Code: code, Stage: stage, Outcome: outcome, Message: message, Fields: fields})
}

func (w *DownloadWorker) recordDownloadEnd(ctx context.Context, task models.DownloadTask, started time.Time, result WorkerResult) {
	var persisted models.DownloadTask
	if w.service.db.First(&persisted, "id = ?", task.ID).Error == nil {
		task = persisted
	}
	fields := map[string]any{"duration_ms": time.Since(started).Milliseconds(), "phase": task.Phase}
	outcome, message := "partial", "本次下载执行已停止，当前尚未确认下载及入库全部完成"
	switch {
	case task.Phase == models.DownloadTaskStatusCancelled || ctx.Err() != nil:
		outcome, message = "cancelled", "本次下载执行已停止；未将取消或暂停记为下载完成"
	case result.ErrorCode == "downloader_submission_unconfirmed":
		outcome, message = "uncertain", "下载器尚未确认原提交结果，保留原任务等待核对，不能断言下载未创建或已完成"
	case result.RetryAt != nil:
		outcome, message = "waiting", "本次执行暂时等待恢复，原任务保留；重试时按原任务身份继续核对"
	case result.ErrorCode != "":
		outcome, message = "failed", "本次下载执行失败："+downloadFailureMessage(result.ErrorCode, false)
	case task.Phase == models.DownloadTaskStatusCompleted:
		outcome, message = "finished", "下载阶段已完成；未配置目标媒体库，本任务不确认入库结果"
		if task.TargetLibraryID != nil {
			var transfer models.TransferTask
			if w.service.db.Select("id", "job_id").Where("download_task_id = ?", task.ID).First(&transfer).Error == nil {
				fields["related_task_id"], fields["related_job_id"] = transfer.ID, transfer.JobID
				message = "下载与文件清单复核阶段已完成，媒体整理任务已接受；移动、重命名及入库结果请查看关联整理任务"
			} else {
				outcome, message = "partial", "下载阶段已结束，但当前无法确认关联整理任务，不能视为媒体已入库"
			}
		}
	}
	if result.ErrorCode != "" {
		fields["error_code"] = result.ErrorCode
	}
	w.recordDownload(ctx, task, "download.finish", "finish", outcome, message, fields)
}
