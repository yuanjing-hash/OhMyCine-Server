package database

import "gorm.io/gorm"

// Constant defaults only; no media enumeration or credential-derived values.
func migrateCatalogConnectionLifetimes(db *gorm.DB) error {
	if err := db.Exec("ALTER TABLE storages ADD COLUMN catalog_connection_epoch INTEGER NOT NULL DEFAULT 0").Error; err != nil {
		return err
	}
	return db.Exec("ALTER TABLE storages ADD COLUMN catalog_connection_revision INTEGER NOT NULL DEFAULT 0").Error
}
