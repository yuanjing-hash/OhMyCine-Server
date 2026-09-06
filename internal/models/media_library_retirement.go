package models

import "time"

// MediaLibraryRetirement is a small durable DB-only deletion receipt. There is
// deliberately no FK to the library or job: both may outlive their configuration.
// Phase/cursor stay private; public clients receive an allowlisted summary.
type MediaLibraryRetirement struct {
	ID                string     `gorm:"primaryKey" json:"-"`
	LibraryID         uint       `gorm:"uniqueIndex" json:"-"`
	ActorID           uint       `json:"-"`
	JobID             string     `json:"-"`
	SourceEpoch       uint64     `json:"-"`
	SourceFingerprint string     `json:"-"`
	ConfigFingerprint string     `json:"-"`
	Phase             string     `json:"-"`
	Cursor            int        `json:"-"`
	Revision          uint64     `json:"-"`
	ProcessedRows     int64      `json:"-"`
	LastErrorCode     string     `json:"-"`
	CreatedAt         time.Time  `json:"-"`
	UpdatedAt         time.Time  `json:"-"`
	CompletedAt       *time.Time `json:"-"`
}
