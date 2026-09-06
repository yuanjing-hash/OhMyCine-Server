package models

import "time"

// Private physical recovery evidence; never expose paths or provider metadata.
type CatalogArtifactWriteReceipt struct {
	ID                 uint64    `gorm:"primaryKey" json:"-"`
	LibraryID          uint      `json:"-"`
	RunID              string    `json:"-"`
	ArtifactID         uint      `json:"-"`
	PhysicalWriteID    uint64    `json:"-"`
	PermitRevision     uint64    `json:"-"`
	Revision           uint64    `json:"-"`
	TargetKind         string    `json:"-"`
	RelativePath       string    `json:"-"`
	RootIdentity       string    `json:"-"`
	PolicyDigest       string    `json:"-"`
	SourceEpoch        uint64    `json:"-"`
	SourceFingerprint  string    `json:"-"`
	ConfigFingerprint  string    `json:"-"`
	BeforeExists       bool      `json:"-"`
	BeforeFingerprint  string    `json:"-"`
	AfterFingerprint   string    `json:"-"`
	BeforeSize         int64     `json:"-"`
	AfterSize          int64     `json:"-"`
	BeforeArtifactJSON string    `json:"-"`
	AfterArtifactJSON  string    `json:"-"`
	Phase              string    `json:"-"`
	ErrorCode          string    `json:"-"`
	CreatedAt          time.Time `json:"-"`
	UpdatedAt          time.Time `json:"-"`
}
