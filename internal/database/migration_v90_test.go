package database

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestCatalogArtifactWriteV90PreservesLegacyCoverageAndRestrictsEvidence(t *testing.T) {
	db := structureMigrationDB(t, 89)
	library := seedStructureMigrationLibrary(t, db, 89, "artifact-receipt", "healthy", 0, 0)
	now := time.Now().UTC()
	legacy := models.CatalogPhysicalWrite{LibraryID: library.ID, OwnerKind: "artifact", OwnerID: "old-run", Revision: 1, State: "quiescent", EnteredAt: now, UpdatedAt: now}
	if err := db.Omit("artifact_receipt_version").Create(&legacy).Error; err != nil {
		t.Fatal(err)
	}
	for pass := 0; pass < 2; pass++ {
		if err := Migrate(db); err != nil {
			t.Fatal(err)
		}
	}
	var proof models.CatalogPhysicalWrite
	if err := db.First(&proof, legacy.ID).Error; err != nil || proof.State != "quiescent" || proof.ArtifactReceiptVersion != 0 {
		t.Fatalf("legacy proof changed: %+v %v", proof, err)
	}
	run := models.MediaArtifactRun{ID: "old-run", LibraryID: library.ID, Generation: 1, PolicyJSON: "{}", Status: models.MediaArtifactStatusFailed}
	if err := db.Create(&run).Error; err != nil {
		t.Fatal(err)
	}
	receipt := models.CatalogArtifactWriteReceipt{LibraryID: library.ID, RunID: run.ID, ArtifactID: 123, PhysicalWriteID: proof.ID, PermitRevision: 1, Revision: 1, TargetKind: "local_projection", RelativePath: "private.nfo", RootIdentity: "private-root", PolicyDigest: "digest", BeforeArtifactJSON: "{}", AfterArtifactJSON: "{}", Phase: "prepared", CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&receipt).Error; err != nil {
		t.Fatal(err)
	}
	if raw, err := json.Marshal(receipt); err != nil || string(raw) != "{}" {
		t.Fatal("receipt leaks private evidence")
	}
	if err := db.Delete(&run).Error; err == nil {
		t.Fatal("deleted referenced run")
	}
	if err := db.Delete(&proof).Error; err == nil {
		t.Fatal("deleted referenced physical evidence")
	}
	if err := db.Model(&receipt).Update("phase", "expired").Error; err == nil {
		t.Fatal("allowed expiry to replace reconciliation")
	}
	if err := db.Model(&proof).Update("artifact_receipt_version", 2).Error; err == nil {
		t.Fatal("unsupported receipt coverage")
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := db.Model(&models.CatalogHead{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatal("migration enabled catalog conversion")
	}
	for _, name := range []string{"idx_catalog_artifact_receipt_library_phase", "idx_catalog_artifact_receipt_run_phase", "idx_catalog_artifact_receipt_physical"} {
		if err := db.Raw("SELECT count(*) FROM sqlite_master WHERE type='index' AND name=?", name).Scan(&count).Error; err != nil || count != 1 {
			t.Fatalf("missing index %s: %v", name, err)
		}
	}
}
