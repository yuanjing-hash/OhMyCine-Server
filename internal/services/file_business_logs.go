package services

import (
	"context"
	"fmt"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/rs/zerolog"
	serverlog "github.com/yuanjing-hash/OhMyCine-Server/internal/logging"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
)

// File displays are derived from a validated plan/root, never by stripping a
// provider's private path. Provider identities are only lookup keys in memory.
type fileBusinessDisplay struct {
	source, target   string
	oldName, newName string
	size             int64
}
type fileBusinessDisplaysKey struct{}
type stagingCleanupObserverKey struct{}
type stagingCleanupObserver func(downloadpkg.File, string, string)

func observeStagingCleanup(ctx context.Context, file downloadpkg.File, outcome, message string) {
	if observer, ok := ctx.Value(stagingCleanupObserverKey{}).(stagingCleanupObserver); ok {
		observer(file, outcome, message)
	}
}

func transferBusinessFields(task models.TransferTask, download models.DownloadTask) map[string]any {
	fields := map[string]any{"job_id": task.JobID, "task_id": task.ID, "user_id": task.OwnerID, "library_id": task.LibraryID, "library_name": task.LibraryName, "target_name": task.LibraryName, "source_name": firstNonEmpty(download.DownloaderName, "下载暂存区"), "transfer_mode": download.TransferMode, "resource_name": download.DisplayName}
	if download.DownloaderID != nil {
		fields["downloader_id"] = *download.DownloaderID
	}
	if download.StagingStorageID != nil {
		fields["source_storage_id"] = *download.StagingStorageID
	}
	if download.TargetStorageID != nil {
		fields["target_storage_id"] = *download.TargetStorageID
	}
	if download.TargetConnectionID != nil {
		fields["target_connection_id"] = *download.TargetConnectionID
	}
	return fields
}

func (w *TransferWorker) transferEvent(ctx context.Context, task models.TransferTask, code, stage, outcome, message string, fields map[string]any) {
	if w == nil || w.service == nil {
		return
	}
	base := map[string]any{"job_id": task.JobID, "task_id": task.ID, "user_id": task.OwnerID, "library_id": task.LibraryID, "library_name": task.LibraryName}
	for key, value := range fields {
		base[key] = value
	}
	operation := serverlog.OperationMediaTransfer
	if strings.HasPrefix(code, "cloud.") || strings.HasPrefix(code, "upload.") {
		operation = serverlog.OperationPan115CloudTransfer
	}
	serverlog.RecordBusiness(ctx, w.service.log, operation, serverlog.BusinessEvent{Code: code, Stage: stage, Outcome: outcome, Message: message, Fields: base})
}

func validatedLocalDisplayRelative(root, file string) string {
	if !filepath.IsAbs(root) || ensureWithin(root, file) != nil {
		return ""
	}
	relative, err := filepath.Rel(root, file)
	if err != nil {
		return ""
	}
	return serverlog.BusinessRelativePath(filepath.ToSlash(relative))
}

func localFileDisplay(download models.DownloadTask, targetRoot string, item transferPlanItem) fileBusinessDisplay {
	return fileBusinessDisplay{source: validatedLocalDisplayRelative(download.StagingAbsolutePath, item.Source), target: validatedLocalDisplayRelative(targetRoot, item.Destination), oldName: filepath.Base(item.Source), newName: filepath.Base(item.Destination), size: item.Size}
}

func (display fileBusinessDisplay) fields() map[string]any {
	fields := map[string]any{"file_name": display.oldName, "old_name": display.oldName, "new_name": display.newName, "size_bytes": display.size}
	if display.source != "" {
		fields["source_relative_path"] = display.source
	}
	if display.target != "" {
		fields["target_relative_path"] = display.target
	}
	return fields
}

func (display fileBusinessDisplay) description() string {
	source, target := firstNonEmpty(display.source, serverlog.BusinessDisplayName(display.oldName)), firstNonEmpty(display.target, serverlog.BusinessDisplayName(display.newName))
	return fmt.Sprintf("%s → %s（%d 字节）", source, target, display.size)
}

func transferModeMessage(mode string) string {
	switch mode {
	case models.MediaLibraryTransferMove:
		return "移动"
	case models.MediaLibraryTransferCopy:
		return "复制"
	case models.MediaLibraryTransferSymlink:
		return "创建软链接"
	default:
		return "整理"
	}
}

// Targets already passed package selection and target-root validation. The
// actual mutation callers still prove provider ancestry before emitting facts.
func withCloudFileDisplays(ctx context.Context, targets []transferTargetItem) context.Context {
	displays := map[string]fileBusinessDisplay{}
	for _, target := range targets {
		source := serverlog.BusinessRelativePath(target.File.RelativePath)
		destination := serverlog.BusinessRelativePath(target.Relative)
		if destination == "" {
			continue
		}
		display := fileBusinessDisplay{source: source, target: destination, oldName: path.Base(strings.ReplaceAll(target.File.RelativePath, "\\", "/")), newName: path.Base(target.Relative), size: target.File.Size}
		if target.File.ProviderItemID != "" {
			displays[target.File.ProviderItemID] = display
		}
		displays[normalizedManifestPath(target.File.RelativePath)] = display
	}
	return context.WithValue(ctx, fileBusinessDisplaysKey{}, displays)
}

func cloudFileDisplay(ctx context.Context, key string) (fileBusinessDisplay, bool) {
	displays, _ := ctx.Value(fileBusinessDisplaysKey{}).(map[string]fileBusinessDisplay)
	display, ok := displays[key]
	return display, ok
}

func withFileDisplayNames(ctx context.Context, source, target string) context.Context {
	return serverlog.WithBusinessFields(ctx, map[string]any{"source_name": source, "target_name": target})
}

func relativeFileDisplay(source, target string, size int64) fileBusinessDisplay {
	return fileBusinessDisplay{source: serverlog.BusinessRelativePath(source), target: serverlog.BusinessRelativePath(target), oldName: path.Base(source), newName: path.Base(target), size: size}
}

func (w *TransferWorker) cloudFileEvent(ctx context.Context, task models.TransferTask, key, code, stage, outcome, message string) bool {
	display, ok := cloudFileDisplay(ctx, key)
	if !ok {
		return false
	}
	w.transferEvent(ctx, task, code, stage, outcome, message+"："+display.description(), display.fields())
	return true
}

func (w *TransferWorker) recordUnchangedFileSummary(ctx context.Context, task models.TransferTask, code string, total, reused, skipped, sampled int) {
	if reused+skipped == 0 {
		return
	}
	omitted := reused + skipped - sampled
	w.transferEvent(ctx, task, code, "reconciliation", "confirmed", fmt.Sprintf("本轮 %d 项文件结果中，复用已确认目标 %d 项、按策略跳过 %d 项；这些项目没有新移动、复制或上传。默认展示 %d 条未改写样本，另 %d 条仅计入汇总", total, reused, skipped, sampled, omitted), map[string]any{"total_count": total, "reused_count": reused, "skipped_count": skipped, "sample_count": sampled, "omitted_count": omitted})
}

func (w *TransferWorker) recordTransferEnd(ctx context.Context, task models.TransferTask, download models.DownloadTask, started time.Time, result WorkerResult) {
	fields := transferBusinessFields(task, download)
	fields["duration_ms"] = time.Since(started).Milliseconds()
	outcome, message := "uncertain", "本次整理执行已退出，尚未确认所有文件及后续目录发布结果"
	var saved models.TransferTask
	if w.service.db.Select("phase", "processed_files", "total_files", "cleanup_status").First(&saved, "id = ?", task.ID).Error == nil {
		fields["processed_count"], fields["total_count"], fields["phase"] = saved.ProcessedFiles, saved.TotalFiles, saved.Phase
		if saved.Phase == models.TransferTaskStatusCompleted {
			outcome, message = "finished", "文件整理结果已保存；后续目录、元数据产物及媒体服务器刷新分别以各阶段记录为准"
		}
		if saved.CleanupStatus == models.TransferCleanupDeferred {
			message += "；下载源仍为做种/软链接保留，暂存清理已延后"
		}
	}
	switch {
	case ctx.Err() != nil || download.Phase == models.DownloadTaskStatusCancelled:
		outcome, message = "cancelled", "本次整理已停止，未执行的文件操作不再继续；已完成操作保留"
	case result.Wait != nil:
		outcome, message = "waiting", "本次整理等待用户处理同名冲突，未将等待状态记为已完成入库"
	case result.RetryAt != nil:
		outcome, message = "retry", "本次整理尚有阶段待恢复："+firstNonEmpty(result.ErrorMessage, "请查看具体文件与阶段记录")+"；将按原任务继续核对，不重复已确认的文件操作"
	case result.ErrorCode != "":
		outcome, message = "failed", "本次整理未全部完成："+firstNonEmpty(result.ErrorMessage, "请查看具体文件与阶段记录")
	}
	if result.ErrorCode != "" {
		fields["error_code"] = result.ErrorCode
	}
	w.transferEvent(ctx, task, "transfer.finish", "finish", outcome, message, fields)
}

func recordReorganizationFile(ctx context.Context, logger zerolog.Logger, item reorganizationPlanItem, outcome, message string) {
	display := fileBusinessDisplay{source: serverlog.BusinessRelativePath(item.OldRelativePath), target: serverlog.BusinessRelativePath(item.NewRelativePath), oldName: path.Base(item.OldRelativePath), newName: path.Base(item.NewRelativePath), size: item.Size}
	serverlog.RecordBusiness(ctx, logger, serverlog.OperationMediaReorganization, serverlog.BusinessEvent{Code: "reorganization.file", Stage: "organize", Outcome: outcome, Message: message + "：" + display.description(), Fields: display.fields()})
}

func (w *MediaReorganizationWorker) reorganizationEvent(ctx context.Context, task models.MediaReorganizationTask, code, stage, outcome, message string, fields map[string]any) {
	base := map[string]any{"job_id": task.JobID, "task_id": task.ID, "library_id": task.LibraryID, "user_id": task.OwnerID}
	for key, value := range fields {
		base[key] = value
	}
	serverlog.RecordBusiness(ctx, w.service.log, serverlog.OperationMediaReorganization, serverlog.BusinessEvent{Code: code, Stage: stage, Outcome: outcome, Message: message, Fields: base})
}

func (w *MediaReorganizationWorker) recordReorganizationEnd(ctx context.Context, task models.MediaReorganizationTask, started time.Time, result WorkerResult) {
	fields := map[string]any{"duration_ms": time.Since(started).Milliseconds()}
	outcome, message := "uncertain", "重新整理执行已退出，尚未确认文件调整及目录结果"
	var saved models.MediaReorganizationTask
	if w.service.db.Select("phase", "processed_items", "total_items").First(&saved, "id = ?", task.ID).Error == nil {
		fields["processed_count"], fields["total_count"], fields["phase"] = saved.ProcessedItems, saved.TotalItems, saved.Phase
		if saved.Phase == models.MediaReorganizationPhaseCompleted {
			outcome, message = "finished", "本次文件整理与目录记录已按原流程收口；逐文件物理确认、云端受理及后续元数据刷新以各阶段记录为准"
		}
	}
	if ctx.Err() != nil {
		outcome, message = "cancelled", "重新整理已停止，已有文件调整结果保留"
	} else if result.ErrorCode != "" {
		outcome, message = "failed", "本次重新整理未全部完成："+firstNonEmpty(result.ErrorMessage, "请查看具体文件阶段")
		fields["error_code"] = result.ErrorCode
	}
	w.reorganizationEvent(ctx, task, "reorganization.finish", "finish", outcome, message, fields)
}

func (s *TransferService) deletionFileEvent(ctx context.Context, relative string, size int64, outcome, message string) {
	display := relativeFileDisplay(relative, "", size)
	fields := map[string]any{"file_name": display.oldName, "relative_path": display.source, "size_bytes": size}
	serverlog.RecordBusiness(ctx, s.log, serverlog.OperationDownloadStagingCleanup, serverlog.BusinessEvent{Code: "deletion.file", Stage: "cleanup", Outcome: outcome, Message: message + "：" + firstNonEmpty(display.source, serverlog.BusinessDisplayName(display.oldName)), Fields: fields})
}
