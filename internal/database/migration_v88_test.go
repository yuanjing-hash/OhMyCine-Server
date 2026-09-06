package database

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestLibraryRetirementV88AdditivePrivateAndRepeatable(t *testing.T) {
	db := structureMigrationDB(t, 87)
	for i := 0; i < 2; i++ {
		if err := Migrate(db); err != nil {
			t.Fatal(err)
		}
	}
	row := models.MediaLibraryRetirement{ID: "receipt", LibraryID: 123, ActorID: 1, JobID: "retained-job", SourceEpoch: 2, SourceFingerprint: "private", ConfigFingerprint: "private", Phase: "queued", Revision: 1, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(row)
	if err != nil || string(raw) != "{}" {
		t.Fatalf("private receipt exposed %s %v", raw, err)
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	var found models.MediaLibraryRetirement
	if err := db.First(&found, "id=?", row.ID).Error; err != nil || found.SourceEpoch != 2 {
		t.Fatalf("receipt lost %+v %v", found, err)
	}
	var count int64
	if err := db.Model(&models.CatalogHead{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatal("migration activated catalog")
	}
	for _, index := range []string{"idx_catalog_snapshots_library_retirement", "idx_favorites_library_retirement", "idx_collection_items_library_retirement", "idx_acquisitions_library_retirement"} {
		var n int64
		if err := db.Raw("SELECT count(*) FROM sqlite_master WHERE type='index' AND name=?", index).Scan(&n).Error; err != nil || n != 1 {
			t.Fatalf("missing index %s %v", index, err)
		}
	}
}
