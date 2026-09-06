package services

import (
	"context"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

// A cursor pass examines at most 250 scan runs and deletes at most 250 staging
// rows. Filtering run age/status AFTER a primary-key page avoids an unbounded
// status/age scan. The existing run_id staging index services <=250 point keys.
func (s *CatalogSnapshotStore) CleanupLegacyScanStaging(ctx context.Context, afterRunID uint, now time.Time) (uint, int64, error) {
	var runs []models.MediaLibraryScanRun
	if err := s.readDB.WithContext(ctx).Select("id,status,started_at").Where("id>?", afterRunID).Order("id").Limit(CatalogBatchRows).Find(&runs).Error; err != nil {
		return afterRunID, 0, err
	}
	if len(runs) == 0 {
		return 0, 0, nil
	}
	cutoff := now.UTC().Add(-mediaLibraryStagingRetention)
	eligible := make([]uint, 0, len(runs))
	for _, run := range runs {
		if (run.Status == "failed" || run.Status == "superseded" || run.Status == "success") && run.StartedAt.Before(cutoff) {
			eligible = append(eligible, run.ID)
		}
	}
	next := runs[len(runs)-1].ID
	if len(eligible) == 0 {
		return next, 0, nil
	}
	var rows []models.MediaLibraryScanStaging
	if err := s.readDB.WithContext(ctx).Select("id,run_id").Where("run_id IN ?", eligible).Order("run_id,id").Limit(CatalogBatchRows).Find(&rows).Error; err != nil {
		return afterRunID, 0, err
	}
	if len(rows) == 0 {
		return next, 0, nil
	}
	ids := make([]uint, len(rows))
	for index, row := range rows {
		ids[index] = row.ID
	}
	var deleted int64
	err := s.writeCatalogBatch(ctx, func(tx *gorm.DB) error {
		// Recheck eligibility in the writer, including restart/status races.
		eligibleRuns := tx.Model(&models.MediaLibraryScanRun{}).Select("id").Where("id IN ? AND status IN ? AND started_at<?", eligible, []string{"failed", "superseded", "success"}, cutoff)
		result := tx.Where("id IN ? AND run_id IN (?)", ids, eligibleRuns).Delete(&models.MediaLibraryScanStaging{})
		deleted = result.RowsAffected
		return result.Error
	})
	if err != nil {
		return afterRunID, 0, err
	}
	// Revisit the first run whose rows were selected; the next pass either
	// removes its remaining prefix or advances when no eligible rows remain.
	return rows[0].RunID - 1, deleted, nil
}
