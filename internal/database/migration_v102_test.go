package database

import (
	"path/filepath"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestStructureMismatchClassificationsV102FreshUpgradeAndRepeat(t *testing.T) {
	db := structureMigrationDB(t, 101)
	library := seedStructureMigrationLibrary(t, db, 101, "mismatch-v102", "issues", 1, 1)
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.MediaLibraryStructureDiagnosis{}).Where("library_id = ?", library.ID).Updates(map[string]any{"naming_mismatch_count": 7, "location_mismatch_count": 3}).Error; err != nil {
		t.Fatal(err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("repeat migration: %v", err)
	}
	var diagnosis models.MediaLibraryStructureDiagnosis
	if err := db.First(&diagnosis, "library_id = ?", library.ID).Error; err != nil {
		t.Fatal(err)
	}
	if diagnosis.NamingMismatchCount != 7 || diagnosis.LocationMismatchCount != 3 {
		t.Fatalf("classification counts changed on repeat: %+v", diagnosis)
	}
	var count int64
	if err := db.Table("schema_migrations").Where("version = 102").Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("v102 ledger count=%d err=%v", count, err)
	}

	fresh, err := Open(filepath.Join(t.TempDir(), "fresh-v102.db"))
	if err != nil {
		t.Fatal(err)
	}
	freshSQL, err := fresh.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = freshSQL.Close() })
	if err := Migrate(fresh); err != nil {
		t.Fatal(err)
	}
	if err := fresh.Table("schema_migrations").Where("version = 102").Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("fresh v102 ledger count=%d err=%v", count, err)
	}
}
