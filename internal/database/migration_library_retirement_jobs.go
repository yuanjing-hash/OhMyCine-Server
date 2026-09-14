package database

import "gorm.io/gorm"

// v100 records the exact queue jobs owned by one library retirement. The rows
// have no foreign keys on purpose: they remain a stable deletion workset while
// the referenced domain and queue rows are removed in bounded transactions.
func migrateLibraryRetirementJobs(db *gorm.DB) error {
	for _, statement := range []string{
		`CREATE TABLE media_library_retirement_jobs (retirement_id TEXT NOT NULL, job_id TEXT NOT NULL, created_at DATETIME NOT NULL, PRIMARY KEY(retirement_id,job_id))`,
		`CREATE INDEX idx_library_retirement_jobs_job ON media_library_retirement_jobs(job_id)`,
		// A retirement accepted by v88-v99 predates the frozen workset. Populate
		// the same exact selected-library ownership graph during the numbered
		// migration so its first v100 worker run cannot skip a live owner.
		`INSERT OR IGNORE INTO media_library_retirement_jobs(retirement_id,job_id,created_at)
		 SELECT r.id,j.id,CURRENT_TIMESTAMP
		 FROM media_library_retirements r
		 JOIN jobs j ON j.job_type<>'media_library_retirement' AND (
			j.resource_key IN ('media-artifact-library:'||CAST(r.library_id AS TEXT),'strm-library:'||CAST(r.library_id AS TEXT),'structure-diagnosis-library:'||CAST(r.library_id AS TEXT))
			OR j.id IN (SELECT p.job_id FROM catalog_physical_writes p WHERE p.library_id=r.library_id AND p.job_id<>'')
			OR j.id IN (SELECT t.job_id FROM transfer_tasks t WHERE t.library_id=r.library_id)
			OR j.id IN (SELECT a.job_id FROM media_artifact_runs a WHERE a.library_id=r.library_id AND a.job_id IS NOT NULL)
			OR j.id IN (SELECT s.job_id FROM catalog_snapshots s WHERE s.library_id=r.library_id AND s.job_id IS NOT NULL)
			OR j.id IN (SELECT d.job_id FROM media_library_structure_diagnoses d WHERE d.library_id=r.library_id)
			OR j.id IN (SELECT p.job_id FROM media_library_structure_repairs p WHERE p.library_id=r.library_id AND p.job_id IS NOT NULL)
			OR j.id IN (SELECT o.job_id FROM media_reorganization_tasks o WHERE o.library_id=r.library_id)
			OR j.id IN (SELECT x.job_id FROM media_server_refresh_runs x WHERE x.target_id IN (SELECT t.id FROM media_server_refresh_targets t WHERE t.library_id=r.library_id))
			OR j.id IN (SELECT x.job_id FROM schedule_runs x WHERE x.schedule_id IN (SELECT d.id FROM schedule_definitions d WHERE d.target_type='media_library' AND d.target_id=CAST(r.library_id AS TEXT)))
		 )
		 WHERE r.phase<>'completed'`,
	} {
		if err := db.Exec(statement).Error; err != nil {
			return err
		}
	}
	return nil
}
