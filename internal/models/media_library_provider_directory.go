package models

import "time"

// Provider directory evidence is private, source-scoped observation data.
type MediaLibraryProviderPath struct {
	ScanRunID         uint      `gorm:"primaryKey" json:"-"`
	LibraryID         uint      `gorm:"primaryKey" json:"-"`
	SourceFingerprint string    `gorm:"primaryKey" json:"-"`
	ProviderID        string    `gorm:"primaryKey" json:"-"`
	ParentProviderID  string    `json:"-"`
	RelativePath      string    `json:"-"`
	IsDir             bool      `json:"-"`
	ObservedAt        time.Time `json:"-"`
}

// Separate authority from user-clearable scan history. A successful catalog
// publication activates just this header; staged path pages stay immutable.
type MediaLibraryProviderPathBatch struct {
	ScanRunID           uint       `gorm:"primaryKey" json:"-"`
	LibraryID           uint       `json:"-"`
	SourceFingerprint   string     `json:"-"`
	ManifestFingerprint string     `json:"-"`
	ObservedAt          time.Time  `json:"-"`
	PublishedAt         *time.Time `json:"-"`
}
