package services

import (
	"time"

	"github.com/google/uuid"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/database"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

// initializeFreshQuarkCatalogTx initializes an empty NEW library; it is not a
// legacy converter and refuses any existing catalog or legacy media facts.
// Stable CatalogIdentity anchors then survive observation tombstones/recovery.
func initializeFreshQuarkCatalogTx(tx *gorm.DB, library models.MediaLibrary, storage models.Storage, profile models.MediaClassificationProfile) error {
	if err := requireCatalogTransaction(tx); err != nil {
		return err
	}
	if storage.Type != "quark" || library.ID == 0 || library.StorageID != storage.ID || library.ProfileID != profile.ID || library.ProviderRootID == "" {
		return ErrCatalogInvalid
	}
	floor, err := database.ReadCatalogFormat(tx.Statement.Context, tx)
	if err != nil {
		return err
	}
	if floor > CatalogFormat {
		return ErrCatalogInvalid
	}
	for _, model := range []any{&models.CatalogHead{}, &models.MediaLibraryEntry{}, &models.MediaLibraryRecognition{}, &models.MediaLibrarySourceAsset{}, &models.MediaManagedItem{}} {
		var count int64
		if err = tx.Model(model).Where("library_id = ?", library.ID).Count(&count).Error; err != nil {
			return err
		}
		if count != 0 {
			return ErrCatalogFence
		}
	}
	now := time.Now().UTC()
	head := models.CatalogHead{LibraryID: library.ID, Mode: "versioned", Revision: 1, SourceEpoch: 1, SourceFingerprint: catalogSourceFingerprint(library, storage), ConfigFingerprint: catalogConfigFingerprint(library, storage, profile), UpdatedAt: now}
	empty := models.CatalogSnapshot{ID: uuid.NewString(), LibraryID: library.ID, Kind: "base", State: "published", ParentRevision: 0, SourceEpoch: head.SourceEpoch, SourceFingerprint: head.SourceFingerprint, ConfigFingerprint: head.ConfigFingerprint, OwnerTokenHash: catalogTokenHash(uuid.NewString()), LeaseExpiresAt: now, PublishedRevision: 1, CreatedAt: now, UpdatedAt: now}
	if err = tx.Create(&head).Error; err != nil {
		return err
	}
	if err = tx.Create(&empty).Error; err != nil {
		return err
	}
	if err = tx.Create(&models.CatalogCollectionPreparation{SnapshotID: empty.ID, State: "prepared"}).Error; err != nil {
		return err
	}
	if err = tx.Create(&models.CatalogHeadLayer{LibraryID: library.ID, Rank: 0, SnapshotID: empty.ID}).Error; err != nil {
		return err
	}
	return tx.Exec("UPDATE catalog_format_floor SET format = ? WHERE id = 1 AND format < ?", CatalogFormat, CatalogFormat).Error
}
