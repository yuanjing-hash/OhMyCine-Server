package models

import "time"

// Private write-ahead evidence; clearing task history must not discard an
// unresolved provider mutation. It is never a client-provided delete request.
type CatalogCloudCleanupClaim struct {
	ID                uint      `gorm:"primaryKey" json:"-"`
	LibraryID         uint      `json:"-"`
	RunID             string    `json:"-"`
	PhysicalWriteID   uint64    `json:"-"`
	ProviderID        string    `json:"-"`
	SourceFingerprint string    `json:"-"`
	DirectoryJSON     string    `json:"-"`
	Status            string    `json:"-"`
	CreatedAt         time.Time `json:"-"`
	UpdatedAt         time.Time `json:"-"`
}
