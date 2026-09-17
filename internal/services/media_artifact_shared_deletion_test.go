package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	serverlog "github.com/yuanjing-hash/OhMyCine-Server/internal/logging"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/medialibrary"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

func TestDeletedArtifactLegacySharedWorkKeepsOtherSeason(t *testing.T) {
	t.Run("transaction", func(t *testing.T) { testDeletedLegacySharedWork(t, false) })
	t.Run("fast_scan", func(t *testing.T) { testDeletedLegacySharedWork(t, true) })
}

func testDeletedLegacySharedWork(t *testing.T, fast bool) {
	s, _, _, library, _ := strmManagementFixture(t)
	var storage models.Storage
	var profile models.MediaClassificationProfile
	if err := s.db.First(&storage, library.StorageID).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.db.First(&profile, library.ProfileID).Error; err != nil {
		t.Fatal(err)
	}
	tmdbID := int64(98765)
	records := []models.MediaLibraryRecognition{
		{LibraryID: library.ID, ProfileID: profile.ID, SourceKey: "show-season-one", MediaType: "tv", TMDBID: &tmdbID},
		{LibraryID: library.ID, ProfileID: profile.ID, SourceKey: "show-season-two", MediaType: "tv", TMDBID: &tmdbID},
	}
	if err := s.db.Create(&records).Error; err != nil {
		t.Fatal(err)
	}
	entries := []models.MediaLibraryEntry{
		{LibraryID: library.ID, ProviderID: "first-season", RelativePath: "/show/s1/01.mkv", RecognitionID: &records[0].ID, WorkKey: "tv:98765"},
		{LibraryID: library.ID, ProviderID: "second-season", RelativePath: "/show/s2/01.mkv", RecognitionID: &records[1].ID, WorkKey: "tv:98765"},
	}
	if err := s.db.Create(&entries).Error; err != nil {
		t.Fatal(err)
	}
	policy, _ := json.Marshal(mediaArtifactPolicy{LibraryID: library.ID, StorageID: storage.ID, SourceBoundaryFingerprint: catalogSourceFingerprint(library, storage)})
	owner := models.MediaArtifactRun{ID: uuid.NewString(), LibraryID: library.ID, PolicyJSON: string(policy)}
	if err := s.db.Create(&owner).Error; err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		artifact := models.MediaArtifact{OpaqueID: uuid.NewString(), LibraryID: library.ID, RunID: owner.ID, SourceIdentity: fmt.Sprintf("recognition:%d:poster", record.ID), RelativePath: fmt.Sprintf("/show/poster-%d.jpg", record.ID), Managed: true, Active: true}
		if err := s.db.Create(&artifact).Error; err != nil {
			t.Fatal(err)
		}
	}
	for index, entry := range entries {
		var manifests []uint
		if fast {
			current, run := catalogScanRun(t, s.libraries, library.ID, storage, profile)
			run.Kind = "event"
			published, err := s.libraries.publishFastPan115Scan(context.Background(), current, storage, profile, run, medialibrary.Result{Partial: true, Scoped: true, DeletedProviderIDs: []string{entry.ProviderID}}, time.Now(), serverlog.OperationLibraryEventScan)
			if err != nil {
				t.Fatal(err)
			}
			var checkpoint batchArtifactCheckpoint
			if err := json.Unmarshal([]byte(published.CheckpointJSON), &checkpoint); err != nil {
				t.Fatal(err)
			}
			manifests = checkpoint.DeletedManifestIDs
		} else if err := s.db.Transaction(func(tx *gorm.DB) error {
			scopes, err := captureDeletedRecognitionScopesTx(tx, library.ID, []string{entry.ProviderID})
			if err != nil {
				return err
			}
			if err := tx.Delete(&entry).Error; err != nil {
				return err
			}
			manifests, err = pruneDeletedEmptyRecognitionsTx(tx, library, storage, profile, scopes)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		var remaining int64
		if err := s.db.Model(&models.MediaLibraryRecognition{}).Where("library_id = ? AND tmdb_id = ?", library.ID, tmdbID).Count(&remaining).Error; err != nil {
			t.Fatal(err)
		}
		if index == 0 && (remaining != 2 || len(manifests) != 0) {
			t.Fatalf("other season not protected: records=%d manifests=%v", remaining, manifests)
		}
		if index == 1 && (remaining != 0 || len(manifests) != 2) {
			t.Fatalf("last season not retired: records=%d manifests=%v", remaining, manifests)
		}
	}
}

func TestDeletedArtifactSharedPosterPhysicalCleanup(t *testing.T) {
	for _, scenario := range []string{"last_work", "user_file", "modified_poster", "missing_guard", "restored_work"} {
		t.Run(scenario, func(t *testing.T) {
			s, _, _, library, root := strmManagementFixture(t)
			run, artifact, initial := createAutoCleanupScenario(t, s, library, root, "event", true, models.MediaArtifactStatusCompleted)
			poster := relocateCleanupArtifact(t, s, artifact, initial, root, "/show/poster.jpg")
			digest := sha256.Sum256([]byte("stale\n"))
			if err := s.db.Model(&artifact).Updates(map[string]any{"kind": models.MediaArtifactKindPoster, "source_identity": "recognition:9999:poster", "content_fingerprint": hex.EncodeToString(digest[:])}).Error; err != nil {
				t.Fatal(err)
			}
			var policy mediaArtifactPolicy
			if err := json.Unmarshal([]byte(run.PolicyJSON), &policy); err != nil {
				t.Fatal(err)
			}
			policy.ScopeVersion, policy.DeletedManifestIDs = 1, []uint{artifact.ID}
			policy.BatchSourceFingerprint, _ = artifactBatchSourceFingerprint(s.db, library)
			tmdbID := int64(424242)
			if scenario != "missing_guard" {
				policy.DeletedWorkGuards = []deletedRecognitionScope{{RecognitionIDs: []uint{9999}, TMDBID: &tmdbID, MediaType: "tv"}}
			}
			raw, _ := json.Marshal(policy)
			if err := s.db.Model(&run).Update("policy_json", string(raw)).Error; err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "user_file":
				if err := os.WriteFile(filepath.Join(root, "show", "notes.txt"), []byte("mine"), 0600); err != nil {
					t.Fatal(err)
				}
			case "modified_poster":
				if err := os.WriteFile(poster, []byte("my poster"), 0600); err != nil {
					t.Fatal(err)
				}
			case "restored_work":
				if err := s.db.Create(&models.MediaLibraryEntry{LibraryID: library.ID, ProviderID: "new-season", RelativePath: "/show/s2/01.mkv", WorkKey: "series:tmdb:424242", TMDBID: &tmdbID, MediaType: "tv"}).Error; err != nil {
					t.Fatal(err)
				}
			}
			result := s.AutoCleanup(context.Background(), run.ID)
			if scenario == "modified_poster" || scenario == "missing_guard" || scenario == "restored_work" {
				if result.ErrorCode == "" {
					t.Fatalf("unsafe shared cleanup accepted: %+v", result)
				}
				if _, err := os.Stat(poster); err != nil {
					t.Fatal("protected poster removed")
				}
				return
			}
			if result.ErrorCode != "" || result.Removed != 1 {
				t.Fatalf("cleanup failed: %+v", result)
			}
			if _, err := os.Stat(poster); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("orphan poster remains")
			}
			if scenario == "last_work" {
				if _, err := os.Stat(filepath.Dir(poster)); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("empty generated directory remains")
				}
			} else if data, err := os.ReadFile(filepath.Join(root, "show", "notes.txt")); err != nil || string(data) != "mine" {
				t.Fatal("user file changed")
			}
			if _, err := os.Stat(root); err != nil {
				t.Fatal("library root removed")
			}
		})
	}
}

func TestDeletedArtifactFrozenWorkGuardSurvivesScanHistoryClearing(t *testing.T) {
	s, _, _, library, root := strmManagementFixture(t)
	run, artifact, _ := createAutoCleanupScenario(t, s, library, root, "event", true, models.MediaArtifactStatusCompleted)
	var policy mediaArtifactPolicy
	if err := json.Unmarshal([]byte(run.PolicyJSON), &policy); err != nil {
		t.Fatal(err)
	}
	var scan models.MediaLibraryScanRun
	if err := s.db.First(&scan, policy.ScanRunID).Error; err != nil {
		t.Fatal(err)
	}
	scan.CheckpointJSON = "{}"
	tmdbID := int64(424242)
	if err := freezeDeletedWorkGuards(&scan, []deletedRecognitionScope{{RecognitionIDs: []uint{9999}, TMDBID: &tmdbID, MediaType: "tv"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.db.Model(&scan).Update("checkpoint_json", scan.CheckpointJSON).Error; err != nil {
		t.Fatal(err)
	}
	if err := freezeArtifactDeletedWorkGuardsTx(s.db, &policy); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(policy)
	if err := json.Unmarshal(raw, &policy); err != nil {
		t.Fatal(err)
	}
	if err := s.db.Delete(&scan).Error; err != nil {
		t.Fatal(err)
	}
	restored := models.MediaLibraryRecognition{LibraryID: library.ID, ProfileID: library.ProfileID, SourceKey: "restored-other-season", TMDBID: &tmdbID, MediaType: "tv"}
	if err := s.db.Create(&restored).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.db.Create(&models.MediaLibraryEntry{LibraryID: library.ID, ProviderID: "reimported", RelativePath: "/show/s3/01.mkv", RecognitionID: &restored.ID}).Error; err != nil {
		t.Fatal(err)
	}
	artifact.SourceIdentity = "recognition:9999:poster"
	err := s.libraries.withCatalogRead(context.Background(), []uint{library.ID}, func(tx *gorm.DB, reader *CatalogReader) error {
		return checkDeletedWorkRestoration(tx, reader, policy, []models.MediaArtifact{artifact})
	})
	if err == nil {
		t.Fatal("clearing history erased restored-work protection")
	}
	var failure *artifactCleanupFailure
	if !errors.As(err, &failure) {
		t.Fatalf("guard depended on deleted scan: %v", err)
	}
}

func TestDeletedArtifactVersionedSharedWorkKeepsOtherSeason(t *testing.T) {
	store, library, first, entries := catalogFixture(t)
	second := first
	second.ID, second.SourceKey = 0, "show-season-two"
	if err := store.writeDB.Create(&second).Error; err != nil {
		t.Fatal(err)
	}
	entries[1].RecognitionID = &second.ID
	if err := store.writeDB.Model(&entries[1]).Update("recognition_id", second.ID).Error; err != nil {
		t.Fatal(err)
	}
	candidate, token := catalogCandidate(t, store, library, "base", 0)
	requests := []CatalogIdentityRequest{
		{Kind: "recognition", SourceKey: first.SourceKey, ExistingID: first.ID},
		{Kind: "recognition", SourceKey: second.SourceKey, ExistingID: second.ID},
	}
	facts := CatalogFactBatch{Recognitions: []models.CatalogRecognitionFact{CatalogRecognitionFromLegacy(first), CatalogRecognitionFromLegacy(second)}}
	for index, entry := range entries {
		requests = append(requests, CatalogIdentityRequest{Kind: "entry", SourceKey: entry.RelativePath, ExistingID: entry.ID})
		record := first
		if index == 1 {
			record = second
		}
		facts.Entries = append(facts.Entries, CatalogEntryFromLegacy(entry, &record))
	}
	if _, err := store.ResolveIdentities(context.Background(), candidate.ID, token, requests); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendBatch(context.Background(), candidate.ID, token, facts); err != nil {
		t.Fatal(err)
	}
	catalogPublish(t, store, candidate, token)
	s := NewMediaLibraryService(store.writeDB, NewAuditService(store.writeDB), zerolog.Nop())
	s.SetCatalogSnapshotStore(store)
	var storage models.Storage
	var profile models.MediaClassificationProfile
	if err := s.db.First(&storage, library.StorageID).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.db.Model(&storage).Update("type", models.StorageTypePan115).Error; err != nil {
		t.Fatal(err)
	}
	storage.Type = models.StorageTypePan115
	if err := s.db.First(&profile, library.ProfileID).Error; err != nil {
		t.Fatal(err)
	}
	for index, entry := range entries {
		current, run := catalogScanRun(t, s, library.ID, storage, profile)
		input := medialibrary.Result{Partial: true, Scoped: true, DeletedProviderIDs: []string{entry.ProviderID}}
		if _, err := s.publishCatalogScan(context.Background(), current, storage, profile, run, input, true, func(_ *gorm.DB, p CatalogScanPublication) error {
			if index == 0 && len(p.ArtifactChanges.Recognitions) != 0 {
				t.Fatalf("surviving work scheduled for shared retirement: %+v", p.ArtifactChanges)
			}
			if index == 1 && len(p.ArtifactChanges.Recognitions) != 2 {
				t.Fatalf("missing orphan shared identities: %+v", p.ArtifactChanges)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if err := store.Read(context.Background(), []uint{library.ID}, func(reader *CatalogReader) error {
			var records []models.MediaLibraryRecognition
			if err := reader.Recognitions().Find(&records).Error; err != nil {
				return err
			}
			if index == 0 && len(records) != 2 {
				t.Fatalf("other season removed: %+v", records)
			}
			if index == 1 && len(records) != 0 {
				t.Fatalf("empty work remains: %+v", records)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}
