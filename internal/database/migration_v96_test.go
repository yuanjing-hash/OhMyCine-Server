package database

import (
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestCatalogArtifactIncrementalV96UpgradeRecoveryAndRepeat(t *testing.T) {
	db := structureMigrationDB(t, 95)
	library := seedStructureMigrationLibrary(t, db, 95, "artifact-incremental", "healthy", 0, 0)
	now := time.Now().UTC()
	if err := db.Model(&library).Updates(map[string]any{"artifact_applied_generation": 5, "artifact_generation": 5}).Error; err != nil {
		t.Fatal(err)
	}
	current := models.MediaArtifactRun{ID: "artifact-current-v96", LibraryID: library.ID, Generation: 5, PolicyJSON: `{}`, Status: models.MediaArtifactStatusCompleted, CleanupStatus: models.MediaArtifactCleanupCompleted, FinishedAt: &now, CreatedAt: now, UpdatedAt: now}
	orphan := models.MediaArtifactRun{ID: "artifact-orphan-v96", LibraryID: library.ID, Generation: 4, PolicyJSON: `{}`, Status: models.MediaArtifactStatusQueued, CleanupStatus: models.MediaArtifactCleanupPending, CreatedAt: now, UpdatedAt: now}
	protected := models.MediaArtifactRun{ID: "artifact-owned-v96", LibraryID: library.ID, Generation: 3, PolicyJSON: `{}`, Status: models.MediaArtifactStatusQueued, CleanupStatus: models.MediaArtifactCleanupPending, CreatedAt: now, UpdatedAt: now}
	if err := db.Omit("catalog_binding_id").Create(&[]models.MediaArtifactRun{current, orphan, protected}).Error; err != nil {
		t.Fatal(err)
	}
	owned := models.MediaArtifact{OpaqueID: "artifact-owned-manifest-v96", RunID: protected.ID, LibraryID: library.ID, SourceIdentity: "recognition:1", Kind: models.MediaArtifactKindNFO, TargetKind: models.MediaArtifactTargetLocalAdjacent, RelativePath: "/owned/tvshow.nfo", Managed: true, Active: true, Status: models.MediaArtifactStatusCompleted, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&owned).Error; err != nil {
		t.Fatal(err)
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{"scope_mode", "scope_prepared", "scope_entry_after_id", "scope_asset_after_id", "scope_recognition_after_id"} {
		if !db.Migrator().HasColumn("catalog_artifact_bindings", column) {
			t.Fatalf("missing catalog_artifact_bindings.%s", column)
		}
	}
	if !db.Migrator().HasTable("catalog_artifact_binding_items") {
		t.Fatal("catalog_artifact_binding_items missing")
	}
	if err := db.First(&orphan, "id = ?", orphan.ID).Error; err != nil || orphan.Status != models.MediaArtifactStatusSuperseded || orphan.CleanupStatus != models.MediaArtifactCleanupSkipped || orphan.FinishedAt == nil {
		t.Fatalf("provable no-I/O orphan did not converge: %+v err=%v", orphan, err)
	}
	if err := db.First(&protected, "id = ?", protected.ID).Error; err != nil || protected.Status != models.MediaArtifactStatusQueued {
		t.Fatalf("owned historical run was changed: %+v err=%v", protected, err)
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := db.Table("schema_migrations").Where("version = 96").Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("v96 ledger count=%d err=%v", count, err)
	}
	if err := db.Model(&models.MediaArtifact{}).Where("id = ?", owned.ID).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("repeat migration changed owned manifest: count=%d err=%v", count, err)
	}
}
