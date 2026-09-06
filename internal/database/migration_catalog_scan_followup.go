package database

import "gorm.io/gorm"

func migrateCatalogScanFollowups(db *gorm.DB) error {
	return db.Exec(`CREATE TABLE catalog_scan_followups (
		library_id INTEGER PRIMARY KEY,
		receipt_id TEXT NOT NULL,
		scan_run_id INTEGER NOT NULL,
		generation INTEGER NOT NULL,
		artifact_generation INTEGER NOT NULL DEFAULT 0,
		source_epoch INTEGER NOT NULL,
		source_fingerprint TEXT NOT NULL,
		config_fingerprint TEXT NOT NULL,
		source_config_fingerprint TEXT NOT NULL,
		recognition_pending INTEGER NOT NULL DEFAULT 0,
		artifact_pending INTEGER NOT NULL DEFAULT 0,
		artwork_pending INTEGER NOT NULL DEFAULT 0,
		diagnosis_pending INTEGER NOT NULL DEFAULT 0,
		complete_observed INTEGER NOT NULL DEFAULT 0,
		updated_at DATETIME NOT NULL,
		FOREIGN KEY(library_id) REFERENCES media_libraries(id) ON DELETE CASCADE
	)`).Error
}
