package models

import "time"

// One immutable catalog binding per artifact refresh. Private reference owner,
// never an API/job payload; multiple refreshes can share a logical generation.
type CatalogArtifactBinding struct {
	ID                      string    `gorm:"primaryKey" json:"-"`
	LibraryID               uint      `json:"-"`
	Generation              uint64    `json:"-"`
	HeadRevision            uint64    `json:"-"`
	SourceEpoch             uint64    `json:"-"`
	SourceFingerprint       string    `json:"-"`
	ConfigFingerprint       string    `json:"-"`
	SourceConfigFingerprint string    `json:"-"`
	LayersJSON              string    `json:"-"`
	State                   string    `json:"-"`
	FinalizeAfterID         uint      `json:"-"`
	RecoveryAttempts        int       `json:"-"`
	CreatedAt               time.Time `json:"-"`
	UpdatedAt               time.Time `json:"-"`
}
