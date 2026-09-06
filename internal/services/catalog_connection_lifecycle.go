package services

import (
	"context"
	"math"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

func (s *ConnectionService) SetCatalogSnapshotStore(store *CatalogSnapshotStore) {
	s.catalogStore = store
}
func (s *ConnectionService) SetMediaChangeService(changes *MediaChangeService) { s.changes = changes }

func (s *ConnectionService) writeCatalogConnection(write func(*gorm.DB) error) error {
	if s.catalogStore == nil {
		return s.db.Transaction(write)
	}
	return s.catalogStore.Admission().WithForeground(context.Background(), func() error { return s.db.Transaction(write) })
}

func (s *ConnectionService) applyCatalogConnectionChangeTx(tx *gorm.DB, before, next models.Connection, credentialChanged bool) error {
	sourceChanged := credentialChanged || before.Endpoint != next.Endpoint
	if !sourceChanged && before.Enabled == next.Enabled {
		return nil
	}
	if err := requireCatalogScopeNotRetiringTx(tx, "storage_id IN (SELECT id FROM storages WHERE connection_id=?)", before.ID); err != nil {
		return err
	}
	if err := assertCatalogPhysicalScopeDrainedTx(tx, "storage_id IN (SELECT id FROM storages WHERE connection_id=?)", before.ID); err != nil {
		return err
	}
	var libraries []models.MediaLibrary
	if err := tx.Model(&models.MediaLibrary{}).Joins("JOIN storages s ON s.id=media_libraries.storage_id").Where("s.connection_id=?", before.ID).Order("media_libraries.id").Limit(catalogStorageMutationLibraries + 1).Find(&libraries).Error; err != nil {
		return err
	}
	if len(libraries) > catalogStorageMutationLibraries {
		return appError(CodeConflict, "连接关联的媒体库较多，请逐库迁移到新连接后再更换来源或启停连接", ErrCatalogBudget)
	}
	storageIDs := map[uint]struct{}{}
	for _, library := range libraries {
		var heads []models.CatalogHead
		if err := tx.Where("library_id=?", library.ID).Limit(1).Find(&heads).Error; err != nil {
			return err
		}
		if len(heads) != 0 && heads[0].Mode == "converting" {
			return appError(CodeConflict, "关联媒体库正在转换，请先完成或恢复转换后再修改连接", ErrCatalogFence)
		}
		if sourceChanged && (len(heads) == 0 || heads[0].Mode == "legacy") {
			return appError(CodeConflict, "关联媒体库尚未转换，不能直接更换账号来源；请新建连接后逐库修改媒体库来源", ErrCatalogFence)
		}
		if len(heads) != 0 && heads[0].Mode != "legacy" && heads[0].Mode != "versioned" {
			return ErrCatalogInvalid
		}
		if len(heads) != 0 && heads[0].Mode == "versioned" && s.catalogStore == nil {
			return ErrCatalogInvalid
		}
		storageIDs[library.StorageID] = struct{}{}
	}
	for id := range storageIDs {
		var storage models.Storage
		if err := tx.First(&storage, id).Error; err != nil {
			return err
		}
		if storage.CatalogConnectionEpoch >= math.MaxInt64 || storage.CatalogConnectionRevision >= math.MaxInt64 {
			return ErrCatalogBudget
		}
		replacement := storage
		replacement.CatalogConnectionRevision++
		if sourceChanged {
			replacement.CatalogConnectionEpoch++
		}
		if err := applyCatalogStorageChangeTx(tx, storage, replacement, s.changes); err != nil {
			return err
		}
		// These read-only model fields are written only by this lifecycle, never
		// accidentally reverted by an old Storage.Save or a probe completion.
		if err := tx.Table("storages").Where("id=?", id).Updates(map[string]any{"catalog_connection_epoch": replacement.CatalogConnectionEpoch, "catalog_connection_revision": replacement.CatalogConnectionRevision}).Error; err != nil {
			return err
		}
	}
	return nil
}
