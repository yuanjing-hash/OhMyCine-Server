package models

import "time"

// MediaLibraryDeletionEvidence retains exact file identity across catalog
// removal and lost delivery acknowledgements. It never authorizes source I/O.
type MediaLibraryDeletionEvidence struct {
	LibraryID         uint      `gorm:"primaryKey" json:"-"`
	SourceFingerprint string    `gorm:"primaryKey" json:"-"`
	ProviderID        string    `gorm:"primaryKey" json:"-"`
	EventTime         time.Time `json:"-"`
	CreatedAt         time.Time `json:"-"`
}
