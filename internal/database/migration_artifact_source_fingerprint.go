package database

import "gorm.io/gorm"

func migrateArtifactSourceFingerprint(db *gorm.DB) error {
	return db.Exec("ALTER TABLE media_artifacts ADD COLUMN source_fingerprint TEXT NOT NULL DEFAULT ''").Error
}
