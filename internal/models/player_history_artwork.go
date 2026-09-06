package models

import "time"

// Body is private durable data, never serialized into history/list responses.
type PlayerHistoryArtwork struct {
	ID        string    `gorm:"primaryKey" json:"-"`
	UserID    uint      `json:"-"`
	SyncKey   string    `json:"-"`
	Slot      string    `json:"-"`
	Body      []byte    `json:"-"`
	CreatedAt time.Time `json:"-"`
}
