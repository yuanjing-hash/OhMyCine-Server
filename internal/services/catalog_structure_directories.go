package services

import (
	"context"
	"errors"
	pathpkg "path"
	"strings"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	cloudpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Directory preparation is an explicitly confirmed plan step, but never moves
// or recycles media. The caller validates the complete SQL budget first. Every
// provider directory receipt is committed separately, not an expanding JSON blob.
func (s *MediaLibraryStructureService) prepareCatalogStructureDirectories(ctx context.Context, repair models.MediaLibraryStructureRepair, state structureCatalogRepairState, plan StructurePlan, boundary StructureBoundary, backend MediaLibraryStructureBackend, claim *ClaimedJob) (map[string]string, error) {
	parents := map[string]string{}
	if boundary.Storage.Type == models.StorageTypeLocal || len(plan.Items) == 0 {
		return parents, nil
	}
	var getter func(uint) (cloudpkg.Driver, error)
	switch value := backend.(type) {
	case pan115MediaLibraryStructureBackend:
		getter = value.driver
	case *pan115MediaLibraryStructureBackend:
		getter = value.driver
	}
	if getter == nil || boundary.Storage.ConnectionID == nil {
		return nil, ErrCatalogInvalid
	}
	driver, err := getter(*boundary.Storage.ConnectionID)
	if err != nil {
		return nil, err
	}
	mutations, ok := driver.(cloudpkg.MutationDriver)
	if !ok {
		return nil, ErrCatalogInvalid
	}
	root := boundary.Library.ProviderRootID
	if root == "" {
		root = boundary.Storage.RootPath
	}
	parents[""], parents["."] = root, root
	for _, item := range plan.Items {
		relative := pathpkg.Dir(safeStructurePath(item.TargetRelative))
		if relative == "." || relative == "" {
			continue
		}
		parent, walked := root, ""
		for _, segment := range strings.Split(relative, "/") {
			if segment == "" || segment == "." || segment == ".." {
				return nil, ErrCatalogInvalid
			}
			walked = pathpkg.Join(walked, segment)
			if cached := parents[walked]; cached != "" {
				parent = cached
				continue
			}
			if err := validateStructureMutation(boundary); err != nil {
				return nil, err
			}
			var receipt models.CatalogStructureDirectoryReceipt
			findErr := s.catalogStore.readDB.WithContext(ctx).First(&receipt, "repair_id = ? AND target_relative = ?", repair.ID, walked).Error
			childID := ""
			if findErr == nil {
				stat, err := driver.Stat(cloudpkg.WithReadClass(ctx, cloudpkg.ReadClassBackground), receipt.ParentProviderID)
				if err != nil || !stat.IsDir || stat.ParentID != parent || stat.Name != segment {
					return nil, ErrCatalogFence
				}
				childID = stat.ID
			} else if !errors.Is(findErr, gorm.ErrRecordNotFound) {
				return nil, findErr
			}
			if childID == "" {
				childID, err = providerChildID(ctx, driver, parent, segment)
				if err != nil {
					return nil, err
				}
				if childID == "" {
					if err := validateStructureMutation(boundary); err != nil {
						return nil, err
					}
					created, err := mutations.CreateDirectory(ctx, parent, segment)
					if err != nil {
						return nil, err
					}
					childID = created.ID
				}
				stat, err := driver.Stat(cloudpkg.WithReadClass(ctx, cloudpkg.ReadClassBackground), childID)
				if err != nil || childID == "" || !stat.IsDir || stat.ParentID != parent || stat.Name != segment {
					return nil, ErrCatalogFence
				}
				if err := s.structureCatalogWriteTx(ctx, func(tx *gorm.DB) error {
					if err := s.validateCatalogStructureExecutionTx(tx, repair, state, claim, true); err != nil {
						return err
					}
					return tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "repair_id"}, {Name: "target_relative"}}, DoUpdates: clause.AssignmentColumns([]string{"parent_provider_id", "prepared_at"})}).Create(&models.CatalogStructureDirectoryReceipt{RepairID: repair.ID, TargetRelative: walked, ParentProviderID: childID, PreparedAt: time.Now().UTC()}).Error
				}); err != nil {
					return nil, err
				}
			}
			parents[walked] = childID
			parent = childID
		}
	}
	return parents, nil
}
