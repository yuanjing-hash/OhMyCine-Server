package models

import "time"

// CatalogPhysicalWrite is evidence of an entered physical operation, not a
// queue or an expiring lease. Domain rows remain the recovery authority. An
// unsettled row must survive cancellation, supersession and process loss.
type CatalogPhysicalWrite struct {
	ID                     uint64     `gorm:"primaryKey" json:"-"`
	LibraryID              uint       `json:"-"`
	OwnerKind              string     `json:"-"`
	OwnerID                string     `json:"-"`
	Revision               uint64     `json:"-"`
	State                  string     `json:"-"`
	JobID                  string     `json:"-"`
	JobLeaseHash           string     `json:"-"`
	RuntimeID              string     `json:"-"`
	ClaimDigest            string     `json:"-"`
	OwnerDigest            string     `json:"-"`
	SourceFingerprint      string     `json:"-"`
	ConfigFingerprint      string     `json:"-"`
	SourceEpoch            uint64     `json:"-"`
	ArtifactReceiptVersion uint       `json:"-"`
	EnteredAt              time.Time  `json:"-"`
	SettledAt              *time.Time `json:"-"`
	UpdatedAt              time.Time  `json:"-"`
}
