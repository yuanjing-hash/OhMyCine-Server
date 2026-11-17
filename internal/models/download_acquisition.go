package models

import "time"

// DownloadAdmission tracks physical acquisition occupancy independently of a
// renewable Job lease. It never expires into free upstream capacity.
type DownloadAdmission struct {
	AcquisitionKey string     `gorm:"primaryKey;size:80" json:"-"`
	DownloaderID   string     `gorm:"size:36;not null;index" json:"-"`
	TaskID         string     `gorm:"size:36;not null" json:"-"`
	State          string     `gorm:"size:16;not null;index" json:"-"`
	CreatedAt      time.Time  `json:"-"`
	UpdatedAt      time.Time  `json:"-"`
	ReleasedAt     *time.Time `json:"-"`
}

// OfflineFailure is acquisition policy history, not a display task. There is
// deliberately no cascading relationship to DownloadTask or Job.
type OfflineFailure struct {
	ID               uint64     `gorm:"primaryKey;autoIncrement" json:"-"`
	TaskID           string     `gorm:"size:36;not null;uniqueIndex" json:"-"`
	OwnerID          uint       `gorm:"not null;index:idx_offline_failure_scope,priority:1" json:"-"`
	AccountNamespace string     `gorm:"size:128;not null;index:idx_offline_failure_scope,priority:2" json:"-"`
	ResourceDigest   string     `gorm:"size:64;not null;index:idx_offline_failure_scope,priority:3" json:"-"`
	RequiredDigest   string     `gorm:"size:64;not null;index:idx_offline_failure_scope,priority:4" json:"-"`
	Reason           string     `gorm:"size:64;not null" json:"-"`
	StartedAt        time.Time  `json:"-"`
	DeadlineAt       time.Time  `json:"-"`
	CreatedAt        time.Time  `json:"-"`
	ResolvedAt       *time.Time `json:"-"`
}

// OfflineConfirmation stores only the random token digest and private bounded
// request binding. Acceptance and its task identity commit together.
type OfflineConfirmation struct {
	IntakeDigest       string    `gorm:"size:64;not null;default:''" json:"-"`
	AuthzVersion       uint64    `gorm:"not null;default:0" json:"-"`
	SourceSiteID       uint      `gorm:"not null;default:0" json:"-"`
	SourceSiteRevision uint64    `gorm:"not null;default:0" json:"-"`
	TokenHash          string    `gorm:"primaryKey;size:64" json:"-"`
	OwnerID            uint      `gorm:"not null;index" json:"-"`
	RequestDigest      string    `gorm:"size:64;not null" json:"-"`
	HistoryRevision    uint64    `gorm:"not null" json:"-"`
	AcceptedTaskID     string    `gorm:"size:36;not null;default:''" json:"-"`
	CreatedAt          time.Time `json:"-"`
	ExpiresAt          time.Time `gorm:"index" json:"-"`
}
