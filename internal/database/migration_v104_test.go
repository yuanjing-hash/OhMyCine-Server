package database

import (
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestBoundedAutomaticJobsV104DeletesObsoleteWorkPreservesArtifacts(t *testing.T) {
	db := structureMigrationDB(t, 103)
	library := seedStructureMigrationLibrary(t, db, 103, "bounded-v104", "healthy", 0, 0)
	oldJob, oldRun, oldPhysical := seedLegacyArtifactV101(t, db, library, "old-automatic")
	newJob, newRun, newPhysical := seedLegacyArtifactV101(t, db, library, "new-bounded")
	fullJob, fullRun, _ := seedLegacyArtifactV101(t, db, library, "explicit-full")
	for id, policy := range map[string]string{
		oldRun.ID:  `{"scan_kind":"event"}`,
		newRun.ID:  `{"scan_kind":"event","scope_version":1,"entry_ids":[1]}`,
		fullRun.ID: `{"scan_kind":"manual"}`,
	} {
		if err := db.Model(&models.MediaArtifactRun{}).Where("id=?", id).Update("policy_json", policy).Error; err != nil {
			t.Fatal(err)
		}
	}
	artifact := models.MediaArtifact{OpaqueID: "preserved-v104", RunID: oldRun.ID, LibraryID: library.ID,
		Kind: models.MediaArtifactKindSTRM, TargetKind: models.MediaArtifactTargetLocalProjection,
		RelativePath: "Series/E01.strm", ContentFingerprint: "written", Managed: true, Active: true,
		Status: models.MediaArtifactStatusCompleted, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := db.Omit("source_fingerprint").Create(&artifact).Error; err != nil {
		t.Fatal(err)
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]int64{oldJob.ID: 0, newJob.ID: 1, fullJob.ID: 1} {
		var count int64
		if err := db.Model(&models.Job{}).Where("id=?", id).Count(&count).Error; err != nil || count != want {
			t.Fatalf("job=%s count=%d want=%d err=%v", id, count, want, err)
		}
	}
	var count int64
	if err := db.Model(&models.CatalogPhysicalWrite{}).Where("id=?", oldPhysical.ID).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("old physical count=%d err=%v", count, err)
	}
	if err := db.Model(&models.CatalogPhysicalWrite{}).Where("id=?", newPhysical.ID).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("new physical count=%d err=%v", count, err)
	}
	var saved models.MediaArtifact
	if err := db.First(&saved, artifact.ID).Error; err != nil || saved.ContentFingerprint != artifact.ContentFingerprint || !saved.Active {
		t.Fatalf("published artifact changed: %+v err=%v", saved, err)
	}
	if err := db.First(&oldRun, "id=?", oldRun.ID).Error; err != nil || oldRun.JobID != nil || oldRun.Status != models.MediaArtifactStatusSuperseded {
		t.Fatalf("artifact provenance is executable: %+v err=%v", oldRun, err)
	}
	if !db.Migrator().HasColumn("transfer_tasks", "catalog_published_at") {
		t.Fatal("batch publication receipt missing")
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("repeat migration: %v", err)
	}
}
