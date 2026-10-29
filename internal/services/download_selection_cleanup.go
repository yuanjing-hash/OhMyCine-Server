package services

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/resourcecontent"
	"gorm.io/gorm"
)

func groupCleanupPurpose(id string) string { return "bt-selection-group:" + id + ":cleanup" }

// Called by the existing per-reference seeding job after that reference's
// frozen policy is satisfied. A pending receipt is reconciled, never replayed.
func (c *selectionGroupClient) retireAndCleanup(ctx context.Context) (done bool, resultErr error) {
	resultErr = c.with(ctx, "retired", func(ctx context.Context, binding downloadpkg.SelectionContext, _ downloadpkg.Task) error {
		var cleanup downloadpkg.GroupCleanup
		ready := false
		err := c.service.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			var group models.BTSelectionGroup
			if err := tx.First(&group, "id = ? AND lease_epoch = ?", c.task.BTSelectionGroupID, binding.Group.Epoch).Error; err != nil {
				return err
			}
			if group.CleanupState == "completed" {
				done = true
				return nil
			}
			if group.CleanupState != "" {
				plain, err := c.service.credentials.Decrypt(groupCleanupPurpose(group.ID), group.CleanupCiphertext)
				if err != nil {
					return err
				}
				if json.Unmarshal([]byte(plain), &cleanup) != nil || cleanup.Key != group.CleanupKey || cleanup.Group.ID != group.ID {
					return downloadpkg.Error("downloader_selection_cleanup_invalid", false, nil)
				}
				cleanup.Group = *binding.Group
				cleanup.ProbeOnly = true
				ready = true
				return nil
			}
			var refs []models.BTSelectionRef
			if err := tx.Where("group_id = ?", group.ID).Limit(1025).Find(&refs).Error; err != nil {
				return err
			}
			if len(refs) > 1024 {
				return downloadpkg.Error("downloader_selection_group_limit", false, nil)
			}
			candidates := map[int]resourcecontent.SelectedFile{}
			protected := map[int]bool{}
			for _, ref := range refs {
				if ref.State != "retired" && ref.State != "released" {
					return nil
				}
				var task models.DownloadTask
				if err := tx.First(&task, "id = ? AND owner_id = ? AND bt_selection_group_id = ?", ref.TaskID, group.OwnerID, group.ID).Error; err != nil {
					return err
				}
				plain, err := c.service.credentials.Decrypt(downloadSourcePurpose(task.ID), task.SourceCiphertext)
				if err != nil {
					return err
				}
				var source downloadSourceEnvelope
				if json.Unmarshal([]byte(plain), &source) != nil || validateStoredContentSelection(source) != nil || source.ContentSelection == nil {
					return downloadpkg.Error("downloader_selection_cleanup_invalid", false, nil)
				}
				if ref.State == "retired" {
					var transfer models.TransferTask
					if err := tx.First(&transfer, "download_task_id = ?", task.ID).Error; err != nil {
						return err
					}
					if transfer.Phase != models.TransferTaskStatusCompleted || transfer.CatalogPublishedAt == nil {
						return nil
					}
					if task.TargetLibraryID == nil || transfer.LibraryID != *task.TargetLibraryID {
						return nil
					}
					if task.SeedingCleanupEnabled {
						var seed models.SeedingTask
						if err := tx.First(&seed, "download_task_id = ?", task.ID).Error; err != nil {
							return err
						}
						if !seedingThresholdReached(seed) {
							return nil
						}
					}
				}
				retain := ref.State == "released" || task.TransferMode == models.MediaLibraryTransferSymlink || task.TransferMode != models.MediaLibraryTransferMove && !task.SeedingCleanupEnabled
				for _, file := range source.ContentSelection.Files {
					if retain {
						protected[file.Index] = true
					} else {
						candidates[file.Index] = file
					}
				}
			}
			files := resourcecontent.Selection{Version: 1, Identity: c.source.ContentSelection.Identity}
			for index, file := range candidates {
				if !protected[index] {
					files.Files = append(files.Files, file)
				}
			}
			sort.Slice(files.Files, func(i, j int) bool { return files.Files[i].Index < files.Files[j].Index })
			deleteFiles := len(files.Files) > 0
			if !deleteFiles {
				files = *c.source.ContentSelection
			}
			key := fmt.Sprintf("%x", sha256.Sum256([]byte(group.ID+":cleanup-v1")))
			cleanup = downloadpkg.GroupCleanup{Key: key, Group: *binding.Group, Files: files, DeleteFiles: deleteFiles}
			raw, _ := json.Marshal(cleanup)
			cipher, err := c.service.credentials.Encrypt(groupCleanupPurpose(group.ID), string(raw))
			if err != nil {
				return err
			}
			r := tx.Model(&models.BTSelectionGroup{}).Where("id = ? AND lease_epoch = ? AND cleanup_state = ''", group.ID, binding.Group.Epoch).Updates(map[string]any{"cleanup_state": "requested", "cleanup_key": key, "cleanup_ciphertext": cipher, "updated_at": time.Now().UTC()})
			if r.Error != nil {
				return r.Error
			}
			if r.RowsAffected != 1 {
				return downloadpkg.Error("downloader_selection_group_busy", true, nil)
			}
			ready = true
			return nil
		})
		if err != nil || !ready || done {
			return err
		}
		cleaner, ok := c.Client.(downloadpkg.SelectionGroupCleaner)
		if !ok {
			return downloadpkg.Error("downloader_selection_group_cleanup_unsupported", false, nil)
		}
		nativeGone, err := cleaner.CleanupSelectionGroup(ctx, c.source, binding, cleanup)
		if err != nil {
			return err
		}
		if !nativeGone {
			return downloadpkg.Error("downloader_selection_cleanup_unconfirmed", true, nil)
		}
		if err = c.service.db.WithContext(ctx).Model(&models.BTSelectionGroup{}).Where("id = ? AND lease_epoch = ?", c.task.BTSelectionGroupID, binding.Group.Epoch).Update("cleanup_state", "native_removed").Error; err != nil {
			return err
		}
		if normalizeExecutionLocation(c.task.ExecutionLocation) != models.NodeLocationRemote && cleanup.DeleteFiles {
			if _, err = downloadpkg.RemoveSelectionGroupFiles(ctx, c.task.StagingAbsolutePath, binding, cleanup.Files); err != nil {
				return err
			}
		}
		return c.service.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			r := tx.Model(&models.BTSelectionGroup{}).Where("id = ? AND lease_epoch = ? AND cleanup_key = ?", c.task.BTSelectionGroupID, binding.Group.Epoch, cleanup.Key).Updates(map[string]any{"cleanup_state": "completed", "updated_at": time.Now().UTC()})
			if r.Error != nil {
				return r.Error
			}
			if r.RowsAffected != 1 {
				return downloadpkg.Error("downloader_selection_group_busy", true, nil)
			}
			var refs []models.BTSelectionRef
			if err := tx.Where("group_id = ? AND state = ?", c.task.BTSelectionGroupID, "retired").Find(&refs).Error; err != nil {
				return err
			}
			removed := map[int]resourcecontent.SelectedFile{}
			if cleanup.DeleteFiles {
				for _, file := range cleanup.Files.Files {
					removed[file.Index] = file
				}
			}
			for _, ref := range refs {
				var task models.DownloadTask
				if err := tx.First(&task, "id = ?", ref.TaskID).Error; err != nil {
					return err
				}
				plain, err := c.service.credentials.Decrypt(downloadSourcePurpose(task.ID), task.SourceCiphertext)
				if err != nil {
					return err
				}
				var source downloadSourceEnvelope
				if json.Unmarshal([]byte(plain), &source) != nil || source.ContentSelection == nil {
					return downloadpkg.Error("downloader_selection_cleanup_invalid", false, nil)
				}
				complete := cleanup.DeleteFiles
				for _, file := range source.ContentSelection.Files {
					complete = complete && removed[file.Index] == file
				}
				status, code := models.TransferCleanupDeferred, "shared_source_retained"
				if complete {
					status, code = models.TransferCleanupCompleted, ""
				} else if task.TransferMode == models.MediaLibraryTransferSymlink {
					status, code = models.TransferCleanupSkipped, "symlink_source_retained"
				}
				if err := tx.Model(&models.TransferTask{}).Where("download_task_id = ?", task.ID).Updates(map[string]any{"cleanup_status": status, "cleanup_error_code": code, "updated_at": time.Now().UTC()}).Error; err != nil {
					return err
				}
			}
			done = true
			return nil
		})
	})
	return
}
