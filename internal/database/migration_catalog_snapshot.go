package database

import "gorm.io/gorm"

// Only empty tables and indexes are created here. Backfill/activation belongs to
// the resumable per-library converter after all catalog consumers are migrated.
func migrateCatalogSnapshots(db *gorm.DB) error {
	statements := []string{
		`CREATE TABLE catalog_format_floor (id INTEGER PRIMARY KEY CHECK(id=1), format INTEGER NOT NULL CHECK(format>=0))`,
		`INSERT INTO catalog_format_floor(id,format) VALUES(1,0)`,
		`CREATE TABLE catalog_heads (library_id INTEGER PRIMARY KEY REFERENCES media_libraries(id) ON DELETE RESTRICT, mode TEXT NOT NULL CHECK(mode IN ('legacy','converting','versioned')), revision INTEGER NOT NULL DEFAULT 0, source_epoch INTEGER NOT NULL CHECK(source_epoch>0), source_fingerprint TEXT NOT NULL, config_fingerprint TEXT NOT NULL, updated_at DATETIME NOT NULL)`,
		`CREATE TABLE catalog_snapshots (id TEXT PRIMARY KEY, library_id INTEGER NOT NULL REFERENCES catalog_heads(library_id) ON DELETE RESTRICT, kind TEXT NOT NULL CHECK(kind IN ('base','delta')), state TEXT NOT NULL CHECK(state IN ('building','validating','ready','published','abandoned','gc')), parent_revision INTEGER NOT NULL, source_epoch INTEGER NOT NULL, source_fingerprint TEXT NOT NULL, config_fingerprint TEXT NOT NULL, owner_token_hash TEXT NOT NULL, job_id TEXT REFERENCES jobs(id) ON DELETE RESTRICT, job_lease_hash TEXT NOT NULL DEFAULT '', lease_expires_at DATETIME NOT NULL, row_count INTEGER NOT NULL DEFAULT 0, byte_count INTEGER NOT NULL DEFAULT 0, published_revision INTEGER NOT NULL DEFAULT 0, created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL)`,
		`CREATE UNIQUE INDEX idx_catalog_one_base_candidate ON catalog_snapshots(library_id) WHERE kind='base' AND state IN ('building','validating','ready')`,
		`CREATE INDEX idx_catalog_snapshot_gc ON catalog_snapshots(state,updated_at,id)`,
		`CREATE TABLE catalog_head_layers (library_id INTEGER NOT NULL REFERENCES catalog_heads(library_id) ON DELETE RESTRICT, rank INTEGER NOT NULL CHECK(rank BETWEEN 0 AND 8), snapshot_id TEXT NOT NULL REFERENCES catalog_snapshots(id) ON DELETE RESTRICT, PRIMARY KEY(library_id,rank), UNIQUE(library_id,snapshot_id))`,
		`CREATE INDEX idx_catalog_layer_snapshot ON catalog_head_layers(snapshot_id)`,
		`CREATE TABLE catalog_identities (library_id INTEGER NOT NULL REFERENCES catalog_heads(library_id) ON DELETE RESTRICT, source_epoch INTEGER NOT NULL, entity_kind TEXT NOT NULL CHECK(entity_kind IN ('entry','recognition','asset')), source_key TEXT NOT NULL, anchor_id INTEGER NOT NULL, created_at DATETIME NOT NULL, PRIMARY KEY(library_id,source_epoch,entity_kind,source_key), UNIQUE(entity_kind,anchor_id))`,
		`CREATE TABLE catalog_entry_facts (snapshot_id TEXT NOT NULL REFERENCES catalog_snapshots(id) ON DELETE RESTRICT, id INTEGER NOT NULL REFERENCES media_library_entries(id) ON DELETE RESTRICT, library_id INTEGER NOT NULL, relative_path TEXT NOT NULL, provider_id TEXT NOT NULL, recognition_id INTEGER REFERENCES media_library_recognitions(id) ON DELETE RESTRICT, size INTEGER NOT NULL, modified_at DATETIME, media_type TEXT NOT NULL, title TEXT NOT NULL, work_key TEXT NOT NULL, series_title TEXT NOT NULL, season INTEGER, episode INTEGER, match_status TEXT NOT NULL, tmdb_id INTEGER, release_year INTEGER, match_confidence REAL, recognition_error_code TEXT NOT NULL, category_name TEXT NOT NULL, matched_rule_id TEXT, last_generation INTEGER NOT NULL, created_at DATETIME, updated_at DATETIME, tombstone BOOLEAN NOT NULL DEFAULT false, shared_override_mask INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(snapshot_id,id))`,
		`CREATE INDEX idx_catalog_entry_path ON catalog_entry_facts(snapshot_id,relative_path)`,
		`CREATE INDEX idx_catalog_entry_recognition ON catalog_entry_facts(snapshot_id,recognition_id)`,
		`CREATE TABLE catalog_recognition_facts (snapshot_id TEXT NOT NULL REFERENCES catalog_snapshots(id) ON DELETE RESTRICT, id INTEGER NOT NULL REFERENCES media_library_recognitions(id) ON DELETE RESTRICT, library_id INTEGER NOT NULL, source_key TEXT NOT NULL, input_fingerprint TEXT NOT NULL, profile_id INTEGER NOT NULL, profile_revision INTEGER NOT NULL, status TEXT NOT NULL, error_code TEXT NOT NULL, media_type TEXT NOT NULL, title TEXT NOT NULL, release_year INTEGER, tmdb_id INTEGER, confidence REAL, category_name TEXT NOT NULL, matched_rule_id TEXT, metadata_json TEXT NOT NULL, manual_override BOOLEAN NOT NULL, last_generation INTEGER NOT NULL, created_at DATETIME, updated_at DATETIME, tombstone BOOLEAN NOT NULL DEFAULT false, work_key TEXT NOT NULL, PRIMARY KEY(snapshot_id,id))`,
		`CREATE INDEX idx_catalog_recognition_source ON catalog_recognition_facts(snapshot_id,source_key)`,
		`CREATE TABLE catalog_source_asset_facts (snapshot_id TEXT NOT NULL REFERENCES catalog_snapshots(id) ON DELETE RESTRICT, id INTEGER NOT NULL REFERENCES media_library_source_assets(id) ON DELETE RESTRICT, library_id INTEGER NOT NULL, generation INTEGER NOT NULL, provider_id TEXT NOT NULL, parent_provider_id TEXT NOT NULL, relative_path TEXT NOT NULL, name TEXT NOT NULL, extension TEXT NOT NULL, size INTEGER NOT NULL, modified_at DATETIME, hash_hint TEXT NOT NULL, active BOOLEAN NOT NULL, created_at DATETIME, updated_at DATETIME, tombstone BOOLEAN NOT NULL DEFAULT false, PRIMARY KEY(snapshot_id,id))`,
		`CREATE INDEX idx_catalog_asset_path ON catalog_source_asset_facts(snapshot_id,relative_path)`,
		`CREATE TABLE catalog_snapshot_references (snapshot_id TEXT NOT NULL REFERENCES catalog_snapshots(id) ON DELETE RESTRICT, owner_kind TEXT NOT NULL, owner_id TEXT NOT NULL, created_at DATETIME NOT NULL, PRIMARY KEY(snapshot_id,owner_kind,owner_id))`,
	}
	for _, statement := range statements {
		if err := db.Exec(statement).Error; err != nil {
			return err
		}
	}
	return nil
}
