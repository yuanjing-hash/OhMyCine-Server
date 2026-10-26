package database

import (
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"testing"
	"time"
)

func TestMigrationV122CalendarAndCapacityAreAdditiveAndRepeatable(t *testing.T) {
	for _, upgrade := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh", true: "upgrade"}[upgrade], func(t *testing.T) {
			db, err := Open(t.TempDir() + "/calendar.db")
			if err != nil {
				t.Fatal(err)
			}
			sql, _ := db.DB()
			t.Cleanup(func() { _ = sql.Close() })
			if upgrade {
				applyMigrationsThrough(t, db, 121)
				now := time.Now().UTC()
				if err := db.Exec(`INSERT INTO storages(id,name,name_normalized,type,root_path,root_path_normalized,capabilities,enabled,last_probe_readable,created_at,updated_at) VALUES(1,?,?,?,?,?,?,1,1,?,?)`, "原数据源", "original", "local", "/owned", "original", "{}", now, now).Error; err != nil {
					t.Fatal(err)
				}
			}
			if err := Migrate(db); err != nil {
				t.Fatal(err)
			}
			if err := Migrate(db); err != nil {
				t.Fatal(err)
			}
			for _, model := range []any{&models.CalendarSeason{}, &models.CalendarEpisode{}, &models.CalendarRefreshCursor{}} {
				if !db.Migrator().HasTable(model) {
					t.Fatal("calendar cache table missing")
				}
			}
			if err := db.Create(&models.CalendarEpisode{SeasonKey: "missing-season", EpisodeNumber: 1}).Error; err == nil {
				t.Fatal("orphan calendar episode accepted")
			}
			if err := db.Create(&models.CalendarSeason{Key: "fixture", Generation: 1, TMDBID: 100, SeasonNumber: 1, Language: "zh-CN"}).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Create(&models.CalendarEpisode{SeasonKey: "fixture", EpisodeNumber: 1}).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Delete(&models.CalendarSeason{}, "key = ?", "fixture").Error; err != nil {
				t.Fatal(err)
			}
			var orphanCount int64
			if err := db.Model(&models.CalendarEpisode{}).Where("season_key = ?", "fixture").Count(&orphanCount).Error; err != nil || orphanCount != 0 {
				t.Fatal("calendar cache deletion did not cascade")
			}
			if upgrade {
				var storage models.Storage
				if err := db.First(&storage, 1).Error; err != nil {
					t.Fatal(err)
				}
				if storage.RootPath != "/owned" || storage.Name != "原数据源" || !storage.LastProbeReadable || storage.LastCapacityStatus != "" || storage.LastCapacityFreeBytes != nil {
					t.Fatalf("old storage changed: %+v", storage)
				}
			}
			var policy models.QueuePolicy
			if err := db.First(&policy, "job_type = ?", "subscription_calendar_refresh").Error; err != nil || policy.Concurrency != 2 {
				t.Fatalf("policy=%+v err=%v", policy, err)
			}
			if err := db.Model(&models.QueuePolicy{}).Where("job_type = ?", policy.JobType).Update("concurrency", 1).Error; err != nil {
				t.Fatal(err)
			}
			if err := Migrate(db); err != nil {
				t.Fatal(err)
			}
			if err := db.First(&policy, "job_type = ?", policy.JobType).Error; err != nil || policy.Concurrency != 1 {
				t.Fatal("migration overwrote administrator queue policy")
			}
		})
	}
}

func TestStorageCapacitySamplesDoNotPolluteLegacyCreate(t *testing.T) {
	db, err := Open(t.TempDir() + "/pre-capacity.db")
	if err != nil {
		t.Fatal(err)
	}
	pool, _ := db.DB()
	t.Cleanup(func() { _ = pool.Close() })
	applyMigrationsThrough(t, db, 121)
	bytes := uint64(1234)
	now := time.Now().UTC()
	row := models.Storage{Name: "旧结构数据源", NameNormalized: "legacy-create", Type: "local", RootPath: "/legacy", RootPathNormalized: "/legacy", Capabilities: "{}", Enabled: true, LastCapacityFreeBytes: &bytes, LastCapacityCheckedAt: &now, LastCapacityStatus: "ok"}
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("current model inserted future sample columns into old schema: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	var saved models.Storage
	if err := db.First(&saved, row.ID).Error; err != nil {
		t.Fatal(err)
	}
	if saved.LastCapacityFreeBytes != nil || saved.LastCapacityCheckedAt != nil || saved.LastCapacityStatus != "" {
		t.Fatal("ordinary Storage create seeded a measurement")
	}
	if err := db.Model(&models.Storage{}).Where("id = ?", row.ID).UpdateColumns(map[string]any{"last_capacity_free_bytes": bytes, "last_capacity_checked_at": now, "last_capacity_status": "ok"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&saved, row.ID).Error; err != nil {
		t.Fatal(err)
	}
	if saved.LastCapacityFreeBytes == nil || *saved.LastCapacityFreeBytes != bytes || saved.LastCapacityStatus != "ok" {
		t.Fatal("explicit sampler lost update permission")
	}
}
