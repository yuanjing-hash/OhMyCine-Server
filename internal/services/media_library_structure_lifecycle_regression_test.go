package services

import (
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func completeStructureRepairForTest(t *testing.T, s *MediaLibraryStructureService, actor Actor, libraryID uint, token string) {
	t.Helper()
	d, err := s.Diagnostics(context.Background(), actor, libraryID)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.PreviewSelectionRepair(context.Background(), actor, libraryID, MediaLibraryStructureSelectionInput{Revision: d.Revision, Selections: []MediaLibraryStructureSelection{{IssueToken: token, Action: StructureSelectionKeepRecommended}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.EnqueueSelectionRepair(context.Background(), actor, libraryID, p.ConfirmationToken, RequestContext{}); err != nil {
		t.Fatal(err)
	}
	job, err := s.queue.Claim([]string{JobTypeMediaLibraryRepair})
	if err != nil || job == nil {
		t.Fatalf("claim: %v", err)
	}
	if result := NewMediaLibraryRepairWorker(s).Run(context.Background(), fastScanTestRuntime{}, *job); result.ErrorCode != "" {
		t.Fatalf("repair: %+v", result)
	}
	if err := s.queue.Complete(job.Job.ID, job.LeaseToken); err != nil {
		t.Fatal(err)
	}
}

func TestStructureLocalScanPublishesAndDiagnosesOnlyOnce(t *testing.T) {
	s, actor, lib := structureConfirmationFixture(t)
	libraries := NewMediaLibraryService(s.db, s.audit, zerolog.Nop())
	defer libraries.Close()
	libraries.SetStructureService(s)
	run, err := libraries.ScanNow(context.Background(), actor, lib.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != "success" || run.CatalogPublishedAt == nil {
		t.Fatalf("run: %+v", run)
	}
	job, err := s.queue.Claim([]string{JobTypeMediaLibraryStructureDiagnosis})
	if err != nil || job == nil {
		t.Fatalf("missing initial diagnosis: %v", err)
	}
	if result := NewMediaLibraryStructureDiagnosisWorker(s).Run(context.Background(), fastScanTestRuntime{}, *job); result.ErrorCode != "" {
		t.Fatalf("diagnose: %+v", result)
	}
	if err := s.queue.Complete(job.Job.ID, job.LeaseToken); err != nil {
		t.Fatal(err)
	}
	if _, err = libraries.ScanNow(context.Background(), actor, lib.ID); err != nil {
		t.Fatal(err)
	}
	next, err := s.queue.Claim([]string{JobTypeMediaLibraryStructureDiagnosis})
	if err != nil || next != nil {
		t.Fatalf("routine scan repeated diagnosis: %+v %v", next, err)
	}
}

func TestStructureRemainingSelectionSurvivesScanAndSummaryConverges(t *testing.T) {
	s, actor, lib, _ := prepareStructureSelectionConflicts(t, 2)
	page, err := s.StructureIssues(context.Background(), actor, lib.ID, MediaLibraryStructureIssueQuery{Actionable: true})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 2 {
		t.Fatalf("groups=%d", page.Total)
	}
	completeStructureRepairForTest(t, s, actor, lib.ID, page.List[0].Token)
	d, err := s.Diagnostics(context.Background(), actor, lib.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d.IssueCount != 1 || d.Classifications.DuplicateTarget != 1 {
		t.Fatalf("stale summary: %+v", d)
	}
	// Ordinary publication advances the catalog generation independently of
	// the saved diagnosis. Source facts for the remaining group are unchanged.
	if err := s.db.Model(&models.MediaLibrary{}).Where("id = ?", lib.ID).Updates(map[string]any{"baseline_generation": 2, "dirty_generation": 2}).Error; err != nil {
		t.Fatal(err)
	}
	completeStructureRepairForTest(t, s, actor, lib.ID, page.List[1].Token)
	d, err = s.Diagnostics(context.Background(), actor, lib.ID)
	if err != nil {
		t.Fatal(err)
	}
	remaining, err := s.StructureIssues(context.Background(), actor, lib.ID, MediaLibraryStructureIssueQuery{Actionable: true})
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != models.MediaLibraryStructureHealthy || d.IssueCount != 0 || d.Classifications.DuplicateTarget != 0 || remaining.Total != 0 || len(d.Issues) != 0 {
		t.Fatalf("summary=%+v remaining=%+v", d, remaining)
	}
}

func TestStructureManualProjectionAfterRoutineGenerationAdvance(t *testing.T) {
	s, actor, lib, _ := prepareStructureSelectionConflicts(t, 1)
	now := time.Now().UTC()
	year, tmdb := 2024, int64(9000)
	r := models.MediaLibraryRecognition{LibraryID: lib.ID, SourceKey: "manual-after-scan", InputFingerprint: "manual-after-scan", ProfileID: lib.ProfileID, ProfileRevision: lib.ProfileRevision, Status: mediaRecognitionStatusMatched, MediaType: "movie", Title: "冲突影片", TMDBID: &tmdb, ReleaseYear: &year, ManualOverride: true, LastGeneration: 2, CreatedAt: now, UpdatedAt: now}
	if err := s.db.Create(&r).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.db.Model(&models.MediaLibraryEntry{}).Where("library_id = ?", lib.ID).Update("recognition_id", r.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.db.Model(&models.MediaLibrary{}).Where("id = ?", lib.ID).Update("baseline_generation", 2).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.RefreshRecognitionProjection(context.Background(), lib.ID, r.ID); err != nil {
		t.Fatal(err)
	}
	page, err := s.StructureIssues(context.Background(), actor, lib.ID, MediaLibraryStructureIssueQuery{Actionable: true})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || page.List[0].State != "manual_identity_resolved" {
		t.Fatalf("manual projection not refreshed: %+v", page)
	}
	d, err := s.Diagnostics(context.Background(), actor, lib.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d.IssueCount != 1 || d.Unrecognized != 0 {
		t.Fatalf("manual summary stale: %+v", d)
	}
}

func TestStructureUpgradeProjectionRecoverySurvivesServiceRestart(t *testing.T) {
	s, actor, lib := structureConfirmationFixture(t)
	if err := s.db.Create(&models.MediaLibraryStructureAutoState{LibraryID: lib.ID, SourceRevision: 1, DiagnosedRevision: 1, Status: "projection_pending", UpdatedAt: time.Now().UTC()}).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.RecoverLegacyProjections(context.Background()); err != nil {
		t.Fatal(err)
	}
	restarted := NewMediaLibraryStructureService(s.db, s.audit, s.queue, nil, zerolog.Nop())
	if err := restarted.RecoverLegacyProjections(context.Background()); err != nil {
		t.Fatal(err)
	}
	job, err := s.queue.Claim([]string{JobTypeMediaLibraryStructureDiagnosis})
	if err != nil || job == nil {
		t.Fatalf("claim: %v", err)
	}
	if result := NewMediaLibraryStructureDiagnosisWorker(restarted).Run(context.Background(), fastScanTestRuntime{}, *job); result.ErrorCode != "" {
		t.Fatalf("worker: %+v", result)
	}
	if err := s.queue.Complete(job.Job.ID, job.LeaseToken); err != nil {
		t.Fatal(err)
	}
	if err := restarted.RecoverLegacyProjections(context.Background()); err != nil {
		t.Fatal(err)
	}
	next, err := s.queue.Claim([]string{JobTypeMediaLibraryStructureDiagnosis})
	if err != nil || next != nil {
		t.Fatalf("repeated recovery: %+v %v", next, err)
	}
	d, err := s.Diagnostics(context.Background(), actor, lib.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != models.MediaLibraryStructureHealthy || d.IssueCount != 0 {
		t.Fatalf("summary: %+v", d)
	}
}
