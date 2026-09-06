package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestCatalogArtifactRecoveryReconcilesRealFilesWithoutReplayingSupersededWrites(t *testing.T) {
	for _, phase := range []string{"before-replace", "after-replace-before-ack", "third-party-change"} {
		t.Run(phase, func(t *testing.T) {
			db, permit, input, _ := catalogArtifactReceiptFixture(t, true)
			var run models.MediaArtifactRun
			if err := db.First(&run, "id=?", permit.evidence.OwnerID).Error; err != nil {
				t.Fatal(err)
			}
			var policy mediaArtifactPolicy
			if err := json.Unmarshal([]byte(run.PolicyJSON), &policy); err != nil {
				t.Fatal(err)
			}
			service := NewMediaArtifactService(db, nil, nil, zerolog.Nop())
			content := []byte("exact generated NFO")
			// The real preparation helper derives actual before/after fingerprints.
			digest := sha256.Sum256(content)
			input.AfterArtifact.ContentFingerprint = hex.EncodeToString(digest[:])
			receipt, err := service.prepareArtifactPhysicalWrite(policy.ProjectionRoot, input.BeforeArtifact, input.AfterArtifact, content, artifactPhysicalExecution{ctx: context.Background(), permit: permit, policy: policy})
			if err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(policy.ProjectionRoot, filepath.FromSlash(input.BeforeArtifact.RelativePath))
			if phase != "before-replace" {
				if phase == "third-party-change" {
					content = []byte("user's later edit")
				}
				if err := atomicWriteArtifact(policy.ProjectionRoot, target, content); err != nil {
					t.Fatal(err)
				}
			}
			err = service.settleSupersededArtifactExecution(permit, policy)
			if (err == nil) != (phase != "third-party-change") {
				t.Fatalf("phase=%s recovery=%v", phase, err)
			}
			var proof models.CatalogPhysicalWrite
			if err := db.First(&proof, permit.evidence.ID).Error; err != nil {
				t.Fatal(err)
			}
			wantState, wantReceipt := "settled", "reconciled_before"
			if phase == "after-replace-before-ack" {
				wantReceipt = "reconciled_after"
			}
			if phase == "third-party-change" {
				wantState, wantReceipt = "quiescent", "conflict"
			}
			if err := db.First(&receipt, receipt.ID).Error; err != nil {
				t.Fatal(err)
			}
			if proof.State != wantState || receipt.Phase != wantReceipt {
				t.Fatalf("state=%s receipt=%s", proof.State, receipt.Phase)
			}
			actual, readErr := os.ReadFile(target)
			if phase == "before-replace" {
				if !os.IsNotExist(readErr) {
					t.Fatal("recovery replayed obsolete output")
				}
			} else if readErr != nil || string(actual) != string(content) {
				t.Fatal("recovery changed file bytes")
			}
			if err := db.First(&run, "id=?", run.ID).Error; err != nil {
				t.Fatal(err)
			}
			if run.Status == models.MediaArtifactStatusCompleted {
				t.Fatal("obsolete generation marked ready")
			}
		})
	}
}
