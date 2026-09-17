package database

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestMigrationV110ProviderDeletionFreshUpgradeRepeat(t *testing.T) {
	for _, previous := range []int{0, 109} {
		t.Run(string(rune('A'+previous)), func(t *testing.T) {
			db, err := Open(filepath.Join(t.TempDir(), "events.db"))
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
			for _, column := range []string{"source_fingerprint", "resolution_code", "resolution_reason", "retry_after"} {
				if !db.Migrator().HasColumn("media_library_provider_events", column) {
					t.Fatal(column)
				}
			}
			for _, table := range []string{"media_library_deletion_evidences", "media_library_provider_paths"} {
				if !db.Migrator().HasTable(table) {
					t.Fatal(table)
				}
			}
			if err := Migrate(db); err != nil {
				t.Fatal(err)
			}
			var count int64
			if err := db.Table("schema_migrations").Where("version = ?", 110).Count(&count).Error; err != nil || count != 1 {
				t.Fatal(count, err)
			}
			for _, record := range []any{models.MediaLibraryProviderEvent{SourceFingerprint: "private", ResolutionReason: "reason"}, models.MediaLibraryProviderPath{ProviderID: "private"}, models.MediaLibraryDeletionEvidence{ProviderID: "private"}} {
				body, err := json.Marshal(record)
				if err != nil || string(body) != "{}" {
					t.Fatal("private event provenance leaked")
				}
			}
		})
	}
}
