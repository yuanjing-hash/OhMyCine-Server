package models

import "time"

type CatalogConversionManifest struct {
	SnapshotID  string    `gorm:"primaryKey" json:"-"`
	State       string    `json:"-"`
	FenceDigest string    `json:"-"`
	InputDigest string    `json:"-"`
	InputRows   int64     `json:"-"`
	UpdatedAt   time.Time `json:"-"`
}
