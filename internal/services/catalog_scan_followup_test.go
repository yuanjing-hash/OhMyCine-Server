package services

import (
	"context"
	"encoding/json"
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

func TestCatalogNoopEventKeepsGenerationAndDoesNotDuplicatePendingRecognition(t *testing.T) {
	s, lib, storage, profile := catalogFollowupFixture(t)
	now := time.Now().UTC().Truncate(time.Second)
	file := medialibrary.File{RelativePath: "/Pending.2026.mkv", ProviderID: "pending-provider", ProviderIDStable: true, Size: 7, ModifiedAt: now}
	lib, firstRun := catalogScanRun(t, s, lib.ID, storage, profile)
	firstRun.Kind = "event"
	first, err := s.publishCatalogScan(context.Background(), lib, storage, profile, firstRun, medialibrary.Result{Partial: true, Files: []medialibrary.File{file}}, true, s.catalogScanCommit)
	if err != nil || first.RecognitionTotal != 1 {
		t.Fatalf("first pending publication=%+v err=%v", first, err)
	}
	if err := s.RecoverCatalogScanFollowups(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	manualDirty := first.Generation + 5
	if err := s.db.Model(&models.MediaLibrary{}).Where("id = ?", lib.ID).Update("dirty_generation", manualDirty).Error; err != nil {
		t.Fatal(err)
	}
	lib, noopRun := catalogScanRun(t, s, lib.ID, storage, profile)
	noopRun.Kind = "event"
	noop, err := s.publishCatalogScan(context.Background(), lib, storage, profile, noopRun, medialibrary.Result{Partial: true, Files: []medialibrary.File{file}}, true, s.catalogScanCommit)
	if err != nil {
		t.Fatal(err)
	}
	if noop.Added != 0 || noop.Updated != 0 || noop.Removed != 0 || noop.RecognitionTotal != 1 || noop.Generation != first.Generation {
		t.Fatalf("no-op did not preserve pending generation: first=%+v noop=%+v", first, noop)
	}
	if err := s.RecoverCatalogScanFollowups(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	var jobs int64
	if err := s.db.Model(&models.Job{}).Where("job_type = ?", JobTypeMediaLibraryRecognition).Count(&jobs).Error; err != nil || jobs != 1 {
		t.Fatalf("no-op duplicated pending recognition jobs=%d err=%v", jobs, err)
	}
	var row models.CatalogScanFollowup
	if err := s.db.First(&row, "library_id = ?", lib.ID).Error; err != nil || row.Generation != first.Generation || row.ArtifactPending {
		t.Fatalf("no-op follow-up=%+v err=%v", row, err)
	}
	var current models.MediaLibrary
	if err := s.db.First(&current, lib.ID).Error; err != nil || current.BaselineGeneration != first.Generation || current.DirtyGeneration != manualDirty {
		t.Fatalf("no-op rewrote logical generation: library=%+v err=%v", current, err)
	}
}

func TestCatalogRecoverySupersedesOnlyProvenLeaseFreeObsoleteJobs(t *testing.T) {
	s, lib, _, _ := catalogFollowupFixture(t)
	now := time.Now().UTC().Truncate(time.Second)
	if err := s.db.Model(&models.MediaLibrary{}).Where("id = ?", lib.ID).Updates(map[string]any{"baseline_generation": 9, "artifact_generation": 9}).Error; err != nil {
		t.Fatal(err)
	}
	oldScan := models.MediaLibraryScanRun{LibraryID: lib.ID, Kind: "event", Status: "catalog_ready", Phase: "recognition_queued", Generation: 4, RecognitionTotal: 1, StartedAt: now}
	currentScan := models.MediaLibraryScanRun{LibraryID: lib.ID, Kind: "event", Status: "catalog_ready", Phase: "recognition_queued", Generation: 9, RecognitionTotal: 1, StartedAt: now}
	if err := s.db.Create(&oldScan).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.db.Create(&currentScan).Error; err != nil {
		t.Fatal(err)
	}
	createJob := func(id, jobType, key string, payload any, status string) models.Job {
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		job := models.Job{ID: id, CreatedByKind: "system", JobType: jobType, Priority: 20, Status: status, DisplayName: id, Provider: "media_library", ResourceKey: mediaArtifactResourceKey(lib.ID), CoalescingKey: key, Generation: 1, Revision: 1, PayloadJSON: string(raw), CheckpointJSON: "{}", CreatedAt: now, UpdatedAt: now}
		if err := s.db.Create(&job).Error; err != nil {
			t.Fatal(err)
		}
		return job
	}
	oldRecognition := createJob("obsolete-recognition", JobTypeMediaLibraryRecognition, "generation:4", mediaLibraryRecognitionJobPayload{LibraryID: lib.ID, ScanRunID: oldScan.ID, Generation: 4}, models.JobStatusQueued)
	currentRecognition := createJob("current-recognition", JobTypeMediaLibraryRecognition, "generation:9", mediaLibraryRecognitionJobPayload{LibraryID: lib.ID, ScanRunID: currentScan.ID, Generation: 9}, models.JobStatusQueued)
	runningRecognition := createJob("running-obsolete-recognition", JobTypeMediaLibraryRecognition, "generation:3", mediaLibraryRecognitionJobPayload{LibraryID: lib.ID, ScanRunID: oldScan.ID, Generation: 3}, models.JobStatusRunning)

	oldPolicy, _ := json.Marshal(mediaArtifactPolicy{LibraryID: lib.ID, Generation: 4})
	oldArtifact := models.MediaArtifactRun{ID: "obsolete-artifact-run", LibraryID: lib.ID, Generation: 4, PolicyJSON: string(oldPolicy), Status: models.MediaArtifactStatusQueued, CleanupStatus: models.MediaArtifactCleanupPending, CreatedAt: now, UpdatedAt: now}
	if err := s.db.Create(&oldArtifact).Error; err != nil {
		t.Fatal(err)
	}
	oldArtifactJob := createJob("obsolete-artifact", JobTypeMediaArtifact, "run:"+oldArtifact.ID, mediaArtifactJobPayload{ArtifactRunID: oldArtifact.ID}, models.JobStatusQueued)
	if err := s.db.Model(&oldArtifact).Update("job_id", oldArtifactJob.ID).Error; err != nil {
		t.Fatal(err)
	}
	owner := models.CatalogPhysicalWrite{LibraryID: lib.ID, OwnerKind: CatalogPhysicalArtifact, OwnerID: oldArtifact.ID, Revision: 1, State: "admitted", JobID: oldArtifactJob.ID, OwnerDigest: "fixture", RuntimeID: "fixture", EnteredAt: now, UpdatedAt: now}
	if err := s.db.Create(&owner).Error; err != nil {
		t.Fatal(err)
	}

	unprovenPolicy, _ := json.Marshal(mediaArtifactPolicy{LibraryID: lib.ID, Generation: 3})
	unprovenRun := models.MediaArtifactRun{ID: "unproven-artifact-run", LibraryID: lib.ID, Generation: 3, PolicyJSON: string(unprovenPolicy), Status: models.MediaArtifactStatusQueued, CleanupStatus: models.MediaArtifactCleanupPending, CreatedAt: now, UpdatedAt: now}
	if err := s.db.Create(&unprovenRun).Error; err != nil {
		t.Fatal(err)
	}
	unprovenJob := createJob("unproven-artifact", JobTypeMediaArtifact, "run:"+unprovenRun.ID, mediaArtifactJobPayload{ArtifactRunID: unprovenRun.ID}, models.JobStatusQueued)
	if err := s.db.Model(&unprovenRun).Update("job_id", unprovenJob.ID).Error; err != nil {
		t.Fatal(err)
	}

	if err := s.supersedeObsoleteCatalogJobs(context.Background(), 20); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []struct{ id, status string }{{oldRecognition.ID, models.JobStatusCancelled}, {oldArtifactJob.ID, models.JobStatusCancelled}, {currentRecognition.ID, models.JobStatusQueued}, {runningRecognition.ID, models.JobStatusRunning}, {unprovenJob.ID, models.JobStatusQueued}} {
		var job models.Job
		if err := s.db.First(&job, "id = ?", expected.id).Error; err != nil || job.Status != expected.status {
			t.Fatalf("job %s status=%s err=%v", expected.id, job.Status, err)
		}
	}
	if err := s.db.First(&oldScan, oldScan.ID).Error; err != nil || oldScan.Status != "superseded" {
		t.Fatalf("old scan=%+v err=%v", oldScan, err)
	}
	if err := s.db.First(&oldArtifact, "id = ?", oldArtifact.ID).Error; err != nil || oldArtifact.Status != models.MediaArtifactStatusSuperseded {
		t.Fatalf("old artifact=%+v err=%v", oldArtifact, err)
	}
	if err := s.db.First(&owner, owner.ID).Error; err != nil || owner.State != "settled" || owner.SettledAt == nil {
		t.Fatalf("old artifact owner=%+v err=%v", owner, err)
	}
	var events int64
	if err := s.db.Model(&models.JobStatusEvent{}).Where("job_id IN ? AND event_type = ?", []string{oldRecognition.ID, oldArtifactJob.ID}, "catalog.generation_superseded").Count(&events).Error; err != nil || events != 2 {
		t.Fatalf("supersede events=%d err=%v", events, err)
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
		return s.commitCatalogScanFollowupTx(tx, CatalogScanPublication{Head: head, Run: run, MetadataChanged: true, RecognitionOnly: true, ArtifactChanges: CatalogArtifactChangeSet{Recognitions: []uint{1}}})
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

func TestCatalogScanFollowupEmptyIncrementalChangeDoesNotCreateArtifactWork(t *testing.T) {
	s, lib, _, _ := catalogFollowupFixture(t)
	s.artifacts = NewMediaArtifactService(s.db, s.queue, nil, zerolog.Nop())
	s.artifacts.SetCatalogSnapshotStore(s.catalogStore)
	if err := s.db.Model(&lib).Update("metadata_artifacts_enabled", true).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	run := models.MediaLibraryScanRun{LibraryID: lib.ID, Generation: 1, Kind: "incremental", Status: "success", StartedAt: now, FinishedAt: &now}
	if err := s.db.Create(&run).Error; err != nil {
		t.Fatal(err)
	}
	var head models.CatalogHead
	if err := s.db.First(&head, "library_id = ?", lib.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.db.Transaction(func(tx *gorm.DB) error {
		return s.commitCatalogScanFollowupTx(tx, CatalogScanPublication{Head: head, Run: run, MetadataChanged: true})
	}); err != nil {
		t.Fatal(err)
	}
	var bindings, runs, jobs int64
	if err := s.db.Model(&models.CatalogArtifactBinding{}).Count(&bindings).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.db.Model(&models.MediaArtifactRun{}).Count(&runs).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.db.Model(&models.Job{}).Where("job_type = ?", JobTypeMediaArtifact).Count(&jobs).Error; err != nil {
		t.Fatal(err)
	}
	if bindings != 0 || runs != 0 || jobs != 0 {
		t.Fatalf("empty incremental diff created artifact work: bindings=%d runs=%d jobs=%d", bindings, runs, jobs)
	}
	var followup models.CatalogScanFollowup
	if err := s.db.First(&followup, "library_id = ?", lib.ID).Error; err != nil {
		t.Fatal(err)
	}
	if followup.ArtifactPending {
		t.Fatalf("empty incremental diff retained artifact intent: %+v", followup)
	}
}

func TestCatalogScanFollowupHealthyCompleteScanDoesNotCreateArtifactWork(t *testing.T) {
	s, lib, _, _ := catalogFollowupFixture(t)
	s.artifacts = NewMediaArtifactService(s.db, s.queue, nil, zerolog.Nop())
	s.artifacts.SetCatalogSnapshotStore(s.catalogStore)
	if err := s.db.Model(&lib).Update("metadata_artifacts_enabled", true).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	run := models.MediaLibraryScanRun{LibraryID: lib.ID, Generation: 1, Kind: "full", Status: "success", StartedAt: now, FinishedAt: &now}
	if err := s.db.Create(&run).Error; err != nil {
		t.Fatal(err)
	}
	var head models.CatalogHead
	if err := s.db.First(&head, "library_id = ?", lib.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.db.Transaction(func(tx *gorm.DB) error {
		return s.commitCatalogScanFollowupTx(tx, CatalogScanPublication{Head: head, Run: run, NoContentChange: true})
	}); err != nil {
		t.Fatal(err)
	}
	var bindings, runs, jobs int64
	if err := s.db.Model(&models.CatalogArtifactBinding{}).Count(&bindings).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.db.Model(&models.MediaArtifactRun{}).Count(&runs).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.db.Model(&models.Job{}).Where("job_type = ?", JobTypeMediaArtifact).Count(&jobs).Error; err != nil {
		t.Fatal(err)
	}
	if bindings != 0 || runs != 0 || jobs != 0 {
		t.Fatalf("healthy complete scan created artifact work: bindings=%d runs=%d jobs=%d", bindings, runs, jobs)
	}
}

func TestCatalogScanFollowupExplicitFullAuditSurvivesUnchangedCatalog(t *testing.T) {
	s, lib, _, _ := catalogFollowupFixture(t)
	s.artifacts = NewMediaArtifactService(s.db, s.queue, nil, zerolog.Nop())
	s.artifacts.SetCatalogSnapshotStore(s.catalogStore)
	if err := s.db.Model(&lib).Update("metadata_artifacts_enabled", true).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	run := models.MediaLibraryScanRun{LibraryID: lib.ID, Generation: 1, Kind: "strm_full_manual", Status: "success", StartedAt: now, FinishedAt: &now}
	if err := s.db.Create(&run).Error; err != nil {
		t.Fatal(err)
	}
	var head models.CatalogHead
	if err := s.db.First(&head, "library_id = ?", lib.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.db.Transaction(func(tx *gorm.DB) error {
		return s.commitCatalogScanFollowupTx(tx, CatalogScanPublication{Head: head, Run: run, NoContentChange: true})
	}); err != nil {
		t.Fatal(err)
	}
	var binding models.CatalogArtifactBinding
	if err := s.db.First(&binding, "library_id = ?", lib.ID).Error; err != nil || binding.Generation <= run.Generation {
		t.Fatalf("explicit audit lost fresh binding: %+v err=%v", binding, err)
	}
	var followup models.CatalogScanFollowup
	if err := s.db.First(&followup, "library_id = ?", lib.ID).Error; err != nil || !followup.ArtifactPending {
		t.Fatalf("explicit audit intent missing: %+v err=%v", followup, err)
	}
}
