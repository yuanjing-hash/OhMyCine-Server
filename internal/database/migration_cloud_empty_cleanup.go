package database

import "gorm.io/gorm"

func migrateCloudEmptyCleanup(db *gorm.DB) error {
	if err := db.Exec("ALTER TABLE media_libraries ADD COLUMN cloud_empty_cleanup_enabled INTEGER NOT NULL DEFAULT 0").Error; err != nil {
		return err
	}
	return db.Exec(`CREATE TABLE catalog_cloud_cleanup_claims (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 library_id INTEGER NOT NULL REFERENCES media_libraries(id) ON DELETE CASCADE,
 run_id TEXT NOT NULL REFERENCES media_artifact_runs(id) ON DELETE CASCADE,
 physical_write_id INTEGER NOT NULL REFERENCES catalog_physical_writes(id),
 provider_id TEXT NOT NULL,
 source_fingerprint TEXT NOT NULL,
 directory_json TEXT NOT NULL,
 status TEXT NOT NULL CHECK(status IN ('prepared','completed','kept','abandoned')),
 created_at DATETIME NOT NULL,
 updated_at DATETIME NOT NULL,
 UNIQUE(run_id, provider_id)
 ); CREATE INDEX idx_cloud_cleanup_library ON catalog_cloud_cleanup_claims(library_id,status);`).Error
}
