package database

import "gorm.io/gorm"

func migrateMediaChangeDispatch(db *gorm.DB) error {
	return db.Exec(`CREATE TABLE media_change_dispatches (
		library_id INTEGER PRIMARY KEY,
		revision INTEGER NOT NULL CHECK(revision > 0),
		after_target_id INTEGER NOT NULL DEFAULT 0,
		updated_at DATETIME NOT NULL,
		FOREIGN KEY(library_id) REFERENCES media_libraries(id) ON DELETE CASCADE
	)`).Error
}
