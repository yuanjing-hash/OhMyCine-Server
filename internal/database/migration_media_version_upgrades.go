package database

import (
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

func migrateMediaVersionUpgrades(db *gorm.DB) error {
	if err := db.AutoMigrate(&models.MediaVersionUpgrade{}, &models.ManagedMediaTrash{}); err != nil {
		return err
	}
	if err := db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_media_upgrade_active_entry ON media_version_upgrades(library_id, entry_id) WHERE state IN ('prepared','submitted','replacement_ready','recycle_pending')`).Error; err != nil {
		return err
	}
	return db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_managed_media_trash_item ON managed_media_trashes(upgrade_id, item_key)`).Error
}
