package models

import "time"

// Private provider preparation receipt. Delete these in bounded batches before
// deleting a repair; never cascade a large receipt set in one foreground writer.
type CatalogStructureDirectoryReceipt struct {
	RepairID         string    `gorm:"primaryKey" json:"-"`
	TargetRelative   string    `gorm:"primaryKey" json:"-"`
	ParentProviderID string    `json:"-"`
	PreparedAt       time.Time `json:"-"`
}
