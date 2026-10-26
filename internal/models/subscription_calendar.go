package models

import "time"

// CalendarSeason contains public schedule facts only, never subscriptions,
// private activity, credentials or authorization decisions.
type CalendarSeason struct {
	Key             string `gorm:"primaryKey;size:96"`
	Generation      uint64 `gorm:"not null;index"`
	TMDBID          int64  `gorm:"not null;index"`
	SeasonNumber    int    `gorm:"not null"`
	Language        string `gorm:"size:16;not null"`
	Revision        uint64 `gorm:"not null;default:0"`
	FetchedAt       *time.Time
	FreshUntil      *time.Time `gorm:"index"`
	RetryAt         *time.Time `gorm:"index"`
	LastRequestedAt *time.Time
	LastErrorCode   string `gorm:"size:64;not null;default:''"`
	Complete        bool   `gorm:"not null;default:false"`
	EpisodeCount    int    `gorm:"not null;default:0"`
	PayloadBytes    int64  `gorm:"not null;default:0"`
	UpdatedAt       time.Time
}

type CalendarEpisode struct {
	SeasonKey     string `gorm:"primaryKey;size:96;index:idx_calendar_date,priority:1"`
	EpisodeNumber int    `gorm:"primaryKey"`
	TMDBID        int64  `gorm:"not null"`
	Name          string `gorm:"size:512;not null"`
	AirDate       string `gorm:"size:10;not null;default:'';index:idx_calendar_date,priority:2"`
}

// CalendarRefreshCursor is private scheduler bookkeeping. Keep subscription
// identity out of the public season/episode cache and resume within a season set.
type CalendarRefreshCursor struct {
	Name           string `gorm:"primaryKey;size:32"`
	Generation     uint64 `gorm:"not null"`
	SubscriptionID string `gorm:"size:36;not null;default:''"`
	SeasonNumber   int    `gorm:"not null;default:-1"`
	UpdatedAt      time.Time
}
