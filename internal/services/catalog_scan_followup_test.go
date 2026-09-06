package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/medialibrary"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

func catalogFollowupFixture(t *testing.T) (*MediaLibraryService, models.MediaLibrary, models.Storage, models.MediaClassificationProfile) {
	t.Helper()
	s, lib, storage, profile, _ := catalogScanFixture(t, false)
	lib.Enabled = true
	if err := s.db.Model(&lib).Update("enabled", true).Error; err != nil {
		t.Fatal(err)
	}
	s.changes = NewMediaChangeService(s.db)
	s.queue = NewQueueService(s.db, NewAuditService(s.db))
	s.EnableCatalogScanFollowups()
	return s, lib, storage, profile
}

func saveCatalogFollowup(t *testing.T, s *MediaLibraryService, lib models.MediaLibrary, storage models.Storage, profile models.MediaClassificationProfile, partial, pending bool) models.CatalogScanFollowup {
	t.Helper()
	_, run := catalogScanRun(t, s, lib.ID, storage, profile)
	run.Partial = partial
	if pending {
		run.RecognitionTotal = 2
	}
	var head models.CatalogHead
	if err := s.db.First(&head, "library_id = ?", lib.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.db.Transaction(func(tx *gorm.DB) error {
		return s.finishCatalogScanTx(tx, profile, &run, CatalogScanPublication{Head: head, NoContentChange: true}, s.catalogScanCommit)
	}); err != nil {
		t.Fatal(err)
	}
	var row models.CatalogScanFollowup
	if err := s.db.First(&row, "library_id = ?", lib.ID).Error; err != nil {
		t.Fatal(err)
	}
	return row
}

func TestCatalogScanFollowupRecognitionReceiptSurvivesRestartAndKeepsJobPolicy(t *testing.T) {
	s, lib, storage, profile := catalogFollowupFixture(t)
	row := saveCatalogFollowup(t, s, lib, storage, profile, false, true)
	var count int64
	if err := s.db.Model(&models.Job{}).Where("job_type = ?", JobTypeMediaLibraryRecognition).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("hook enqueued inline: %d %v", count, err)
	}
	// Versioned runs must not also be picked up by the legacy startup requeue.
	if err := s.recoverMediaLibraryRecognitionJobs(); err != nil {
		t.Fatal(err)
	}
	if err := s.db.Model(&models.Job{}).Where("job_type = ?", JobTypeMediaLibraryRecognition).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("legacy startup bypassed receipt: %d %v", count, err)
	}
	restarted := NewMediaLibraryService(s.db, NewAuditService(s.db), zerolog.Nop())
	restarted.SetCatalogSnapshotStore(s.catalogStore)
	restarted.queue = s.queue
	for i := 0; i < 3; i++ {
		if err := restarted.RecoverCatalogScanFollowups(context.Background(), 10); err != nil {
			t.Fatal(err)
		}
	}
	var job models.Job
	if err := s.db.Where("job_type = ?", JobTypeMediaLibraryRecognition).First(&job).Error; err != nil {
		t.Fatal(err)
	}
	if job.Generation != 1 || job.Revision != 1 {
		t.Fatalf("recovery bumped job: %+v", job)
	}
	if err := s.db.Model(&job).Update("status", models.JobStatusCancelled).Error; err != nil {
		t.Fatal(err)
	}
	// Even an older receipt replay cannot revive a user's cancelled occurrence.
	if err := s.db.Model(&row).Update("recognition_pending", true).Error; err != nil {
		t.Fatal(err)
	}
	if err := restarted.RecoverCatalogScanFollowups(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	var after models.Job
	if err := s.db.First(&after, "id = ?", job.ID).Error; err != nil {
		t.Fatal(err)
	}
	if after.Status != models.JobStatusCancelled || after.Generation != 1 {
		t.Fatalf("cancelled job revived: %+v", after)
	}
}

func TestCatalogScanFollowupRealPublicationRollbackAndCompactMarker(t *testing.T) {
	s, lib, storage, profile := catalogFollowupFixture(t)
	entries := catalogReadEntries(t, s.catalogStore, lib.ID, "")
	lib, run := catalogScanRun(t, s, lib.ID, storage, profile)
	changed := scanFile(entries[0])
	changed.Size = 777
	input := medialibrary.Result{Partial: true, Files: []medialibrary.File{changed}}
	injected := errors.New("receipt failed")
	_, err := s.publishCatalogScan(context.Background(), lib, storage, profile, run, input, false, func(tx *gorm.DB, p CatalogScanPublication) error {
		if err := s.commitCatalogScanFollowupTx(tx, p); err != nil {
			return err
		}
		return injected
	})
	if !errors.Is(err, injected) {
		t.Fatalf("expected receipt failure: %v", err)
	}
	if rows := catalogReadEntries(t, s.catalogStore, lib.ID, ""); rows[0].Size == 777 {
		t.Fatal("failed receipt leaked catalog")
	}
	var count int64
	if err := s.db.Model(&models.CatalogScanFollowup{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("receipt leaked: %d %v", count, err)
	}
	if err := s.db.Model(&models.MediaLibraryChange{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("change leaked: %d %v", count, err)
	}
	if _, err := s.publishCatalogScan(context.Background(), lib, storage, profile, run, input, false, s.catalogScanCommit); err != nil {
		t.Fatal(err)
	}
	var change models.MediaLibraryChange
	if err := s.db.First(&change).Error; err != nil {
		t.Fatal(err)
	}
	if change.State != models.MediaLibraryChangeReady {
		t.Fatalf("artifact-free publication pending: %+v", change)
	}
	var dispatch models.MediaChangeDispatch
	if err := s.db.First(&dispatch, "library_id = ?", lib.ID).Error; err != nil || dispatch.Revision != change.Revision {
		t.Fatalf("fanout receipt missing: %+v %v", dispatch, err)
	}
}

func TestCatalogScanFollowupFullIntentSurvivesPartialAndRunsOnlyOnce(t *testing.T) {
	s, lib, storage, profile := catalogFollowupFixture(t)
	s.structure = NewMediaLibraryStructureService(s.db, NewAuditService(s.db), s.queue, nil, zerolog.Nop())
	s.structure.SetCatalogSnapshotStore(s.catalogStore)
	full := saveCatalogFollowup(t, s, lib, storage, profile, false, false)
	partial := saveCatalogFollowup(t, s, lib, storage, profile, true, false)
	if !partial.CompleteObserved || !partial.DiagnosisPending {
		t.Fatalf("lost full intent: %+v", partial)
	}
	if err := s.acknowledgeCatalogFollowup(context.Background(), full, "diagnosis_pending"); !errors.Is(err, ErrCatalogFence) {
		t.Fatalf("old ack accepted: %v", err)
	}
	if err := s.RecoverCatalogScanFollowups(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	var diagnosis models.MediaLibraryStructureDiagnosis
	if err := s.db.First(&diagnosis, "library_id = ?", lib.ID).Error; err != nil {
		t.Fatal(err)
	}
	if diagnosis.Generation != partial.Generation || !diagnosis.Automatic {
		t.Fatalf("not current diagnosis: %+v", diagnosis)
	}
	if err := s.db.Model(&models.MediaLibraryStructureAutoState{}).Where("library_id = ?", lib.ID).Updates(map[string]any{"diagnosed_revision": 1, "status": "completed"}).Error; err != nil {
		t.Fatal(err)
	}
	_ = saveCatalogFollowup(t, s, lib, storage, profile, false, false)
	if err := s.RecoverCatalogScanFollowups(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	var after models.MediaLibraryStructureDiagnosis
	if err := s.db.First(&after, "library_id = ?", lib.ID).Error; err != nil {
		t.Fatal(err)
	}
	if after.JobID != diagnosis.JobID || after.Generation != diagnosis.Generation {
		t.Fatal("routine scan rediagnosed source")
	}
}

type catalogFollowupArtwork struct {
	calls   int
	failure error
}

func (a *catalogFollowupArtwork) ScheduleGeneration(uint, bool) error {
	panic("durable receipt must await actual artwork completion")
}
func (a *catalogFollowupArtwork) ReconcileMediaLibrary(context.Context, uint, bool) error {
	a.calls++
	return a.failure
}

func TestCatalogScanFollowupArtworkFailureAndSourceFence(t *testing.T) {
	s, lib, storage, profile := catalogFollowupFixture(t)
	a := &catalogFollowupArtwork{failure: errors.New("render failed")}
	s.libraryArtwork = a
	row := saveCatalogFollowup(t, s, lib, storage, profile, false, false)
	if err := s.db.Model(&row).Updates(map[string]any{"artwork_pending": true, "updated_at": time.Now().Add(-time.Hour)}).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.RecoverCatalogScanFollowups(context.Background(), 1); err == nil {
		t.Fatal("render failure hidden")
	}
	var pending models.CatalogScanFollowup
	if err := s.db.First(&pending, "library_id = ?", lib.ID).Error; err != nil || !pending.ArtworkPending || !pending.UpdatedAt.After(row.UpdatedAt) {
		t.Fatalf("failed receipt not retained/rotated: %+v %v", pending, err)
	}
	a.failure = nil
	if err := s.RecoverCatalogScanFollowups(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if a.calls != 2 {
		t.Fatalf("render calls %d", a.calls)
	}
	if err := s.db.Model(&row).Update("artwork_pending", true).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.db.Model(&lib).Update("relative_root", "changed-root").Error; err != nil {
		t.Fatal(err)
	}
	if err := s.RecoverCatalogScanFollowups(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if a.calls != 2 {
		t.Fatal("stale source executed artwork")
	}
}

func TestCatalogScanFollowupArtifactUsesLatestLogicalGenerationAndDoesNotReviveCancelledJob(t *testing.T) {
	s, lib, _, _ := catalogFollowupFixture(t)
	s.artifacts = NewMediaArtifactService(s.db, s.queue, nil, zerolog.Nop())
	s.artifacts.SetCatalogSnapshotStore(s.catalogStore)
	if err := s.db.Model(&lib).Updates(map[string]any{"metadata_artifacts_enabled": true, "dirty_generation": 9, "artifact_generation": 9}).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	run := models.MediaLibraryScanRun{LibraryID: lib.ID, Generation: 4, Kind: "full", Status: "catalog_ready", Phase: "recognition_running", CatalogPublishedAt: &now, StartedAt: now, RecognitionTotal: 2, RecognitionCompleted: 1}
	if err := s.db.Create(&run).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.db.Model(&lib).Update("baseline_generation", 4).Error; err != nil {
		t.Fatal(err)
	}
	var head models.CatalogHead
	if err := s.db.First(&head, "library_id = ?", lib.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.db.Transaction(func(tx *gorm.DB) error {
		return s.commitCatalogScanFollowupTx(tx, CatalogScanPublication{Head: head, Run: run, MetadataChanged: true, RecognitionOnly: true})
	}); err != nil {
		t.Fatal(err)
	}
	var row models.CatalogScanFollowup
	if err := s.db.First(&row, "library_id = ?", lib.ID).Error; err != nil {
		t.Fatal(err)
	}
	if row.Generation != 4 || row.ArtifactGeneration != 9 || row.RecognitionPending {
		t.Fatalf("mixed scan/artifact generations: %+v", row)
	}
	var change models.MediaLibraryChange
	if err := s.db.First(&change).Error; err != nil || change.Generation != 9 || change.State != models.MediaLibraryChangePending {
		t.Fatalf("change readiness: %+v %v", change, err)
	}
	if err := s.RecoverCatalogScanFollowups(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	var job models.Job
	if err := s.db.Where("job_type = ?", JobTypeMediaArtifact).First(&job).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.db.Model(&job).Update("status", models.JobStatusCancelled).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.db.Model(&row).Update("artifact_pending", true).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.RecoverCatalogScanFollowups(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	var after models.Job
	if err := s.db.First(&after, "id = ?", job.ID).Error; err != nil {
		t.Fatal(err)
	}
	if after.Status != models.JobStatusCancelled || after.Generation != job.Generation {
		t.Fatal("receipt replay revived artifact job")
	}
}
