package database

import (
	"path/filepath"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestMigrationV71AddsUnifiedStructureIssueProjection(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "v71.db"))
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	if !db.Migrator().HasTable(&models.MediaLibraryStructureAutoState{}) {
		t.Fatal("automatic diagnosis state table missing")
	}
	for _, column := range []string{"automatic", "source_revision"} {
		if !db.Migrator().HasColumn(&models.MediaLibraryStructureDiagnosis{}, column) {
			t.Fatalf("diagnosis.%s missing", column)
		}
	}
	if !db.Migrator().HasTable(&models.MediaLibraryStructureIssue{}) || !db.Migrator().HasTable(&models.MediaLibraryStructureIssueMember{}) {
		t.Fatal("structure issue projection tables missing")
	}
	if !db.Migrator().HasTable(&models.MediaLibraryStructureRepairDraft{}) {
		t.Fatal("structure repair draft table missing")
	}
	var count int64
	if err := db.Table("schema_migrations").Where("version = ?", 71).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("v71 count=%d err=%v", count, err)
	}
}
