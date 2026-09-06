package services

import (
	"context"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/database"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

// RecoverCatalogPhysicalRuntimeBatch uses the still-held OS database lock as
// proof the previous process is gone. It records only execution quiescence,
// never domain success. Startup calls this before admitting any worker. Empty
// historical runtime identities are deliberately not guessed or age-reaped.
func RecoverCatalogPhysicalRuntimeBatch(ctx context.Context, db *gorm.DB) (int64, error) {
	var changed int64
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		runtimeID, err := database.ExclusiveRuntimeID(tx)
		if err != nil {
			return err
		}
		if runtimeID == "" {
			return ErrCatalogInvalid
		}
		var ids []uint64
		if err := tx.Model(&models.CatalogPhysicalWrite{}).Select("id").Where("state='entered' AND runtime_id<>'' AND runtime_id<>?", runtimeID).Order("id").Limit(CatalogBatchRows).Pluck("id", &ids).Error; err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		result := tx.Model(&models.CatalogPhysicalWrite{}).Where("id IN ? AND state='entered' AND runtime_id<>'' AND runtime_id<>?", ids, runtimeID).Updates(map[string]any{"state": "quiescent", "updated_at": time.Now().UTC()})
		changed = result.RowsAffected
		return result.Error
	})
	return changed, err
}
