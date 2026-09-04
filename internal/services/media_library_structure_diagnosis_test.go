package services

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestStructureDiagnosisQueueCoalescesLatestGenerationAndSurvivesRestart(t *testing.T) {
	queue, _, _ := queueFixture(t)
	var profile models.MediaClassificationProfile
	if err := queue.db.Where("code = ?", "default-v1").First(&profile).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	storage := models.Storage{Name: "Diagnosis", NameNormalized: "diagnosis", Type: models.StorageTypeLocal, RootPath: t.TempDir(), RootDisplayPath: "safe", RootPathNormalized: "safe", Enabled: true, Capabilities: "{}"}
	if err := queue.db.Create(&storage).Error; err != nil {
		t.Fatal(err)
	}
	library := models.MediaLibrary{Name: "Diagnosis", NameNormalized: "diagnosis", StorageID: storage.ID, ProfileID: profile.ID, ProfileRevision: profile.Revision, RelativeRoot: "/", BaselineGeneration: 1, DirtyGeneration: 1, Enabled: true, Recursive: true, VideoExtensionsJSON: `[".mkv"]`, STRMAssetExtraExtensionsJSON: `[]`, IgnorePatternsJSON: `[]`, StructureStatus: models.MediaLibraryStructurePending, CreatedAt: now, UpdatedAt: now}
	if err := queue.db.Create(&library).Error; err != nil {
		t.Fatal(err)
	}
	firstRun := models.MediaLibraryScanRun{LibraryID: library.ID, Kind: "full", Status: "success", Generation: 1, CatalogPublishedAt: &now, StartedAt: now, FinishedAt: &now}
	if err := queue.db.Create(&firstRun).Error; err != nil {
		t.Fatal(err)
	}
	service := NewMediaLibraryStructureService(queue.db, queue.audit, queue, nil, zerolog.Nop())
	service.planner = StructurePlanner{observe: func(string) { panic("planner must not run while catalog publication enqueues") }}
	if err := service.EnqueueDiagnosis(context.Background(), library.ID, firstRun.ID, 1, firstRun.Kind); err != nil {
		t.Fatal(err)
	}
	claimed, err := queue.Claim([]string{JobTypeMediaLibraryStructureDiagnosis})
	if err != nil || claimed == nil {
		t.Fatalf("claim=%+v err=%v", claimed, err)
	}
	if err := queue.db.Model(&library).Updates(map[string]any{"baseline_generation": 2, "dirty_generation": 2}).Error; err != nil {
		t.Fatal(err)
	}
	secondRun := models.MediaLibraryScanRun{LibraryID: library.ID, Kind: "event", Status: "success", Generation: 2, CatalogPublishedAt: &now, StartedAt: now, FinishedAt: &now}
	if err := queue.db.Create(&secondRun).Error; err != nil {
		t.Fatal(err)
	}
	service.planner = StructurePlanner{}
	if err := service.EnqueueDiagnosis(context.Background(), library.ID, secondRun.ID, 2, secondRun.Kind); err != nil {
		t.Fatal(err)
	}
	var active []models.Job
	if err := queue.db.Where("job_type = ? AND status IN ?", JobTypeMediaLibraryStructureDiagnosis, activeJobStatuses()).Find(&active).Error; err != nil || len(active) != 1 {
		t.Fatalf("active=%d err=%v", len(active), err)
	}
	var latestPayload mediaLibraryStructureDiagnosisJobPayload
	if json.Unmarshal([]byte(active[0].PayloadJSON), &latestPayload) != nil || latestPayload.Generation != 2 || latestPayload.ScanRunID != secondRun.ID {
		t.Fatalf("payload=%+v", latestPayload)
	}
	worker := NewMediaLibraryStructureDiagnosisWorker(service)
	if result := worker.Run(context.Background(), fastScanTestRuntime{}, *claimed); result.ErrorCode != "" {
		t.Fatalf("stale worker=%+v", result)
	}
	if err := queue.Complete(claimed.Job.ID, claimed.LeaseToken); err != nil {
		t.Fatal(err)
	}
	// A fresh QueueService over the same SQLite database represents process
	// restart. The coalesced Job must remain claimable with the latest payload.
	restarted := NewQueueService(queue.db, queue.audit)
	claimed, err = restarted.Claim([]string{JobTypeMediaLibraryStructureDiagnosis})
	if err != nil || claimed == nil {
		t.Fatalf("restart claim=%+v err=%v", claimed, err)
	}
	if result := worker.Run(context.Background(), fastScanTestRuntime{}, *claimed); result.ErrorCode != "" {
		t.Fatalf("latest worker=%+v", result)
	}
	if err := restarted.Complete(claimed.Job.ID, claimed.LeaseToken); err != nil {
		t.Fatal(err)
	}
	var diagnosis models.MediaLibraryStructureDiagnosis
	if err := queue.db.First(&diagnosis, "library_id = ?", library.ID).Error; err != nil {
		t.Fatal(err)
	}
	if diagnosis.Generation != 2 || optionalUintValue(diagnosis.ScanRunID) != secondRun.ID || diagnosis.Status != models.MediaLibraryStructureHealthy {
		t.Fatalf("diagnosis=%+v", diagnosis)
	}
}

func TestStructureDiagnosisFailureDoesNotChangePublishedScan(t *testing.T) {
	service, _, library := structureConfirmationFixture(t)
	now := time.Now().UTC()
	run := models.MediaLibraryScanRun{LibraryID: library.ID, Kind: "full", Status: "success", Generation: library.BaselineGeneration, CatalogPublishedAt: &now, StartedAt: now, FinishedAt: &now}
	if err := service.db.Create(&run).Error; err != nil {
		t.Fatal(err)
	}
	if err := service.EnqueueDiagnosis(context.Background(), library.ID, run.ID, run.Generation, run.Kind); err != nil {
		t.Fatal(err)
	}
	var diagnosis models.MediaLibraryStructureDiagnosis
	if err := service.db.First(&diagnosis, "library_id = ?", library.ID).Error; err != nil {
		t.Fatal(err)
	}
	var job models.Job
	if err := service.db.First(&job, "id = ?", diagnosis.JobID).Error; err != nil {
		t.Fatal(err)
	}
	service.failDiagnosis(mediaLibraryStructureDiagnosisJobPayload{LibraryID: library.ID, ScanRunID: run.ID, Generation: run.Generation, ScanKind: run.Kind}, diagnosis.JobID, job.Generation)
	if err := service.db.First(&run, run.ID).Error; err != nil {
		t.Fatal(err)
	}
	if run.Status != "success" || run.CatalogPublishedAt == nil {
		t.Fatalf("diagnosis failure changed catalog scan: %+v", run)
	}
}

func TestStructureDiagnosisSamePayloadRequeueCannotBeOverwrittenByStaleWorker(t *testing.T) {
	queue, _, _ := queueFixture(t)
	var profile models.MediaClassificationProfile
	if err := queue.db.Where("code = ?", "default-v1").First(&profile).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	storage := models.Storage{Name: "Same payload diagnosis", NameNormalized: "same-payload-diagnosis", Type: models.StorageTypeLocal, RootPath: t.TempDir(), RootDisplayPath: "safe", RootPathNormalized: "safe", Enabled: true, Capabilities: "{}"}
	if err := queue.db.Create(&storage).Error; err != nil {
		t.Fatal(err)
	}
	library := models.MediaLibrary{Name: "Same payload diagnosis", NameNormalized: "same-payload-diagnosis", StorageID: storage.ID, ProfileID: profile.ID, ProfileRevision: profile.Revision, RelativeRoot: "/", BaselineGeneration: 1, DirtyGeneration: 1, Enabled: true, Recursive: true, VideoExtensionsJSON: `[".mkv"]`, STRMAssetExtraExtensionsJSON: `[]`, IgnorePatternsJSON: `[]`, StructureStatus: models.MediaLibraryStructurePending, CreatedAt: now, UpdatedAt: now}
	if err := queue.db.Create(&library).Error; err != nil {
		t.Fatal(err)
	}
	run := models.MediaLibraryScanRun{LibraryID: library.ID, Kind: "manual", Status: "success", Generation: 1, CatalogPublishedAt: &now, StartedAt: now, FinishedAt: &now}
	if err := queue.db.Create(&run).Error; err != nil {
		t.Fatal(err)
	}
	entry := models.MediaLibraryEntry{
		LibraryID: library.ID, RelativePath: "/等待识别/示例.mkv", MediaType: "movie", Title: "等待识别",
		WorkKey: "file:pending", MatchStatus: mediaRecognitionStatusPending, LastGeneration: 1,
		ModifiedAt: now, CreatedAt: now, UpdatedAt: now,
	}
	if err := queue.db.Create(&entry).Error; err != nil {
		t.Fatal(err)
	}
	service := NewMediaLibraryStructureService(queue.db, queue.audit, queue, nil, zerolog.Nop())
	if err := service.EnqueueDiagnosis(context.Background(), library.ID, run.ID, 1, run.Kind); err != nil {
		t.Fatal(err)
	}
	claimed, err := queue.Claim([]string{JobTypeMediaLibraryStructureDiagnosis})
	if err != nil || claimed == nil {
		t.Fatalf("claim=%+v err=%v", claimed, err)
	}
	if err := queue.db.Model(&entry).Updates(map[string]any{"match_status": mediaRecognitionStatusUnrecognized, "recognition_error_code": "tmdb_no_match", "updated_at": time.Now().UTC()}).Error; err != nil {
		t.Fatal(err)
	}
	if err := service.EnqueueDiagnosis(context.Background(), library.ID, run.ID, 1, run.Kind); err != nil {
		t.Fatal(err)
	}
	worker := NewMediaLibraryStructureDiagnosisWorker(service)
	if result := worker.Run(context.Background(), fastScanTestRuntime{}, *claimed); result.ErrorCode != "" {
		t.Fatalf("stale worker=%+v", result)
	}
	var diagnosis models.MediaLibraryStructureDiagnosis
	if err := queue.db.First(&diagnosis, "library_id = ?", library.ID).Error; err != nil {
		t.Fatal(err)
	}
	if diagnosis.Status != models.MediaLibraryStructureQueued || diagnosis.ProcessedItems != 0 || diagnosis.FinishedAt != nil {
		t.Fatalf("stale worker overwrote latest projection: %+v", diagnosis)
	}
	if err := queue.Complete(claimed.Job.ID, claimed.LeaseToken); err != nil {
		t.Fatal(err)
	}
	if err := queue.db.First(&diagnosis, "library_id = ?", library.ID).Error; err != nil {
		t.Fatal(err)
	}
	if diagnosis.Status != models.MediaLibraryStructureQueued || diagnosis.UnrecognizedCount != 0 || diagnosis.IssueCount != 0 {
		t.Fatalf("stale worker published recognition results before the latest diagnosis: %+v", diagnosis)
	}
	claimed, err = queue.Claim([]string{JobTypeMediaLibraryStructureDiagnosis})
	if err != nil || claimed == nil {
		t.Fatalf("latest claim=%+v err=%v", claimed, err)
	}
	if result := worker.Run(context.Background(), fastScanTestRuntime{}, *claimed); result.ErrorCode != "" {
		t.Fatalf("latest worker=%+v", result)
	}
	if err := queue.Complete(claimed.Job.ID, claimed.LeaseToken); err != nil {
		t.Fatal(err)
	}
	if err := queue.db.First(&diagnosis, "library_id = ?", library.ID).Error; err != nil {
		t.Fatal(err)
	}
	if diagnosis.Status != models.MediaLibraryStructureIssues || diagnosis.UnrecognizedCount != 1 || diagnosis.IssueCount != 1 {
		t.Fatalf("latest diagnosis did not publish the committed recognition result: %+v", diagnosis)
	}
}
