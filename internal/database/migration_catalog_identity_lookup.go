package database

import "gorm.io/gorm"

func migrateCatalogIdentityLookup(db *gorm.DB) error {
	// Versioned identity tables are introduced in this unreleased architecture;
	// no production legacy entry table is rewritten or indexed at startup.
	return db.Exec(`CREATE INDEX idx_catalog_identity_scope_anchor ON catalog_identities(library_id,source_epoch,entity_kind,anchor_id)`).Error
}
