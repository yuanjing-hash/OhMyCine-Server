package models

import "time"

// CatalogScanFollowup is a private coalesced receipt, not an executable file
// plan. A new publication replaces ReceiptID so an old dispatcher cannot
// acknowledge newer work. CompleteObserved retains full-scan diagnosis intent.
type CatalogScanFollowup struct {
	LibraryID               uint      `gorm:"primaryKey" json:"-"`
	ReceiptID               string    `json:"-"`
	ScanRunID               uint      `json:"-"`
	Generation              uint64    `json:"-"`
	ArtifactGeneration      uint64    `json:"-"`
	SourceEpoch             uint64    `json:"-"`
	SourceFingerprint       string    `json:"-"`
	ConfigFingerprint       string    `json:"-"`
	SourceConfigFingerprint string    `json:"-"`
	RecognitionPending      bool      `json:"-"`
	ArtifactPending         bool      `json:"-"`
	ArtworkPending          bool      `json:"-"`
	DiagnosisPending        bool      `json:"-"`
	CompleteObserved        bool      `json:"-"`
	UpdatedAt               time.Time `json:"-"`
}
