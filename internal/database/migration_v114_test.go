package database

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestMigrationV114PreservesPreviousDownloaderListOrder(t *testing.T) {
	db, err := Open(t.TempDir() + "/migration-v114.db")
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	applyMigrationsThrough(t, db, 113)
	now := time.Now().UTC()
	for _, name := range []string{"Zulu", "Alpha"} {
		record := models.Downloader{ID: uuid.NewString(), Name: name, NameNormalized: name, Type: models.DownloaderTypeQBittorrent, BaseURL: "http://127.0.0.1:8080", Enabled: true, CapabilitiesJSON: `{}`, CreatedAt: now, UpdatedAt: now}
		if err := db.Omit("SortOrder").Create(&record).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("repeated startup changed migration state: %v", err)
	}
	var records []models.Downloader
	if err := db.Order("sort_order,id").Find(&records).Error; err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || records[0].Name != "Alpha" || records[0].SortOrder != 1 || records[1].Name != "Zulu" || records[1].SortOrder != 2 {
		t.Fatalf("legacy order was not retained: %+v", records)
	}
}
