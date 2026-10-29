package database

import (
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

func migrateProviderOperations(db *gorm.DB) error {
	if err := db.AutoMigrate(&models.ProviderOperationReceipt{}); err != nil {
		return err
	}
	return db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_provider_operation_resource ON provider_operation_receipts(provider,account_key,resource_key) WHERE resource_key <> ''`).Error
}
