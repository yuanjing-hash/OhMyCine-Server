package services

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	serverlog "github.com/yuanjing-hash/OhMyCine-Server/internal/logging"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/medialibrary"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	cloudpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"gorm.io/gorm"
)

func (w *TransferWorker) finishCompletedTransfer(ctx context.Context, task models.TransferTask) WorkerResult {
	var download models.DownloadTask
	if err := w.service.db.First(&download, "id = ?", task.DownloadTaskID).Error; err != nil {
		return WorkerResult{ErrorCode: "transfer_download_missing", ErrorMessage: "原下载任务不存在"}
	}
	if err := w.recoverCompletedRemoteUploadOperations(ctx, &task, download); err != nil {
		next := time.Now().UTC().Add(time.Minute)
		return WorkerResult{RetryAt: &next, ErrorCode: "transfer_batch_publish_failed", ErrorMessage: "正在核对原传输节点的完成回执，将仅重试本批次"}
	}
	// The persisted successful targets are the only authorized catalog scope.
	// Failure retries this batch; it must never wake an unscoped library scan.
	if w.service.libraryReconciler != nil {
		w.transferEvent(ctx, task, "transfer.catalog.start", "publishing", "started", "文件整理结果已保存，开始按本批成功目标更新目录；不会触发整个媒体库扫描", nil)
		if err := w.service.libraryReconciler.ReconcileTransferBatch(ctx, task.ID); err != nil {
			w.transferEvent(ctx, task, "transfer.catalog.wait", "publishing", "retry", "本批目录发布未完成，保留已经整理的文件，仅按原批次重试目录更新", map[string]any{"error_code": "transfer_batch_publish_failed"})
			next := time.Now().UTC().Add(time.Minute)
			return WorkerResult{RetryAt: &next, ErrorCode: "transfer_batch_publish_failed", ErrorMessage: "入库文件目录更新失败，将仅重试本批次"}
		}
		w.transferEvent(ctx, task, "transfer.catalog.finish", "publishing", "confirmed", "本批成功目标的目录更新调用已完成；NFO、图片、STRM和下游媒体刷新仍分别记录", nil)
	}
	if err := w.service.db.Transaction(func(tx *gorm.DB) error {
		if err := ensureDownloadPipelineActive(tx, task.DownloadTaskID); err != nil {
			return err
		}
		return tx.Model(&models.FollowEpisodeClaim{}).Where("download_task_id = ?", task.DownloadTaskID).Updates(map[string]any{"state": "imported", "updated_at": time.Now().UTC()}).Error
	}); errors.Is(err, context.Canceled) {
		return WorkerResult{}
	} else if err != nil {
		next := time.Now().UTC().Add(time.Minute)
		return WorkerResult{RetryAt: &next, ErrorCode: "follow_claim_sync_failed", ErrorMessage: "入库已完成，订阅状态同步将自动重试"}
	}
	if err := ensureDownloadPipelineActive(w.service.db, task.DownloadTaskID); errors.Is(err, context.Canceled) {
		return WorkerResult{}
	} else if err != nil {
		return WorkerResult{ErrorCode: "transfer_download_missing", ErrorMessage: "原下载任务不存在"}
	}
	if download.ProviderType != models.DownloaderTypePan115Offline && download.ProviderType != models.DownloaderTypeQuarkShare && w.service.seeding != nil {
		if err := w.service.seeding.AfterTransfer(ctx, download); err != nil {
			next := time.Now().UTC().Add(time.Minute)
			return WorkerResult{RetryAt: &next, ErrorCode: "post_transfer_provider_failed", ErrorMessage: "下载器收尾失败，将自动重试"}
		}
	}
	if download.ProviderType == models.DownloaderTypeQuarkShare {
		if err := cleanupCrossSourceManagedRoot(task, download); err != nil {
			next := time.Now().UTC().Add(time.Minute)
			return WorkerResult{RetryAt: &next, ErrorCode: "cross_source_staging_cleanup_failed", ErrorMessage: "入库已完成，跨数据源暂存清理将自动重试"}
		}
		// Quark recycle is not verified. Keep the source and empty package; never
		// reinterpret provider paths as local staging or falsely claim cleanup.
		if err := w.service.db.Model(&models.TransferTask{}).Where("id = ?", task.ID).Updates(map[string]any{"cleanup_status": models.TransferCleanupSkipped, "cleanup_error_code": "quark_recycle_unavailable", "updated_at": time.Now().UTC()}).Error; err != nil {
			next := time.Now().UTC().Add(time.Minute)
			return WorkerResult{RetryAt: &next, ErrorCode: "transfer_state_persist_failed", ErrorMessage: "入库已完成，源文件保留状态将自动重试"}
		}
		if task.CleanupStatus != models.TransferCleanupSkipped {
			serverlog.OperationDownloadStagingCleanup.Event(w.service.log.Info()).Str("task_id", task.ID).Str("source_cleanup", "retained").Msg(serverlog.OperationDownloadStagingCleanup.Message("夸克入库已完成；回收能力尚未开放，保留云端接收目录和剩余文件"))
		}
		return WorkerResult{}
	}
	if download.ProviderType == models.DownloaderTypePan115Offline && normalizeExecutionLocation(download.ExecutionLocation) == models.NodeLocationRemote {
		client, err := w.service.downloader.clientForDownloadTask(ctx, download)
		if err != nil {
			next := time.Now().UTC().Add(time.Minute)
			return WorkerResult{RetryAt: &next, ErrorCode: "node_source_cleanup_failed", ErrorMessage: "入库已完成，传输节点临时文件清理将自动重试"}
		}
		cleaner, ok := client.(downloadpkg.ManagedSourceCleaner)
		if !ok || cleaner.CleanupManagedSource(ctx) != nil {
			next := time.Now().UTC().Add(time.Minute)
			return WorkerResult{RetryAt: &next, ErrorCode: "node_source_cleanup_failed", ErrorMessage: "入库已完成，传输节点临时文件清理将自动重试"}
		}
	}
	if download.ProviderType == models.DownloaderTypeQBittorrent && normalizeExecutionLocation(download.ExecutionLocation) == models.NodeLocationRemote {
		if err := cleanupCrossSourceManagedRoot(task, download); err != nil {
			next := time.Now().UTC().Add(time.Minute)
			return WorkerResult{RetryAt: &next, ErrorCode: "node_staging_cleanup_failed", ErrorMessage: "入库已完成，主 Server 接收暂存清理将自动重试"}
		}
		if err := w.service.db.Where("transfer_task_id = ?", task.ID).Delete(&models.RemoteTransferFile{}).Error; err != nil {
			next := time.Now().UTC().Add(time.Minute)
			return WorkerResult{RetryAt: &next, ErrorCode: "node_staging_checkpoint_cleanup_failed", ErrorMessage: "入库已完成，断点记录清理将自动重试"}
		}
		_ = w.service.db.Model(&models.TransferTask{}).Where("id = ?", task.ID).Updates(map[string]any{"cleanup_status": models.TransferCleanupCompleted, "cleanup_error_code": "", "updated_at": time.Now().UTC()}).Error
		return WorkerResult{}
	}
	if download.ProviderType == models.DownloaderTypePluginHTTP {
		if download.TransferMode == models.MediaLibraryTransferSymlink {
			_ = w.service.db.Model(&models.TransferTask{}).Where("id = ?", task.ID).Updates(map[string]any{"cleanup_status": models.TransferCleanupSkipped, "cleanup_error_code": "", "updated_at": time.Now().UTC()}).Error
			return WorkerResult{}
		}
		removed, err := cleanupPluginDownloadOutput(download)
		if err != nil {
			next := time.Now().UTC().Add(time.Minute)
			return WorkerResult{RetryAt: &next, ErrorCode: "download_staging_cleanup_failed", ErrorMessage: "入库已完成，站点下载暂存清理将自动重试"}
		}
		_ = w.service.db.Model(&models.TransferTask{}).Where("id = ?", task.ID).Updates(map[string]any{"cleanup_status": models.TransferCleanupCompleted, "cleanup_removed": removed, "cleanup_error_code": "", "updated_at": time.Now().UTC()}).Error
		serverlog.OperationDownloadStagingCleanup.Event(w.service.log.Info()).Str("task_id", task.ID).Int("removed", removed).Str("source_cleanup", "plugin_managed_output").Msg(serverlog.OperationDownloadStagingCleanup.Message("已清理站点下载暂存产物"))
		return WorkerResult{}
	}
	if download.ProviderType == models.DownloaderTypeQBittorrent && (download.TransferMode == models.MediaLibraryTransferCopy || download.TransferMode == models.MediaLibraryTransferSymlink) {
		if task.CleanupStatus != models.TransferCleanupCompleted {
			_ = w.service.db.Model(&models.TransferTask{}).Where("id = ?", task.ID).Updates(map[string]any{"cleanup_status": models.TransferCleanupDeferred, "cleanup_error_code": "", "updated_at": time.Now().UTC()}).Error
		}
		w.transferEvent(ctx, task, "transfer.cleanup.deferred", "cleanup", "waiting", "来源文件仍用于做种或软链接，暂存清理已延后；不会提前删除下载源", map[string]any{"transfer_mode": download.TransferMode})
		return WorkerResult{}
	}
	if task.CleanupStatus == models.TransferCleanupCompleted || task.CleanupStatus == models.TransferCleanupSkipped {
		if err := cleanupCrossSourceManagedRoot(task, download); err != nil {
			next := time.Now().UTC().Add(time.Minute)
			return WorkerResult{RetryAt: &next, ErrorCode: "cross_source_staging_cleanup_failed", ErrorMessage: "入库已完成，跨数据源暂存清理将自动重试"}
		}
		return WorkerResult{}
	}
	if err := ensureDownloadPipelineActive(w.service.db, task.DownloadTaskID); errors.Is(err, context.Canceled) {
		return WorkerResult{}
	} else if err != nil {
		return WorkerResult{ErrorCode: "transfer_download_missing", ErrorMessage: "原下载任务不存在"}
	}
	if _, err := w.service.cleanupTransferStaging(ctx, task, download); err != nil {
		next := time.Now().UTC().Add(time.Minute)
		return WorkerResult{RetryAt: &next, ErrorCode: "download_staging_cleanup_failed", ErrorMessage: "入库已完成，下载暂存清理将自动重试"}
	}
	if err := cleanupCrossSourceManagedRoot(task, download); err != nil {
		next := time.Now().UTC().Add(time.Minute)
		return WorkerResult{RetryAt: &next, ErrorCode: "cross_source_staging_cleanup_failed", ErrorMessage: "入库已完成，跨数据源暂存清理将自动重试"}
	}
	return WorkerResult{}
}

// CleanupAfterSeeding is called only after qBittorrent has acknowledged its
// seeding cleanup. deleteData means the provider already removed the entire
// source package; symlink mode keeps selected media and removes only the
// unselected, manifest-owned files.
func (s *TransferService) CleanupAfterSeeding(ctx context.Context, downloadTaskID string, deleteData bool) error {
	var task models.TransferTask
	if err := s.db.First(&task, "download_task_id = ?", downloadTaskID).Error; err != nil {
		return err
	}
	if task.CleanupStatus == models.TransferCleanupCompleted || task.CleanupStatus == models.TransferCleanupSkipped {
		return nil
	}
	var download models.DownloadTask
	if err := s.db.First(&download, "id = ?", downloadTaskID).Error; err != nil {
		return err
	}
	if deleteData {
		extras, err := transferCleanupDifference(task)
		if err != nil {
			return s.persistTransferCleanupFailure(task.ID, "download_staging_manifest_invalid", err)
		}
		now := time.Now().UTC()
		if err := s.db.Model(&models.TransferTask{}).Where("id = ?", task.ID).Updates(map[string]any{"cleanup_status": models.TransferCleanupCompleted, "cleanup_removed": len(extras), "cleanup_error_code": "", "updated_at": now}).Error; err != nil {
			return err
		}
		serverlog.OperationDownloadStagingCleanup.Event(s.log.Info()).Str("task_id", task.ID).Int("removed", len(extras)).Str("source_cleanup", "provider_delete_data").Msg(serverlog.OperationDownloadStagingCleanup.Message("做种结束后已由下载器清理暂存数据"))
		return nil
	}
	_, err := s.cleanupTransferStaging(ctx, task, download)
	return err
}

func (s *TransferService) cleanupTransferStaging(ctx context.Context, task models.TransferTask, download models.DownloadTask) (removed int, resultErr error) {
	ctx = serverlog.WithBusinessFields(ctx, transferBusinessFields(task, download))
	started := time.Now()
	serverlog.RecordBusiness(ctx, s.log, serverlog.OperationDownloadStagingCleanup, serverlog.BusinessEvent{Code: "cleanup.start", Stage: "cleanup", Outcome: "started", Message: "开始核对完整下载清单与已选入库清单，只清理可证明归属的未选项；其他视频及未匹配字幕保留"})
	defer func() {
		outcome, message := "finished", "本次受控暂存清理已完成，未选视频和未匹配字幕不在删除范围"
		fields := map[string]any{"deleted_count": removed, "duration_ms": time.Since(started).Milliseconds()}
		if resultErr != nil {
			outcome, message = "partial", "暂存清理未全部完成，后续文件保留，按原任务恢复；已确认清理数量单独记录"
			fields["error_code"] = transferStagingCleanupCode(resultErr)
		}
		serverlog.RecordBusiness(ctx, s.log, serverlog.OperationDownloadStagingCleanup, serverlog.BusinessEvent{Code: "cleanup.finish", Stage: "cleanup", Outcome: outcome, Message: message, Fields: fields})
	}()
	ctx = context.WithValue(ctx, stagingCleanupObserverKey{}, stagingCleanupObserver(func(file downloadpkg.File, outcome, message string) {
		display := relativeFileDisplay(file.RelativePath, "", file.Size)
		fields := display.fields()
		delete(fields, "new_name")
		serverlog.RecordBusiness(ctx, s.log, serverlog.OperationDownloadStagingCleanup, serverlog.BusinessEvent{Code: "cleanup.file", Stage: "cleanup", Outcome: outcome, Message: message + "：" + firstNonEmpty(display.source, serverlog.BusinessDisplayName(display.oldName)), Fields: fields})
	}))
	extras, err := transferCleanupDifference(task)
	if err != nil {
		return 0, s.persistTransferCleanupFailure(task.ID, "download_staging_manifest_invalid", err)
	}
	cloudCleanup := download.ProviderType == models.DownloaderTypePan115Offline
	if len(extras) == 0 && !cloudCleanup {
		if err := s.persistTransferCleanupSuccess(task.ID, 0); err != nil {
			return 0, err
		}
		return 0, nil
	}
	_ = s.db.Model(&models.TransferTask{}).Where("id = ?", task.ID).Updates(map[string]any{"cleanup_status": models.TransferCleanupRunning, "cleanup_error_code": "", "updated_at": time.Now().UTC()}).Error
	removed = 0
	if cloudCleanup {
		if task.Phase != models.TransferTaskStatusCompleted {
			err = transferStagingCleanupFailure("download_staging_package_not_completed", errors.New("transfer is not completed"))
		} else {
			var boundary cloudStagingCleanupBoundary
			boundary, err = s.prepareCloudStagingCleanup(ctx, download)
			if err == nil && !boundary.packageMissing && len(extras) > 0 {
				removed, err = cleanupCloudStagingItems(ctx, boundary, extras)
			}
			if err == nil && !boundary.packageMissing {
				var packageRemoved int
				packageRemoved, err = cleanupEmptyCloudStagingPackage(ctx, boundary)
				removed += packageRemoved
			}
		}
	} else {
		removed, err = cleanupLocalStagingObserved(download, extras, func(file downloadpkg.File, outcome, message string) {
			observeStagingCleanup(ctx, file, outcome, message)
		})
	}
	if err != nil {
		return removed, s.persistTransferCleanupFailureWithCount(task.ID, transferStagingCleanupCode(err), removed, err)
	}
	if err := s.persistTransferCleanupSuccess(task.ID, removed); err != nil {
		return removed, err
	}
	serverlog.OperationDownloadStagingCleanup.Event(s.log.Info()).Str("task_id", task.ID).Int("removed", removed).Msg(serverlog.OperationDownloadStagingCleanup.Message("已清理未选中的暂存文件"))
	return removed, nil
}

func transferCleanupDifference(task models.TransferTask) ([]downloadpkg.File, error) {
	plan, err := buildTransferCleanupPlan(task)
	if err != nil {
		return nil, err
	}
	return plan.Removable, nil
}

type transferCleanupPlan struct {
	Removable      []downloadpkg.File
	ProtectedCount int
}

type transferStagingCleanupError struct {
	code  string
	cause error
}

func (e *transferStagingCleanupError) Error() string { return e.code }
func (e *transferStagingCleanupError) Unwrap() error { return e.cause }

func transferStagingCleanupFailure(code string, cause error) error {
	return &transferStagingCleanupError{code: code, cause: cause}
}

func transferStagingCleanupCode(err error) string {
	var failure *transferStagingCleanupError
	if errors.As(err, &failure) && strings.TrimSpace(failure.code) != "" {
		return failure.code
	}
	return "download_staging_cleanup_failed"
}

func buildTransferCleanupPlan(task models.TransferTask) (transferCleanupPlan, error) {
	var source, selected downloadpkg.Manifest
	if json.Unmarshal([]byte(task.SourceManifestJSON), &source) != nil || json.Unmarshal([]byte(task.ManifestJSON), &selected) != nil || !source.Complete || !selected.Complete || len(selected.Files) == 0 {
		return transferCleanupPlan{}, errors.New("transfer cleanup manifest is incomplete")
	}
	selectedKeys := make(map[string]struct{}, len(selected.Files))
	for _, file := range selected.Files {
		key := transferCleanupFileKey(file)
		if key == "" {
			return transferCleanupPlan{}, errors.New("transfer cleanup selected file identity is invalid")
		}
		if _, duplicate := selectedKeys[key]; duplicate {
			return transferCleanupPlan{}, errors.New("transfer cleanup selected file identity is duplicated")
		}
		selectedKeys[key] = struct{}{}
	}
	plan := transferCleanupPlan{Removable: make([]downloadpkg.File, 0, len(source.Files))}
	seen := map[string]struct{}{}
	for _, file := range source.Files {
		key := transferCleanupFileKey(file)
		if key == "" {
			return transferCleanupPlan{}, errors.New("transfer cleanup file identity is invalid")
		}
		if _, duplicate := seen[key]; duplicate {
			return transferCleanupPlan{}, errors.New("transfer cleanup file identity is duplicated")
		}
		seen[key] = struct{}{}
		if _, keep := selectedKeys[key]; !keep {
			// A package selector can be confidently wrong even after metadata
			// verification. Never make that mistake irreversible by deleting an
			// unselected video or an unmatched subtitle. Only clearly non-media
			// manifest items are eligible for automatic staging cleanup.
			if isVideoFile(file.RelativePath) || isAutomaticTransferSubtitleFile(file.RelativePath) || isAutomaticTransferDanmakuFile(file.RelativePath) {
				plan.ProtectedCount++
				continue
			}
			plan.Removable = append(plan.Removable, file)
		}
	}
	for key := range selectedKeys {
		if _, exists := seen[key]; !exists {
			return transferCleanupPlan{}, errors.New("transfer cleanup selection is not a source subset")
		}
	}
	return plan, nil
}

func transferCleanupFileKey(file downloadpkg.File) string {
	raw := strings.ReplaceAll(file.RelativePath, "\\", "/")
	path := normalizedManifestPath(raw)
	providerItemID := strings.TrimSpace(file.ProviderItemID)
	providerParentID := strings.TrimSpace(file.ProviderParentID)
	sha1 := strings.TrimSpace(file.SHA1)
	if raw == "" || raw != strings.TrimSpace(raw) || strings.HasPrefix(raw, "/") || path == "." || path == ".." ||
		strings.HasPrefix(path, "../") || path != raw || strings.Contains(path, ":") ||
		strings.ContainsAny(path, "\x00\r\n") || providerItemID != file.ProviderItemID ||
		providerParentID != file.ProviderParentID || sha1 != file.SHA1 ||
		strings.ContainsAny(providerItemID+providerParentID+sha1, "\x00\r\n") {
		return ""
	}
	return path + "\x00" + providerItemID + "\x00" + providerParentID + "\x00" + strconv.FormatInt(file.Size, 10) + "\x00" + strings.ToLower(sha1)
}

func cleanupLocalStaging(download models.DownloadTask, extras []downloadpkg.File) (int, error) {
	return cleanupLocalStagingObserved(download, extras, nil)
}

func cleanupLocalStagingObserved(download models.DownloadTask, extras []downloadpkg.File, observe stagingCleanupObserver) (int, error) {
	stagingRoot := filepath.Clean(download.StagingAbsolutePath)
	if stagingRoot == "." || !filepath.IsAbs(stagingRoot) {
		return 0, errors.New("download staging root is invalid")
	}
	categoryRoot := filepath.Join(stagingRoot, firstNonEmpty(download.StagingCategory, download.ScrapeCategory))
	if err := ensureWithin(stagingRoot, categoryRoot); err != nil {
		return 0, err
	}
	removed := 0
	for _, file := range extras {
		target, err := resolveManifestSource(categoryRoot, stagingRoot, file.RelativePath)
		if err != nil {
			return removed, err
		}
		info, err := os.Lstat(target)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || !info.Mode().IsRegular() || medialibrary.IsUnsafeDirectory(target, fs.FileInfoToDirEntry(info)) || (file.Size > 0 && info.Size() != file.Size) {
			return removed, errors.New("download staging file changed")
		}
		if err := os.Remove(target); err != nil {
			if observe != nil {
				observe(file, "failed", "本地暂存文件移除失败，当前与后续文件保留")
			}
			return removed, err
		}
		if observe != nil {
			observe(file, "confirmed", "已移除本清单归属的本地未选暂存文件")
		}
		removed++
		pruneEmptyStagingDirectories(stagingRoot, filepath.Dir(target))
	}
	return removed, nil
}

func pruneEmptyStagingDirectories(root, directory string) {
	root, directory = filepath.Clean(root), filepath.Clean(directory)
	for directory != root && ensureWithin(root, directory) == nil {
		if err := os.Remove(directory); err != nil {
			return
		}
		directory = filepath.Dir(directory)
	}
}

type cloudStagingCleanupBoundary struct {
	driver         cloudpkg.Driver
	mutations      cloudpkg.MutationDriver
	proof          *providerBoundaryProof
	packageRoot    cloudpkg.Item
	packageMissing bool
}

func (s *TransferService) prepareCloudStagingCleanup(ctx context.Context, download models.DownloadTask) (cloudStagingCleanupBoundary, error) {
	var boundary cloudStagingCleanupBoundary
	packageRootID := strings.TrimSpace(download.ProviderOutputID)
	listenerRootID := strings.TrimSpace(download.StagingProviderDirectoryID)
	if s.connections == nil || download.StagingStorageID == nil || packageRootID == "" || packageRootID != download.ProviderOutputID || (listenerRootID != "" && listenerRootID != download.StagingProviderDirectoryID) {
		return boundary, transferStagingCleanupFailure("download_staging_package_boundary_invalid", errors.New("cloud staging identity is unavailable"))
	}
	var storage models.Storage
	if err := s.db.First(&storage, *download.StagingStorageID).Error; err != nil || storage.Type != models.StorageTypePan115 || storage.ConnectionID == nil {
		return boundary, transferStagingCleanupFailure("download_staging_package_boundary_invalid", errors.New("cloud staging storage is unavailable"))
	}
	storageRootID := strings.TrimSpace(storage.RootPath)
	if storageRootID == "" || storageRootID != storage.RootPath || packageRootID == storageRootID || (listenerRootID != "" && packageRootID == listenerRootID) {
		return boundary, transferStagingCleanupFailure("download_staging_package_boundary_invalid", errors.New("cloud staging package equals a protected root"))
	}
	_, driver, err := s.connections.driver(*storage.ConnectionID)
	if err != nil {
		return boundary, transferStagingCleanupFailure("download_staging_package_boundary_unavailable", err)
	}
	proof := newProviderBoundaryProof(driver)
	storageRoot, err := proof.stat(ctx, storageRootID)
	if err != nil {
		return boundary, transferStagingCleanupFailure("download_staging_package_boundary_unavailable", err)
	}
	if !storageRoot.IsDir || storageRoot.ID != storageRootID {
		return boundary, transferStagingCleanupFailure("download_staging_package_boundary_invalid", errors.New("cloud staging storage root identity changed"))
	}
	packageBoundaryID := storageRootID
	if listenerRootID != "" {
		packageBoundaryID = listenerRootID
	}
	packageRoot, err := proof.within(ctx, packageRootID, packageBoundaryID)
	if err != nil {
		if code, _ := cloudpkg.ErrorInfo(err); code == cloudpkg.CodeNotFound {
			boundary.driver, boundary.proof, boundary.packageMissing = driver, proof, true
			return boundary, nil
		}
		return boundary, transferStagingCleanupFailure("download_staging_package_boundary_invalid", err)
	}
	if !packageRoot.IsDir || packageRoot.ID != packageRootID {
		return boundary, transferStagingCleanupFailure("download_staging_package_boundary_invalid", errors.New("cloud staging package identity changed"))
	}
	if listenerRootID != "" {
		listener, listenerErr := proof.within(ctx, listenerRootID, storageRootID)
		if listenerErr != nil || !listener.IsDir || listener.ID != listenerRootID {
			return boundary, transferStagingCleanupFailure("download_staging_package_boundary_invalid", errors.New("cloud staging listener boundary changed"))
		}
	}
	mutations, ok := driver.(cloudpkg.MutationDriver)
	if !ok || !mutations.Capabilities().Recycle {
		return boundary, transferStagingCleanupFailure("download_staging_package_recycle_unavailable", errors.New("cloud staging recycle capability is unavailable"))
	}
	boundary.driver, boundary.mutations, boundary.proof, boundary.packageRoot = driver, mutations, proof, packageRoot
	return boundary, nil
}

func (s *TransferService) cleanupCloudStaging(ctx context.Context, download models.DownloadTask, extras []downloadpkg.File) (int, error) {
	boundary, err := s.prepareCloudStagingCleanup(ctx, download)
	if err != nil || boundary.packageMissing {
		return 0, err
	}
	return cleanupCloudStagingItems(ctx, boundary, extras)
}

func cleanupCloudStagingItems(ctx context.Context, boundary cloudStagingCleanupBoundary, extras []downloadpkg.File) (int, error) {
	driver, mutations, proof := boundary.driver, boundary.mutations, boundary.proof
	packageRootID := boundary.packageRoot.ID
	byParent := make(map[string][]downloadpkg.File)
	for _, file := range extras {
		itemID, parentID := strings.TrimSpace(file.ProviderItemID), strings.TrimSpace(file.ProviderParentID)
		if itemID == "" || parentID == "" || itemID != file.ProviderItemID || parentID != file.ProviderParentID {
			return 0, errors.New("cloud staging item identity is missing")
		}
		byParent[parentID] = append(byParent[parentID], file)
	}
	parentIDs := make([]string, 0, len(byParent))
	for parentID := range byParent {
		parentIDs = append(parentIDs, parentID)
	}
	sort.Strings(parentIDs)
	removed := 0
	batchMutations, hasBatch := driver.(cloudpkg.BatchMutationDriver)
	for _, parentID := range parentIDs {
		parent, proofErr := proof.within(ctx, parentID, packageRootID)
		if proofErr != nil || !parent.IsDir {
			return removed, errors.New("cloud staging item boundary is invalid")
		}
		listed, listErr := listCloudDirectory(ctx, driver, parentID)
		if listErr != nil {
			return removed, listErr
		}
		current := make(map[string]cloudpkg.Item, len(listed))
		for _, item := range listed {
			if _, duplicate := current[item.ID]; duplicate {
				return removed, errors.New("cloud staging listing contains duplicate identity")
			}
			current[item.ID] = item
		}
		candidates := make([]string, 0, len(byParent[parentID]))
		for _, file := range byParent[parentID] {
			item, exists := current[file.ProviderItemID]
			if !exists {
				continue
			}
			if item.IsDir || item.ParentID != parentID || item.Size != file.Size || (file.SHA1 != "" && !strings.EqualFold(item.SHA1, file.SHA1)) {
				return removed, errors.New("cloud staging item changed")
			}
			candidates = append(candidates, item.ID)
		}
		for start := 0; start < len(candidates); start += cloudpkg.MaxBatchMutationItems {
			end := start + cloudpkg.MaxBatchMutationItems
			if end > len(candidates) {
				end = len(candidates)
			}
			chunk := candidates[start:end]
			var mutationErr error
			if hasBatch {
				mutationErr = executeCloudBatchMutation(ctx, func() error { return batchMutations.RecycleMany(ctx, chunk) })
			} else {
				for _, itemID := range chunk {
					if mutationErr = mutations.Recycle(ctx, itemID); mutationErr != nil {
						break
					}
				}
			}
			// Provider acknowledgement can be ambiguous. Re-list the one proven
			// parent and count only identities that are actually absent.
			after, reconcileErr := listCloudDirectory(ctx, driver, parentID)
			if reconcileErr != nil {
				if mutationErr != nil {
					return removed, mutationErr
				}
				return removed, reconcileErr
			}
			remaining := make(map[string]struct{}, len(after))
			for _, item := range after {
				remaining[item.ID] = struct{}{}
			}
			missing := 0
			for _, itemID := range chunk {
				if _, exists := remaining[itemID]; !exists {
					missing++
					for _, file := range byParent[parentID] {
						if file.ProviderItemID == itemID {
							observeStagingCleanup(ctx, file, "confirmed", "现有回收后目录核对已确认此未选文件不再位于下载包中")
							break
						}
					}
				}
			}
			removed += missing
			if missing != len(chunk) {
				if mutationErr != nil {
					return removed, mutationErr
				}
				return removed, errors.New("cloud staging batch result is incomplete")
			}
		}
	}
	return removed, nil
}

func cleanupEmptyCloudStagingPackage(ctx context.Context, boundary cloudStagingCleanupBoundary) (int, error) {
	items, err := listCloudDirectory(ctx, boundary.driver, boundary.packageRoot.ID)
	if err != nil {
		if code, _ := cloudpkg.ErrorInfo(err); code == cloudpkg.CodeNotFound {
			return 0, nil
		}
		return 0, transferStagingCleanupFailure("download_staging_package_list_failed", err)
	}
	if len(items) != 0 {
		return 0, nil
	}
	// The package ID was proven below the immutable Storage/listener boundary.
	// Re-read the package immediately after the empty listing so moving or
	// replacing that exact claimed directory fails before recursive recycle.
	current, err := boundary.driver.Stat(ctx, boundary.packageRoot.ID)
	if err != nil {
		if code, _ := cloudpkg.ErrorInfo(err); code == cloudpkg.CodeNotFound {
			return 0, nil
		}
		return 0, transferStagingCleanupFailure("download_staging_package_boundary_unavailable", err)
	}
	if current.ID != boundary.packageRoot.ID || !current.IsDir || current.ParentID != boundary.packageRoot.ParentID {
		return 0, transferStagingCleanupFailure("download_staging_package_boundary_invalid", errors.New("cloud staging package boundary changed"))
	}
	mutationErr := boundary.mutations.Recycle(ctx, boundary.packageRoot.ID)
	after, reconcileErr := boundary.driver.Stat(ctx, boundary.packageRoot.ID)
	if reconcileErr != nil {
		if code, _ := cloudpkg.ErrorInfo(reconcileErr); code == cloudpkg.CodeNotFound {
			// The root existed at the immediately preceding proof and is now gone.
			// Count this convergence once even if the provider acknowledgement was
			// ambiguous; later retries see the missing root and add nothing.
			return 1, nil
		}
		if mutationErr != nil {
			return 0, transferStagingCleanupFailure("download_staging_package_recycle_failed", mutationErr)
		}
		return 0, transferStagingCleanupFailure("download_staging_package_reconcile_failed", reconcileErr)
	}
	if mutationErr != nil {
		return 0, transferStagingCleanupFailure("download_staging_package_recycle_failed", mutationErr)
	}
	if after.ID != boundary.packageRoot.ID || !after.IsDir {
		return 0, transferStagingCleanupFailure("download_staging_package_reconcile_failed", errors.New("cloud staging package reconcile identity changed"))
	}
	return 0, transferStagingCleanupFailure("download_staging_package_reconcile_failed", errors.New("cloud staging package still exists after recycle"))
}

func (s *TransferService) persistTransferCleanupSuccess(taskID string, removed int) error {
	return s.db.Model(&models.TransferTask{}).Where("id = ?", taskID).Updates(map[string]any{"cleanup_status": models.TransferCleanupCompleted, "cleanup_removed": gorm.Expr("cleanup_removed + ?", removed), "cleanup_error_code": "", "updated_at": time.Now().UTC()}).Error
}

func (s *TransferService) persistTransferCleanupFailure(taskID, code string, cause error) error {
	return s.persistTransferCleanupFailureWithCount(taskID, code, 0, cause)
}

func (s *TransferService) persistTransferCleanupFailureWithCount(taskID, code string, removed int, cause error) error {
	if err := s.db.Model(&models.TransferTask{}).Where("id = ?", taskID).Updates(map[string]any{"cleanup_status": models.TransferCleanupFailed, "cleanup_removed": gorm.Expr("cleanup_removed + ?", removed), "cleanup_error_code": safeLabel(code, 96), "updated_at": time.Now().UTC()}).Error; err != nil {
		return err
	}
	serverlog.OperationDownloadStagingCleanup.Event(s.log.Warn()).Str("task_id", taskID).Str("error_code", safeLabel(code, 96)).Msg(serverlog.OperationDownloadStagingCleanup.Message("暂存清理失败，将保留文件并重试"))
	return cause
}
