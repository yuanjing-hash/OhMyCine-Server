package models

import "time"

// MediaVersionUpgrade is a private acquisition intent bound to ONE physical
// catalog entry. Public handlers must use the safe service DTO, never this row.
// Snapshot and receipts remain after completion for restart/recycle recovery.
type MediaVersionUpgrade struct {
	ID               string     `gorm:"primaryKey;size:36" json:"-"`
	OwnerID          uint       `gorm:"not null;index" json:"-"`
	LibraryID        uint       `gorm:"not null;index" json:"-"`
	EntryID          uint       `gorm:"not null;index" json:"-"`
	WorkKey          string     `gorm:"not null" json:"-"`
	Revision         uint64     `gorm:"not null" json:"-"`
	State            string     `gorm:"not null;index" json:"-"`
	Stage            string     `gorm:"not null" json:"-"`
	SnapshotJSON     string     `gorm:"type:text;not null" json:"-"`
	DownloadTaskID   string     `gorm:"not null;default:'';index" json:"-"`
	TransferTaskID   string     `gorm:"not null;default:''" json:"-"`
	JobID            string     `gorm:"not null;default:''" json:"-"`
	RecycleReceiptID string     `gorm:"not null;default:''" json:"-"`
	OutputJSON       string     `gorm:"type:text;not null;default:'{}'" json:"-"`
	RecycleJSON      string     `gorm:"type:text;not null;default:'{}'" json:"-"`
	ErrorCode        string     `gorm:"not null;default:''" json:"-"`
	NextAttemptAt    *time.Time `json:"-"`
	ExpiresAt        time.Time  `gorm:"not null" json:"-"`
	FinishedAt       *time.Time `json:"-"`
	CreatedAt        time.Time  `json:"-"`
	UpdatedAt        time.Time  `json:"-"`
}

// ManagedMediaTrash is a reversible LOCAL rename receipt. No automatic purge.
// Destination and original relative path are private; Restore uses this mapping
// and refuses any occupied destination. It never overwrites a newer file.
type ManagedMediaTrash struct {
	ID                   string    `gorm:"primaryKey;size:36" json:"-"`
	UpgradeID            string    `gorm:"not null;index" json:"-"`
	LibraryID            uint      `gorm:"not null;index" json:"-"`
	ItemKey              string    `gorm:"not null" json:"-"`
	OriginalRelativePath string    `gorm:"not null" json:"-"`
	TrashRelativePath    string    `gorm:"not null" json:"-"`
	FileIdentity         string    `gorm:"not null" json:"-"`
	Size                 int64     `gorm:"not null" json:"-"`
	ModifiedAt           time.Time `json:"-"`
	State                string    `gorm:"not null" json:"-"`
	CreatedAt            time.Time `json:"-"`
	UpdatedAt            time.Time `json:"-"`
}
