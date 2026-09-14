package database

import "gorm.io/gorm"

// Persist the same exact conflict categories that the paged issue API filters.
// Derive each diagnosis independently; no job, source, media or repair is changed.
func migrateStructureConflictCounters(tx *gorm.DB) error {
	for _, statement := range []string{
		`ALTER TABLE media_library_structure_diagnoses ADD COLUMN recognition_suspect_conflict_count INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE media_library_structure_diagnoses ADD COLUMN catalog_duplicate_conflict_count INTEGER NOT NULL DEFAULT 0`,
		`UPDATE media_library_structure_diagnoses AS d SET
		duplicate_target_count=(SELECT COUNT(*) FROM media_library_structure_issues i WHERE i.library_id=d.library_id AND i.diagnosis_job_id=d.job_id AND i.generation=d.generation AND i.code='duplicate_target'),
		recognition_suspect_conflict_count=(SELECT COUNT(*) FROM media_library_structure_issues i WHERE i.library_id=d.library_id AND i.diagnosis_job_id=d.job_id AND i.generation=d.generation AND i.code='recognition_suspect_conflict'),
		catalog_duplicate_conflict_count=(SELECT COUNT(*) FROM media_library_structure_issues i WHERE i.library_id=d.library_id AND i.diagnosis_job_id=d.job_id AND i.generation=d.generation AND i.code='catalog_duplicate_conflict')`,
	} {
		if err := tx.Exec(statement).Error; err != nil {
			return err
		}
	}
	return nil
}
