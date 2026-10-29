package services

import (
	"context"
	"encoding/hex"
	"path/filepath"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

// The frozen group workspace is a child of the already validated Server
// staging root. qB has not created it when the first Download worker resolves
// its snapshot. Create only this exact persisted group/ref's directory; keep
// the ordinary staging resolver strict for all other tasks and user paths.
func (s *DownloadService) prepareBTSelectionWorkspace(ctx context.Context, task models.DownloadTask, executor models.Downloader) error {
	if task.BTSelectionGroupID == "" {
		return nil
	}
	if len(task.BTSelectionGroupID) != 64 {
		return appError(CodeDownloadStagingUnavailable, "精确下载的受管工作目录无效", nil)
	}
	if _, err := hex.DecodeString(task.BTSelectionGroupID); err != nil {
		return appError(CodeDownloadStagingUnavailable, "精确下载的受管工作目录无效", nil)
	}
	var group models.BTSelectionGroup
	if err := s.db.WithContext(ctx).First(&group, "id=?", task.BTSelectionGroupID).Error; err != nil {
		return err
	}
	var ref models.BTSelectionRef
	if err := s.db.WithContext(ctx).First(&ref, "task_id=? AND group_id=?", task.ID, group.ID).Error; err != nil {
		return err
	}
	if group.OwnerID != task.OwnerID || group.DownloaderID != executor.ID || task.DownloaderID == nil || *task.DownloaderID != executor.ID || group.Workspace != task.StagingAbsolutePath || group.CleanupState != "" || ref.State == "released" || !filepath.IsAbs(group.Workspace) {
		return appError(CodeDownloadStagingUnavailable, "精确下载的工作目录归属或代际已变化", nil)
	}
	root := filepath.Dir(filepath.Dir(group.Workspace))
	if filepath.Join(root, ".omc-bt", group.ID) != group.Workspace {
		return appError(CodeDownloadStagingUnavailable, "精确下载的工作目录越过受管范围", nil)
	}
	canonical, err := validateGlobalStaging(ctx, root)
	if err != nil {
		return err
	}
	if err := ensureSafeDirectoryPath(canonical, group.Workspace, true); err != nil {
		return appError(CodeDownloadStagingUnavailable, "精确下载的受管工作目录不可用或包含符号链接/Reparse Point", nil)
	}
	return nil
}
