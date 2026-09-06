package database

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestCatalogConnectionV87DefaultsArePrivateAndDoNotActivate(t *testing.T) {
	db := structureMigrationDB(t, 86)
	storage := models.Storage{Name: "legacy-storage", NameNormalized: "legacy-storage", Type: models.StorageTypeLocal, RootPath: "/legacy", RootPathNormalized: "/legacy", Capabilities: "{}"}
	if err := db.Create(&storage).Error; err != nil {
		t.Fatal(err)
	}
	for pass := 0; pass < 2; pass++ {
		if err := Migrate(db); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.First(&storage, storage.ID).Error; err != nil || storage.CatalogConnectionEpoch != 0 || storage.CatalogConnectionRevision != 0 {
		t.Fatal("migration changed source lifetime")
	}
	data, err := json.Marshal(storage)
	if err != nil || strings.Contains(string(data), "catalog_connection") {
		t.Fatal("private lifetime fields escaped DTO")
	}
	var heads int64
	if err := db.Model(&models.CatalogHead{}).Count(&heads).Error; err != nil || heads != 0 {
		t.Fatal("connection migration activated catalog")
	}
}
