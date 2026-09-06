package models

import "time"

// Catalog storage is private. These models must never be returned as API DTOs.
// Absence of a head means legacy; creating the schema does not activate it.
type CatalogHead struct {
	LibraryID         uint      `gorm:"primaryKey" json:"-"`
	Mode              string    `json:"-"`
	Revision          uint64    `json:"-"`
	SourceEpoch       uint64    `json:"-"`
	SourceFingerprint string    `json:"-"`
	ConfigFingerprint string    `json:"-"`
	UpdatedAt         time.Time `json:"-"`
}

type CatalogHeadLayer struct {
	LibraryID  uint   `gorm:"primaryKey" json:"-"`
	Rank       int    `gorm:"primaryKey" json:"-"`
	SnapshotID string `json:"-"`
}

type CatalogSnapshot struct {
	ID                string    `gorm:"primaryKey" json:"-"`
	LibraryID         uint      `json:"-"`
	Kind              string    `json:"-"`
	State             string    `json:"-"`
	ParentRevision    uint64    `json:"-"`
	SourceEpoch       uint64    `json:"-"`
	SourceFingerprint string    `json:"-"`
	ConfigFingerprint string    `json:"-"`
	OwnerTokenHash    string    `json:"-"`
	JobID             *string   `json:"-"`
	JobLeaseHash      string    `json:"-"`
	LeaseExpiresAt    time.Time `json:"-"`
	RowCount          int64     `json:"-"`
	ByteCount         int64     `json:"-"`
	PublishedRevision uint64    `json:"-"`
	CreatedAt         time.Time `json:"-"`
	UpdatedAt         time.Time `json:"-"`
}

// Identity anchors survive snapshot GC. An anchor belongs to exactly one source
// lifetime, so replacing a library root cannot revive an old playback identity.
type CatalogIdentity struct {
	LibraryID   uint      `gorm:"primaryKey" json:"-"`
	SourceEpoch uint64    `gorm:"primaryKey" json:"-"`
	EntityKind  string    `gorm:"primaryKey" json:"-"`
	SourceKey   string    `gorm:"primaryKey" json:"-"`
	AnchorID    uint      `json:"-"`
	CreatedAt   time.Time `json:"-"`
}

type CatalogEntryFact struct {
	SnapshotID        string `gorm:"primaryKey" json:"-"`
	MediaLibraryEntry `gorm:"embedded" json:"-"`
	Tombstone         bool `json:"-"`
	// SharedOverrideMask preserves per-file exceptions to recognition metadata.
	SharedOverrideMask uint64 `json:"-"`
}

type CatalogRecognitionFact struct {
	SnapshotID              string `gorm:"primaryKey" json:"-"`
	MediaLibraryRecognition `gorm:"embedded" json:"-"`
	Tombstone               bool   `json:"-"`
	WorkKey                 string `json:"-"`
}

type CatalogSourceAssetFact struct {
	SnapshotID              string `gorm:"primaryKey" json:"-"`
	MediaLibrarySourceAsset `gorm:"embedded" json:"-"`
	Tombstone               bool `json:"-"`
}

// References deliberately have no automatic expiry deletion. Queue ownership
// must prove a crashed owner cannot resume before its reference is released.
type CatalogSnapshotReference struct {
	SnapshotID string    `gorm:"primaryKey" json:"-"`
	OwnerKind  string    `gorm:"primaryKey" json:"-"`
	OwnerID    string    `gorm:"primaryKey" json:"-"`
	CreatedAt  time.Time `json:"-"`
}
