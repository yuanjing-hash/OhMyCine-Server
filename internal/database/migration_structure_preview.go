package database

import "gorm.io/gorm"

// Existing drafts remain valid for their original confirmation contract; their
// display manifest is unavailable and the UI must request a fresh preview.
func migrateStructurePreviewItems(db *gorm.DB) error {
	return db.Exec(`ALTER TABLE media_library_structure_repair_drafts ADD COLUMN preview_items_json TEXT NOT NULL DEFAULT ''`).Error
}
