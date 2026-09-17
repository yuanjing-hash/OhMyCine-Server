package database

import (
	"path/filepath"
	"testing"
)

func TestMigrationV111CloudCleanupDefaultsOff(t *testing.T) {
	for _, previous := range []int{0, 110} {
		db, err := Open(filepath.Join(t.TempDir(), "cleanup.db"))
		if err != nil {
			t.Fatal(err)
		}
		sqlDB, _ := db.DB()
		t.Cleanup(func() { _ = sqlDB.Close() })
		if previous != 0 {
			applyMigrationsThrough(t, db, previous)
		}
		if err := Migrate(db); err != nil {
			t.Fatal(err)
		}
		var defaultValue string
		if err := db.Raw("SELECT dflt_value FROM pragma_table_info('media_libraries') WHERE name = 'cloud_empty_cleanup_enabled'").Scan(&defaultValue).Error; err != nil || defaultValue != "0" {
			t.Fatal("unsafe default", defaultValue, err)
		}
		if err := Migrate(db); err != nil {
			t.Fatal(err)
		}
		var count int64
		if err := db.Table("schema_migrations").Where("version = 111").Count(&count).Error; err != nil || count != 1 {
			t.Fatal(count, err)
		}
	}
}
