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
	ScopeMode               string    `json:"-"`
	ScopePrepared           bool      `json:"-"`
	ScopeEntryAfterID       uint      `json:"-"`
	ScopeAssetAfterID       uint      `json:"-"`
	ScopeRecognitionAfterID uint      `json:"-"`
	State                   string    `json:"-"`
	FinalizeAfterID         uint      `json:"-"`
	RecoveryAttempts        int       `json:"-"`
	CreatedAt               time.Time `json:"-"`
	UpdatedAt               time.Time `json:"-"`
}

// CatalogArtifactBindingItem is the durable, publication-time artifact delta.
// It stores only stable Catalog anchor IDs; paths/provider facts remain in the
// exact immutable binding. Full bindings populate the same work table during
// their durable manifest audit, including manifest-only cleanup differences.
type CatalogArtifactBindingItem struct {
	BindingID        string     `gorm:"primaryKey" json:"-"`
	EntityKind       string     `gorm:"primaryKey" json:"-"`
	EntityID         uint       `gorm:"primaryKey" json:"-"`
	Status           string     `gorm:"not null;default:'pending'" json:"-"`
	Attempts         int        `gorm:"not null;default:0" json:"-"`
	Outcome          string     `gorm:"not null;default:''" json:"-"`
	ErrorCode        string     `gorm:"not null;default:''" json:"-"`
	ErrorMessage     string     `gorm:"<-:update;not null;default:''" json:"-"`
	SafeRelativePath string     `gorm:"<-:update;not null;default:''" json:"-"`
	Retryable        bool       `gorm:"<-:update;not null;default:false" json:"-"`
	NextAttemptAt    *time.Time `gorm:"<-:update" json:"-"`
	FinishedAt       *time.Time `gorm:"<-:update" json:"-"`
	UpdatedAt        time.Time  `json:"-"`
}
