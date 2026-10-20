package database

import "gorm.io/gorm"

// A nullable, additive policy preserves legacy probe/download timeouts. Older
// applications can continue to ignore it without rewriting any Site row.
func migrateSiteSearchTimeout(tx *gorm.DB) error {
	return tx.Exec(`ALTER TABLE sites ADD COLUMN search_timeout_seconds INTEGER CHECK(search_timeout_seconds IS NULL OR search_timeout_seconds BETWEEN 3 AND 180)`).Error
}
