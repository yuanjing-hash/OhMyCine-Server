package models

import "time"

// Cleanup only reclaims superseded pending invalidations. Ready rows are never
// covered by this marker; feed cursor retention remains a separate contract.
type MediaChangePendingCleanup struct {
	LibraryID     uint      `gorm:"primaryKey" json:"-"`
	Generation    uint64    `json:"-"`
	MaxSequence   uint64    `json:"-"`
	AfterSequence uint64    `json:"-"`
	UpdatedAt     time.Time `json:"-"`
}
