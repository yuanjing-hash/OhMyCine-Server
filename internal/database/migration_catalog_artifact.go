package database

import "gorm.io/gorm"

// v78 is additive only. Binding/backfill is part of a catalog publication, not
// startup migration, and never scans or changes a media/source filesystem.
func migrateCatalogArtifactBindings(db *gorm.DB) error {
	statements := []string{
		`CREATE TABLE catalog_artifact_bindings (id TEXT PRIMARY KEY, library_id INTEGER NOT NULL REFERENCES catalog_heads(library_id) ON DELETE RESTRICT, generation INTEGER NOT NULL, head_revision INTEGER NOT NULL, source_epoch INTEGER NOT NULL, source_fingerprint TEXT NOT NULL, config_fingerprint TEXT NOT NULL, source_config_fingerprint TEXT NOT NULL, layers_json TEXT NOT NULL, state TEXT NOT NULL CHECK(state IN ('pending','scheduled','running','applying','completed','superseded','failed')), created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL, UNIQUE(library_id,generation,head_revision))`,
		`CREATE INDEX idx_catalog_artifact_pending ON catalog_artifact_bindings(state,library_id,generation,id)`,
		`ALTER TABLE catalog_artifact_bindings ADD COLUMN finalize_after_id INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE catalog_artifact_bindings ADD COLUMN recovery_attempts INTEGER NOT NULL DEFAULT 0`,
	}
	for _, statement := range statements {
		if err := db.Exec(statement).Error; err != nil {
			return err
		}
	}
	if !db.Migrator().HasColumn("media_artifacts", "catalog_binding_id") {
		if err := db.Exec(`ALTER TABLE media_artifacts ADD COLUMN catalog_binding_id TEXT NOT NULL DEFAULT ''`).Error; err != nil {
			return err
		}
	}
	return db.Exec(`CREATE INDEX IF NOT EXISTS idx_catalog_artifact_finalize ON media_artifacts(library_id,target_kind,active,id)`).Error
}
