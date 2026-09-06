package database

import (
	"context"
	"fmt"

	"gorm.io/gorm"
)

// ReadCatalogFormat is safe before migrations. A pre-v75 database has format 0;
// an existing but malformed floor table is an error, never permission to boot
// an older binary. This only reads and never opens/changes runtime data itself.
func ReadCatalogFormat(ctx context.Context, db *gorm.DB) (int, error) {
	var count int64
	if err := db.WithContext(ctx).Raw("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='catalog_format_floor'").Scan(&count).Error; err != nil {
		return 0, err
	}
	if count == 0 {
		return 0, nil
	}
	var rows []struct {
		ID     int
		Format int
	}
	if err := db.WithContext(ctx).Table("catalog_format_floor").Limit(2).Find(&rows).Error; err != nil {
		return 0, err
	}
	if len(rows) != 1 || rows[0].ID != 1 || rows[0].Format < 0 {
		return 0, fmt.Errorf("catalog format floor is invalid")
	}
	return rows[0].Format, nil
}
