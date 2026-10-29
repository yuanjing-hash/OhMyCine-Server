package models

import "time"

// ProviderOperationReceipt is a private, encrypted per-operation checkpoint.
// The generic table does not change historical Quark receipts or their purpose.
type ProviderOperationReceipt struct {
	ResourceKey          string    `gorm:"not null;default:'';size:64" json:"-"`
	ID                   string    `gorm:"primaryKey;size:64" json:"-"`
	ConnectionID         uint      `gorm:"not null;index" json:"-"`
	Provider             string    `gorm:"not null;size:64" json:"-"`
	AccountKey           string    `gorm:"not null;size:64" json:"-"`
	Route                string    `gorm:"not null;size:80" json:"-"`
	Kind                 string    `gorm:"not null;size:80" json:"-"`
	State                string    `gorm:"not null;size:32" json:"-"`
	Revision             uint64    `gorm:"not null" json:"-"`
	CheckpointCiphertext []byte    `gorm:"not null" json:"-"`
	CreatedAt            time.Time `json:"-"`
	UpdatedAt            time.Time `json:"-"`
}
