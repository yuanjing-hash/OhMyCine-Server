package database

import (
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"testing"
	"time"
)

func TestMigrationV118NullableSearchTimeoutFreshUpgradeAndRepeat(t *testing.T) {
	for _, upgrade := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh", true: "upgrade"}[upgrade], func(t *testing.T) {
			db, err := Open(t.TempDir() + "/search.db")
			if err != nil {
				t.Fatal(err)
			}
			sql, _ := db.DB()
			t.Cleanup(func() { _ = sql.Close() })
			if upgrade {
				applyMigrationsThrough(t, db, 117)
				now := time.Now()
				if err = db.Exec(`INSERT INTO sites(name,name_normalized,kind,base_url,credential_ciphertext,timeout_seconds,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`, "Jackett", "jackett", "torznab", "http://fixture:9117", "encrypted-preserved", 12, now, now).Error; err != nil {
					t.Fatal(err)
				}
			}
			if err = Migrate(db); err != nil {
				t.Fatal(err)
			}
			if err = Migrate(db); err != nil {
				t.Fatal(err)
			}
			if !db.Migrator().HasColumn(&models.Site{}, "search_timeout_seconds") {
				t.Fatal("column missing")
			}
			if upgrade {
				var record models.Site
				if err = db.First(&record).Error; err != nil {
					t.Fatal(err)
				}
				if record.SearchTimeoutSeconds != nil || record.TimeoutSeconds != 12 || record.CredentialCiphertext != "encrypted-preserved" {
					t.Fatalf("legacy changed: %+v", record)
				}
				if err = db.Model(&record).Update("search_timeout_seconds", 181).Error; err == nil {
					t.Fatal("DB accepted invalid timeout")
				}
				if err = db.Model(&record).Update("search_timeout_seconds", 160).Error; err != nil {
					t.Fatal(err)
				}
				if err = Migrate(db); err != nil {
					t.Fatal(err)
				}
				if err = db.First(&record).Error; err != nil || record.SearchTimeoutSeconds == nil || *record.SearchTimeoutSeconds != 160 {
					t.Fatalf("explicit policy changed: %+v %v", record, err)
				}
			}
		})
	}
}
