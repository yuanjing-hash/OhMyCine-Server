package database

import "gorm.io/gorm"

// Empty additive schema only. Old collection UUIDs are adopted in bounded
// per-candidate preparation; startup does not scan or rebuild user catalogues.
func migrateCatalogCollections(db *gorm.DB) error {
	for _, statement := range []string{
		`CREATE TABLE catalog_collection_identities (tmdb_collection_id INTEGER PRIMARY KEY CHECK(tmdb_collection_id>0), collection_id TEXT NOT NULL UNIQUE)`,
		`CREATE TABLE catalog_collection_member_facts (snapshot_id TEXT NOT NULL REFERENCES catalog_snapshots(id) ON DELETE RESTRICT, library_id INTEGER NOT NULL, work_key TEXT NOT NULL, tmdb_collection_id INTEGER NOT NULL DEFAULT 0, tmdb_movie_id INTEGER NOT NULL DEFAULT 0, name TEXT NOT NULL DEFAULT '', poster_path TEXT NOT NULL DEFAULT '', backdrop_path TEXT NOT NULL DEFAULT '', metadata_updated_at DATETIME, tombstone BOOLEAN NOT NULL DEFAULT false, PRIMARY KEY(snapshot_id,work_key))`,
		`CREATE INDEX idx_catalog_collection_member_identity ON catalog_collection_member_facts(tmdb_collection_id,library_id,work_key)`,
		`CREATE TABLE catalog_collection_preparations (snapshot_id TEXT PRIMARY KEY REFERENCES catalog_snapshots(id) ON DELETE RESTRICT, state TEXT NOT NULL CHECK(state IN ('preparing','prepared')), after_work TEXT NOT NULL DEFAULT '', input_row_count INTEGER NOT NULL, input_byte_count INTEGER NOT NULL, row_count INTEGER NOT NULL DEFAULT 0, byte_count INTEGER NOT NULL DEFAULT 0)`,
	} {
		if err := db.Exec(statement).Error; err != nil {
			return err
		}
	}
	return nil
}
