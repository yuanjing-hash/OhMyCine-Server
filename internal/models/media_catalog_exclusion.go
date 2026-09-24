package models

import "time"

// Catalog exclusions are private library-scoped visibility rules. Source
// identities remain server-side and never appear in browser responses.
type MediaCatalogExclusion struct {
	ID                string    `gorm:"primaryKey;size:36" json:"-"`
	LibraryID         uint      `gorm:"not null;index" json:"-"`
	SourceEpoch       uint64    `gorm:"not null" json:"-"`
	SourceFingerprint string    `gorm:"size:64;not null" json:"-"`
	WorkKey           string    `gorm:"size:80;not null" json:"-"`
	Title             string    `gorm:"size:512;not null" json:"-"`
	Kind              string    `gorm:"size:16;not null" json:"-"`
	EntryCount        int       `gorm:"not null" json:"-"`
	CreatedAt         time.Time `gorm:"not null" json:"-"`
}

type MediaCatalogExclusionMember struct {
	ExclusionID  string `gorm:"primaryKey;size:36" json:"-"`
	RelativePath string `gorm:"primaryKey;size:2048" json:"-"`
	ProviderID   string `gorm:"size:128;not null" json:"-"`
}
