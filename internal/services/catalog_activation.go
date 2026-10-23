package services

import (
	"context"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/updater"
	"gorm.io/gorm"
)

// ensureCatalogActivation persists rollback protection before any new-format
// write transaction. Authority must belong to the exact opened main database.
func ensureCatalogActivation(ctx context.Context, db *gorm.DB, compatibility *updater.CatalogCompatibility) error {
	if db == nil || db.Statement == nil {
		return ErrCatalogInvalid
	}
	if _, inTransaction := db.Statement.ConnPool.(gorm.TxCommitter); inTransaction {
		return ErrCatalogInvalid
	}
	var databases []struct{ Name, File string }
	if err := db.WithContext(ctx).Raw("PRAGMA database_list").Scan(&databases).Error; err != nil {
		return err
	}
	for _, database := range databases {
		if database.Name == "main" && database.File != "" {
			if err := compatibility.ValidateCatalogDatabase(database.File); err != nil {
				return err
			}
			return compatibility.EnsureCatalogFormat()
		}
	}
	return ErrCatalogInvalid
}
