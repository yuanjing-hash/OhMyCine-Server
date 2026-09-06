package services

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

func catalogArtifactReceiptFixture(t *testing.T, tracked bool) (*gorm.DB, CatalogPhysicalWritePermit, CatalogArtifactWriteInput, CatalogPhysicalWriteInput) {
	t.Helper()
	management, queue, _, library, root := strmManagementFixture(t)
	_, identity, err := canonicalProjectionRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	policy, _ := json.Marshal(mediaArtifactPolicy{LibraryID: library.ID, Generation: library.ArtifactGeneration, ProjectionRoot: root, ProjectionRootIdentity: identity, TargetKind: models.MediaArtifactTargetLocalProjection})
	run := models.MediaArtifactRun{ID: "receipt-run", LibraryID: library.ID, Generation: library.ArtifactGeneration, PolicyJSON: string(policy), Status: models.MediaArtifactStatusQueued}
	job, err := queue.Enqueue(EnqueueJobInput{System: true, JobType: JobTypeMediaArtifact, Priority: 100, DisplayName: "receipt", Provider: "media_library", ResourceKey: mediaArtifactResourceKey(library.ID), CoalescingKey: "receipt-run", Payload: mediaArtifactJobPayload{ArtifactRunID: run.ID}})
	if err != nil {
		t.Fatal(err)
	}
	run.JobID = &job.ID
	input := CatalogPhysicalWriteInput{LibraryID: library.ID, OwnerKind: CatalogPhysicalArtifact, OwnerID: run.ID, ArtifactReceiptVersion: 1}
	if err := management.db.Transaction(func(tx *gorm.DB) error {
		if tracked {
			return RegisterCatalogPhysicalOwnerTx(tx, input, func(tx *gorm.DB) error { return tx.Create(&run).Error })
		}
		return tx.Create(&run).Error
	}); err != nil {
		t.Fatal(err)
	}
	claim, err := queue.Claim([]string{JobTypeMediaArtifact})
	if err != nil || claim == nil {
		t.Fatalf("claim=%v %v", claim, err)
	}
	input.Job = claim
	var permit CatalogPhysicalWritePermit
	if err := management.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&run).Update("status", models.MediaArtifactStatusRunning).Error; err != nil {
			return err
		}
		var err error
		permit, err = EnterCatalogPhysicalWriteTx(tx, input)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	before := models.MediaArtifact{OpaqueID: "receipt-artifact", LibraryID: library.ID, RunID: run.ID, TargetKind: models.MediaArtifactTargetLocalProjection, RelativePath: "Movie/movie.nfo", Kind: models.MediaArtifactKindNFO, Managed: true, Active: true, Status: models.MediaArtifactStatusQueued, ProviderItemID: "private-provider", ProviderParentID: "private-parent"}
	if err := management.db.Create(&before).Error; err != nil {
		t.Fatal(err)
	}
	if err := management.db.First(&before, before.ID).Error; err != nil {
		t.Fatal(err)
	}
	after := before
	after.ContentFingerprint = strings.Repeat("a", 64)
	after.Status = models.MediaArtifactStatusCompleted
	expires := time.Now().UTC().Truncate(time.Second)
	after.ContentExpiresAt = &expires
	after.TargetProviderID = "private-target"
	after.ContentFormatVersion = "v1"
	return management.db, permit, CatalogArtifactWriteInput{BeforeArtifact: before, AfterArtifact: after, RootIdentity: identity, AfterFingerprint: after.ContentFingerprint, AfterSize: 123}, input
}

func TestCatalogArtifactReceiptBeforeAfterConflictAndPrivateMetadata(t *testing.T) {
	for _, scenario := range []string{"before", "after", "unknown", "wrong-root", "manifest-change"} {
		t.Run(scenario, func(t *testing.T) {
			db, permit, input, _ := catalogArtifactReceiptFixture(t, true)
			var receipt models.CatalogArtifactWriteReceipt
			if err := db.Transaction(func(tx *gorm.DB) error {
				var err error
				receipt, err = PrepareCatalogArtifactWriteTx(tx, permit, input)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if receipt.ID == 0 {
				t.Fatal("prepare returned no durable ID")
			}
			if raw, _ := json.Marshal(receipt); string(raw) != "{}" {
				t.Fatal("private receipt exposed")
			}
			observation := CatalogArtifactObservation{RootIdentity: input.RootIdentity}
			if scenario != "before" {
				observation.Exists = true
				observation.Fingerprint = input.AfterFingerprint
				observation.Size = input.AfterSize
			}
			if scenario == "unknown" {
				observation.Fingerprint = strings.Repeat("b", 64)
			}
			if scenario == "wrong-root" {
				observation.RootIdentity = "different-root"
			}
			if scenario == "manifest-change" {
				if err := db.Model(&models.MediaArtifact{}).Where("id=?", receipt.ArtifactID).Update("provider_item_id", "user-change").Error; err != nil {
					t.Fatal(err)
				}
			}
			if err := db.Transaction(func(tx *gorm.DB) error { return ReconcileCatalogArtifactWriteTx(tx, permit, receipt, observation) }); err != nil {
				t.Fatal(err)
			}
			var current models.CatalogArtifactWriteReceipt
			if err := db.First(&current, receipt.ID).Error; err != nil {
				t.Fatal(err)
			}
			want := "conflict"
			if scenario == "before" {
				want = "reconciled_before"
			}
			if scenario == "after" {
				want = "reconciled_after"
			}
			if current.Phase != want {
				t.Fatalf("phase=%s want=%s", current.Phase, want)
			}
			if current.Revision != receipt.Revision+1 {
				t.Fatal("reconciliation did not advance receipt CAS")
			}
			if err := db.Transaction(func(tx *gorm.DB) error { return ReconcileCatalogArtifactWriteTx(tx, permit, receipt, observation) }); err == nil {
				t.Fatal("stale observation overwrote reconciled receipt")
			}
			if scenario == "after" {
				var actual models.MediaArtifact
				if err := db.First(&actual, receipt.ArtifactID).Error; err != nil {
					t.Fatal(err)
				}
				got, _ := encodeCatalogArtifactMetadata(actual)
				expected, _ := encodeCatalogArtifactMetadata(input.AfterArtifact)
				if got != expected {
					t.Fatalf("private manifest not preserved: %s != %s", got, expected)
				}
			}
			if err := db.Transaction(func(tx *gorm.DB) error { return QuiesceCatalogPhysicalWriteTx(tx, permit) }); err != nil {
				t.Fatal(err)
			}
			if err := db.Model(&models.MediaArtifactRun{}).Where("id=?", permit.evidence.OwnerID).Updates(map[string]any{"status": models.MediaArtifactStatusSuperseded, "finished_at": time.Now().UTC(), "cleanup_status": models.MediaArtifactCleanupSkipped}).Error; err != nil {
				t.Fatal(err)
			}
			err := db.Transaction(func(tx *gorm.DB) error { return SettleSupersededCatalogArtifactTx(tx, permit) })
			if (err == nil) != (want != "conflict") {
				t.Fatalf("settle=%v phase=%s", err, want)
			}
		})
	}
}

func TestCatalogArtifactReceiptLegacyCannotGainCoverageOnRecovery(t *testing.T) {
	db, permit, input, owner := catalogArtifactReceiptFixture(t, false)
	if permit.evidence.ArtifactReceiptVersion != 0 {
		t.Fatal("untracked history gained coverage")
	}
	if err := db.Transaction(func(tx *gorm.DB) error { _, err := PrepareCatalogArtifactWriteTx(tx, permit, input); return err }); err == nil {
		t.Fatal("legacy receipt prepared")
	}
	if err := db.Transaction(func(tx *gorm.DB) error { return QuiesceCatalogPhysicalWriteTx(tx, permit) }); err != nil {
		t.Fatal(err)
	}
	var resumed CatalogPhysicalWritePermit
	if err := db.Transaction(func(tx *gorm.DB) error {
		var err error
		resumed, err = EnterCatalogPhysicalWriteTx(tx, owner)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if resumed.evidence.ArtifactReceiptVersion != 0 {
		t.Fatal("quiescent history gained coverage")
	}
}

func TestCatalogArtifactReceiptFencesAndDrainCannotHidePending(t *testing.T) {
	db, permit, input, _ := catalogArtifactReceiptFixture(t, true)
	var receipt models.CatalogArtifactWriteReceipt
	if err := db.Transaction(func(tx *gorm.DB) error {
		var err error
		receipt, err = PrepareCatalogArtifactWriteTx(tx, permit, input)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error { _, err := PrepareCatalogArtifactWriteTx(tx, permit, input); return err }); err == nil {
		t.Fatal("overwrote pending intent")
	}
	stale := receipt
	stale.Revision++
	if err := db.Transaction(func(tx *gorm.DB) error {
		return ReconcileCatalogArtifactWriteTx(tx, permit, stale, CatalogArtifactObservation{RootIdentity: input.RootIdentity})
	}); err == nil {
		t.Fatal("stale receipt accepted")
	}
	if err := db.Model(&models.CatalogPhysicalWrite{}).Where("id=?", permit.evidence.ID).Update("state", "settled").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error { return AssertCatalogPhysicalDrainedTx(tx, permit.evidence.LibraryID) }); err == nil {
		t.Fatal("settled ledger hid pending per-file intent")
	}
}
