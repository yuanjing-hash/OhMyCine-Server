package database

import "gorm.io/gorm"

// migrateStructureMismatchClassifications adds durable counters for diagnoses
// produced by the refined naming/location classifier. Existing path_mismatch
// rows remain truthful legacy results and are refined by the next diagnosis;
// the migration never guesses from historical display strings.
func migrateStructureMismatchClassifications(db *gorm.DB) error {
	for _, statement := range []string{
		`ALTER TABLE media_library_structure_diagnoses ADD COLUMN naming_mismatch_count INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE media_library_structure_diagnoses ADD COLUMN location_mismatch_count INTEGER NOT NULL DEFAULT 0`,
	} {
		if err := db.Exec(statement).Error; err != nil {
			return err
		}
	}
	return nil
}
