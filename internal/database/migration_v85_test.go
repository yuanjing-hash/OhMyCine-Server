package database

import (
	"context"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestCatalogConversionV85UpgradeIsEmptyRepeatableAndDoesNotActivate(t *testing.T) {
	db := structureMigrationDB(t, 84)
	library := seedStructureMigrationLibrary(t, db, 84, "conversion-upgrade", "healthy", 0, 0)
	entry := models.MediaLibraryEntry{LibraryID: library.ID, RelativePath: "Show/Season 02/01.mkv", Title: "Original"}
	if err := db.Create(&entry).Error; err != nil {
		t.Fatal(err)
	}
	for pass := 0; pass < 2; pass++ {
		if err := Migrate(db); err != nil {
			t.Fatal(err)
		}
	}
	if !db.Migrator().HasTable(&models.CatalogConversionManifest{}) {
		t.Fatal("missing conversion ledger")
	}
	for _, model := range []any{&models.CatalogConversionManifest{}, &models.CatalogSnapshot{}, &models.CatalogHead{}} {
		var count int64
		if err := db.Model(model).Count(&count).Error; err != nil || count != 0 {
			t.Fatalf("migration activated conversion: %T count=%d err=%v", model, count, err)
		}
	}
	var retained models.MediaLibraryEntry
	if err := db.First(&retained, entry.ID).Error; err != nil || retained.Title != entry.Title || retained.RelativePath != entry.RelativePath {
		t.Fatalf("migration changed legacy media: %+v %v", retained, err)
	}
	if floor, err := ReadCatalogFormat(context.Background(), db); err != nil || floor != 0 {
		t.Fatalf("empty migration raised storage floor: %d %v", floor, err)
	}
}
