package database

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestMigrationV113CatalogExclusionsFreshUpgradeRepeat(t *testing.T) {
	for _, previous := range []int{0, 112} {
		t.Run(map[int]string{0: "fresh", 112: "upgrade"}[previous], func(t *testing.T) {
			db, err := Open(filepath.Join(t.TempDir(), "exclusions.db"))
			if err != nil {
				t.Fatal(err)
			}
			sqlDB, _ := db.DB()
			t.Cleanup(func() { _ = sqlDB.Close() })
			if previous > 0 {
				applyMigrationsThrough(t, db, previous)
			}
			if err := Migrate(db); err != nil {
				t.Fatal(err)
			}
			if !db.Migrator().HasColumn(&models.MediaLibrary{}, "exclusion_epoch") ||
				!db.Migrator().HasTable(&models.MediaCatalogExclusion{}) ||
				!db.Migrator().HasTable(&models.MediaCatalogExclusionMember{}) {
				t.Fatal("incomplete catalog exclusion schema")
			}
			var defaultValue string
			if err := db.Raw("SELECT dflt_value FROM pragma_table_info('media_libraries') WHERE name='exclusion_epoch'").Scan(&defaultValue).Error; err != nil || defaultValue != "1" {
				t.Fatalf("exclusion source epoch default=%q err=%v", defaultValue, err)
			}
			if err := Migrate(db); err != nil {
				t.Fatal(err)
			}
			var count int64
			if err := db.Table("schema_migrations").Where("version=113").Count(&count).Error; err != nil || count != 1 {
				t.Fatalf("migration receipts=%d err=%v", count, err)
			}
			for _, record := range []any{
				models.MediaCatalogExclusion{SourceFingerprint: "private-fingerprint", WorkKey: "private-work"},
				models.MediaCatalogExclusionMember{RelativePath: "/private/file.mkv", ProviderID: "private-id"},
			} {
				body, err := json.Marshal(record)
				if err != nil || string(body) != "{}" {
					t.Fatalf("private exclusion identity leaked: %s err=%v", body, err)
				}
			}
		})
	}
}
