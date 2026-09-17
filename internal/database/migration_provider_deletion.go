package database

import "gorm.io/gorm"

func migrateProviderDeletion(db *gorm.DB) error {
	for _, statement := range []string{
		"ALTER TABLE media_library_provider_events ADD COLUMN source_fingerprint TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE media_library_provider_events ADD COLUMN resolution_code TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE media_library_provider_events ADD COLUMN resolution_reason TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE media_library_provider_events ADD COLUMN retry_after DATETIME",
		"CREATE INDEX idx_library_provider_event_pending ON media_library_provider_events(library_id, processed_at, resolution_code, retry_after)",
		`CREATE TABLE media_library_deletion_evidences (
		library_id INTEGER NOT NULL REFERENCES media_libraries(id) ON DELETE CASCADE,
		source_fingerprint TEXT NOT NULL,
		provider_id TEXT NOT NULL,
		event_time DATETIME NOT NULL,
		created_at DATETIME NOT NULL,
		PRIMARY KEY(library_id, source_fingerprint, provider_id))`,
		`CREATE TABLE media_library_provider_path_batches (
		scan_run_id INTEGER PRIMARY KEY,
		library_id INTEGER NOT NULL REFERENCES media_libraries(id) ON DELETE CASCADE,
		source_fingerprint TEXT NOT NULL,
		manifest_fingerprint TEXT NOT NULL,
		observed_at DATETIME NOT NULL,
		published_at DATETIME)`,
		`CREATE TABLE media_library_provider_paths (
		scan_run_id INTEGER NOT NULL REFERENCES media_library_provider_path_batches(scan_run_id) ON DELETE CASCADE,
		library_id INTEGER NOT NULL REFERENCES media_libraries(id) ON DELETE CASCADE,
		source_fingerprint TEXT NOT NULL,
		provider_id TEXT NOT NULL,
		parent_provider_id TEXT NOT NULL,
		relative_path TEXT NOT NULL,
		is_dir NUMERIC NOT NULL,
		observed_at DATETIME NOT NULL,
		PRIMARY KEY(library_id, source_fingerprint, provider_id, scan_run_id))`,
		"CREATE INDEX idx_provider_directory_path ON media_library_provider_paths(library_id, source_fingerprint, relative_path)",
	} {
		if err := db.Exec(statement).Error; err != nil {
			return err
		}
	}
	return nil
}
