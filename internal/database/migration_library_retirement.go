package database

import (
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func migrateMediaLibraryRetirements(tx *gorm.DB) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS media_library_retirements (id TEXT PRIMARY KEY, library_id INTEGER NOT NULL UNIQUE, actor_id INTEGER NOT NULL, job_id TEXT NOT NULL, source_epoch INTEGER NOT NULL, source_fingerprint TEXT NOT NULL, config_fingerprint TEXT NOT NULL, phase TEXT NOT NULL CHECK(phase IN ('queued','draining','owners','snapshots','derived','anchors','finalizing','completed','failed')), cursor INTEGER NOT NULL DEFAULT 0, revision INTEGER NOT NULL DEFAULT 1, processed_rows INTEGER NOT NULL DEFAULT 0, last_error_code TEXT NOT NULL DEFAULT '', created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL, completed_at DATETIME)`,
		`CREATE INDEX IF NOT EXISTS idx_library_retirements_pending ON media_library_retirements(phase,updated_at,id)`,
		`CREATE INDEX IF NOT EXISTS idx_catalog_snapshots_library_retirement ON catalog_snapshots(library_id,id)`,
		`CREATE INDEX IF NOT EXISTS idx_catalog_snapshots_job_retirement ON catalog_snapshots(job_id,id)`,
		`CREATE INDEX IF NOT EXISTS idx_favorites_library_retirement ON player_media_favorites(library_id)`,
		`CREATE INDEX IF NOT EXISTS idx_collection_items_library_retirement ON player_media_collection_items(library_id,id)`,
		`CREATE INDEX IF NOT EXISTS idx_acquisitions_library_retirement ON media_acquisitions(target_library_id,id)`,
		`CREATE INDEX IF NOT EXISTS idx_schedules_target_retirement ON schedule_definitions(target_type,target_id,id)`,
	}
	for _, statement := range statements {
		if err := tx.Exec(statement).Error; err != nil {
			return err
		}
	}
	now := time.Now().UTC()
	return tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&models.QueuePolicy{JobType: "media_library_retirement", Concurrency: 1, ResourceConcurrency: 1, MaxAttempts: 5, LeaseSeconds: 30, Revision: 1, CreatedAt: now, UpdatedAt: now}).Error
}
