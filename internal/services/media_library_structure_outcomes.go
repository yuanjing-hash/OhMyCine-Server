package services

import (
	"context"
	"errors"
	"os"
	pathpkg "path"
	"path/filepath"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/medialibrary"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	cloudpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

// A failed syscall is not proof that no file moved (notably move + rename on
// cloud storage). Before finalizing a partial attempt, observe every remaining
// source at its frozen identity. This never retries a mutation or invents a
// successful checkpoint. Missing/moved/unknown sources retain recovery fences.
func (s *MediaLibraryStructureService) verifyStructureUnchangedFailures(ctx context.Context, repair models.MediaLibraryStructureRepair, plan StructurePlan, boundary StructureBoundary, backend MediaLibraryStructureBackend) error {
	var rows []models.MediaLibraryStructureRepairItem
	if err := s.db.WithContext(ctx).Where("repair_id=? AND status<>?", repair.ID, structureRepairItemSucceeded).Order("ordinal").Find(&rows).Error; err != nil {
		return err
	}
	var providerIndex *providerStructureDirectoryIndex
	if boundary.Storage.Type == models.StorageTypePan115 {
		provider, ok := backend.(pan115MediaLibraryStructureBackend)
		if !ok || provider.driver == nil || boundary.Storage.ConnectionID == nil {
			return ErrCatalogFence
		}
		driver, err := provider.driver(*boundary.Storage.ConnectionID)
		if err != nil {
			return err
		}
		root := boundary.Library.ProviderRootID
		if root == "" {
			root = boundary.Storage.RootPath
		}
		providerIndex = newProviderStructureDirectoryIndex(cloudpkg.WithReadClass(ctx, cloudpkg.ReadClassPipeline), driver, root)
	}
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return err
		}
		if row.Status != structureRepairItemFailed && row.Status != structureRepairItemBlocked {
			return ErrCatalogFence
		}
		var source, providerID string
		var size, modified int64
		if row.Ordinal < len(plan.RecycleItems) {
			item := plan.RecycleItems[row.Ordinal]
			source, providerID, size, modified = item.SourceRelative, item.ProviderID, item.Size, item.ModifiedAtUnixNano
		} else {
			index := row.Ordinal - len(plan.RecycleItems)
			if index < 0 || index >= len(plan.Items) {
				return ErrCatalogFence
			}
			item := plan.Items[index]
			source, providerID, size, modified = item.SourceRelative, item.ProviderID, item.Size, item.ModifiedAtUnixNano
		}
		if source == "" || safeStructurePath(source) != source || row.SourceRelative != source {
			return ErrCatalogFence
		}
		if providerIndex != nil {
			parent, err := providerIndex.directoryID(pathpkg.Dir(source))
			if err != nil {
				return err
			}
			listing, err := providerIndex.directory(parent, false)
			if err != nil {
				return err
			}
			item, ok := listing.byID[providerID]
			if !ok || providerID == "" || item.IsDir || item.ParentID != parent || item.Name != pathpkg.Base(source) || (size > 0 && item.Size != size) {
				return ErrCatalogFence
			}
			continue
		}
		if boundary.Storage.Type != models.StorageTypeLocal {
			return ErrCatalogFence
		}
		root, err := medialibrary.ResolveRoot(boundary.Storage.RootPath, boundary.Library.RelativeRoot)
		if err != nil {
			return err
		}
		name := filepath.Join(root, filepath.FromSlash(source))
		if ensureWithin(root, name) != nil || ensureSafeDirectoryPath(root, filepath.Dir(name), false) != nil {
			return ErrCatalogFence
		}
		info, err := os.Lstat(name)
		if err != nil {
			return errors.Join(ErrCatalogFence, err)
		}
		if !info.Mode().IsRegular() || (size > 0 && info.Size() != size) || (modified != 0 && info.ModTime().UTC().UnixNano() != modified) {
			return ErrCatalogFence
		}
	}
	return nil
}
