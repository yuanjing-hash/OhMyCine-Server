package database

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestArtifactRecoveryHistoryV97PreservesReferencesAndSeparatesLifetimes(t *testing.T) {
	db := structureMigrationDB(t, 96)
	library := seedStructureMigrationLibrary(t, db, 96, "artifact-v97", "healthy", 0, 0)
	now := time.Now().UTC()
	job := models.Job{ID: "artifact-job-v97", CreatedByKind: "system", JobType: "media_artifact", Status: models.JobStatusCompleted, Revision: 1, PayloadJSON: `{}`, CheckpointJSON: `{}`, DisplayName: "artifact", FinishedAt: &now, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&job).Error; err != nil {
		t.Fatal(err)
	}
	policy := `{"catalog_binding_id":"binding-v97"}`
	if err := db.Exec(`INSERT INTO media_artifact_runs(id,library_id,generation,job_id,policy_json,status,expected_count,written_count,updated_count,removed_count,skipped_count,failed_count,retry_count,error_code,cleanup_status,cleanup_error_code,started_at,finished_at,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		"run-v97", library.ID, 7, job.ID, policy, models.MediaArtifactStatusCompleted, 3, 1, 1, 0, 0, 1, 2, "artifact_write_failed", models.MediaArtifactCleanupCompleted, "", now, now, now, now).Error; err != nil {
		t.Fatal(err)
	}
	artifacts := []models.MediaArtifact{
		{OpaqueID: "artifact-v97-a", RunID: "run-v97", LibraryID: library.ID, Kind: models.MediaArtifactKindSTRM, TargetKind: models.MediaArtifactTargetLocalProjection, RelativePath: "/a.strm", Managed: true, Active: true, Status: models.MediaArtifactStatusCompleted, CreatedAt: now, UpdatedAt: now},
		{OpaqueID: "artifact-v97-b", RunID: "run-v97", LibraryID: library.ID, Kind: models.MediaArtifactKindNFO, TargetKind: models.MediaArtifactTargetLocalProjection, RelativePath: "/a.nfo", Managed: true, Active: true, Status: models.MediaArtifactStatusCompleted, CreatedAt: now, UpdatedAt: now},
	}
	if err := db.Omit("source_fingerprint").Create(&artifacts).Error; err != nil {
		t.Fatal(err)
	}
	physical := models.CatalogPhysicalWrite{LibraryID: library.ID, OwnerKind: "artifact", OwnerID: "run-v97", Revision: 1, State: "settled", JobID: job.ID, OwnerDigest: "owner", SourceFingerprint: "source", ConfigFingerprint: "config", EnteredAt: now, SettledAt: &now, UpdatedAt: now}
	if err := db.Create(&physical).Error; err != nil {
		t.Fatal(err)
	}
	receipt := models.CatalogArtifactWriteReceipt{LibraryID: library.ID, RunID: "run-v97", ArtifactID: artifacts[0].ID, PhysicalWriteID: physical.ID, PermitRevision: 1, Revision: 1, TargetKind: models.MediaArtifactTargetLocalProjection, RelativePath: "/a.strm", RootIdentity: "root", PolicyDigest: "policy", SourceFingerprint: "source", ConfigFingerprint: "config", BeforeArtifactJSON: `{}`, AfterArtifactJSON: `{}`, Phase: "reconciled_after", CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&receipt).Error; err != nil {
		t.Fatal(err)
	}
	ownerRunID, physicalID := "run-v97", physical.ID
	claim := models.CatalogArtifactCleanupClaim{ArtifactID: artifacts[1].ID, LibraryID: library.ID, OwnerRunID: &ownerRunID, PhysicalWriteID: &physicalID, PermitRevision: 1, OriginalStatus: models.MediaArtifactStatusCompleted, ManifestDigest: "manifest", RootIdentity: "root", GeneratorPolicyDigest: "policy", CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&claim).Error; err != nil {
		t.Fatal(err)
	}

	applyMigrationsThrough(t, db, 97)
	var run models.MediaArtifactRun
	if err := db.First(&run, "id = ?", "run-v97").Error; err != nil {
		t.Fatal(err)
	}
	if run.CatalogBindingID != "binding-v97" || run.ProcessedCount != 3 || run.SucceededCount != 2 || run.RetryCount != 2 || run.JobID == nil || *run.JobID != job.ID {
		t.Fatalf("run not preserved: %+v", run)
	}
	for table, want := range map[string]int64{"media_artifacts": 2, "catalog_artifact_write_receipts": 1, "catalog_artifact_cleanup_claims": 1, "catalog_physical_writes": 1} {
		var count int64
		if err := db.Table(table).Count(&count).Error; err != nil || count != want {
			t.Fatalf("%s count=%d want=%d err=%v", table, count, want, err)
		}
	}
	var violations int64
	if err := db.Raw(`SELECT COUNT(*) FROM pragma_foreign_key_check`).Scan(&violations).Error; err != nil || violations != 0 {
		t.Fatalf("foreign key violations=%d err=%v", violations, err)
	}
	second := models.MediaArtifactRun{ID: "run-v97-next-lifetime", LibraryID: library.ID, Generation: 7, CatalogBindingID: "binding-v97-next", PolicyJSON: `{"catalog_binding_id":"binding-v97-next"}`, Status: models.MediaArtifactStatusQueued, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&second).Error; err != nil {
		t.Fatalf("same generation in a new catalog lifetime was rejected: %v", err)
	}
	duplicate := models.MediaArtifactRun{ID: "run-v97-duplicate", LibraryID: library.ID, Generation: 8, CatalogBindingID: second.CatalogBindingID, PolicyJSON: second.PolicyJSON, Status: models.MediaArtifactStatusQueued, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&duplicate).Error; err == nil {
		t.Fatal("duplicate catalog lifetime unexpectedly created a second run")
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("repeat migrate: %v", err)
	}
}

func TestManagementHistoryRetryV98FreshUpgradeAndRepeat(t *testing.T) {
	db := structureMigrationDB(t, 97)
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	columns := map[string][]string{
		"jobs":                    {"failure_retry_count", "history_cleared_at"},
		"media_artifact_runs":     {"history_cleared_at"},
		"media_library_scan_runs": {"history_cleared_at"},
		"follow_runs":             {"history_cleared_at"},
		"schedule_runs":           {"history_cleared_at"},
		"download_tasks":          {"history_cleared_at"},
		"transfer_tasks":          {"history_cleared_at"},
	}
	for table, names := range columns {
		for _, column := range names {
			if !db.Migrator().HasColumn(table, column) {
				t.Fatalf("upgrade missing %s.%s", table, column)
			}
		}
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("repeat migrate: %v", err)
	}
	var count int64
	if err := db.Table("schema_migrations").Where("version IN ?", []int{97, 98}).Count(&count).Error; err != nil || count != 2 {
		t.Fatalf("v97/v98 ledger count=%d err=%v", count, err)
	}

	fresh, err := Open(filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatal(err)
	}
	freshSQL, err := fresh.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = freshSQL.Close() })
	if err := Migrate(fresh); err != nil {
		t.Fatal(err)
	}
	for table, names := range columns {
		for _, column := range names {
			if !fresh.Migrator().HasColumn(table, column) {
				t.Fatalf("fresh schema missing %s.%s", table, column)
			}
		}
	}
	if !fresh.Migrator().HasColumn("media_artifact_runs", "catalog_binding_id") {
		t.Fatal("fresh schema is missing media_artifact_runs.catalog_binding_id")
	}
}

func TestManagementSeedingHistoryV99FreshUpgradeAndRepeat(t *testing.T) {
	db := structureMigrationDB(t, 98)
	if db.Migrator().HasColumn("seeding_tasks", "history_cleared_at") {
		t.Fatal("v98 unexpectedly contains the v99 seeding history marker")
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	if !db.Migrator().HasColumn("seeding_tasks", "history_cleared_at") {
		t.Fatal("upgrade missing seeding_tasks.history_cleared_at")
	}
	if !db.Migrator().HasIndex("seeding_tasks", "idx_seeding_tasks_history_cleared") {
		t.Fatal("upgrade missing idx_seeding_tasks_history_cleared")
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("repeat migrate: %v", err)
	}
	var count int64
	if err := db.Table("schema_migrations").Where("version = ?", 99).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("v99 ledger count=%d err=%v", count, err)
	}

	fresh, err := Open(filepath.Join(t.TempDir(), "fresh-v99.db"))
	if err != nil {
		t.Fatal(err)
	}
	freshSQL, err := fresh.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = freshSQL.Close() })
	if err := Migrate(fresh); err != nil {
		t.Fatal(err)
	}
	if !fresh.Migrator().HasColumn("seeding_tasks", "history_cleared_at") || !fresh.Migrator().HasIndex("seeding_tasks", "idx_seeding_tasks_history_cleared") {
		t.Fatal("fresh schema is missing v99 seeding history schema")
	}
}
