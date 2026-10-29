package models

import "time"

// BTSelectionGroup owns one physical executor task/workspace. Individual
// source selections and routes remain in the encrypted DownloadTask records.
type BTSelectionGroup struct {
	ID                string     `gorm:"primaryKey;size:64" json:"-"`
	OwnerID           uint       `gorm:"not null;index" json:"-"`
	DownloaderID      string     `gorm:"not null;size:36;index" json:"-"`
	ScopeDigest       string     `gorm:"not null;size:64;uniqueIndex:idx_bt_group_generation,priority:1" json:"-"`
	Generation        uint64     `gorm:"not null;default:1;uniqueIndex:idx_bt_group_generation,priority:2" json:"-"`
	CleanupState      string     `gorm:"size:32;not null;default:''" json:"-"`
	CleanupKey        string     `gorm:"size:64;not null;default:''" json:"-"`
	CleanupCiphertext string     `gorm:"type:text;not null;default:''" json:"-"`
	Workspace         string     `gorm:"type:text;not null" json:"-"`
	ProviderTaskID    string     `gorm:"size:256;not null;default:''" json:"-"`
	Revision          uint64     `gorm:"not null;default:1" json:"-"`
	AppliedRevision   uint64     `gorm:"not null;default:0" json:"-"`
	AppliedDigest     string     `gorm:"size:64;not null;default:''" json:"-"`
	PendingDigest     string     `gorm:"size:64;not null;default:''" json:"-"`
	KnownDigestsJSON  string     `gorm:"type:text;not null;default:'[]'" json:"-"`
	LeaseOwner        string     `gorm:"size:36;not null;default:''" json:"-"`
	LeaseEpoch        uint64     `gorm:"not null;default:0" json:"-"`
	LeaseUntil        *time.Time `json:"-"`
	CreatedAt         time.Time  `json:"-"`
	UpdatedAt         time.Time  `json:"-"`
}

type BTSelectionRef struct {
	TaskID    string    `gorm:"primaryKey;size:36" json:"-"`
	GroupID   string    `gorm:"size:64;not null;index" json:"-"`
	OwnDigest string    `gorm:"size:64;not null" json:"-"`
	State     string    `gorm:"size:24;not null;default:'queued';index" json:"-"`
	Revision  uint64    `gorm:"not null;default:1" json:"-"`
	CreatedAt time.Time `json:"-"`
	UpdatedAt time.Time `json:"-"`
}
