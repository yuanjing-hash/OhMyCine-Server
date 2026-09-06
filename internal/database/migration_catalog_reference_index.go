package database

import "gorm.io/gorm"

func migrateCatalogReferenceIndex(db *gorm.DB) error {
	for _, statement := range []string{
		`CREATE INDEX idx_catalog_reference_owner ON catalog_snapshot_references(owner_kind,owner_id,snapshot_id)`,
		`CREATE INDEX idx_catalog_preparation ON catalog_snapshots(kind,library_id,id) WHERE state IN ('building','validating','ready')`,
	} {
		if err := db.Exec(statement).Error; err != nil {
			return err
		}
	}
	return nil
}
