package models

import "time"

// PluginBrowserState is Host-owned, encrypted Cookie state. It is not plugin
// private KV, a browser profile, or a public API projection.
type PluginBrowserState struct {
	ConnectionID string    `gorm:"primaryKey;size:36" json:"-"`
	PluginID     string    `gorm:"size:128;not null;index" json:"-"`
	OwnerID      uint      `gorm:"not null;index" json:"-"`
	Fingerprint  string    `gorm:"size:64;not null" json:"-"`
	Ciphertext   string    `gorm:"type:text;not null" json:"-"`
	UpdatedAt    time.Time `gorm:"not null" json:"-"`
}
