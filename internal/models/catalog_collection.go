package models

import "time"

// Identities are not visible collections. They retain old UUIDs across source
// snapshots without installing candidate metadata in the mutable global table.
type CatalogCollectionIdentity struct {
	TMDBCollectionID int64  `gorm:"primaryKey" json:"-"`
	CollectionID     string `json:"-"`
}

type CatalogCollectionMemberFact struct {
	SnapshotID        string    `gorm:"primaryKey" json:"-"`
	LibraryID         uint      `json:"-"`
	WorkKey           string    `gorm:"primaryKey" json:"-"`
	TMDBCollectionID  int64     `json:"-"`
	TMDBMovieID       int64     `json:"-"`
	Name              string    `json:"-"`
	PosterPath        string    `json:"-"`
	BackdropPath      string    `json:"-"`
	MetadataUpdatedAt time.Time `json:"-"`
	Tombstone         bool      `json:"-"`
}

type CatalogCollectionPreparation struct {
	SnapshotID     string `gorm:"primaryKey" json:"-"`
	State          string `json:"-"`
	AfterWork      string `json:"-"`
	InputRowCount  int64  `json:"-"`
	InputByteCount int64  `json:"-"`
	RowCount       int64  `json:"-"`
	ByteCount      int64  `json:"-"`
}
