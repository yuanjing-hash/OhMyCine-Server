package database

import (
	"path/filepath"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestPlayerMediaStateMigrationIsPresentAndIdempotent(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "player-media-state.db"))
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
	for _, model := range []any{&models.PlayerMediaFavorite{}, &models.PlayerMediaCollection{}, &models.PlayerMediaCollectionItem{}} {
		if !db.Migrator().HasTable(model) {
			t.Fatalf("missing table for %T", model)
		}
	}
	var count int64
	if err := db.Table("schema_migrations").Where("version = ?", 65).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("v65 migration count=%d err=%v", count, err)
	}
}
