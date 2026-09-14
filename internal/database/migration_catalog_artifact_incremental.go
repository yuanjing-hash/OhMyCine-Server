package database

import "gorm.io/gorm"

func migrateCatalogArtifactIncremental(db *gorm.DB) error {
	statements := []string{
		`ALTER TABLE catalog_artifact_bindings ADD COLUMN scope_mode TEXT NOT NULL DEFAULT 'full' CHECK(scope_mode IN ('full','incremental'))`,
		`ALTER TABLE catalog_artifact_bindings ADD COLUMN scope_prepared NUMERIC NOT NULL DEFAULT 0`,
		`ALTER TABLE catalog_artifact_bindings ADD COLUMN scope_entry_after_id INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE catalog_artifact_bindings ADD COLUMN scope_asset_after_id INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE catalog_artifact_bindings ADD COLUMN scope_recognition_after_id INTEGER NOT NULL DEFAULT 0`,
		`CREATE TABLE catalog_artifact_binding_items (
			binding_id TEXT NOT NULL REFERENCES catalog_artifact_bindings(id) ON DELETE CASCADE,
			entity_kind TEXT NOT NULL CHECK(entity_kind IN ('entry','recognition','asset','manifest')),
			entity_id INTEGER NOT NULL,
			status TEXT NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','running','completed','failed')),
			attempts INTEGER NOT NULL DEFAULT 0,
			outcome TEXT NOT NULL DEFAULT '' CHECK(outcome IN ('','noop','written','updated','skipped')),
			error_code TEXT NOT NULL DEFAULT '',
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY(binding_id,entity_kind,entity_id)
		)`,
		`CREATE INDEX idx_catalog_artifact_binding_items_work ON catalog_artifact_binding_items(binding_id,status,entity_kind,entity_id)`,
		`CREATE INDEX idx_catalog_artifact_binding_items_entity ON catalog_artifact_binding_items(entity_kind,entity_id,binding_id)`,
		`UPDATE media_artifact_runs AS run SET
			status='superseded', cleanup_status='skipped', cleanup_error_code='', cleanup_at=CURRENT_TIMESTAMP,
			finished_at=CURRENT_TIMESTAMP, error_code='catalog_generation_superseded', updated_at=CURRENT_TIMESTAMP
		WHERE run.job_id IS NULL
		  AND run.generation < COALESCE((SELECT library.artifact_applied_generation FROM media_libraries library WHERE library.id=run.library_id),0)
		  AND EXISTS (
			SELECT 1 FROM media_artifact_runs newer
			JOIN media_libraries library ON library.id=newer.library_id
			WHERE newer.library_id=run.library_id
			  AND newer.generation=library.artifact_applied_generation
			  AND newer.generation>run.generation
			  AND newer.status='completed' AND newer.finished_at IS NOT NULL
			  AND newer.failed_count=0 AND newer.error_code=''
		  )
		  AND NOT EXISTS (SELECT 1 FROM catalog_physical_writes physical WHERE physical.owner_kind='artifact' AND physical.owner_id=run.id)
		  AND NOT EXISTS (SELECT 1 FROM catalog_artifact_write_receipts receipt WHERE receipt.run_id=run.id AND receipt.phase IN ('prepared','conflict'))
		  AND NOT EXISTS (SELECT 1 FROM catalog_artifact_cleanup_claims claim WHERE claim.owner_run_id=run.id)
		  AND NOT EXISTS (SELECT 1 FROM media_artifacts artifact WHERE artifact.run_id=run.id)`,
	}
	for _, statement := range statements {
		if err := db.Exec(statement).Error; err != nil {
			return err
		}
	}
	return nil
}
