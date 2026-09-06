package database

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestCatalogArtifactCleanupV90PreservesHistoricalMarkersWithoutAttribution(t *testing.T) {
	db := structureMigrationDB(t, 89)
	library := seedStructureMigrationLibrary(t, db, 89, "cleanup-owner", "healthy", 0, 0)
	run := models.MediaArtifactRun{ID: "legacy-generator", LibraryID: library.ID, Generation: 1, PolicyJSON: "{}", Status: models.MediaArtifactStatusCompleted}
	if err := db.Create(&run).Error; err != nil {
		t.Fatal(err)
	}
	artifact := models.MediaArtifact{OpaqueID: "cleanup-private", LibraryID: library.ID, RunID: run.ID, RelativePath: "movie.nfo", Managed: true, Status: models.MediaArtifactStatusCleanup}
	if err := db.Create(&artifact).Error; err != nil {
		t.Fatal(err)
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := db.Model(&models.CatalogArtifactCleanupClaim{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("migration attributed historical cleanup: %d %v", count, err)
	}
	claim := models.CatalogArtifactCleanupClaim{ArtifactID: artifact.ID, LibraryID: library.ID, OriginalStatus: models.MediaArtifactStatusCompleted, ManifestDigest: "digest", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := db.Create(&claim).Error; err != nil {
		t.Fatal(err)
	}
	if raw, err := json.Marshal(claim); err != nil || string(raw) != "{}" {
		t.Fatal("cleanup private proof exposed")
	}
	if err := db.Model(&claim).Update("owner_run_id", run.ID).Error; err == nil {
		t.Fatal("accepted partial owner proof")
	}
	if err := db.Delete(&artifact).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.CatalogArtifactCleanupClaim{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("exact manifest deletion did not remove claim: %d %v", count, err)
	}
	for _, name := range []string{"idx_catalog_artifact_cleanup_owner", "idx_catalog_artifact_cleanup_library", "idx_catalog_artifact_receipt_target_phase"} {
		if err := db.Raw("SELECT count(*) FROM sqlite_master WHERE type = 'index' AND name = ?", name).Scan(&count).Error; err != nil || count != 1 {
			t.Fatalf("index=%s count=%d err=%v", name, count, err)
		}
	}
}
