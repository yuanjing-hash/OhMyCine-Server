package database

import "gorm.io/gorm"

// No startup backfill and no FK cascade: unresolved physical evidence cannot
// disappear when a queue or domain history row is deleted.
func migrateCatalogPhysicalWrites(db *gorm.DB) error {
	statements := []string{
		`CREATE TABLE catalog_physical_writes (
		 id INTEGER PRIMARY KEY AUTOINCREMENT,
		 library_id INTEGER NOT NULL,
		 owner_kind TEXT NOT NULL CHECK(owner_kind IN ('transfer','repair','reorganization','artifact','catalog_deletion','transfer_deletion')),
		 owner_id TEXT NOT NULL,
		 revision INTEGER NOT NULL CHECK(revision > 0),
		 state TEXT NOT NULL CHECK(state IN ('admitted','entered','quiescent','settled')),
		 job_id TEXT NOT NULL DEFAULT '', job_lease_hash TEXT NOT NULL DEFAULT '',
		 runtime_id TEXT NOT NULL DEFAULT '',
		 claim_digest TEXT NOT NULL DEFAULT '', owner_digest TEXT NOT NULL,
		 source_fingerprint TEXT NOT NULL, config_fingerprint TEXT NOT NULL,
		 source_epoch INTEGER NOT NULL DEFAULT 0,
		 entered_at DATETIME NOT NULL, settled_at DATETIME, updated_at DATETIME NOT NULL,
		 UNIQUE(owner_kind,owner_id)
		)`,
		`CREATE INDEX idx_catalog_physical_library_state ON catalog_physical_writes(library_id,state,id)`,
		`CREATE INDEX idx_catalog_physical_runtime_recovery ON catalog_physical_writes(state,id,runtime_id)`,
	}
	for _, statement := range statements {
		if err := db.Exec(statement).Error; err != nil {
			return err
		}
	}
	return nil
}
