package database

import (
	"path/filepath"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestPersistentMediaCategoryArtworkMigrationIsPresentAndIdempotent(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "media-category-artwork.db"))
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("repeat migrate: %v", err)
	}
	if !db.Migrator().HasTable(&models.MediaCategoryArtwork{}) {
		t.Fatal("media category artwork table missing")
	}
	for _, column := range []string{"generation_key", "pending_generation_key", "candidate_digest", "template_version", "content_hash", "relative_path", "revision", "status", "last_error_code"} {
		if !db.Migrator().HasColumn(&models.MediaCategoryArtwork{}, column) {
			t.Fatalf("media category artwork column %q missing", column)
		}
	}
	var count int64
	if err := db.Table("schema_migrations").Where("version = ?", 67).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("v67 migration count=%d err=%v", count, err)
	}
}
