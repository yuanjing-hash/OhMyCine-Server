package database

import (
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"testing"
)

func TestStructureRepairProgressV94UpgradeRepeat(t *testing.T) {
	db := structureMigrationDB(t, 93)
	seedStructureMigrationLibrary(t, db, 93, "progress", "issues", 1, 1)
	for i := 0; i < 2; i++ {
		if err := Migrate(db); err != nil {
			t.Fatal(err)
		}
	}
	for _, column := range []string{"current_action", "current_item", "current_batch_size"} {
		if !db.Migrator().HasColumn(&models.MediaLibraryStructureRepair{}, column) {
			t.Fatal(column)
		}
	}
}
