package database

import "gorm.io/gorm"

func migrateCatalogArtifactWriteReceipts(db *gorm.DB) error {
	statements := []string{
		`ALTER TABLE catalog_physical_writes ADD COLUMN artifact_receipt_version INTEGER NOT NULL DEFAULT 0 CHECK(artifact_receipt_version IN (0,1))`,
		`CREATE TABLE catalog_artifact_write_receipts (
		id INTEGER PRIMARY KEY AUTOINCREMENT, library_id INTEGER NOT NULL,
		run_id TEXT NOT NULL REFERENCES media_artifact_runs(id) ON DELETE RESTRICT,
		artifact_id INTEGER NOT NULL,
		physical_write_id INTEGER NOT NULL REFERENCES catalog_physical_writes(id) ON DELETE RESTRICT,
		permit_revision INTEGER NOT NULL CHECK(permit_revision>0), revision INTEGER NOT NULL CHECK(revision>0),
		target_kind TEXT NOT NULL, relative_path TEXT NOT NULL, root_identity TEXT NOT NULL,
		policy_digest TEXT NOT NULL, source_epoch INTEGER NOT NULL,
		source_fingerprint TEXT NOT NULL, config_fingerprint TEXT NOT NULL,
		before_exists INTEGER NOT NULL CHECK(before_exists IN (0,1)),
		before_fingerprint TEXT NOT NULL, after_fingerprint TEXT NOT NULL,
		before_size INTEGER NOT NULL CHECK(before_size>=0), after_size INTEGER NOT NULL CHECK(after_size>=0),
		before_artifact_json TEXT NOT NULL, after_artifact_json TEXT NOT NULL,
		phase TEXT NOT NULL CHECK(phase IN ('prepared','reconciled_before','reconciled_after','conflict')),
		error_code TEXT NOT NULL DEFAULT '', created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL,
		UNIQUE(run_id,target_kind,relative_path))`,
		`CREATE INDEX idx_catalog_artifact_receipt_library_phase ON catalog_artifact_write_receipts(library_id,phase,id)`,
		`CREATE INDEX idx_catalog_artifact_receipt_run_phase ON catalog_artifact_write_receipts(run_id,phase,id)`,
		`CREATE INDEX idx_catalog_artifact_receipt_physical ON catalog_artifact_write_receipts(physical_write_id,id)`,
		`CREATE INDEX idx_catalog_artifact_receipt_target_phase ON catalog_artifact_write_receipts(library_id,target_kind,relative_path,phase,id)`,
		`CREATE TABLE catalog_artifact_cleanup_claims (
		artifact_id INTEGER PRIMARY KEY REFERENCES media_artifacts(id) ON DELETE CASCADE,
		library_id INTEGER NOT NULL,
		owner_run_id TEXT REFERENCES media_artifact_runs(id) ON DELETE RESTRICT,
		physical_write_id INTEGER REFERENCES catalog_physical_writes(id) ON DELETE RESTRICT,
		permit_revision INTEGER NOT NULL DEFAULT 0,
		original_status TEXT NOT NULL, manifest_digest TEXT NOT NULL,
		root_identity TEXT NOT NULL DEFAULT '', generator_policy_digest TEXT NOT NULL DEFAULT '',
		created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL,
		CHECK((owner_run_id IS NULL AND physical_write_id IS NULL AND permit_revision=0) OR
		(owner_run_id IS NOT NULL AND physical_write_id IS NOT NULL AND permit_revision>0)))`,
		`CREATE INDEX idx_catalog_artifact_cleanup_owner ON catalog_artifact_cleanup_claims(physical_write_id,owner_run_id,artifact_id)`,
		`CREATE INDEX idx_catalog_artifact_cleanup_library ON catalog_artifact_cleanup_claims(library_id,artifact_id)`,
	}
	for _, statement := range statements {
		if err := db.Exec(statement).Error; err != nil {
			return err
		}
	}
	return nil
}
