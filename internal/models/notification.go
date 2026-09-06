package models

import "time"

// One receipt per account/job. It acknowledges one occurrence, never changes
// the underlying business status or grants continued access after revocation.
type NotificationReceipt struct {
	UserID     uint   `gorm:"primaryKey"`
	JobID      string `gorm:"primaryKey"`
	Occurrence uint64
	ReadAt     time.Time
}
