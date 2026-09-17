package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/medialibrary"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

func TestDeletedArtifactScopedCleanup(t *testing.T) {
	for _, scenario := range []string{"delete", "already_missing", "modified", "root_changed", "restored", "shared_work_restored", "active_counterpart", "retry"} {
		t.Run(scenario, func(t *testing.T) {
			s, queue, _, library, root := strmManagementFixture(t)
			var run models.MediaArtifactRun
			var artifact models.MediaArtifact
			var path string
			run, artifact, path = createAutoCleanupScenario(t, s, library, root, "event", true, models.MediaArtifactStatusCompleted)
			digest := sha256.Sum256([]byte("stale\n"))
			artifact.ContentFingerprint = hex.EncodeToString(digest[:])
			if err := s.db.Model(&artifact).Update("content_fingerprint", artifact.ContentFingerprint).Error; err != nil {
				t.Fatal(err)
			}
			var policy mediaArtifactPolicy
			if err := s.db.Model(&artifact).Update("provider_item_id", "deleted-provider").Error; err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(run.PolicyJSON), &policy); err != nil {
				t.Fatal(err)
			}
			policy.ScopeVersion = 1
			policy.DeletedManifestIDs = []uint{artifact.ID}
			policy.BatchSourceFingerprint, _ = artifactBatchSourceFingerprint(s.db, library)
			raw, _ := json.Marshal(policy)
			if err := s.db.Model(&run).Update("policy_json", string(raw)).Error; err != nil {
				t.Fatal(err)
			}
			other := artifact
			other.ID = 0
			other.OpaqueID = uuid.NewString()
			other.RelativePath = "/unrelated.strm"
			if err := s.db.Create(&other).Error; err != nil {
				t.Fatal(err)
			}
			if err := s.db.Model(&other).Update("active", false).Error; err != nil {
				t.Fatal(err)
			}
			unrelated := filepath.Join(root, "unrelated.strm")
			if err := os.WriteFile(unrelated, []byte("unrelated"), 0600); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if scenario == "retry" {
				// Register only after the fixture's immutable scope is finalized.
				if err := s.db.First(&run, "id = ?", run.ID).Error; err != nil {
					t.Fatal(err)
				}
				if err := s.db.Delete(&run).Error; err != nil {
					t.Fatal(err)
				}
				_, err := queue.EnqueueWith(EnqueueJobInput{System: true, JobType: JobTypeMediaArtifact, Priority: 100, DisplayName: "cleanup", Provider: "media_library", ResourceKey: mediaArtifactResourceKey(library.ID), CoalescingKey: run.ID, Payload: mediaArtifactJobPayload{ArtifactRunID: run.ID}}, func(tx *gorm.DB, job models.Job) error {
					run.JobID = &job.ID
					return RegisterCatalogPhysicalOwnerTx(tx, CatalogPhysicalWriteInput{LibraryID: library.ID, OwnerKind: CatalogPhysicalArtifact, OwnerID: run.ID, ArtifactReceiptVersion: 1}, func(tx *gorm.DB) error { return tx.Create(&run).Error })
				})
				if err != nil {
					t.Fatal(err)
				}
				claim, err := queue.Claim([]string{JobTypeMediaArtifact})
				if err != nil || claim == nil {
					t.Fatalf("claim %v", err)
				}
				var permit CatalogPhysicalWritePermit
				if err := s.db.Transaction(func(tx *gorm.DB) error {
					var err error
					permit, err = EnterCatalogPhysicalWriteTx(tx, CatalogPhysicalWriteInput{LibraryID: library.ID, OwnerKind: CatalogPhysicalArtifact, OwnerID: run.ID, Job: claim, ArtifactReceiptVersion: 1})
					return err
				}); err != nil {
					t.Fatal(err)
				}
				ctx = withArtifactCleanupPermit(ctx, permit)
			}
			switch scenario {
			case "already_missing":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "modified":
				if err := os.WriteFile(path, []byte("user modified"), 0600); err != nil {
					t.Fatal(err)
				}
			case "root_changed":
				if err := s.db.Model(&library).Update("strm_local_root", t.TempDir()).Error; err != nil {
					t.Fatal(err)
				}
			case "restored":
				if err := s.db.Create(&models.MediaLibraryEntry{LibraryID: library.ID, ProviderID: "deleted-provider", RelativePath: "/new-name.mkv", WorkKey: "restored"}).Error; err != nil {
					t.Fatal(err)
				}
			case "active_counterpart":
				if err := s.db.Model(&artifact).Updates(map[string]any{"active": true, "source_identity": "recognition:9999:poster"}).Error; err != nil {
					t.Fatal(err)
				}
			case "shared_work_restored":
				tmdbID := int64(424242)
				if err := s.db.Model(&artifact).Updates(map[string]any{"source_identity": "recognition:9999:poster", "provider_item_id": ""}).Error; err != nil {
					t.Fatal(err)
				}
				var scan models.MediaLibraryScanRun
				if err := s.db.First(&scan, policy.ScanRunID).Error; err != nil {
					t.Fatal(err)
				}
				if err := freezeDeletedWorkGuards(&scan, []deletedRecognitionScope{{RecognitionID: 9999, RecognitionIDs: []uint{9999}, TMDBID: &tmdbID, MediaType: "tv"}}); err != nil {
					t.Fatal(err)
				}
				if err := s.db.Model(&scan).Update("checkpoint_json", scan.CheckpointJSON).Error; err != nil {
					t.Fatal(err)
				}
				if err := freezeArtifactDeletedWorkGuardsTx(s.db, &policy); err != nil {
					t.Fatal(err)
				}
				policyRaw, _ := json.Marshal(policy)
				if err := s.db.Model(&run).Update("policy_json", string(policyRaw)).Error; err != nil {
					t.Fatal(err)
				}
				if err := s.db.Create(&models.MediaLibraryEntry{LibraryID: library.ID, ProviderID: "another-season-restored", RelativePath: "/show/Season 02/01.mkv", WorkKey: "series:tmdb:424242", TMDBID: &tmdbID, MediaType: "tv"}).Error; err != nil {
					t.Fatal(err)
				}
			case "retry":
				s.removeFile = func(string) error { return errors.New("synthetic failure") }
			}
			result := s.AutoCleanup(ctx, run.ID)
			if scenario == "active_counterpart" {
				if result.Removed != 0 {
					t.Fatalf("restored manifest removed: %+v", result)
				}
				if _, err := os.Stat(path); err != nil {
					t.Fatal("restored output removed")
				}
			} else if scenario == "modified" || scenario == "root_changed" || scenario == "restored" || scenario == "shared_work_restored" || scenario == "retry" {
				if result.ErrorCode == "" {
					t.Fatalf("unsafe cleanup accepted: %+v", result)
				}
				if _, err := os.Stat(path); err != nil {
					t.Fatal("protected file removed")
				}
				if scenario == "retry" {
					if result.ErrorCode != "artifact_cleanup_delete_failed" {
						t.Fatalf("did not reach physical fault: %+v", result)
					}
					s.removeFile = os.Remove
					if next := s.AutoCleanup(ctx, run.ID); next.ErrorCode != "" || next.Removed != 1 {
						t.Fatalf("retry failed %+v", next)
					}
				}
			} else {
				if result.ErrorCode != "" || result.Skipped {
					t.Fatalf("cleanup %+v", result)
				}
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("deleted projection remains")
				}
				if next := s.AutoCleanup(context.Background(), run.ID); next.ErrorCode != "" || next.Removed != 0 {
					t.Fatalf("repeat %+v", next)
				}
			}
			if data, err := os.ReadFile(unrelated); err != nil || string(data) != "unrelated" {
				t.Fatal("unrelated inactive projection changed")
			}
		})
	}
}

func TestDeletedArtifactOrphanPublishesManifestBinding(t *testing.T) {
	s, library, storage, profile, _ := catalogScanFixture(t, true)
	library, ownerScan := catalogScanRun(t, s, library.ID, storage, profile)
	ownerPolicy, _ := json.Marshal(mediaArtifactPolicy{LibraryID: library.ID, StorageID: storage.ID, ScanRunID: ownerScan.ID, SourceBoundaryFingerprint: catalogSourceFingerprint(library, storage)})
	owner := models.MediaArtifactRun{ID: uuid.NewString(), LibraryID: library.ID, Generation: ownerScan.Generation, PolicyJSON: string(ownerPolicy), Status: models.MediaArtifactStatusCompleted}
	if err := s.db.Create(&owner).Error; err != nil {
		t.Fatal(err)
	}
	orphan := models.MediaArtifact{OpaqueID: uuid.NewString(), RunID: owner.ID, LibraryID: library.ID, ProviderItemID: "deleted-orphan", SourceIdentity: "entry:999999", Kind: models.MediaArtifactKindSTRM, TargetKind: models.MediaArtifactTargetLocalProjection, RelativePath: "/orphan.strm", Managed: true, Active: true, Status: models.MediaArtifactStatusCompleted}
	if err := s.db.Create(&orphan).Error; err != nil {
		t.Fatal(err)
	}
	if applied, err := s.providerDeltaAlreadyApplied(context.Background(), library.ID, medialibrary.Result{DeletedProviderIDs: []string{"deleted-orphan"}}); err != nil || applied {
		t.Fatalf("orphan swallowed %v %v", applied, err)
	}
	library, run := catalogScanRun(t, s, library.ID, storage, profile)
	run.Kind = "event"
	if err := s.db.Model(&run).Update("kind", "event").Error; err != nil {
		t.Fatal(err)
	}
	called := false
	var binding models.CatalogArtifactBinding
	artifactService := &MediaArtifactService{db: s.db, catalogStore: s.catalogStore}
	_, err := s.publishCatalogScan(context.Background(), library, storage, profile, run, medialibrary.Result{Partial: true, Scoped: true, DeletedProviderIDs: []string{"deleted-orphan"}}, false, func(tx *gorm.DB, p CatalogScanPublication) error {
		called = true
		if p.NoContentChange || len(p.ArtifactChanges.Manifests) != 1 || p.ArtifactChanges.Manifests[0] != orphan.ID {
			t.Fatalf("lost deletion work: %+v", p.ArtifactChanges)
		}
		var bindErr error
		binding, bindErr = artifactService.BindCatalogGenerationChangesTx(tx, library.ID, p.Run.Generation, p.ArtifactChanges)
		return bindErr
	})
	if err != nil || !called {
		t.Fatalf("publication err=%v called=%v", err, called)
	}
	var work []models.CatalogArtifactBindingItem
	if err := s.db.Where("binding_id = ?", binding.ID).Find(&work).Error; err != nil || len(work) != 1 || work[0].EntityKind != "manifest" || work[0].EntityID != orphan.ID {
		t.Fatalf("binding lost exact manifest %+v %v", work, err)
	}
	if err := s.db.Model(&profile).Update("revision", profile.Revision+1).Error; err != nil {
		t.Fatal(err)
	}
	if ids, err := deletedProviderArtifactIDs(s.db, library.ID, []string{"deleted-orphan"}); err != nil || len(ids) != 1 {
		t.Fatalf("config-only change lost source ownership %v", err)
	}
	if err := s.db.Model(&library).Update("relative_root", "/replacement").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := deletedProviderArtifactIDs(s.db, library.ID, []string{"deleted-orphan"}); !errors.Is(err, ErrCatalogFence) {
		t.Fatalf("old source accepted %v", err)
	}
}
