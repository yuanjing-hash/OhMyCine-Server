package services

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/medialibrary"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

func catalogRecognitionJob(t *testing.T, service *MediaLibraryService) ClaimedJob {
	t.Helper()
	expires := time.Now().UTC().Add(time.Minute)
	token := "catalog-background-test-lease"
	job := models.Job{ID: "catalog-background-recognition", CreatedByKind: "system", JobType: JobTypeMediaLibraryRecognition, Status: models.JobStatusRunning, LeaseTokenHash: leaseHash(token), LeaseExpiresAt: &expires, Generation: 1, PayloadJSON: "{}"}
	if err := service.db.Create(&job).Error; err != nil {
		t.Fatal(err)
	}
	return ClaimedJob{Job: job, LeaseToken: token}
}

func TestCatalogScanBackgroundRecognitionLinksPendingEntriesWithoutGenerationSweep(t *testing.T) {
	service, library, storage, profile, _ := catalogScanFixture(t, true)
	library, run := catalogScanRun(t, service, library.ID, storage, profile)
	files := []medialibrary.File{{RelativePath: "New show/Season 02/S02E01.h264.mkv", ProviderID: "new-a", ProviderIDStable: true}, {RelativePath: "New show/Season 02/S02E01.h265.mkv", ProviderID: "new-b", ProviderIDStable: true}}
	ready, err := service.publishCatalogScan(context.Background(), library, storage, profile, run, medialibrary.Result{Files: files}, true, service.catalogScanCommit)
	if err != nil {
		t.Fatal(err)
	}
	if ready.Status != "catalog_ready" || ready.RecognitionTotal != 1 {
		t.Fatalf("pending run: %+v", ready)
	}
	before := catalogReadEntries(t, service.catalogStore, library.ID, "")
	if len(before) != 2 || before[0].RecognitionID != nil {
		t.Fatalf("pending rows: %+v", before)
	}
	job := catalogRecognitionJob(t, service)
	metadataCommits, completions := 0, 0
	service.SetCatalogScanCommit(func(tx *gorm.DB, p CatalogScanPublication) error {
		if !p.RecognitionOnly || p.Run.Generation != ready.Generation {
			t.Fatalf("recognition hook drift: %+v", p)
		}
		if p.Candidate != nil {
			metadataCommits++
			reader, err := PinCatalogTx(tx, []uint{library.ID})
			if err != nil {
				return err
			}
			var rows []models.MediaLibraryEntry
			if err := reader.Entries().Find(&rows).Error; err != nil {
				return err
			}
			if len(rows) != 2 || rows[0].RecognitionID == nil {
				t.Fatal("hook did not see new entry associations")
			}
		}
		if p.Run.Status == "success" {
			completions++
		}
		return nil
	})
	payload := mediaLibraryRecognitionJobPayload{LibraryID: library.ID, ScanRunID: ready.ID, Generation: ready.Generation}
	if err := service.completeFastMediaLibraryRecognition(context.Background(), fastScanTestRuntime{}, payload, job); err != nil {
		t.Fatal(err)
	}
	after := catalogReadEntries(t, service.catalogStore, library.ID, "")
	if len(after) != 2 || after[0].ID != before[0].ID || after[1].ID != before[1].ID || after[0].RecognitionID == nil || *after[0].RecognitionID != *after[1].RecognitionID || *after[0].Season != 2 || *after[0].Episode != 1 {
		t.Fatalf("recognized versions: %+v", after)
	}
	var current models.MediaLibrary
	if err := service.db.First(&current, library.ID).Error; err != nil {
		t.Fatal(err)
	}
	if current.DirtyGeneration != ready.Generation || current.BaselineGeneration != ready.Generation {
		t.Fatal("recognition changed scan generation")
	}
	var completed models.MediaLibraryScanRun
	if err := service.db.First(&completed, ready.ID).Error; err != nil {
		t.Fatal(err)
	}
	if completed.Status != "success" || completed.RecognitionCompleted != 1 || completed.Unrecognized != 1 || metadataCommits != 1 || completions != 1 {
		t.Fatalf("completed=%+v commits=%d completions=%d", completed, metadataCommits, completions)
	}
	var anchor models.MediaLibraryEntry
	if err := service.db.First(&anchor, before[0].ID).Error; err != nil {
		t.Fatal(err)
	}
	if anchor.RecognitionID != nil || anchor.LastGeneration != 0 {
		t.Fatal("background mutated stable anchor instead of facts")
	}
	if err := service.completeFastMediaLibraryRecognition(context.Background(), fastScanTestRuntime{}, payload, job); err != nil {
		t.Fatal(err)
	}
	if metadataCommits != 1 {
		t.Fatal("completed recognition replay published twice")
	}
}

func TestCatalogScanBackgroundLargeBacklogPublishesOneBase(t *testing.T) {
	service, library, storage, profile, original := catalogScanFixture(t, true)
	library, run := catalogScanRun(t, service, library.ID, storage, profile)
	files := make([]medialibrary.File, 1004)
	for i := range files {
		files[i] = medialibrary.File{RelativePath: fmt.Sprintf("Large show/Season 02/S02E%03d.variant%d.mkv", i/4+1, i%4), ProviderID: fmt.Sprintf("file-%d", i), ProviderIDStable: true}
	}
	for _, row := range original {
		files = append(files, scanFile(row))
	}
	ready, err := service.publishCatalogScan(context.Background(), library, storage, profile, run, medialibrary.Result{Files: files, Assets: []medialibrary.SourceAsset{{RelativePath: "Show/poster.jpg", ProviderID: "preserved-poster", Name: "poster.jpg", Extension: ".jpg", Size: 123}}}, true, service.catalogScanCommit)
	if err != nil {
		t.Fatal(err)
	}
	var preserved models.CatalogEntryFact
	var assetsBefore []models.CatalogSourceAssetFact
	var initialHead models.CatalogHead
	if err := service.catalogStore.Read(context.Background(), []uint{library.ID}, func(r *CatalogReader) error {
		initialHead, _ = r.Head(library.ID)
		if err := r.RawEntryFacts().Where("id=?", original[0].ID).First(&preserved).Error; err != nil {
			return err
		}
		return r.RawSourceAssetFacts().Order("id").Find(&assetsBefore).Error
	}); err != nil {
		t.Fatal(err)
	}
	// Explicit file override equal to its current shared value must not disappear
	// when a background base copies otherwise unselected already-known works.
	preserved.SharedOverrideMask |= 1 << 3
	delta, deltaToken := catalogCandidate(t, service.catalogStore, library, "delta", initialHead.Revision)
	if err := service.catalogStore.AppendBatch(context.Background(), delta.ID, deltaToken, CatalogFactBatch{Entries: []models.CatalogEntryFact{preserved}}); err != nil {
		t.Fatal(err)
	}
	catalogPublish(t, service.catalogStore, delta, deltaToken)
	before := catalogReadEntries(t, service.catalogStore, library.ID, "")
	job := catalogRecognitionJob(t, service)
	publications := 0
	service.SetCatalogScanCommit(func(tx *gorm.DB, p CatalogScanPublication) error {
		if p.Candidate != nil {
			publications++
			if p.Candidate.Kind != "base" {
				t.Fatal("large backlog fragmented into repeated deltas/compaction")
			}
			reader, err := PinCatalogTx(tx, []uint{library.ID})
			if err != nil {
				return err
			}
			count, err := reader.EntryCount()
			if err != nil {
				return err
			}
			if count != int64(len(files)) {
				t.Fatal("published incomplete recognized base")
			}
		}
		return nil
	})
	payload := mediaLibraryRecognitionJobPayload{LibraryID: library.ID, ScanRunID: ready.ID, Generation: ready.Generation}
	if err := service.completeFastMediaLibraryRecognition(context.Background(), fastScanTestRuntime{}, payload, job); err != nil {
		t.Fatal(err)
	}
	after := catalogReadEntries(t, service.catalogStore, library.ID, "")
	if publications != 1 || len(after) != len(before) {
		t.Fatalf("publications=%d files=%d", publications, len(after))
	}
	for i, row := range after {
		if row.ID != before[i].ID || row.RecognitionID == nil || row.MatchStatus == mediaRecognitionStatusPending || row.Season == nil || *row.Season != 2 || row.Episode == nil {
			t.Fatalf("file identity/season/version lost at %d", i)
		}
		if row.ID != original[0].ID && row.ID != original[1].ID {
			var fileNumber int
			if _, err := fmt.Sscanf(row.ProviderID, "file-%d", &fileNumber); err != nil || *row.Episode != fileNumber/4+1 {
				t.Fatalf("episode/version lost at %d", i)
			}
		}
	}
	if err := service.catalogStore.Read(context.Background(), []uint{library.ID}, func(r *CatalogReader) error {
		var raw models.CatalogEntryFact
		if err := r.RawEntryFacts().Where("id=?", original[0].ID).First(&raw).Error; err != nil {
			return err
		}
		raw.SnapshotID, preserved.SnapshotID = "", ""
		if !reflect.DeepEqual(raw, preserved) {
			t.Fatal("unselected file raw override changed")
		}
		var assetsAfter []models.CatalogSourceAssetFact
		if err := r.RawSourceAssetFacts().Order("id").Find(&assetsAfter).Error; err != nil {
			return err
		}
		for i := range assetsBefore {
			assetsBefore[i].SnapshotID = ""
		}
		for i := range assetsAfter {
			assetsAfter[i].SnapshotID = ""
		}
		if len(assetsAfter) != 1 || !reflect.DeepEqual(assetsBefore, assetsAfter) {
			t.Fatal("unselected sidecar changed")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var head models.CatalogHead
	if err := service.db.First(&head, "library_id=?", library.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := service.completeFastMediaLibraryRecognition(context.Background(), fastScanTestRuntime{}, payload, job); err != nil {
		t.Fatal(err)
	}
	if publications != 1 {
		t.Fatal("completed retry published again")
	}
}

func TestCatalogScanBackgroundRecognitionRejectsExpiredLease(t *testing.T) {
	service, library, storage, profile, _ := catalogScanFixture(t, true)
	library, run := catalogScanRun(t, service, library.ID, storage, profile)
	ready, err := service.publishCatalogScan(context.Background(), library, storage, profile, run, medialibrary.Result{Files: []medialibrary.File{{RelativePath: "Unknown/S02E01.mkv", ProviderID: "unknown", ProviderIDStable: true}}}, true, service.catalogScanCommit)
	if err != nil {
		t.Fatal(err)
	}
	job := catalogRecognitionJob(t, service)
	if err := service.db.Model(&models.Job{}).Where("id=?", job.Job.ID).Update("lease_expires_at", time.Now().UTC().Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	err = service.completeFastMediaLibraryRecognition(context.Background(), fastScanTestRuntime{}, mediaLibraryRecognitionJobPayload{LibraryID: library.ID, ScanRunID: ready.ID, Generation: ready.Generation}, job)
	if !errors.Is(err, ErrCatalogFence) {
		t.Fatalf("expired lease: %v", err)
	}
	rows := catalogReadEntries(t, service.catalogStore, library.ID, "")
	if len(rows) != 1 || rows[0].RecognitionID != nil {
		t.Fatal("expired job published metadata")
	}
}

func TestCatalogScanBackgroundRecognitionSurvivesManualArtifactDirtyGeneration(t *testing.T) {
	service, library, storage, profile, entries := catalogScanFixture(t, true)
	library, run := catalogScanRun(t, service, library.ID, storage, profile)
	files := []medialibrary.File{scanFile(entries[0]), scanFile(entries[1]), {RelativePath: "Another show/Season 02/S02E01.mkv", ProviderID: "unknown", ProviderIDStable: true}}
	ready, err := service.publishCatalogScan(context.Background(), library, storage, profile, run, medialibrary.Result{Files: files}, true, service.catalogScanCommit)
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := service.loadCatalogScanBaseline(context.Background(), library.ID)
	if err != nil {
		t.Fatal(err)
	}
	manual := baseline.recognitions[0]
	manual.Title, manual.ManualOverride = "User correction", true
	if err := service.publishCatalogRecognitionDelta(context.Background(), baseline.head, []models.MediaLibraryRecognition{manual}, func(*gorm.DB, *CatalogReader) error { return nil }, func(tx *gorm.DB) error {
		return tx.Model(&models.MediaLibrary{}).Where("id=?", library.ID).Update("dirty_generation", ready.Generation+1).Error
	}); err != nil {
		t.Fatal(err)
	}
	job := catalogRecognitionJob(t, service)
	if err := service.completeFastMediaLibraryRecognition(context.Background(), fastScanTestRuntime{}, mediaLibraryRecognitionJobPayload{LibraryID: library.ID, ScanRunID: ready.ID, Generation: ready.Generation}, job); err != nil {
		t.Fatalf("manual edit interrupted other pending work: %v", err)
	}
	rows := catalogReadEntries(t, service.catalogStore, library.ID, "")
	if len(rows) != 3 {
		t.Fatalf("rows: %+v", rows)
	}
	for _, row := range rows {
		if row.RecognitionID == nil {
			t.Fatal("pending association was abandoned")
		}
		if row.ID == entries[0].ID && row.Title != "User correction" {
			t.Fatalf("manual result overwritten: %+v", row)
		}
	}
	var current models.MediaLibrary
	if err := service.db.First(&current, library.ID).Error; err != nil {
		t.Fatal(err)
	}
	if current.BaselineGeneration != ready.Generation || current.DirtyGeneration != ready.Generation+1 {
		t.Fatalf("semantic generations changed: %+v", current)
	}
	var completed models.MediaLibraryScanRun
	if err := service.db.First(&completed, ready.ID).Error; err != nil {
		t.Fatal(err)
	}
	if completed.Status != "success" {
		t.Fatalf("pending scan never completed: %+v", completed)
	}
}
