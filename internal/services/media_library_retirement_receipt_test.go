package services

import (
	"context"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestLibraryRetirementDiscardsArtifactReceiptsAfterQuiescence(t *testing.T) {
	for _, phase := range []string{"prepared", "conflict", "reconciled_before", "reconciled_after"} {
		t.Run(phase, func(t *testing.T) {
			s, library, actor := retirementFixture(t)
			now := time.Now().UTC()
			run := models.MediaArtifactRun{ID: "artifact-retirement", LibraryID: library.ID, Generation: 9, PolicyJSON: "{}", Status: models.MediaArtifactStatusCompleted, CleanupStatus: "completed", FinishedAt: &now, CreatedAt: now, UpdatedAt: now}
			if err := s.db.Create(&run).Error; err != nil {
				t.Fatal(err)
			}
			proof := models.CatalogPhysicalWrite{LibraryID: library.ID, OwnerKind: CatalogPhysicalArtifact, OwnerID: run.ID, State: "settled", Revision: 2, OwnerDigest: "test", EnteredAt: now, UpdatedAt: now}
			if err := s.db.Create(&proof).Error; err != nil {
				t.Fatal(err)
			}
			receipt := models.CatalogArtifactWriteReceipt{LibraryID: library.ID, RunID: run.ID, PhysicalWriteID: proof.ID, PermitRevision: 1, Revision: 1, TargetKind: "nfo", RelativePath: "private.nfo", BeforeArtifactJSON: "{}", AfterArtifactJSON: "{}", Phase: phase, CreatedAt: now, UpdatedAt: now}
			if err := s.db.Create(&receipt).Error; err != nil {
				t.Fatal(err)
			}
			claim, row := claimRetirement(t, s, library, actor)
			for i := 0; i < 80 && row.Phase != "completed"; i++ {
				if _, _, err := NewMediaLibraryRetirementWorker(s).step(context.Background(), claim, &row); err != nil {
					t.Fatal(err)
				}
			}
			if row.Phase != "completed" {
				t.Fatal("receipt FK prevented finalization")
			}
			for _, model := range []any{&models.CatalogArtifactWriteReceipt{}, &models.CatalogPhysicalWrite{}, &models.MediaArtifactRun{}} {
				var count int64
				if err := s.db.Model(model).Where("library_id=?", library.ID).Count(&count).Error; err != nil || count != 0 {
					t.Fatalf("receipt dependency survived %T: %d %v", model, count, err)
				}
			}
		})
	}
}

func TestLibraryRetirementDiscardsArtifactCleanupClaimsAfterQuiescence(t *testing.T) {
	for _, owner := range []string{"manual_null_owner", "settled_owner"} {
		t.Run(owner, func(t *testing.T) {
			s, library, actor := retirementFixture(t)
			now := time.Now().UTC()
			run := models.MediaArtifactRun{ID: "old-generator", LibraryID: library.ID, Generation: 9, PolicyJSON: "{}", Status: models.MediaArtifactStatusCompleted, CleanupStatus: "completed", FinishedAt: &now, CreatedAt: now, UpdatedAt: now}
			if err := s.db.Create(&run).Error; err != nil {
				t.Fatal(err)
			}
			proof := models.CatalogPhysicalWrite{LibraryID: library.ID, OwnerKind: CatalogPhysicalArtifact, OwnerID: run.ID, State: "settled", Revision: 2, OwnerDigest: "old-generation", EnteredAt: now, UpdatedAt: now}
			if err := s.db.Create(&proof).Error; err != nil {
				t.Fatal(err)
			}
			artifact := models.MediaArtifact{OpaqueID: "retirement-cleanup-artifact", RunID: run.ID, LibraryID: library.ID, Kind: "nfo", TargetKind: "source", RelativePath: "private.nfo", Managed: true, Active: true, Status: "written"}
			if err := s.db.Create(&artifact).Error; err != nil {
				t.Fatal(err)
			}
			claim := models.CatalogArtifactCleanupClaim{ArtifactID: artifact.ID, LibraryID: library.ID, OriginalStatus: "written", ManifestDigest: "frozen", CreatedAt: now, UpdatedAt: now}
			if owner == "settled_owner" {
				claim.OwnerRunID = &run.ID
				claim.PhysicalWriteID = &proof.ID
				claim.PermitRevision = 1
			}
			if err := s.db.Create(&claim).Error; err != nil {
				t.Fatal(err)
			}
			retirementClaim, row := claimRetirement(t, s, library, actor)
			for i := 0; i < 100 && row.Phase != "completed"; i++ {
				if _, _, err := NewMediaLibraryRetirementWorker(s).step(context.Background(), retirementClaim, &row); err != nil {
					t.Fatal(err)
				}
			}
			if row.Phase != "completed" {
				t.Fatalf("retirement did not finish: %s", row.Phase)
			}
			for _, table := range []string{"catalog_artifact_cleanup_claims", "media_artifacts", "catalog_physical_writes"} {
				var count int64
				if err := s.db.Table(table).Where("library_id = ?", library.ID).Count(&count).Error; err != nil || count != 0 {
					t.Fatalf("%s retained %d rows: %v", table, count, err)
				}
			}
		})
	}
}
