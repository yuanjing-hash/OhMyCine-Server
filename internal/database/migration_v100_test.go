package database

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestLibraryRetirementJobsV100FreshUpgradeAndRepeat(t *testing.T) {
	t.Run("upgrade", func(t *testing.T) {
		db := structureMigrationDB(t, 99)
		now := time.Now().UTC()
		retirement := models.MediaLibraryRetirement{ID: "retirement-v100", LibraryID: 42, ActorID: 1, JobID: "retirement-job-v100", Phase: "queued", Revision: 1, CreatedAt: now, UpdatedAt: now}
		if err := db.Create(&retirement).Error; err != nil {
			t.Fatal(err)
		}
		preexisting := models.Job{ID: "owned-pre-v100", CreatedByKind: "system", JobType: "media_artifact", Priority: 1, LanePosition: 1, Revision: 1, Status: models.JobStatusQueued, DisplayName: "pre-v100", ResourceKey: "media-artifact-library:42", PayloadJSON: `{}`, CheckpointJSON: `{}`, Generation: 1, CreatedAt: now, UpdatedAt: now}
		if err := db.Create(&preexisting).Error; err != nil {
			t.Fatal(err)
		}
		if err := Migrate(db); err != nil {
			t.Fatal(err)
		}
		for _, jobID := range []string{"owned-a", "owned-b"} {
			if err := db.Create(&models.MediaLibraryRetirementJob{RetirementID: retirement.ID, JobID: jobID, CreatedAt: now}).Error; err != nil {
				t.Fatal(err)
			}
		}
		if err := Migrate(db); err != nil {
			t.Fatalf("repeat migrate: %v", err)
		}
		var count int64
		if err := db.Model(&models.MediaLibraryRetirementJob{}).Where("retirement_id = ?", retirement.ID).Count(&count).Error; err != nil || count != 3 {
			t.Fatalf("frozen jobs count=%d err=%v", count, err)
		}
		if err := db.Table("schema_migrations").Where("version = 100").Count(&count).Error; err != nil || count != 1 {
			t.Fatalf("v100 ledger count=%d err=%v", count, err)
		}
		var foreignKeys int64
		if err := db.Raw("SELECT COUNT(*) FROM pragma_foreign_key_list('media_library_retirement_jobs')").Scan(&foreignKeys).Error; err != nil || foreignKeys != 0 {
			t.Fatalf("unexpected retirement-job foreign keys=%d err=%v", foreignKeys, err)
		}
		var heads int64
		if err := db.Model(&models.CatalogHead{}).Count(&heads).Error; err != nil || heads != 0 {
			t.Fatalf("migration activated catalog heads=%d err=%v", heads, err)
		}
	})

	t.Run("fresh", func(t *testing.T) {
		db, err := Open(filepath.Join(t.TempDir(), "fresh-v100.db"))
		if err != nil {
			t.Fatal(err)
		}
		sqlDB, err := db.DB()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = sqlDB.Close() })
		if err := Migrate(db); err != nil {
			t.Fatal(err)
		}
		if !db.Migrator().HasTable("media_library_retirement_jobs") {
			t.Fatal("media_library_retirement_jobs missing")
		}
		var indexes int64
		if err := db.Raw("SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_library_retirement_jobs_job'").Scan(&indexes).Error; err != nil || indexes != 1 {
			t.Fatalf("job index count=%d err=%v", indexes, err)
		}
	})
}
