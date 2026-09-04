package services

import (
	"context"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestAutomaticStructureDiagnosisRunsOncePerSourceRevision(t *testing.T) {
	service, _, library := structureConfirmationFixture(t)
	now := time.Now().UTC()
	if err := service.db.Create(&models.MediaLibraryStructureAutoState{LibraryID: library.ID, SourceRevision: 1, Status: "pending", UpdatedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	run := models.MediaLibraryScanRun{LibraryID: library.ID, Generation: 1, Kind: "initial", Status: "success", CatalogPublishedAt: &now, RecognitionTotal: 0, RecognitionCompleted: 0, StartedAt: now}
	if err := service.db.Create(&run).Error; err != nil {
		t.Fatal(err)
	}
	if err := service.EnqueueAutomaticDiagnosis(context.Background(), library.ID, run.ID, 1, run.Kind); err != nil {
		t.Fatal(err)
	}
	var first models.MediaLibraryStructureDiagnosis
	if err := service.db.First(&first, "library_id = ?", library.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !first.Automatic || first.SourceRevision != 1 {
		t.Fatalf("first=%+v", first)
	}
	if err := service.EnqueueAutomaticDiagnosis(context.Background(), library.ID, run.ID, 1, run.Kind); err != nil {
		t.Fatal(err)
	}
	var duplicate models.MediaLibraryStructureDiagnosis
	if err := service.db.First(&duplicate, "library_id = ?", library.ID).Error; err != nil {
		t.Fatal(err)
	}
	if duplicate.JobID != first.JobID {
		t.Fatalf("automatic diagnosis duplicated: %s != %s", duplicate.JobID, first.JobID)
	}
	claimed, err := service.queue.Claim([]string{JobTypeMediaLibraryStructureDiagnosis})
	if err != nil || claimed == nil {
		t.Fatalf("claim=%+v err=%v", claimed, err)
	}
	if result := NewMediaLibraryStructureDiagnosisWorker(service).Run(context.Background(), fastScanTestRuntime{}, *claimed); result.ErrorCode != "" {
		t.Fatalf("worker=%+v", result)
	}
	var state models.MediaLibraryStructureAutoState
	if err := service.db.First(&state, "library_id = ?", library.ID).Error; err != nil {
		t.Fatal(err)
	}
	if state.DiagnosedRevision != 1 || state.Status != "completed" {
		t.Fatalf("state=%+v", state)
	}

	secondRun := models.MediaLibraryScanRun{LibraryID: library.ID, Generation: 1, Kind: "full", Status: "success", CatalogPublishedAt: &now, StartedAt: now}
	if err := service.db.Create(&secondRun).Error; err != nil {
		t.Fatal(err)
	}
	if err := service.EnqueueAutomaticDiagnosis(context.Background(), library.ID, secondRun.ID, 1, secondRun.Kind); err != nil {
		t.Fatal(err)
	}
	if err := service.db.First(&duplicate, "library_id = ?", library.ID).Error; err != nil {
		t.Fatal(err)
	}
	if duplicate.JobID != first.JobID {
		t.Fatal("routine full scan scheduled another automatic diagnosis")
	}
}

func TestAutomaticStructureDiagnosisWaitsForRecognitionConvergence(t *testing.T) {
	service, _, library := structureConfirmationFixture(t)
	now := time.Now().UTC()
	if err := service.db.Create(&models.MediaLibraryStructureAutoState{LibraryID: library.ID, SourceRevision: 1, Status: "pending", UpdatedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	run := models.MediaLibraryScanRun{LibraryID: library.ID, Generation: 1, Kind: "initial", Status: "catalog_ready", CatalogPublishedAt: &now, RecognitionTotal: 2, RecognitionCompleted: 0, StartedAt: now}
	if err := service.db.Create(&run).Error; err != nil {
		t.Fatal(err)
	}
	if err := service.EnqueueAutomaticDiagnosis(context.Background(), library.ID, run.ID, 1, run.Kind); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := service.db.Model(&models.MediaLibraryStructureDiagnosis{}).Where("library_id = ?", library.ID).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("premature count=%d err=%v", count, err)
	}
	if err := service.db.Model(&run).Updates(map[string]any{"status": "success", "recognition_completed": 2}).Error; err != nil {
		t.Fatal(err)
	}
	if err := service.EnqueueAutomaticDiagnosis(context.Background(), library.ID, run.ID, 1, run.Kind); err != nil {
		t.Fatal(err)
	}
	if err := service.db.Model(&models.MediaLibraryStructureDiagnosis{}).Where("library_id = ?", library.ID).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("converged count=%d err=%v", count, err)
	}
}
