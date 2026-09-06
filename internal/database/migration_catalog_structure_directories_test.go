package database

import "testing"

func TestCatalogStructureDirectoryReceiptMigrationIsAdditive(t *testing.T) {
	db := structureMigrationDB(t, 82)
	for i := 0; i < 2; i++ {
		if err := Migrate(db); err != nil {
			t.Fatal(err)
		}
	}
	var count int64
	if err := db.Table("catalog_structure_directory_receipts").Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("receipts=%d %v", count, err)
	}
	var foreignKeys []struct{ Table, OnDelete string }
	if err := db.Raw("PRAGMA foreign_key_list(catalog_structure_directory_receipts)").Scan(&foreignKeys).Error; err != nil {
		t.Fatal(err)
	}
	if len(foreignKeys) != 1 || foreignKeys[0].Table != "media_library_structure_repairs" || foreignKeys[0].OnDelete != "RESTRICT" {
		t.Fatalf("unbounded cascade: %+v", foreignKeys)
	}
}
