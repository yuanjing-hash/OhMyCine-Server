package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/medialibrary"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

func TestQuarkObservationRevocationRejectsUnprovenOrOversizedWorkset(t *testing.T) {
	for _, scenario := range []string{"provider_budget", "manifest_budget", "unprepared_manifest", "owner_changed", "entry_still_present", "115_unchanged"} {
		t.Run(scenario, func(t *testing.T) {
			f := newObservationRevocationFixture(t)
			original, _ := f.artifact(t, "removed")
			providerIDs := []string{"missing-fixture"}
			changes := CatalogArtifactChangeSet{}
			want := ErrCatalogFence
			if scenario == "provider_budget" {
				providerIDs = make([]string, CatalogBatchRows+1)
				for i := range providerIDs {
					providerIDs[i] = fmt.Sprintf("missing-%d", i)
				}
				want = ErrCatalogBudget
			} else if scenario == "entry_still_present" || scenario == "115_unchanged" {
				providerIDs = []string{original.ProviderItemID}
				changes.Manifests = []uint{original.ID}
				if scenario == "115_unchanged" {
					if err := f.s.db.Model(&models.Storage{}).Where("id = ?", f.library.StorageID).Update("type", models.StorageTypePan115).Error; err != nil {
						t.Fatal(err)
					}
					want = nil
				}
			} else {
				count := 1
				if scenario == "manifest_budget" {
					count, want = CatalogBatchRows+1, ErrCatalogBudget
				}
				owner := original.RunID
				if scenario == "owner_changed" {
					owner = "wrong-source-fixture"
					policy, err := json.Marshal(mediaArtifactPolicy{LibraryID: f.library.ID, StorageID: f.library.StorageID, SourceBoundaryFingerprint: "different-source"})
					if err != nil {
						t.Fatal(err)
					}
					if err = f.s.db.Create(&models.MediaArtifactRun{ID: owner, LibraryID: f.library.ID, Generation: 1, PolicyJSON: string(policy), Status: models.MediaArtifactStatusCompleted}).Error; err != nil {
						t.Fatal(err)
					}
				}
				rows := make([]models.MediaArtifact, count)
				for i := range rows {
					rows[i] = original
					rows[i].ID, rows[i].RunID, rows[i].ProviderItemID = 0, owner, providerIDs[0]
					rows[i].OpaqueID = fmt.Sprintf("revocation-fixture-%d", i)
					rows[i].RelativePath = fmt.Sprintf("revocation-fixture-%d.strm", i)
				}
				if err := f.s.db.CreateInBatches(&rows, CatalogBatchRows).Error; err != nil {
					t.Fatal(err)
				}
				if scenario != "unprepared_manifest" {
					for _, row := range rows {
						changes.Manifests = append(changes.Manifests, row.ID)
					}
				}
			}
			ctx := withProviderChangeScope(context.Background(), providerChangeScope{VerifiedResult: &medialibrary.Result{Partial: true, Scoped: true, DeletedProviderIDs: providerIDs}})
			ctx = context.WithValue(ctx, providerObservationCommitContextKey{}, func(*gorm.DB) error { return nil })
			err := f.s.db.Transaction(func(tx *gorm.DB) error {
				return revokeQuarkObservationSTRMTx(ctx, tx, f.library, &models.MediaLibraryScanRun{LibraryID: f.library.ID, Kind: "provider_observation"}, changes)
			})
			if !errors.Is(err, want) {
				t.Fatalf("guard result: got %v, want %v", err, want)
			}
			var inactive int64
			if err := f.s.db.Model(&models.MediaArtifact{}).Where("library_id = ? AND active = ?", f.library.ID, false).Count(&inactive).Error; err != nil || inactive != 0 {
				t.Fatalf("guard changed unrelated or unproven manifests: %d %v", inactive, err)
			}
		})
	}
}

func TestQuarkObservationRecoveryUsesEffectiveRowsOutsidePartialBaseline(t *testing.T) {
	f := newObservationRevocationFixture(t)
	entries := catalogReadEntries(t, f.s.catalogStore, f.library.ID, "")
	if len(entries) != 2 || entries[0].RecognitionID == nil {
		t.Fatal("fixture identity missing")
	}
	entry := entries[0]
	var head models.CatalogHead
	var recognition models.MediaLibraryRecognition
	if err := f.s.withCatalogRead(context.Background(), []uint{f.library.ID}, func(_ *gorm.DB, reader *CatalogReader) error {
		head, _ = reader.Head(f.library.ID)
		return reader.Recognitions().Where("id = ?", *entry.RecognitionID).First(&recognition).Error
	}); err != nil {
		t.Fatal(err)
	}
	oldEntry, oldRecognition := entry, recognition
	oldEntry.Title, oldRecognition.Title = "stale-envelope-title", "stale-envelope-title"
	recovery := &observationRecovery{scope: f.observe.scope, epoch: head.SourceEpoch,
		entries: map[uint]models.MediaLibraryEntry{entry.ID: oldEntry}, recognitions: map[uint]models.MediaLibraryRecognition{recognition.ID: oldRecognition}}
	ctx := context.WithValue(context.Background(), observationRecoveryContextKey{}, recovery)
	baseline := catalogScanBaseline{head: head}
	if err := appendObservationRecoveryBaseline(ctx, f.s.catalogStore.readDB, f.library.ID, &baseline); err != nil {
		t.Fatal(err)
	}
	if len(baseline.entries) != 1 || baseline.entries[0].Title != entry.Title || len(baseline.recognitions) != 1 || baseline.recognitions[0].Title != recognition.Title || len(baseline.restoredEntries)+len(baseline.restoredRecognitions) != 0 {
		t.Fatal("partial-baseline absence was mistaken for effective-head absence or stale metadata won")
	}
	baseline = catalogScanBaseline{head: head}
	baseline.head.Revision++
	if err := appendObservationRecoveryBaseline(ctx, f.s.catalogStore.readDB, f.library.ID, &baseline); !errors.Is(err, ErrCatalogFence) {
		t.Fatalf("changed head accepted: %v", err)
	}
}
