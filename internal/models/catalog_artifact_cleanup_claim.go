package models

import "time"

// Cleanup ownership is separate from MediaArtifact.RunID (the generator).
// Null ownership denotes manual/legacy execution, never adoptable by a run.
type CatalogArtifactCleanupClaim struct {
	ArtifactID            uint      `gorm:"primaryKey" json:"-"`
	LibraryID             uint      `json:"-"`
	OwnerRunID            *string   `json:"-"`
	PhysicalWriteID       *uint64   `json:"-"`
	PermitRevision        uint64    `json:"-"`
	OriginalStatus        string    `json:"-"`
	ManifestDigest        string    `json:"-"`
	RootIdentity          string    `json:"-"`
	GeneratorPolicyDigest string    `json:"-"`
	CreatedAt             time.Time `json:"-"`
	UpdatedAt             time.Time `json:"-"`
}
