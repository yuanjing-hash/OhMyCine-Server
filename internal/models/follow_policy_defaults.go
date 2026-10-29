package models

import "time"

// FollowPolicyDefaults affects only newly prepared subscription snapshots.
// Existing subscriptions and runs retain their own frozen filters.
type FollowPolicyDefaults struct {
	ID            uint `gorm:"primaryKey"`
	Revision      uint64
	SubtitlesJSON string
	UpdatedAt     time.Time
}

func (FollowPolicyDefaults) TableName() string { return "follow_policy_defaults" }
