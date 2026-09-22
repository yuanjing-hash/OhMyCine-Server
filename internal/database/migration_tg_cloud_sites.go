package database

import "gorm.io/gorm"

func migrateTGCloudSites(db *gorm.DB) error {
	return db.Exec(`ALTER TABLE sites ADD COLUMN cloud_config_json TEXT NOT NULL DEFAULT '{}'`).Error
}
