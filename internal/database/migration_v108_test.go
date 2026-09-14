package database

import (
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"testing"
)

func TestArtifactSourceFingerprintV108PreservesManagedManifest(t *testing.T) {
	db := structureMigrationDB(t, 107)
	library := seedStructureMigrationLibrary(t, db, 107, "source-v108", "healthy", 0, 0)
	_, run, _ := seedLegacyArtifactV101(t, db, library, "source-v108")
	row := models.MediaArtifact{OpaqueID: "source-v108", RunID: run.ID, LibraryID: library.ID, SourceIdentity: "asset:1", RelativePath: "/Movie.srt", TargetKind: models.MediaArtifactTargetLocalProjection, Kind: models.MediaArtifactKindSubtitle, Managed: true, Active: true, Status: models.MediaArtifactStatusCompleted, ContentFingerprint: "existing-digest"}
	if err := db.Omit("SourceFingerprint").Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	var saved models.MediaArtifact
	if err := db.First(&saved, row.ID).Error; err != nil {
		t.Fatal(err)
	}
	if saved.SourceFingerprint != "" || saved.ContentFingerprint != row.ContentFingerprint || !saved.Managed || !saved.Active {
		t.Fatalf("manifest changed: %+v", saved)
	}
	if err := db.Model(&saved).Update("source_fingerprint", "new-cloud-snapshot").Error; err != nil {
		t.Fatal(err)
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&saved, row.ID).Error; err != nil || saved.SourceFingerprint != "new-cloud-snapshot" {
		t.Fatalf("repeat migration lost fingerprint: %+v err=%v", saved, err)
	}
}
