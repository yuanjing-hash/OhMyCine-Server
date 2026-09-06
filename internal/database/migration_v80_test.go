package database

import (
	"encoding/json"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestCatalogScanFollowupMigrationV80IsEmptyAndRepeatable(t *testing.T) {
	db := structureMigrationDB(t, 79)
	for i := 0; i < 2; i++ {
		if err := Migrate(db); err != nil {
			t.Fatal(err)
		}
	}
	var count int64
	if err := db.Model(&models.CatalogScanFollowup{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("unexpected backfill %d %v", count, err)
	}
	if encoded, err := json.Marshal(models.CatalogScanFollowup{ReceiptID: "private"}); err != nil || string(encoded) != "{}" {
		t.Fatalf("private receipt leaked: %s %v", encoded, err)
	}
}
