package models

import "time"

// PluginCatalogueSnapshot holds public presentation facts, never playback
// plans, account state, signed navigation tokens or credential-bearing URLs.
type PluginCatalogueSnapshot struct {
	ID             string    `gorm:"primaryKey;size:64"`
	LibraryID      string    `gorm:"size:36;not null;index"`
	ScopeKey       string    `gorm:"size:64;not null;index"`
	Kind           string    `gorm:"size:16;not null"`
	Selector       string    `gorm:"size:256;not null"`
	Depth          int       `gorm:"not null;default:0"`
	AncestorsJSON  string    `gorm:"type:text;not null;default:'[]'"`
	ResponseJSON   string    `gorm:"type:text;not null"`
	RefreshSession string    `gorm:"size:36;not null;default:''"`
	FreshUntil     time.Time `gorm:"not null;index"`
	StaleUntil     time.Time `gorm:"not null;index"`
	RetryAt        time.Time `gorm:"not null;index"`
	Failures       int       `gorm:"not null;default:0"`
	UpdatedAt      time.Time `gorm:"not null"`
}

// Detail-derived public identity facts, independent from entitlement. Stored
// history outlives these bounded proofs and every new playback resolves live.
type PluginOnlineMediaIdentity struct {
	ID           string    `gorm:"primaryKey;size:64"`
	LibraryID    string    `gorm:"size:36;not null;index"`
	ScopeKey     string    `gorm:"size:64;not null;index"`
	ItemToken    string    `gorm:"size:512;not null;index"`
	MetadataJSON string    `gorm:"type:text;not null"`
	ExpiresAt    time.Time `gorm:"not null;index"`
	UpdatedAt    time.Time `gorm:"not null;index"`
}

type PluginOnlinePlaybackReceipt struct {
	UserID      uint      `gorm:"primaryKey"`
	IdentityKey string    `gorm:"primaryKey;size:64"`
	ScopeKey    string    `gorm:"size:64;not null"`
	ExpiresAt   time.Time `gorm:"not null;index"`
}
