package models

import "time"

// MediaChangeDispatch is a compact recoverable fanout cursor. It is private,
// coalesces to the latest ready revision, and is independent of feed retention.
type MediaChangeDispatch struct {
	LibraryID     uint      `gorm:"primaryKey" json:"-"`
	Revision      uint64    `json:"-"`
	AfterTargetID uint      `json:"-"`
	UpdatedAt     time.Time `json:"-"`
}
