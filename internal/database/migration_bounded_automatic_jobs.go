package database

import "gorm.io/gorm"

// Startup migration: the server runtime lock is held and workers have not
// started. Obsolete automatic jobs are discarded, never replayed or widened.
// Published artifact manifest rows remain as file provenance; deleting their
// run would cascade-delete that inventory, so referenced runs become inert.
func migrateBoundedAutomaticJobs(db *gorm.DB) error {
	statements := []string{
		`ALTER TABLE transfer_tasks ADD COLUMN catalog_published_at DATETIME`,
		`CREATE TEMP TABLE obsolete_automatic_runs AS
		 SELECT id, library_id FROM media_artifact_runs
		 WHERE catalog_binding_id = '' AND (
		 NOT json_valid(policy_json) OR
		 (COALESCE(json_extract(CASE WHEN json_valid(policy_json) THEN policy_json ELSE '{}' END,'$.scope_version'),0)<>1 AND
		 (COALESCE(json_extract(CASE WHEN json_valid(policy_json) THEN policy_json ELSE '{}' END,'$.scan_kind'),'') NOT IN ('initial','manual','full','strm_full_manual','reorganization')
		 OR json_extract(CASE WHEN json_valid(policy_json) THEN policy_json ELSE '{}' END,'$.scan_partial')=1)))`,
		`CREATE TEMP TABLE obsolete_automatic_jobs AS
		 SELECT j.id FROM jobs j WHERE
		 (j.job_type='media_artifact' AND json_valid(j.payload_json)
		 AND json_extract(CASE WHEN json_valid(j.payload_json) THEN j.payload_json ELSE '{}' END,'$.artifact_run_id') IN (SELECT id FROM obsolete_automatic_runs))
		 OR (j.job_type='media_library_recognition' AND j.status<>'completed'
		 AND json_valid(j.payload_json) AND json_extract(CASE WHEN json_valid(j.payload_json) THEN j.payload_json ELSE '{}' END,'$.scan_run_id') IN
		 (SELECT id FROM media_library_scan_runs WHERE kind IN ('event','incremental','catch_up','transfer_batch') OR partial=1))`,
		`DELETE FROM catalog_artifact_cleanup_claims WHERE owner_run_id IN (SELECT id FROM obsolete_automatic_runs)`,
		`DELETE FROM catalog_artifact_write_receipts WHERE run_id IN (SELECT id FROM obsolete_automatic_runs)`,
		`DELETE FROM catalog_physical_writes WHERE owner_kind='artifact' AND owner_id IN (SELECT id FROM obsolete_automatic_runs)`,
		`UPDATE media_library_scan_runs SET status='cancelled', phase='cancelled', finished_at=CURRENT_TIMESTAMP
		 WHERE id IN (SELECT json_extract(CASE WHEN json_valid(j.payload_json) THEN j.payload_json ELSE '{}' END,'$.scan_run_id') FROM jobs j
		 JOIN obsolete_automatic_jobs o ON o.id=j.id WHERE j.job_type='media_library_recognition')`,
		`DELETE FROM media_library_retirement_jobs WHERE job_id IN (SELECT id FROM obsolete_automatic_jobs)`,
		`DELETE FROM jobs WHERE id IN (SELECT id FROM obsolete_automatic_jobs)`,
		`UPDATE media_artifact_runs SET job_id=NULL,status='superseded',cleanup_status='skipped',
		 cleanup_error_code='',error_code='',finished_at=CURRENT_TIMESTAMP,updated_at=CURRENT_TIMESTAMP
		 WHERE id IN (SELECT id FROM obsolete_automatic_runs)`,
		`UPDATE media_libraries SET artifact_status='superseded',artifact_error=''
		 WHERE id IN (SELECT library_id FROM obsolete_automatic_runs)
		 AND NOT EXISTS (SELECT 1 FROM media_artifact_runs r WHERE r.library_id=media_libraries.id AND r.job_id IS NOT NULL AND r.status IN ('queued','running'))`,
		`DELETE FROM media_artifact_runs WHERE id IN (SELECT id FROM obsolete_automatic_runs)
		 AND NOT EXISTS (SELECT 1 FROM media_artifacts a WHERE a.run_id=media_artifact_runs.id)`,
		`DROP TABLE obsolete_automatic_jobs`,
		`DROP TABLE obsolete_automatic_runs`,
	}
	for _, statement := range statements {
		if err := db.Exec(statement).Error; err != nil {
			return err
		}
	}
	return nil
}
