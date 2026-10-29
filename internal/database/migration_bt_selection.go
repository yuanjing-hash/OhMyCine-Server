package database

import (
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

func migrateBTSelectionGroups(db *gorm.DB) error {
	if !db.Migrator().HasColumn(&models.DownloadTask{}, "ContentSelectionRequired") {
		if err := db.Migrator().AddColumn(&models.DownloadTask{}, "ContentSelectionRequired"); err != nil {
			return err
		}
	}
	if !db.Migrator().HasColumn(&models.DownloadTask{}, "BTSelectionGroupID") {
		if err := db.Migrator().AddColumn(&models.DownloadTask{}, "BTSelectionGroupID"); err != nil {
			return err
		}
	}
	return db.AutoMigrate(&models.BTSelectionGroup{}, &models.BTSelectionRef{})
}
