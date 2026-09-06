package services

import (
	"context"
	"errors"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func (s *MediaChangeService) recordPendingCleanupTx(tx *gorm.DB, libraryID uint, generation, maxSequence uint64, now time.Time) error {
	return tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "library_id"}}, DoUpdates: clause.Assignments(map[string]any{
		"generation":     gorm.Expr("MAX(media_change_pending_cleanups.generation,excluded.generation)"),
		"max_sequence":   gorm.Expr("MAX(media_change_pending_cleanups.max_sequence,excluded.max_sequence)"),
		"after_sequence": 0, "updated_at": now,
	})}).Create(&models.MediaChangePendingCleanup{LibraryID: libraryID, Generation: generation, MaxSequence: maxSequence, UpdatedAt: now}).Error
}

// CleanupPendingBatch is a restart-safe, bounded background writer. Its cutoff
// never expands to pending events inserted after the readiness publication.
func (s *MediaChangeService) CleanupPendingBatch(ctx context.Context) (bool, error) {
	found := false
	err := withBackgroundTransaction(ctx, s.db, s.writeAdmission, func(tx *gorm.DB) error {
		var marker models.MediaChangePendingCleanup
		if err := tx.Order("updated_at,library_id").First(&marker).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		} else if err != nil {
			return err
		}
		found = true
		var ids []uint64
		if err := tx.Model(&models.MediaLibraryChange{}).Where("library_id = ? AND state = ? AND generation <= ? AND sequence > ? AND sequence <= ?", marker.LibraryID, models.MediaLibraryChangePending, marker.Generation, marker.AfterSequence, marker.MaxSequence).Order("sequence").Limit(mediaChangeDispatchBatch).Pluck("sequence", &ids).Error; err != nil {
			return err
		}
		if len(ids) == 0 {
			return tx.Delete(&marker).Error
		}
		if err := tx.Where("sequence IN ? AND state = ?", ids, models.MediaLibraryChangePending).Delete(&models.MediaLibraryChange{}).Error; err != nil {
			return err
		}
		return tx.Model(&marker).Updates(map[string]any{"after_sequence": ids[len(ids)-1], "updated_at": time.Now().UTC()}).Error
	})
	return found, err
}
