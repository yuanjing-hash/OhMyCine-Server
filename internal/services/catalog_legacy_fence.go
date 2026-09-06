package services

import (
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

// Pre-enumeration mode selection is not authority for a later legacy write.
// Check again in its actual transaction so conversion cannot race a late writer.
func requireLegacyCatalogWriteTx(tx *gorm.DB, libraryID uint) error {
	if err := requireCatalogTransaction(tx); err != nil {
		return err
	}
	if err := requireMediaLibraryNotRetiringTx(tx, libraryID); err != nil {
		return err
	}
	var heads []models.CatalogHead
	if err := tx.Where("library_id=?", libraryID).Limit(1).Find(&heads).Error; err != nil {
		return err
	}
	if len(heads) != 0 && heads[0].Mode != "legacy" {
		return ErrCatalogFence
	}
	return nil
}
