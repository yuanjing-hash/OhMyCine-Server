package database

import "gorm.io/gorm"

// Empty ledger only; conversion never runs inside startup migrations.
func migrateCatalogConversion(db *gorm.DB) error {
	return db.Exec(`CREATE TABLE catalog_conversion_manifests (snapshot_id TEXT PRIMARY KEY REFERENCES catalog_snapshots(id) ON DELETE RESTRICT, state TEXT NOT NULL CHECK(state IN ('capturing','captured','verified')), fence_digest TEXT NOT NULL, input_digest TEXT NOT NULL DEFAULT '', input_rows INTEGER NOT NULL DEFAULT 0, updated_at DATETIME NOT NULL)`).Error
}
