package database

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestCatalogPhysicalV89AdditivePrivateAndRepeatable(t *testing.T) {
	db := structureMigrationDB(t, 88)
	for pass := 0; pass < 2; pass++ {
		if err := Migrate(db); err != nil {
			t.Fatal(err)
		}
	}
	row := models.CatalogPhysicalWrite{LibraryID: 123, OwnerKind: "repair", OwnerID: "retained-owner", Revision: 1, State: "entered", OwnerDigest: "private", SourceFingerprint: "source", ConfigFingerprint: "config", EnteredAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&row).Update("state", "quiescent").Error; err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(row)
	if err != nil || string(data) != "{}" {
		t.Fatalf("private proof exposed: %s %v", data, err)
	}
	var heads int64
	if err := db.Model(&models.CatalogHead{}).Count(&heads).Error; err != nil || heads != 0 {
		t.Fatal("physical evidence migration activated a catalog")
	}
	if err := db.Model(&row).Update("state", "expired").Error; err == nil {
		t.Fatal("ledger allowed time-based expiry")
	}
}
