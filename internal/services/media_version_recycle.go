package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/medialibrary"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	storagefs "github.com/yuanjing-hash/OhMyCine-Server/internal/storage"
	cloudpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"gorm.io/gorm"
)

func (s *MediaVersionUpgradeService) captureRecycleItems(ctx context.Context, library models.MediaLibrary, storage models.Storage, entry models.MediaLibraryEntry, assets []models.MediaLibrarySourceAsset) ([]mediaUpgradeRecycleItem, error) {
	items := []mediaUpgradeRecycleItem{{Key: fmt.Sprintf("entry:%d", entry.ID), RelativePath: entry.RelativePath, ProviderID: entry.ProviderID, Size: entry.Size, ModifiedAt: entry.ModifiedAt}}
	for _, asset := range assets {
		items = append(items, mediaUpgradeRecycleItem{Key: fmt.Sprintf("asset:%d", asset.ID), RelativePath: asset.RelativePath, ProviderID: asset.ProviderID, Size: asset.Size, ModifiedAt: asset.ModifiedAt})
	}
	if storage.Type == models.StorageTypeLocal {
		if !managedMediaRecycleSupported() {
			return nil, appError(CodeMediaUpgradeUnsupported, "当前平台未提供安全的本地回收能力", nil)
		}
		root, err := catalogLocalDeletionRoot(storage, library)
		if err != nil {
			return nil, err
		}
		for i := range items {
			item := &items[i]
			target, err := catalogLocalDeletionTarget(root, item.RelativePath)
			if err != nil {
				return nil, err
			}
			if medialibrary.IsManagedTrashPath(item.RelativePath) || ensureSafeDirectoryPath(root, filepath.Dir(target), false) != nil {
				return nil, mediaUpgradeChanged()
			}
			info, err := os.Lstat(target)
			if err != nil || !info.Mode().IsRegular() || storagefs.IsReparsePoint(target, info) || info.Size() != item.Size || !info.ModTime().Equal(item.ModifiedAt) {
				return nil, mediaUpgradeChanged()
			}
			item.FileIdentity, err = managedMediaFileIdentity(target)
			if err != nil {
				return nil, mediaUpgradeChanged()
			}
			item.Name = filepath.Base(target)
		}
		return items, nil
	}
	driver, _, err := s.recycleDriver(storage)
	if err != nil {
		return nil, err
	}
	root, err := providerItemWithinRoot(ctx, driver, library.ProviderRootID, storage.RootPath)
	if err != nil {
		return nil, err
	}
	for i := range items {
		item := &items[i]
		actual, err := providerItemWithinRoot(ctx, driver, item.ProviderID, root.ID)
		if err != nil || actual.IsDir || actual.ID != item.ProviderID || actual.Size != item.Size || (!item.ModifiedAt.IsZero() && !actual.ModifiedAt.IsZero() && !actual.ModifiedAt.Equal(item.ModifiedAt)) {
			return nil, mediaUpgradeChanged()
		}
		item.ParentID, item.Name, item.SHA1, item.ProviderRevision = actual.ParentID, actual.Name, actual.SHA1, actual.Revision
	}
	return items, nil
}

func matchesUpgradeProviderItem(item mediaUpgradeRecycleItem, actual cloudpkg.Item) bool {
	return !actual.IsDir && actual.ID == item.ProviderID && actual.ParentID == item.ParentID && actual.Name == item.Name && actual.Size == item.Size &&
		(item.SHA1 == "" || strings.EqualFold(strings.TrimSpace(actual.SHA1), strings.TrimSpace(item.SHA1))) &&
		(item.ProviderRevision == "" || actual.Revision == item.ProviderRevision) &&
		(item.ModifiedAt.IsZero() || actual.ModifiedAt.IsZero() || item.ModifiedAt.Equal(actual.ModifiedAt))
}
func (s *MediaVersionUpgradeService) persistUpgradeRecycle(id string, state mediaUpgradeRecycleState) error {
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return s.db.Model(&models.MediaVersionUpgrade{}).Where("id=? AND state IN ?", id, []string{"replacement_ready", "recycle_pending"}).Updates(map[string]any{"recycle_json": string(raw), "updated_at": s.now()}).Error
}

// Recycle is dispatched at most once for an uncertain provider operation.
// Only Stat can reconcile it afterwards; existence does not authorize a replay.
func (s *MediaVersionUpgradeService) recycleCloudItem(ctx context.Context, row models.MediaVersionUpgrade, item mediaUpgradeRecycleItem, storage models.Storage, state *mediaUpgradeRecycleState) error {
	if state.Items[item.Key] == "done" {
		return nil
	}
	driver, mutations, err := s.recycleDriver(storage)
	if err != nil {
		return err
	}
	actual, err := driver.Stat(ctx, item.ProviderID)
	if code, _ := cloudpkg.ErrorInfo(err); err != nil && code == cloudpkg.CodeNotFound {
		state.Items[item.Key] = "done"
		return s.persistUpgradeRecycle(row.ID, *state)
	}
	if err != nil {
		return err
	}
	if !matchesUpgradeProviderItem(item, actual) {
		return mediaUpgradeChanged()
	}
	if state.Items[item.Key] == "dispatched" {
		return appError(CodeMediaUpgradePending, "原回收结果尚未确认", nil)
	}
	// Re-prove ancestry, not just a potentially re-used provider ID.
	var library models.MediaLibrary
	if err := s.db.First(&library, row.LibraryID).Error; err != nil {
		return err
	}
	if _, err := providerItemWithinRoot(ctx, driver, item.ProviderID, library.ProviderRootID); err != nil {
		return mediaUpgradeChanged()
	}
	state.Items[item.Key] = "dispatched"
	if err := s.persistUpgradeRecycle(row.ID, *state); err != nil {
		return err
	}
	if err := mutations.Recycle(ctx, item.ProviderID); err != nil {
		return appError(CodeMediaUpgradePending, "原回收结果尚未确认", err)
	}
	state.Items[item.Key] = "done"
	return s.persistUpgradeRecycle(row.ID, *state)
}

func managedTrashRoots(library models.MediaLibrary, storage models.Storage) (string, string, error) {
	libraryRoot, err := catalogLocalDeletionRoot(storage, library)
	if err != nil {
		return "", "", err
	}
	storageRoot, err := (storagefs.LocalDriver{}).CanonicalizeRoot(storage.RootPath)
	if err != nil {
		return "", "", err
	}
	if ensureWithin(storageRoot, libraryRoot) != nil {
		return "", "", mediaUpgradeChanged()
	}
	return libraryRoot, storageRoot, nil
}
func matchesManagedTrashFile(target string, item mediaUpgradeRecycleItem) bool {
	info, err := os.Lstat(target)
	if err != nil || !info.Mode().IsRegular() || storagefs.IsReparsePoint(target, info) || info.Size() != item.Size || !info.ModTime().Equal(item.ModifiedAt) {
		return false
	}
	identity, err := managedMediaFileIdentity(target)
	return err == nil && identity == item.FileIdentity
}
func (s *MediaVersionUpgradeService) recycleLocalItem(ctx context.Context, row models.MediaVersionUpgrade, item mediaUpgradeRecycleItem, library models.MediaLibrary, storage models.Storage) error {
	libraryRoot, storageRoot, err := managedTrashRoots(library, storage)
	if err != nil {
		return err
	}
	original, err := catalogLocalDeletionTarget(libraryRoot, item.RelativePath)
	if err != nil {
		return err
	}
	if err := s.protectUpgradeSeedingSource(ctx, original); err != nil {
		return err
	}
	var receipt models.ManagedMediaTrash
	err = s.db.WithContext(ctx).Where("upgrade_id=? AND item_key=?", row.ID, item.Key).First(&receipt).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		now := s.now()
		receipt = models.ManagedMediaTrash{ID: uuid.NewString(), UpgradeID: row.ID, LibraryID: row.LibraryID, ItemKey: item.Key, OriginalRelativePath: item.RelativePath, TrashRelativePath: filepath.ToSlash(filepath.Join(medialibrary.ManagedTrashDirectory, row.ID, uuid.NewString()+filepath.Ext(item.RelativePath))), FileIdentity: item.FileIdentity, Size: item.Size, ModifiedAt: item.ModifiedAt, State: "prepared", CreatedAt: now, UpdatedAt: now}
		if err = s.db.WithContext(ctx).Create(&receipt).Error; err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if receipt.FileIdentity != item.FileIdentity || receipt.OriginalRelativePath != item.RelativePath || receipt.State == "restored" {
		return mediaUpgradeChanged()
	}
	destination, err := catalogLocalDeletionTarget(storageRoot, receipt.TrashRelativePath)
	if err != nil {
		return err
	}
	if !medialibrary.IsManagedTrashPath(receipt.TrashRelativePath) {
		return mediaUpgradeChanged()
	}
	if ensureSafeDirectoryPath(storageRoot, filepath.Dir(destination), true) != nil || ensureSafeDirectoryPath(libraryRoot, filepath.Dir(original), false) != nil {
		return mediaUpgradeChanged()
	}
	_, sourceErr := os.Lstat(original)
	_, destinationErr := os.Lstat(destination)
	if errors.Is(sourceErr, os.ErrNotExist) && destinationErr == nil && matchesManagedTrashFile(destination, item) {
		return s.db.Model(&receipt).Updates(map[string]any{"state": "recycled", "updated_at": s.now()}).Error
	}
	if sourceErr != nil || !errors.Is(destinationErr, os.ErrNotExist) || !matchesManagedTrashFile(original, item) {
		return mediaUpgradeChanged()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Unique private destination and no destination overwrite. Local atomic rename
	// can be reconciled by file ID after a crash before the following DB update.
	if err := managedMediaMoveNoReplace(original, destination); err != nil {
		return err
	}
	if !matchesManagedTrashFile(destination, item) {
		return mediaUpgradeChanged()
	}
	return s.db.Model(&receipt).Updates(map[string]any{"state": "recycled", "updated_at": s.now()}).Error
}

// RestoreManagedTrash is an internal administrative primitive. It restores one
// saved mapping only, never a glob or directory; occupied paths are preserved.
func (s *MediaVersionUpgradeService) RestoreManagedTrash(ctx context.Context, actor Actor, receiptID string) error {
	var receipt models.ManagedMediaTrash
	if err := s.db.First(&receipt, "id=?", receiptID).Error; err != nil {
		return err
	}
	row, err := s.load(ctx, actor, receipt.UpgradeID)
	if err != nil {
		return err
	}
	snapshot, err := decodeMediaUpgradeSnapshot(row.SnapshotJSON)
	if err != nil {
		return err
	}
	var library models.MediaLibrary
	var storage models.Storage
	if err = s.db.First(&library, row.LibraryID).Error; err != nil {
		return err
	}
	if err = s.db.First(&storage, library.StorageID).Error; err != nil {
		return err
	}
	if catalogDeletionBoundaryDigest(library, storage) != snapshot.Fence.BoundaryDigest || storage.Type != models.StorageTypeLocal {
		return mediaUpgradeChanged()
	}
	libraryRoot, storageRoot, err := managedTrashRoots(library, storage)
	if err != nil {
		return err
	}
	source, err := catalogLocalDeletionTarget(storageRoot, receipt.TrashRelativePath)
	if err != nil {
		return err
	}
	target, err := catalogLocalDeletionTarget(libraryRoot, receipt.OriginalRelativePath)
	if err != nil {
		return err
	}
	item := mediaUpgradeRecycleItem{FileIdentity: receipt.FileIdentity, Size: receipt.Size, ModifiedAt: receipt.ModifiedAt}
	if receipt.State != "recycled" || !medialibrary.IsManagedTrashPath(receipt.TrashRelativePath) || !matchesManagedTrashFile(source, item) || ensureSafeDirectoryPath(storageRoot, filepath.Dir(source), false) != nil || ensureSafeDirectoryPath(libraryRoot, filepath.Dir(target), true) != nil {
		return mediaUpgradeChanged()
	}
	if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
		return appError(CodeConflict, "原位置已有文件，未覆盖任何文件", nil)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := managedMediaMoveNoReplace(source, target); err != nil {
		return err
	}
	return s.db.Model(&receipt).Updates(map[string]any{"state": "restored", "updated_at": s.now()}).Error
}
