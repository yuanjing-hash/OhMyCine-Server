package database

import "gorm.io/gorm"

func migrateStructureRepairProgress(db *gorm.DB) error {
	for _, statement := range []string{
		"ALTER TABLE media_library_structure_repairs ADD COLUMN current_action TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE media_library_structure_repairs ADD COLUMN current_item TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE media_library_structure_repairs ADD COLUMN current_batch_size INTEGER NOT NULL DEFAULT 0",
	} {
		if err := db.Exec(statement).Error; err != nil {
			return err
		}
	}
	return nil
}
