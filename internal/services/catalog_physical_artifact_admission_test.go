package services

import (
	"context"
	"testing"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

func artifactAdmissionRefreshFixture(t *testing.T) (*MediaArtifactService, *QueueService, models.MediaArtifactRun, models.CatalogPhysicalWrite) {
	t.Helper()
	management, queue, _, library, _ := strmManagementFixture(t)
	service := NewMediaArtifactService(management.db, queue, &SignedProxyService{}, zerolog.Nop())
	if err := service.ScheduleGeneration(library.ID, library.ArtifactGeneration); err != nil {
		t.Fatal(err)
	}
	var run models.MediaArtifactRun
	if err := management.db.Where("library_id=? AND generation=?", library.ID, library.ArtifactGeneration).First(&run).Error; err != nil {
		t.Fatal(err)
	}
	return service, queue, run, assertPhysicalOwnerState(t, management.db, CatalogPhysicalArtifact, run.ID, "admitted")
}

func TestCatalogArtifactAdmissionRefreshBeforeEnterRebindsExactQueuedOwner(t *testing.T) {
	service, queue, run, original := artifactAdmissionRefreshFixture(t)
	if err := service.RefreshGeneration(run.LibraryID, run.Generation); err != nil {
		t.Fatal(err)
	}
	refreshed := assertPhysicalOwnerState(t, service.db, CatalogPhysicalArtifact, run.ID, "admitted")
	if refreshed.Revision != original.Revision+1 || refreshed.ID != original.ID || refreshed.JobID != original.JobID || refreshed.OwnerDigest == original.OwnerDigest || refreshed.SourceEpoch != original.SourceEpoch || refreshed.SourceFingerprint != original.SourceFingerprint || refreshed.ConfigFingerprint != original.ConfigFingerprint {
		t.Fatal("refresh changed owner/source boundary or failed to rebind exact policy")
	}
	claim, err := queue.Claim([]string{JobTypeMediaArtifact})
	if err != nil || claim == nil {
		t.Fatalf("claim=%v %v", claim, err)
	}
	if err := service.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&run).Update("status", models.MediaArtifactStatusRunning).Error; err != nil {
			return err
		}
		_, err := EnterCatalogPhysicalWriteTx(tx, CatalogPhysicalWriteInput{LibraryID: run.LibraryID, OwnerKind: CatalogPhysicalArtifact, OwnerID: run.ID, Job: claim, ArtifactReceiptVersion: 1})
		return err
	}); err != nil {
		t.Fatalf("refreshed queued owner could not enter: %v", err)
	}
	entered := assertPhysicalOwnerState(t, service.db, CatalogPhysicalArtifact, run.ID, "entered")
	if entered.ArtifactReceiptVersion != 1 {
		t.Fatal("new registered execution lost intent coverage")
	}
}

func TestCatalogArtifactAdmissionRefreshRejectsLeaseUnresolvedAndChangedBoundary(t *testing.T) {
	for _, scenario := range []string{"claimed", "entered", "quiescent", "source", "old-policy"} {
		t.Run(scenario, func(t *testing.T) {
			service, queue, run, _ := artifactAdmissionRefreshFixture(t)
			if scenario == "claimed" || scenario == "entered" || scenario == "quiescent" {
				claim, err := queue.Claim([]string{JobTypeMediaArtifact})
				if err != nil || claim == nil {
					t.Fatalf("claim=%v %v", claim, err)
				}
				if scenario != "claimed" {
					if err := service.db.Transaction(func(tx *gorm.DB) error {
						if err := tx.Model(&run).Update("status", models.MediaArtifactStatusRunning).Error; err != nil {
							return err
						}
						permit, err := EnterCatalogPhysicalWriteTx(tx, CatalogPhysicalWriteInput{LibraryID: run.LibraryID, OwnerKind: CatalogPhysicalArtifact, OwnerID: run.ID, Job: claim, ArtifactReceiptVersion: 1})
						if err != nil {
							return err
						}
						if scenario == "quiescent" {
							if err := QuiesceCatalogPhysicalWriteTx(tx, permit); err != nil {
								return err
							}
						}
						return tx.Model(&run).Update("status", models.MediaArtifactStatusFailed).Error
					}); err != nil {
						t.Fatal(err)
					}
				}
			}
			if scenario == "source" {
				var library models.MediaLibrary
				if err := service.db.First(&library, run.LibraryID).Error; err != nil {
					t.Fatal(err)
				}
				if err := service.db.Model(&models.Storage{}).Where("id=?", library.StorageID).Update("root_path", "changed-provider-root").Error; err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "old-policy" {
				if err := service.db.Model(&run).Update("policy_json", `{"unreviewed":true}`).Error; err != nil {
					t.Fatal(err)
				}
			}
			var before models.MediaArtifactRun
			var ledger models.CatalogPhysicalWrite
			var job models.Job
			if err := service.db.First(&before, "id=?", run.ID).Error; err != nil {
				t.Fatal(err)
			}
			if err := service.db.Where("owner_kind=? AND owner_id=?", CatalogPhysicalArtifact, run.ID).First(&ledger).Error; err != nil {
				t.Fatal(err)
			}
			if err := service.db.First(&job, "id=?", *run.JobID).Error; err != nil {
				t.Fatal(err)
			}
			if err := service.RefreshGeneration(run.LibraryID, run.Generation); err == nil {
				t.Fatal("unsafe admitted refresh succeeded")
			}
			var after models.MediaArtifactRun
			var current models.CatalogPhysicalWrite
			var currentJob models.Job
			if err := service.db.First(&after, "id=?", run.ID).Error; err != nil {
				t.Fatal(err)
			}
			if err := service.db.First(&current, ledger.ID).Error; err != nil {
				t.Fatal(err)
			}
			if err := service.db.First(&currentJob, "id=?", job.ID).Error; err != nil {
				t.Fatal(err)
			}
			if after.PolicyJSON != before.PolicyJSON || after.Status != before.Status || current.OwnerDigest != ledger.OwnerDigest || current.Revision != ledger.Revision || current.State != ledger.State || currentJob.Generation != job.Generation || currentJob.Revision != job.Revision {
				t.Fatal("rejected refresh partially changed queue/domain/proof")
			}
		})
	}
}

func TestCatalogArtifactAdmissionRefreshCannotRegisterUnknownHistory(t *testing.T) {
	service, _, run, original := artifactAdmissionRefreshFixture(t)
	if err := service.db.Delete(&models.CatalogPhysicalWrite{}, original.ID).Error; err != nil {
		t.Fatal(err)
	}
	_ = service.RefreshGeneration(run.LibraryID, run.Generation)
	var count int64
	if err := service.db.WithContext(context.Background()).Model(&models.CatalogPhysicalWrite{}).Where("owner_kind=? AND owner_id=?", CatalogPhysicalArtifact, run.ID).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("legacy history fabricated admitted proof: count=%d err=%v", count, err)
	}
}
