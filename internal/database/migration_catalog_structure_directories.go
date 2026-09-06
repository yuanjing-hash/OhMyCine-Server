package database

import "gorm.io/gorm"

func migrateCatalogStructureDirectoryReceipts(db *gorm.DB) error {
	return db.Exec(`CREATE TABLE catalog_structure_directory_receipts (repair_id TEXT NOT NULL REFERENCES media_library_structure_repairs(id) ON DELETE RESTRICT, target_relative TEXT NOT NULL, parent_provider_id TEXT NOT NULL, prepared_at DATETIME NOT NULL, PRIMARY KEY(repair_id,target_relative))`).Error
}
