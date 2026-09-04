package database

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestMigrationV70AddsFastStructureDiagnosisProjection(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "v70.db"))
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
	if !db.Migrator().HasTable(&models.MediaLibraryStructureDiagnosis{}) {
		t.Fatal("media_library_structure_diagnoses missing")
	}
	for _, column := range []string{"library_id", "job_id", "scan_run_id", "generation", "scan_kind", "status", "total_items", "processed_items", "issue_count", "repairable_count", "unrecognized_count", "missing_episode_count", "invalid_path_count", "template_error_count", "duplicate_target_count", "sidecar_conflict_count", "issues_json", "last_error_code"} {
		if !db.Migrator().HasColumn(&models.MediaLibraryStructureDiagnosis{}, column) {
			t.Fatalf("media_library_structure_diagnoses.%s missing", column)
		}
	}
	var policy models.QueuePolicy
	if err := db.First(&policy, "job_type = ?", "media_library_structure_diagnosis").Error; err != nil || policy.Concurrency != 2 || policy.ResourceConcurrency != 1 || policy.MaxAttempts != 3 {
		t.Fatalf("diagnosis policy=%+v err=%v", policy, err)
	}
	var count int64
	if err := db.Table("schema_migrations").Where("version = ?", 70).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("v70 migration count=%d err=%v", count, err)
	}
	raw, err := json.Marshal(models.MediaLibraryStructureDiagnosis{IssuesJSON: `[{"current_path":"private/path.mkv"}]`})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "private/path") || strings.Contains(string(raw), "issues_json") {
		t.Fatalf("private diagnosis samples leaked: %s", raw)
	}
}
