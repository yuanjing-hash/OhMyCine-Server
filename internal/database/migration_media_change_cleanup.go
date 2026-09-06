package database

import "gorm.io/gorm"

func migrateMediaChangePendingCleanup(db *gorm.DB) error {
	for _, sql := range []string{
		`CREATE TABLE media_change_pending_cleanups (library_id INTEGER PRIMARY KEY REFERENCES media_libraries(id) ON DELETE CASCADE, generation INTEGER NOT NULL, max_sequence INTEGER NOT NULL, after_sequence INTEGER NOT NULL DEFAULT 0, updated_at DATETIME NOT NULL)`,
		`CREATE INDEX idx_media_change_pending_kind ON media_library_changes(library_id,state,generation,kind,revision DESC)`,
		`CREATE INDEX idx_media_change_pending_sequence ON media_library_changes(library_id,state,sequence)`,
		`CREATE INDEX idx_media_change_pending_revision ON media_library_changes(library_id,state,revision DESC)`,
	} {
		if err := db.Exec(sql).Error; err != nil {
			return err
		}
	}
	return nil
}
