package database

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestFastMediaLibraryScanMigrationAndPrivateState(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "v69.db"))
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{"phase", "enumerated", "processed", "persisted", "deduplicated", "recognition_total", "recognition_completed", "persistence_stage", "database_error_class", "catalog_published_at", "source_fingerprint", "checkpoint_json"} {
		if !db.Migrator().HasColumn(&models.MediaLibraryScanRun{}, column) {
			t.Fatalf("media_library_scan_runs.%s missing", column)
		}
	}
	if !db.Migrator().HasTable(&models.MediaLibraryScanStaging{}) {
		t.Fatal("media_library_scan_stagings missing")
	}
	if !db.Migrator().HasTable(&models.MediaLibraryProviderEvent{}) {
		t.Fatal("media_library_provider_events missing")
	}
	var policy models.QueuePolicy
	if err := db.First(&policy, "job_type = ?", "media_library_recognition").Error; err != nil || policy.Concurrency != 2 || policy.ResourceConcurrency != 1 {
		t.Fatalf("recognition policy=%+v err=%v", policy, err)
	}
	payload, err := json.Marshal(models.MediaLibraryScanRun{SourceFingerprint: "provider-private", CheckpointJSON: `{"offset":1150}`})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), "provider-private") || strings.Contains(string(payload), "offset") || strings.Contains(string(payload), "checkpoint") || strings.Contains(string(payload), "source_fingerprint") {
		t.Fatalf("private scan state leaked: %s", payload)
	}
}
