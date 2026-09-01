package database

import (
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestMigrationV60AddsStructureDiagnosticsAndRepairQueue(t *testing.T) {
	db, err := Open(t.TempDir() + "/migration-v60.db")
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{"structure_status", "structure_issue_count", "structure_error_code", "structure_checked_at"} {
		if !db.Migrator().HasColumn(&models.MediaLibrary{}, column) {
			t.Fatalf("media_libraries missing %s", column)
		}
	}
	if !db.Migrator().HasTable(&models.MediaLibraryStructureRepair{}) {
		t.Fatal("media_library_structure_repairs table missing")
	}
	var policy models.QueuePolicy
	if err := db.First(&policy, "job_type = ?", "media_library_repair").Error; err != nil {
		t.Fatal(err)
	}
	if policy.ResourceConcurrency != 1 || policy.MaxAttempts != 5 {
		t.Fatalf("policy=%+v", policy)
	}
	var count int64
	if err := db.Table("schema_migrations").Where("version = ?", 60).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("v60 migration count=%d err=%v", count, err)
	}
}
