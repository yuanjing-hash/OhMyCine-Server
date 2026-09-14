package database

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

func seedLegacyArtifactV101(t *testing.T, db *gorm.DB, library models.MediaLibrary, suffix string) (models.Job, models.MediaArtifactRun, models.CatalogPhysicalWrite) {
	t.Helper()
	now := time.Now().UTC()
	runID := "legacy-v101-run-" + suffix
	job := models.Job{
		ID: "legacy-v101-job-" + suffix, CreatedByKind: "system", JobType: "media_artifact",
		Status: models.JobStatusCancelled, Revision: 1, DisplayName: "legacy artifact",
		PayloadJSON: fmt.Sprintf(`{"artifact_run_id":%q}`, runID), CheckpointJSON: `{}`,
		FinishedAt: &now, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&job).Error; err != nil {
		t.Fatal(err)
	}
	run := models.MediaArtifactRun{
		ID: runID, LibraryID: library.ID, Generation: 1, JobID: &job.ID, PolicyJSON: `{}`,
		Status: models.MediaArtifactStatusFailed, ErrorCode: "historic failure",
		CleanupStatus: models.MediaArtifactCleanupSkipped, CleanupAt: &now, FinishedAt: &now,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&run).Error; err != nil {
		t.Fatal(err)
	}
	physical := models.CatalogPhysicalWrite{
		LibraryID: library.ID, OwnerKind: "artifact", OwnerID: run.ID, Revision: 2,
		State: "quiescent", JobID: job.ID, JobLeaseHash: "historic-lease", RuntimeID: "historic-runtime",
		OwnerDigest: "owner", SourceFingerprint: "source", ConfigFingerprint: "config",
		ArtifactReceiptVersion: 0, EnteredAt: now, UpdatedAt: now,
	}
	if err := db.Create(&physical).Error; err != nil {
		t.Fatal(err)
	}
	return job, run, physical
}

func TestLegacyArtifactNoIORecoveryV101FreshUpgradeAndRepeat(t *testing.T) {
	db := structureMigrationDB(t, 100)
	// v108 adds source_fingerprint; historical v100 inserts must omit it.
	library := seedStructureMigrationLibrary(t, db, 100, "legacy-v101", "healthy", 0, 0)
	_, safeRun, safePhysical := seedLegacyArtifactV101(t, db, library, "safe")
	for index := 0; index < 3; index++ {
		artifact := models.MediaArtifact{
			OpaqueID: fmt.Sprintf("legacy-v101-placeholder-%d", index), RunID: safeRun.ID, LibraryID: library.ID,
			Kind: models.MediaArtifactKindNFO, TargetKind: models.MediaArtifactTargetLocalProjection,
			RelativePath: fmt.Sprintf("/pending-%d.nfo", index), Managed: true, Active: true,
			Status: models.MediaArtifactStatusQueued, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}
		if err := db.Omit("source_fingerprint").Create(&artifact).Error; err != nil {
			t.Fatal(err)
		}
	}

	_, unsafeRun, unsafePhysical := seedLegacyArtifactV101(t, db, library, "unsafe-artifact")
	unsafeArtifact := models.MediaArtifact{
		OpaqueID: "legacy-v101-written", RunID: unsafeRun.ID, LibraryID: library.ID,
		Kind: models.MediaArtifactKindNFO, TargetKind: models.MediaArtifactTargetLocalProjection,
		RelativePath: "/written.nfo", ContentFingerprint: "published-fingerprint", Managed: true, Active: true,
		Status: models.MediaArtifactStatusCompleted, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := db.Omit("source_fingerprint").Create(&unsafeArtifact).Error; err != nil {
		t.Fatal(err)
	}

	_, unsafeCountRun, unsafeCountPhysical := seedLegacyArtifactV101(t, db, library, "unsafe-count")
	if err := db.Model(&unsafeCountRun).Update("written_count", 1).Error; err != nil {
		t.Fatal(err)
	}
	_, _, unsafeSettledAtPhysical := seedLegacyArtifactV101(t, db, library, "unsafe-settled-at")
	if err := db.Model(&unsafeSettledAtPhysical).Update("settled_at", time.Now().UTC()).Error; err != nil {
		t.Fatal(err)
	}
	unsafeIdentityJob, unsafeIdentityRun, unsafeIdentityPhysical := seedLegacyArtifactV101(t, db, library, "unsafe-identity")
	if err := db.Model(&unsafeIdentityJob).Update("payload_json", fmt.Sprintf(`{"artifact_run_id":%q,"unexpected":true}`, unsafeIdentityRun.ID)).Error; err != nil {
		t.Fatal(err)
	}
	unsafeDuplicateJob, unsafeDuplicateRun, unsafeDuplicatePhysical := seedLegacyArtifactV101(t, db, library, "unsafe-duplicate-payload")
	if err := db.Model(&unsafeDuplicateJob).Update("payload_json", fmt.Sprintf(`{"artifact_run_id":%q,"artifact_run_id":%q}`, unsafeDuplicateRun.ID, "different-run")).Error; err != nil {
		t.Fatal(err)
	}

	_, unsafeReceiptRun, unsafeReceiptPhysical := seedLegacyArtifactV101(t, db, library, "unsafe-receipt")
	unsafeReceiptArtifact := models.MediaArtifact{
		OpaqueID: "legacy-v101-receipt-placeholder", RunID: unsafeReceiptRun.ID, LibraryID: library.ID,
		Kind: models.MediaArtifactKindNFO, TargetKind: models.MediaArtifactTargetLocalProjection,
		RelativePath: "/receipt-pending.nfo", Managed: true, Active: true,
		Status: models.MediaArtifactStatusQueued, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := db.Omit("source_fingerprint").Create(&unsafeReceiptArtifact).Error; err != nil {
		t.Fatal(err)
	}
	receipt := models.CatalogArtifactWriteReceipt{
		LibraryID: library.ID, RunID: unsafeReceiptRun.ID, ArtifactID: unsafeReceiptArtifact.ID,
		PhysicalWriteID: unsafeReceiptPhysical.ID, PermitRevision: unsafeReceiptPhysical.Revision,
		Revision: 1, TargetKind: unsafeReceiptArtifact.TargetKind, RelativePath: unsafeReceiptArtifact.RelativePath,
		RootIdentity: "root", PolicyDigest: "policy", SourceFingerprint: "source", ConfigFingerprint: "config",
		BeforeArtifactJSON: `{}`, AfterArtifactJSON: `{}`, Phase: "prepared",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := db.Create(&receipt).Error; err != nil {
		t.Fatal(err)
	}

	_, unsafeClaimRun, unsafeClaimPhysical := seedLegacyArtifactV101(t, db, library, "unsafe-claim")
	unsafeClaimArtifact := models.MediaArtifact{
		OpaqueID: "legacy-v101-claim-placeholder", RunID: unsafeClaimRun.ID, LibraryID: library.ID,
		Kind: models.MediaArtifactKindNFO, TargetKind: models.MediaArtifactTargetLocalProjection,
		RelativePath: "/claim-pending.nfo", Managed: true, Active: true,
		Status: models.MediaArtifactStatusQueued, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := db.Omit("source_fingerprint").Create(&unsafeClaimArtifact).Error; err != nil {
		t.Fatal(err)
	}
	claimRunID, claimPhysicalID := unsafeClaimRun.ID, unsafeClaimPhysical.ID
	claim := models.CatalogArtifactCleanupClaim{
		ArtifactID: unsafeClaimArtifact.ID, LibraryID: library.ID, OwnerRunID: &claimRunID,
		PhysicalWriteID: &claimPhysicalID, PermitRevision: unsafeClaimPhysical.Revision,
		OriginalStatus: models.MediaArtifactStatusQueued, ManifestDigest: "manifest",
		RootIdentity: "root", GeneratorPolicyDigest: "policy", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := db.Create(&claim).Error; err != nil {
		t.Fatal(err)
	}

	other := seedStructureMigrationLibrary(t, db, 100, "legacy-v101-other", "healthy", 0, 0)
	otherRun := models.MediaArtifactRun{ID: "legacy-v101-other-run", LibraryID: other.ID, Generation: 1, PolicyJSON: `{}`, Status: models.MediaArtifactStatusCompleted, CleanupStatus: models.MediaArtifactCleanupCompleted, FinishedAt: safeRun.FinishedAt, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := db.Create(&otherRun).Error; err != nil {
		t.Fatal(err)
	}
	otherArtifact := models.MediaArtifact{OpaqueID: "legacy-v101-other-artifact", RunID: otherRun.ID, LibraryID: other.ID, Kind: models.MediaArtifactKindSTRM, TargetKind: models.MediaArtifactTargetLocalProjection, RelativePath: "/keep.strm", ContentFingerprint: "keep", Managed: true, Active: true, Status: models.MediaArtifactStatusCompleted, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := db.Omit("source_fingerprint").Create(&otherArtifact).Error; err != nil {
		t.Fatal(err)
	}

	applyMigrationsThrough(t, db, 101)
	applyMigrationsThrough(t, db, 101)
	var safe models.CatalogPhysicalWrite
	if err := db.First(&safe, safePhysical.ID).Error; err != nil {
		t.Fatal(err)
	}
	if safe.State != "settled" || safe.SettledAt == nil {
		t.Fatalf("safe v0 proof was not settled: %+v", safe)
	}
	var count int64
	if err := db.Model(&models.MediaArtifact{}).Where("run_id=?", safeRun.ID).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("safe placeholders count=%d err=%v", count, err)
	}
	for _, id := range []uint64{unsafePhysical.ID, unsafeCountPhysical.ID, unsafeSettledAtPhysical.ID, unsafeIdentityPhysical.ID, unsafeDuplicatePhysical.ID, unsafeReceiptPhysical.ID, unsafeClaimPhysical.ID} {
		var row models.CatalogPhysicalWrite
		if err := db.First(&row, id).Error; err != nil {
			t.Fatal(err)
		}
		if row.State != "quiescent" || (id != unsafeSettledAtPhysical.ID && row.SettledAt != nil) {
			t.Fatalf("unsafe v0 proof %d changed: %+v", id, row)
		}
	}
	if err := db.First(&otherArtifact, otherArtifact.ID).Error; err != nil {
		t.Fatalf("other library artifact changed: %v", err)
	}
	if err := db.Table("schema_migrations").Where("version=101").Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("v101 ledger count=%d err=%v", count, err)
	}

	fresh, err := Open(filepath.Join(t.TempDir(), "fresh-v101.db"))
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
	if err := fresh.Table("schema_migrations").Where("version=101").Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("fresh v101 ledger count=%d err=%v", count, err)
	}
}
