package database

import "gorm.io/gorm"

// v98 adds presentation-only retirement markers for durable management
// histories, plus a retry counter independent from credential waits.
// It never rewrites or deletes customer history.
func migrateManagementHistoryRetry(db *gorm.DB) error {
	for _, statement := range []string{
		`ALTER TABLE jobs ADD COLUMN failure_retry_count INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE jobs ADD COLUMN history_cleared_at DATETIME`,
		`CREATE INDEX idx_jobs_history_cleared ON jobs(history_cleared_at)`,
		`ALTER TABLE media_artifact_runs ADD COLUMN history_cleared_at DATETIME`,
		`CREATE INDEX idx_media_artifact_runs_history_cleared ON media_artifact_runs(history_cleared_at)`,
		`ALTER TABLE media_library_scan_runs ADD COLUMN history_cleared_at DATETIME`,
		`CREATE INDEX idx_media_library_scan_runs_history_cleared ON media_library_scan_runs(history_cleared_at)`,
		`ALTER TABLE follow_runs ADD COLUMN history_cleared_at DATETIME`,
		`CREATE INDEX idx_follow_runs_history_cleared ON follow_runs(history_cleared_at)`,
		`ALTER TABLE schedule_runs ADD COLUMN history_cleared_at DATETIME`,
		`CREATE INDEX idx_schedule_runs_history_cleared ON schedule_runs(history_cleared_at)`,
		`ALTER TABLE download_tasks ADD COLUMN history_cleared_at DATETIME`,
		`CREATE INDEX idx_download_tasks_history_cleared ON download_tasks(history_cleared_at)`,
		`ALTER TABLE transfer_tasks ADD COLUMN history_cleared_at DATETIME`,
		`CREATE INDEX idx_transfer_tasks_history_cleared ON transfer_tasks(history_cleared_at)`,
	} {
		if err := db.Exec(statement).Error; err != nil {
			return err
		}
	}
	return nil
}
