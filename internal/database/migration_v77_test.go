package database

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestMediaChangeDispatchMigrationV77PreservesExistingCatalog(t *testing.T) {
	db := structureMigrationDB(t, 76)
	library := seedStructureMigrationLibrary(t, db, 76, "dispatch", "healthy", 0, 0)
	for i := 0; i < 2; i++ {
		if err := Migrate(db); err != nil {
			t.Fatal(err)
		}
	}
	marker := models.MediaChangeDispatch{LibraryID: library.ID, Revision: 7, AfterTargetID: 42, UpdatedAt: time.Now().UTC()}
	if err := db.Create(&marker).Error; err != nil {
		t.Fatal(err)
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&marker, "library_id=?", library.ID).Error; err != nil || marker.Revision != 7 || marker.AfterTargetID != 42 {
		t.Fatalf("marker changed: %+v %v", marker, err)
	}
	if raw, err := json.Marshal(marker); err != nil || string(raw) != "{}" {
		t.Fatalf("private marker exposed: %s %v", raw, err)
	}
	var versions int64
	if err := db.Table("schema_migrations").Where("version=77").Count(&versions).Error; err != nil || versions != 1 {
		t.Fatalf("v77=%d %v", versions, err)
	}
}
