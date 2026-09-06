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

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

func TestCatalogArtifactCleanupRecoveryOnlyReconcilesExactOwnedManifest(t *testing.T) {
	for _, scenario := range []string{"missing", "matching", "changed", "historical", "manual-owner", "different-owner", "changed-root", "changed-manifest"} {
		t.Run(scenario, func(t *testing.T) {
			db, permit, input, _ := catalogArtifactReceiptFixture(t, true)
			var library models.MediaLibrary
			if err := db.First(&library, permit.evidence.LibraryID).Error; err != nil {
				t.Fatal(err)
			}
			var run models.MediaArtifactRun
			if err := db.First(&run, "id = ?", permit.evidence.OwnerID).Error; err != nil {
				t.Fatal(err)
			}
			var policy mediaArtifactPolicy
			if err := json.Unmarshal([]byte(run.PolicyJSON), &policy); err != nil {
				t.Fatal(err)
			}
			artifact := input.BeforeArtifact
			// The manifest's generating run is intentionally not its cleanup owner.
			generator := models.MediaArtifactRun{ID: "original-generator", LibraryID: library.ID, Generation: run.Generation - 1, PolicyJSON: run.PolicyJSON, Status: models.MediaArtifactStatusCompleted}
			if err := db.Create(&generator).Error; err != nil {
				t.Fatal(err)
			}
			content := []byte("original managed metadata")
			hash := sha256.Sum256(content)
			artifact.RunID, artifact.Active, artifact.Status, artifact.ContentFingerprint = generator.ID, false, models.MediaArtifactStatusCompleted, hex.EncodeToString(hash[:])
			if err := db.Save(&artifact).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.First(&artifact, artifact.ID).Error; err != nil {
				t.Fatal(err)
			}
			libraries := NewMediaLibraryService(db, NewAuditService(db), zerolog.Nop())
			t.Cleanup(libraries.Close)
			service := NewSTRMManagementService(db, NewAuditService(db), nil, libraries, nil)
			service.removeFile = func(string) error { t.Fatal("recovery removed a physical file"); return nil }
			service.removeDir = func(string) error { t.Fatal("recovery pruned a directory"); return nil }
			plan := artifactCleanupPlan{Library: library, Run: &run, Policy: policy, Automatic: true, PhysicalPermit: &permit, Targets: map[uint]artifactCleanupTarget{artifact.ID: {Root: policy.ProjectionRoot, RootIdentity: policy.ProjectionRootIdentity}}}
			if scenario == "manual-owner" {
				plan.Automatic, plan.Run, plan.PhysicalPermit = false, nil, nil
			}
			if scenario == "historical" {
				if err := db.Model(&artifact).Update("status", models.MediaArtifactStatusCleanup).Error; err != nil {
					t.Fatal(err)
				}
			} else if err := service.claimCleanupArtifact(plan, artifact); err != nil {
				t.Fatal(err)
			}
			if scenario == "different-owner" {
				if err := db.Model(&models.CatalogArtifactCleanupClaim{}).Where("artifact_id = ?", artifact.ID).Update("owner_run_id", generator.ID).Error; err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "changed-root" {
				changedPolicy := policy
				changedPolicy.ProjectionRoot = t.TempDir()
				raw, _ := json.Marshal(changedPolicy)
				if err := db.Model(&generator).Update("policy_json", string(raw)).Error; err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "changed-manifest" {
				if err := db.Model(&artifact).Update("provider_item_id", "new-owner-item").Error; err != nil {
					t.Fatal(err)
				}
			}
			target := filepath.Join(policy.ProjectionRoot, filepath.FromSlash(artifact.RelativePath))
			if scenario == "matching" || scenario == "changed" {
				if scenario == "changed" {
					content = []byte("unrelated external bytes")
				}
				if err := atomicWriteArtifact(policy.ProjectionRoot, target, content); err != nil {
					t.Fatal(err)
				}
			}
			generation := NewMediaArtifactService(db, nil, nil, zerolog.Nop())
			generation.cleanup = service
			err := generation.settleSupersededArtifactExecution(permit, policy)
			wantSuccess := scenario == "missing" || scenario == "matching"
			if (err == nil) != wantSuccess {
				t.Fatalf("recovery error=%v success=%v", err, wantSuccess)
			}
			var proof models.CatalogPhysicalWrite
			if err := db.First(&proof, permit.evidence.ID).Error; err != nil {
				t.Fatal(err)
			}
			if (proof.State == "settled") != wantSuccess {
				t.Fatalf("proof state=%s", proof.State)
			}
			var count int64
			if err := db.Model(&models.CatalogArtifactCleanupClaim{}).Where("artifact_id = ?", artifact.ID).Count(&count).Error; err != nil {
				t.Fatal(err)
			}
			wantClaim := int64(1)
			if wantSuccess || scenario == "historical" {
				wantClaim = 0
			}
			if count != wantClaim {
				t.Fatalf("claims=%d want=%d", count, wantClaim)
			}
			err = db.First(&artifact, artifact.ID).Error
			if scenario == "missing" {
				if err != gorm.ErrRecordNotFound {
					t.Fatalf("removed file manifest remained: %v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				wantStatus := models.MediaArtifactStatusCleanup
				if scenario == "matching" {
					wantStatus = models.MediaArtifactStatusCompleted
				}
				if artifact.Active || artifact.Status != wantStatus {
					t.Fatalf("artifact active=%v status=%s", artifact.Active, artifact.Status)
				}
			}
			if scenario == "matching" || scenario == "changed" {
				actual, err := os.ReadFile(target)
				if err != nil || string(actual) != string(content) {
					t.Fatal("recovery changed physical bytes")
				}
			}
		})
	}
}

func TestCatalogArtifactCleanupRefusesUnresolvedWriteBeforeClaim(t *testing.T) {
	db, permit, input, _ := catalogArtifactReceiptFixture(t, true)
	input.BeforeArtifact.Active = false
	if err := db.Save(&input.BeforeArtifact).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&input.BeforeArtifact, input.BeforeArtifact.ID).Error; err != nil {
		t.Fatal(err)
	}
	input.AfterArtifact.Active = false
	if err := db.Transaction(func(tx *gorm.DB) error { _, err := PrepareCatalogArtifactWriteTx(tx, permit, input); return err }); err != nil {
		t.Fatal(err)
	}
	var library models.MediaLibrary
	if err := db.First(&library, permit.evidence.LibraryID).Error; err != nil {
		t.Fatal(err)
	}
	service := &STRMManagementService{db: db}
	if err := service.claimCleanupArtifact(artifactCleanupPlan{Library: library}, input.BeforeArtifact); cleanupStableCode(err) != "artifact_cleanup_write_unresolved" {
		t.Fatalf("claim result=%v", err)
	}
	var count int64
	if err := db.Model(&models.CatalogArtifactCleanupClaim{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("created cleanup claim=%d %v", count, err)
	}
	if err := service.ReconcileSupersededCleanup(context.Background(), permit); err == nil {
		t.Fatal("cleanup recovery accepted missing shared boundary")
	}
}

func TestCatalogArtifactWriteRefusesCleanupTarget(t *testing.T) {
	for _, scenario := range []string{"marker", "claim-without-marker", "different-path"} {
		t.Run(scenario, func(t *testing.T) {
			db, permit, input, _ := catalogArtifactReceiptFixture(t, true)
			artifact := input.BeforeArtifact
			if scenario == "different-path" {
				artifact.ID, artifact.OpaqueID, artifact.SourceIdentity = 0, "old-cleanup", "old-source"
				if scenario == "different-path" {
					artifact.RelativePath = "Other/movie.nfo"
				}
				if err := db.Create(&artifact).Error; err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "claim-without-marker" {
				claim := models.CatalogArtifactCleanupClaim{ArtifactID: artifact.ID, LibraryID: artifact.LibraryID, OriginalStatus: artifact.Status, ManifestDigest: "private-proof"}
				if err := db.Create(&claim).Error; err != nil {
					t.Fatal(err)
				}
			} else if err := db.Model(&artifact).Update("status", models.MediaArtifactStatusCleanup).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.First(&input.BeforeArtifact, input.BeforeArtifact.ID).Error; err != nil {
				t.Fatal(err)
			}
			err := db.Transaction(func(tx *gorm.DB) error { _, err := PrepareCatalogArtifactWriteTx(tx, permit, input); return err })
			if (err == nil) != (scenario == "different-path") {
				t.Fatalf("write guard=%v", err)
			}
			var count int64
			if err := db.Model(&models.CatalogArtifactWriteReceipt{}).Count(&count).Error; err != nil {
				t.Fatal(err)
			}
			if scenario != "different-path" && count != 0 {
				t.Fatal("write prepared over cleanup ownership")
			}
		})
	}
}

func TestAutomaticSTRMCleanupSupersededAfterDirectoryFailureSettlesWithoutPruning(t *testing.T) {
	service, _, _, library, root := strmManagementFixture(t)
	var permit CatalogPhysicalWritePermit
	run, artifact, original := createAutoCleanupScenario(t, service, library, root, "full", false, models.MediaArtifactStatusCompleted, &permit)
	target := relocateCleanupArtifact(t, service, artifact, original, root, "/Movie/movie.strm")
	service.removeDir = func(string) error { return errors.New("injected pruning failure") }
	result := service.AutoCleanup(withArtifactCleanupPermit(context.Background(), permit), run.ID)
	if result.ErrorCode != "artifact_cleanup_directory_delete_failed" {
		t.Fatalf("cleanup=%+v", result)
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("file was not deleted before pruning failure")
	}
	if err := service.db.Model(&library).Update("artifact_generation", run.Generation+1).Error; err != nil {
		t.Fatal(err)
	}
	service.removeFile = func(string) error { t.Fatal("obsolete recovery attempted file removal"); return nil }
	service.removeDir = func(string) error { t.Fatal("obsolete recovery attempted directory pruning"); return nil }
	var policy mediaArtifactPolicy
	if err := json.Unmarshal([]byte(run.PolicyJSON), &policy); err != nil {
		t.Fatal(err)
	}
	generation := NewMediaArtifactService(service.db, nil, nil, zerolog.Nop())
	generation.cleanup = service
	if err := generation.settleSupersededArtifactExecution(permit, policy); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(target)); err != nil {
		t.Fatal("bookkeeping recovery pruned obsolete directory")
	}
	var proof models.CatalogPhysicalWrite
	if err := service.db.First(&proof, permit.evidence.ID).Error; err != nil || proof.State != "settled" {
		t.Fatalf("proof=%+v err=%v", proof, err)
	}
	var count int64
	if err := service.db.Model(&models.CatalogArtifactCleanupClaim{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("cleanup claims remain=%d %v", count, err)
	}
}
