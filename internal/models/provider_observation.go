package models

import "time"

// Provider observation records are private metadata, never public API DTOs.
// Provider IDs are meaningful only in this scope's account/source namespace.
type ProviderObservationScope struct {
	ID                 string     `gorm:"primaryKey" json:"-"`
	LibraryID          uint       `gorm:"index:idx_observation_library" json:"-"`
	RequestedScanRunID uint       `json:"-"`
	ConnectionID       uint       `json:"-"`
	AccountKey         string     `gorm:"index:idx_observation_account" json:"-"`
	Provider           string     `json:"-"`
	Kind               string     `json:"-"` // library or explicitly enabled intake
	RootID             string     `json:"-"`
	RootPath           string     `json:"-"`
	Fence              string     `json:"-"` // source epoch + root + profile + configuration
	Enabled            bool       `json:"-"`
	AutoIngest         bool       `json:"-"`
	OwnerID            uint       `json:"-"`
	DownloaderID       string     `json:"-"`
	CoverageRound      uint64     `json:"-"`
	CoverageStartedAt  time.Time  `json:"-"`
	LastCompleteAt     *time.Time `json:"-"`
	NextCoverageAt     time.Time  `json:"-"`
	LastScheduledAt    time.Time  `json:"-"`
	LastErrorCode      string     `json:"-"`
	NextRetryAt        *time.Time `json:"-"`
	CreatedAt          time.Time  `json:"-"`
	UpdatedAt          time.Time  `json:"-"`
}

type ProviderObservationBatch struct {
	ID                 string     `gorm:"primaryKey" json:"-"`
	ScopeID            string     `gorm:"index:idx_observation_batch_scope" json:"-"`
	DirectoryID        string     `json:"-"`
	Fence              string     `json:"-"`
	CoverageRound      uint64     `json:"-"`
	RuntimeID          string     `json:"-"`
	State              string     `gorm:"index:idx_observation_batch_state" json:"-"`
	Offset             int64      `json:"-"`
	ExpectedTotal      *int64     `json:"-"`
	Pages              int        `json:"-"`
	SeenCount          int64      `json:"-"`
	Revision           uint64     `json:"-"`
	PublishedRevision  uint64     `json:"-"`
	IncompleteReason   string     `json:"-"`
	ConfirmationPhase  string     `json:"-"`
	ConfirmationCursor string     `json:"-"`
	StartedAt          time.Time  `json:"-"`
	CompletedAt        *time.Time `json:"-"`
	UpdatedAt          time.Time  `json:"-"`
}

// BatchID="" is the current identity/parent graph and durable directory queue.
// Other rows are observed facts / publication deltas for exactly one batch.
// Keeping batch facts separate prevents a partial page from proving absence.
type ProviderObservationItem struct {
	ScopeID             string     `gorm:"primaryKey" json:"-"`
	BatchID             string     `gorm:"primaryKey" json:"-"`
	ResourceID          string     `gorm:"primaryKey" json:"-"`
	ParentID            string     `gorm:"index:idx_observation_parent" json:"-"`
	Name                string     `json:"-"`
	IsDir               bool       `json:"-"`
	Size                int64      `json:"-"`
	ModifiedAt          time.Time  `json:"-"`
	ContentVersion      string     `json:"-"`
	Digest              string     `json:"-"`
	RelativePath        string     `json:"-"`
	PreviousPath        string     `json:"-"`
	Fingerprint         string     `json:"-"`
	Change              string     `json:"-"`
	Revision            uint64     `json:"-"`
	LastSeenRound       uint64     `json:"-"`
	SeenAt              time.Time  `json:"-"`
	StableSince         time.Time  `json:"-"`
	StableObservations  int        `json:"-"`
	LastStableBatchID   string     `json:"-"`
	IntakeAttemptedAt   *time.Time `json:"-"`
	MissingSince        *time.Time `json:"-"`
	MissingLastAt       *time.Time `json:"-"`
	MissingObservations int        `json:"-"`
	MissingRound        uint64     `json:"-"`
	MissingBatchID      string     `json:"-"`
	DownlistedAt        *time.Time `json:"-"`
	RecoveryUntil       *time.Time `json:"-"`
	QueueRound          uint64     `json:"-"`
	CompletedRound      uint64     `json:"-"`
	// A complete parent listing may prove this branch is unreachable. This is
	// distinct from reading the directory completely, and never grants deletion.
	AbsentParentRound       uint64     `gorm:"not null;default:0" json:"-"`
	DueAt                   time.Time  `gorm:"index:idx_observation_due" json:"-"`
	LastCompleteAt          *time.Time `json:"-"`
	HotUntil                *time.Time `json:"-"`
	ActiveBatchID           string     `json:"-"`
	Excluded                bool       `json:"-"`
	PathRepairPending       bool       `json:"-"`
	PathRepairCursor        string     `json:"-"`
	CatalogSourceEpoch      uint64     `json:"-"`
	RecoveryEntryJSON       string     `gorm:"type:text" json:"-"`
	RecoveryRecognitionJSON string     `gorm:"type:text" json:"-"`
	UpdatedAt               time.Time  `json:"-"`
}

// All background observation adapters for one connection/account share this
// admission row. Interactive callers can also extend its common cooldown.
type ProviderObservationAccountBudget struct {
	AccountKey    string    `gorm:"primaryKey" json:"-"`
	WindowStart   time.Time `json:"-"`
	HotRequests   int       `json:"-"`
	ColdRequests  int       `json:"-"`
	NextRequestAt time.Time `json:"-"`
	CooldownUntil time.Time `json:"-"`
	LeaseToken    string    `json:"-"`
	LeaseUntil    time.Time `json:"-"`
	LastScopeID   string    `json:"-"`
	UpdatedAt     time.Time `json:"-"`
}

// Receipt metadata does not contain credentials, URLs, share codes or a raw
// upstream response. Sensitive adapter-specific checkpoints use ciphertext.
type QuarkOperationReceipt struct {
	ID                   string    `gorm:"primaryKey" json:"-"`
	ConnectionID         uint      `gorm:"index:idx_quark_receipt_connection" json:"-"`
	ScopeID              string    `gorm:"index:idx_quark_receipt_scope" json:"-"`
	OperationKey         string    `gorm:"uniqueIndex:idx_quark_receipt_operation" json:"-"`
	Adapter              string    `json:"-"`
	Kind                 string    `json:"-"`
	Fence                string    `json:"-"`
	DirectoryID          string    `json:"-"`
	State                string    `json:"-"`
	Revision             uint64    `json:"-"`
	ProviderOperationID  string    `json:"-"`
	CheckpointCiphertext []byte    `json:"-"`
	OutputBatchID        string    `json:"-"`
	CreatedAt            time.Time `json:"-"`
	UpdatedAt            time.Time `json:"-"`
}

type ProviderObservationIntakeClaim struct {
	ID          string    `gorm:"primaryKey" json:"-"`
	ScopeID     string    `json:"-"`
	ResourceID  string    `json:"-"`
	Fingerprint string    `json:"-"`
	Fence       string    `json:"-"`
	TaskID      string    `json:"-"`
	CreatedAt   time.Time `json:"-"`
}
