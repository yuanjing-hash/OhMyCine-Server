package database

import "gorm.io/gorm"

// v97 separates artifact executions by their immutable Catalog lifetime and
// enriches the existing durable workset with safe, retryable item outcomes.
// Rebuilding the run table is required to remove the legacy
// UNIQUE(library_id,generation) table constraint; no runtime/customer-specific
// repair is performed here.
func migrateArtifactRecoveryHistory(db *gorm.DB) error {
	statements := []string{
		`CREATE TABLE media_artifact_runs_v97 (
			id TEXT PRIMARY KEY, library_id INTEGER NOT NULL, generation INTEGER NOT NULL,
			catalog_binding_id TEXT NOT NULL DEFAULT '', job_id TEXT, policy_json TEXT NOT NULL,
			status TEXT NOT NULL, expected_count INTEGER NOT NULL DEFAULT 0,
			written_count INTEGER NOT NULL DEFAULT 0, updated_count INTEGER NOT NULL DEFAULT 0,
			removed_count INTEGER NOT NULL DEFAULT 0, skipped_count INTEGER NOT NULL DEFAULT 0,
			failed_count INTEGER NOT NULL DEFAULT 0, processed_count INTEGER NOT NULL DEFAULT 0,
			succeeded_count INTEGER NOT NULL DEFAULT 0, retry_count INTEGER NOT NULL DEFAULT 0,
			error_code TEXT NOT NULL DEFAULT '', cleanup_status TEXT NOT NULL DEFAULT 'pending',
			cleanup_error_code TEXT NOT NULL DEFAULT '', cleanup_at DATETIME,
			started_at DATETIME, finished_at DATETIME, created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL,
			UNIQUE(job_id),
			FOREIGN KEY(library_id) REFERENCES media_libraries(id) ON DELETE CASCADE,
			FOREIGN KEY(job_id) REFERENCES jobs(id) ON DELETE SET NULL
		)`,
		`INSERT INTO media_artifact_runs_v97(
			id,library_id,generation,catalog_binding_id,job_id,policy_json,status,expected_count,
			written_count,updated_count,removed_count,skipped_count,failed_count,processed_count,
			succeeded_count,retry_count,error_code,cleanup_status,cleanup_error_code,cleanup_at,
			started_at,finished_at,created_at,updated_at)
		 SELECT id,library_id,generation,
			CASE WHEN json_valid(policy_json) THEN COALESCE(json_extract(policy_json,'$.catalog_binding_id'),'') ELSE '' END,
			job_id,policy_json,status,expected_count,written_count,updated_count,removed_count,
			skipped_count,failed_count,
			CASE WHEN expected_count>0 THEN MIN(expected_count,written_count+updated_count+skipped_count+failed_count) ELSE written_count+updated_count+skipped_count+failed_count END,
			written_count+updated_count+skipped_count,retry_count,error_code,cleanup_status,
			cleanup_error_code,cleanup_at,started_at,finished_at,created_at,updated_at
		 FROM media_artifact_runs`,
		`DROP TABLE media_artifact_runs`,
		`ALTER TABLE media_artifact_runs_v97 RENAME TO media_artifact_runs`,
		`CREATE INDEX idx_media_artifact_runs_library ON media_artifact_runs(library_id)`,
		`CREATE INDEX idx_media_artifact_runs_generation ON media_artifact_runs(library_id,generation)`,
		`CREATE INDEX idx_media_artifact_runs_status ON media_artifact_runs(status)`,
		`CREATE INDEX idx_media_artifact_runs_binding ON media_artifact_runs(catalog_binding_id)`,
		`CREATE UNIQUE INDEX idx_media_artifact_run_lifetime ON media_artifact_runs(library_id,catalog_binding_id) WHERE catalog_binding_id<>''`,
		`ALTER TABLE catalog_artifact_binding_items ADD COLUMN error_message TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE catalog_artifact_binding_items ADD COLUMN safe_relative_path TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE catalog_artifact_binding_items ADD COLUMN retryable NUMERIC NOT NULL DEFAULT 0`,
		`ALTER TABLE catalog_artifact_binding_items ADD COLUMN next_attempt_at DATETIME`,
		`ALTER TABLE catalog_artifact_binding_items ADD COLUMN finished_at DATETIME`,
		`CREATE INDEX idx_catalog_artifact_binding_items_retry ON catalog_artifact_binding_items(binding_id,status,retryable,attempts,next_attempt_at,entity_kind,entity_id)`,
	}
	for _, statement := range statements {
		if err := db.Exec(statement).Error; err != nil {
			return err
		}
	}
	return nil
}
