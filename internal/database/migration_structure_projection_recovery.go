package database

import "gorm.io/gorm"

// migrateStructureIssueProjectionRecovery repairs the upgrade boundary between
// the v70 bounded summary and the v71 complete issue projection. Rebuilding from
// catalog facts belongs to the service queue; this migration never touches
// source files, catalog entries, or manual recognition overrides.
func migrateStructureIssueProjectionRecovery(db *gorm.DB) error {
	statements := []string{
		// A completed repair already made the library healthy, but v71 could
		// leave the old diagnosis summary behind. Do not resurrect those issues.
		`UPDATE media_library_structure_diagnoses SET
			status = 'healthy', issue_count = 0, repairable_count = 0,
			unrecognized_count = 0, missing_episode_count = 0, invalid_path_count = 0,
			template_error_count = 0, duplicate_target_count = 0, sidecar_conflict_count = 0,
			issues_json = '[]', last_error_code = ''
		 WHERE status IN ('healthy', 'issues') AND library_id IN (
			SELECT l.id FROM media_libraries l
			WHERE l.structure_status = 'healthy' AND l.structure_issue_count = 0
			AND NOT EXISTS (SELECT 1 FROM media_library_structure_issues i WHERE i.library_id = l.id)
		 )`,
		// Preserve the source revisions, including a previously completed
		// automatic diagnosis. The marker requests one compatibility rebuild.
		`INSERT OR IGNORE INTO media_library_structure_auto_states
			(library_id, source_revision, diagnosed_revision, status, updated_at)
		 SELECT l.id, 1, 1, 'projection_pending', l.updated_at FROM media_libraries l
		 WHERE l.structure_status = 'issues' AND l.structure_checked_at IS NOT NULL
		 AND NOT EXISTS (SELECT 1 FROM media_library_structure_issues i WHERE i.library_id = l.id)`,
		`UPDATE media_library_structure_auto_states SET status = 'projection_pending'
		 WHERE library_id IN (
			SELECT l.id FROM media_libraries l
			WHERE l.structure_status = 'issues' AND l.structure_checked_at IS NOT NULL
			AND NOT EXISTS (SELECT 1 FROM media_library_structure_issues i WHERE i.library_id = l.id)
		 )`,
		// The diagnosis status CHECK has no pending state. Remove only this
		// obsolete projection so GET cannot advertise counts without rows.
		`DELETE FROM media_library_structure_diagnoses WHERE library_id IN (
			SELECT library_id FROM media_library_structure_auto_states WHERE status = 'projection_pending'
		 )`,
		`DELETE FROM media_library_structure_repair_drafts WHERE library_id IN (
			SELECT library_id FROM media_library_structure_auto_states WHERE status = 'projection_pending'
		 )`,
		`UPDATE media_libraries SET structure_status = 'pending', structure_issue_count = 0,
			structure_error_code = '', structure_checked_at = NULL
		 WHERE id IN (
			SELECT library_id FROM media_library_structure_auto_states WHERE status = 'projection_pending'
		 )`,
	}
	for _, statement := range statements {
		if err := db.Exec(statement).Error; err != nil {
			return err
		}
	}
	return nil
}
