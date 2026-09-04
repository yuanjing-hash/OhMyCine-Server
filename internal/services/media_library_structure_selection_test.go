package services

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

type structureNoRecycleBackend struct{}

func (structureNoRecycleBackend) StorageType() string { return models.StorageTypeLocal }
func (structureNoRecycleBackend) ValidateRecycle(context.Context, StructureBoundary) error {
	return errors.New("recoverable recycle unavailable")
}
func (structureNoRecycleBackend) Recycle(context.Context, StructureBoundary, []StructureRecycleItem, StructureProgress) error {
	return errors.New("must not execute")
}
func (structureNoRecycleBackend) Apply(context.Context, StructureBoundary, []StructurePlanItem, StructureProgress) error {
	return nil
}

func prepareStructureSelectionConflicts(t *testing.T, groupCount int) (*MediaLibraryStructureService, Actor, models.MediaLibrary, MediaLibraryStructureDiagnostics) {
	t.Helper()
	service, actor, library := structureConfirmationFixture(t)
	if err := service.db.Model(&models.MediaLibrary{}).Where("id = ?", library.ID).Updates(map[string]any{
		"movie_directory_template": "电影/{title} ({year})",
		"movie_filename_template":  "{title} ({year})",
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := service.db.Create(&models.MediaLibraryStructureAutoState{LibraryID: library.ID, SourceRevision: 1, Status: "pending", UpdatedAt: time.Now().UTC()}).Error; err != nil {
		t.Fatal(err)
	}
	var storage models.Storage
	if err := service.db.First(&storage, library.StorageID).Error; err != nil {
		t.Fatal(err)
	}
	year := 2024
	for index := 0; index < groupCount; index++ {
		title := "冲突影片"
		if groupCount > 1 {
			title += string(rune('A' + index))
		}
		tmdbID := int64(9000 + index)
		first := filepath.Join(storage.RootPath, "incoming", title+".mkv")
		copyPath := filepath.Join(storage.RootPath, "incoming", title+" (1).mkv")
		if err := os.MkdirAll(filepath.Dir(first), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(first, []byte("primary"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(copyPath, []byte("copy"), 0o644); err != nil {
			t.Fatal(err)
		}
		firstInfo, err := os.Stat(first)
		if err != nil {
			t.Fatal(err)
		}
		copyInfo, err := os.Stat(copyPath)
		if err != nil {
			t.Fatal(err)
		}
		entries := []models.MediaLibraryEntry{
			{LibraryID: library.ID, RelativePath: "/incoming/" + title + ".mkv", ProviderID: "primary-" + title, MediaType: "movie", Title: title, WorkKey: "movie:tmdb:" + uintID(uint(tmdbID)), MatchStatus: mediaRecognitionStatusMatched, TMDBID: &tmdbID, ReleaseYear: &year, Size: 7, ModifiedAt: firstInfo.ModTime().UTC()},
			{LibraryID: library.ID, RelativePath: "/incoming/" + title + " (1).mkv", ProviderID: "copy-" + title, MediaType: "movie", Title: title, WorkKey: "movie:tmdb:" + uintID(uint(tmdbID)), MatchStatus: mediaRecognitionStatusMatched, TMDBID: &tmdbID, ReleaseYear: &year, Size: 4, ModifiedAt: copyInfo.ModTime().UTC()},
		}
		if err := service.db.Create(&entries).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := service.EnqueueDiagnosis(context.Background(), library.ID, 0, library.BaselineGeneration, "manual"); err != nil {
		t.Fatal(err)
	}
	claimed, err := service.queue.Claim([]string{JobTypeMediaLibraryStructureDiagnosis})
	if err != nil || claimed == nil {
		t.Fatalf("claim diagnosis=%+v err=%v", claimed, err)
	}
	if result := NewMediaLibraryStructureDiagnosisWorker(service).Run(context.Background(), fastScanTestRuntime{}, *claimed); result.ErrorCode != "" {
		t.Fatalf("diagnosis worker=%+v", result)
	}
	if err := service.queue.Complete(claimed.Job.ID, claimed.LeaseToken); err != nil {
		t.Fatal(err)
	}
	diagnostics, err := service.Diagnostics(context.Background(), actor, library.ID)
	if err != nil {
		t.Fatal(err)
	}
	return service, actor, library, diagnostics
}

func TestStructureSelectionKeepsRecommendedAndRecyclesLoser(t *testing.T) {
	service, actor, library, diagnostics := prepareStructureSelectionConflicts(t, 1)
	page, err := service.StructureIssues(context.Background(), actor, library.ID, MediaLibraryStructureIssueQuery{Page: 1, PageSize: 1, Actionable: true})
	if err != nil || page.Total != 1 || len(page.List) != 1 {
		t.Fatalf("issues=%+v err=%v", page, err)
	}
	issue := page.List[0]
	if issue.RecommendedMemberToken == "" || len(issue.Members) != 2 {
		t.Fatalf("conflict recommendation missing: %+v", issue)
	}
	preview, err := service.PreviewSelectionRepair(context.Background(), actor, library.ID, MediaLibraryStructureSelectionInput{Revision: diagnostics.Revision, Selections: []MediaLibraryStructureSelection{{IssueToken: issue.Token, Action: StructureSelectionKeepRecommended}}})
	if err != nil {
		t.Fatal(err)
	}
	if preview.IssueCount != 1 || preview.MoveCount != 1 || preview.RecycleCount != 1 || preview.SkippedCount != 0 || preview.Selections[0].MemberToken != issue.RecommendedMemberToken {
		t.Fatalf("preview=%+v", preview)
	}
	repair, err := service.EnqueueSelectionRepair(context.Background(), actor, library.ID, preview.ConfirmationToken, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := service.queue.Claim([]string{JobTypeMediaLibraryRepair})
	if err != nil || claimed == nil || repair.JobID == nil || claimed.Job.ID != *repair.JobID {
		t.Fatalf("claim repair=%+v record=%+v err=%v", claimed, repair, err)
	}
	if result := NewMediaLibraryRepairWorker(service).Run(context.Background(), fastScanTestRuntime{}, *claimed); result.ErrorCode != "" {
		t.Fatalf("repair worker=%+v", result)
	}
	if err := service.queue.Complete(claimed.Job.ID, claimed.LeaseToken); err != nil {
		t.Fatal(err)
	}
	var storage models.Storage
	if err := service.db.First(&storage, library.StorageID).Error; err != nil {
		t.Fatal(err)
	}
	canonical := filepath.Join(storage.RootPath, "电影", "冲突影片 (2024)", "冲突影片 (2024).mkv")
	data, err := os.ReadFile(canonical)
	if err != nil || string(data) != "primary" {
		t.Fatalf("canonical=%q err=%v", data, err)
	}
	recycled := false
	_ = filepath.WalkDir(filepath.Join(storage.RootPath, ".ohmycine-recycle"), func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr == nil && !entry.IsDir() && strings.HasSuffix(filepath.ToSlash(path), "/incoming/冲突影片 (1).mkv") {
			recycled = true
		}
		return nil
	})
	if !recycled {
		t.Fatal("losing source was not moved to recoverable local recycle directory")
	}
	if _, err := service.EnqueueSelectionRepair(context.Background(), actor, library.ID, preview.ConfirmationToken, RequestContext{}); ErrorCode(err) != CodeInvalidRequest {
		t.Fatalf("one-time confirmation was replayable: %v", err)
	}
	remaining, err := service.StructureIssues(context.Background(), actor, library.ID, MediaLibraryStructureIssueQuery{Page: 1, PageSize: 10, Actionable: true})
	if err != nil || remaining.Total != 0 {
		t.Fatalf("resolved issue remained visible: %+v err=%v", remaining, err)
	}
}

func TestStructureSelectionWorkerRejectsSameSizeLocalReplacement(t *testing.T) {
	service, actor, library, diagnostics := prepareStructureSelectionConflicts(t, 1)
	page, err := service.StructureIssues(context.Background(), actor, library.ID, MediaLibraryStructureIssueQuery{Page: 1, PageSize: 10, Actionable: true})
	if err != nil {
		t.Fatal(err)
	}
	preview, err := service.PreviewSelectionRepair(context.Background(), actor, library.ID, MediaLibraryStructureSelectionInput{Revision: diagnostics.Revision, Selections: []MediaLibraryStructureSelection{{IssueToken: page.List[0].Token, Action: StructureSelectionKeepRecommended}}})
	if err != nil {
		t.Fatal(err)
	}
	repair, err := service.EnqueueSelectionRepair(context.Background(), actor, library.ID, preview.ConfirmationToken, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := service.queue.Claim([]string{JobTypeMediaLibraryRepair})
	if err != nil || claimed == nil || repair.JobID == nil || claimed.Job.ID != *repair.JobID {
		t.Fatalf("claim repair=%+v err=%v", claimed, err)
	}
	var storage models.Storage
	if err := service.db.First(&storage, library.StorageID).Error; err != nil {
		t.Fatal(err)
	}
	loser := filepath.Join(storage.RootPath, "incoming", "冲突影片 (1).mkv")
	if err := os.WriteFile(loser, []byte("evil"), 0o644); err != nil {
		t.Fatal(err)
	}
	changedAt := time.Now().UTC().Add(2 * time.Second)
	if err := os.Chtimes(loser, changedAt, changedAt); err != nil {
		t.Fatal(err)
	}
	if result := NewMediaLibraryRepairWorker(service).Run(context.Background(), fastScanTestRuntime{}, *claimed); result.ErrorCode == "" {
		t.Fatal("same-size replacement was recycled under a stale plan")
	}
	if data, err := os.ReadFile(loser); err != nil || string(data) != "evil" {
		t.Fatalf("replacement changed: %q err=%v", data, err)
	}
}

func TestStructureSelectionBulkCoversAllPagesAndExplicitSelectionWins(t *testing.T) {
	service, actor, library, diagnostics := prepareStructureSelectionConflicts(t, 3)
	page, err := service.StructureIssues(context.Background(), actor, library.ID, MediaLibraryStructureIssueQuery{Page: 1, PageSize: 1, Actionable: true})
	if err != nil || page.Total != 3 || len(page.List) != 1 {
		t.Fatalf("paged issues=%+v err=%v", page, err)
	}
	preview, err := service.PreviewSelectionRepair(context.Background(), actor, library.ID, MediaLibraryStructureSelectionInput{
		Revision:    diagnostics.Revision,
		BulkActions: []MediaLibraryStructureBulkAction{{Codes: []string{"duplicate_target"}, Action: StructureSelectionKeepRecommended}},
		Selections:  []MediaLibraryStructureSelection{{IssueToken: page.List[0].Token, Action: StructureSelectionSkip}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if preview.IssueCount != 3 || preview.SkippedCount != 1 || preview.RecycleCount != 2 || preview.MoveCount != 2 {
		t.Fatalf("bulk preview did not cover authoritative result set: %+v", preview)
	}
}

func TestStructureSelectionRejectsStaleSourceRevision(t *testing.T) {
	service, actor, library, diagnostics := prepareStructureSelectionConflicts(t, 1)
	page, err := service.StructureIssues(context.Background(), actor, library.ID, MediaLibraryStructureIssueQuery{Page: 1, PageSize: 10, Actionable: true})
	if err != nil {
		t.Fatal(err)
	}
	preview, err := service.PreviewSelectionRepair(context.Background(), actor, library.ID, MediaLibraryStructureSelectionInput{Revision: diagnostics.Revision, Selections: []MediaLibraryStructureSelection{{IssueToken: page.List[0].Token, Action: StructureSelectionKeepRecommended}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.db.Model(&models.MediaLibraryStructureAutoState{}).Where("library_id = ?", library.ID).Update("source_revision", 2).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := service.EnqueueSelectionRepair(context.Background(), actor, library.ID, preview.ConfirmationToken, RequestContext{}); ErrorCode(err) != CodeConflict {
		t.Fatalf("stale source revision was accepted: %v", err)
	}
}

func TestStructureSelectionConfirmationRejectsTamperActorAndExpiry(t *testing.T) {
	service, actor, library, diagnostics := prepareStructureSelectionConflicts(t, 1)
	page, err := service.StructureIssues(context.Background(), actor, library.ID, MediaLibraryStructureIssueQuery{Page: 1, PageSize: 10, Actionable: true})
	if err != nil {
		t.Fatal(err)
	}
	selection := MediaLibraryStructureSelectionInput{Revision: diagnostics.Revision, Selections: []MediaLibraryStructureSelection{{IssueToken: page.List[0].Token, Action: StructureSelectionKeepRecommended}}}
	preview, err := service.PreviewSelectionRepair(context.Background(), actor, library.ID, selection)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.EnqueueSelectionRepair(context.Background(), actor, library.ID, preview.ConfirmationToken+"x", RequestContext{}); ErrorCode(err) != CodeInvalidRequest {
		t.Fatalf("tampered confirmation was accepted: %v", err)
	}
	otherActor := actor
	otherActor.User.ID++
	if _, err := service.EnqueueSelectionRepair(context.Background(), otherActor, library.ID, preview.ConfirmationToken, RequestContext{}); ErrorCode(err) != CodeInvalidRequest {
		t.Fatalf("cross-actor confirmation was accepted: %v", err)
	}
	claim, err := service.verifyStructureClaim(preview.ConfirmationToken)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.db.Model(&models.MediaLibraryStructureRepairDraft{}).Where("id = ?", claim.DraftID).Update("expires_at", time.Now().UTC().Add(-time.Minute)).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := service.EnqueueSelectionRepair(context.Background(), actor, library.ID, preview.ConfirmationToken, RequestContext{}); ErrorCode(err) != CodeInvalidRequest {
		t.Fatalf("expired confirmation was accepted: %v", err)
	}
}

func TestStructureSelectionPreviewRejectsUnrecoverableConflictHandling(t *testing.T) {
	service, actor, library, diagnostics := prepareStructureSelectionConflicts(t, 1)
	service.backends = NewMediaLibraryStructureBackendRegistry(structureNoRecycleBackend{})
	page, err := service.StructureIssues(context.Background(), actor, library.ID, MediaLibraryStructureIssueQuery{Page: 1, PageSize: 10, Actionable: true})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.PreviewSelectionRepair(context.Background(), actor, library.ID, MediaLibraryStructureSelectionInput{Revision: diagnostics.Revision, Selections: []MediaLibraryStructureSelection{{IssueToken: page.List[0].Token, Action: StructureSelectionKeepRecommended}}})
	if ErrorCode(err) != CodeMediaLibraryStructureUnavailable {
		t.Fatalf("unrecoverable conflict handling was previewed: %v", err)
	}
}

func TestStructureSelectionKeepsEveryConflictMemberAsVersion(t *testing.T) {
	service, actor, library, diagnostics := prepareStructureSelectionConflicts(t, 1)
	var storage models.Storage
	if err := service.db.First(&storage, library.StorageID).Error; err != nil {
		t.Fatal(err)
	}
	occupiedRelative := "/电影/冲突影片 (2024)/冲突影片 (2024) (2).mkv"
	occupiedPath := filepath.Join(storage.RootPath, filepath.FromSlash(strings.TrimPrefix(occupiedRelative, "/")))
	if err := os.MkdirAll(filepath.Dir(occupiedPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(occupiedPath, []byte("sentinel"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := service.db.Create(&models.MediaLibraryEntry{LibraryID: library.ID, RelativePath: occupiedRelative, ProviderID: "healthy-version-two", MatchStatus: mediaRecognitionStatusPending, Size: 8}).Error; err != nil {
		t.Fatal(err)
	}
	page, err := service.StructureIssues(context.Background(), actor, library.ID, MediaLibraryStructureIssueQuery{Page: 1, PageSize: 10, Actionable: true})
	if err != nil {
		t.Fatal(err)
	}
	preview, err := service.PreviewSelectionRepair(context.Background(), actor, library.ID, MediaLibraryStructureSelectionInput{Revision: diagnostics.Revision, Selections: []MediaLibraryStructureSelection{{IssueToken: page.List[0].Token, Action: StructureSelectionKeepAllVersions}}})
	if err != nil {
		t.Fatal(err)
	}
	if preview.MoveCount != 2 || preview.RecycleCount != 0 {
		t.Fatalf("version preview=%+v", preview)
	}
	repair, err := service.EnqueueSelectionRepair(context.Background(), actor, library.ID, preview.ConfirmationToken, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := service.queue.Claim([]string{JobTypeMediaLibraryRepair})
	if err != nil || claimed == nil || repair.JobID == nil || claimed.Job.ID != *repair.JobID {
		t.Fatalf("claim repair=%+v err=%v", claimed, err)
	}
	if result := NewMediaLibraryRepairWorker(service).Run(context.Background(), fastScanTestRuntime{}, *claimed); result.ErrorCode != "" {
		t.Fatalf("repair worker=%+v", result)
	}
	for _, relative := range []string{"电影/冲突影片 (2024)/冲突影片 (2024).mkv", "电影/冲突影片 (2024)/冲突影片 (2024) (3).mkv"} {
		if _, err := os.Stat(filepath.Join(storage.RootPath, filepath.FromSlash(relative))); err != nil {
			t.Fatalf("version %s missing: %v", relative, err)
		}
	}
	if data, err := os.ReadFile(occupiedPath); err != nil || string(data) != "sentinel" {
		t.Fatalf("occupied version was modified: %q err=%v", data, err)
	}
}

func TestStructureSelectionRejectsIssueTokenFromAnotherLibrary(t *testing.T) {
	firstService, firstActor, firstLibrary, _ := prepareStructureSelectionConflicts(t, 1)
	page, err := firstService.StructureIssues(context.Background(), firstActor, firstLibrary.ID, MediaLibraryStructureIssueQuery{Page: 1, PageSize: 10, Actionable: true})
	if err != nil {
		t.Fatal(err)
	}
	secondLibrary := firstLibrary
	secondLibrary.ID = 0
	secondLibrary.Name = "Other library"
	secondLibrary.NameNormalized = "other-library"
	secondLibrary.StructureStatus = models.MediaLibraryStructurePending
	secondLibrary.StructureCheckedAt = nil
	if err := firstService.db.Create(&secondLibrary).Error; err != nil {
		t.Fatal(err)
	}
	if err := firstService.db.Create(&models.MediaLibraryStructureAutoState{LibraryID: secondLibrary.ID, SourceRevision: 1, Status: "pending", UpdatedAt: time.Now().UTC()}).Error; err != nil {
		t.Fatal(err)
	}
	if err := firstService.EnqueueDiagnosis(context.Background(), secondLibrary.ID, 0, secondLibrary.BaselineGeneration, "manual"); err != nil {
		t.Fatal(err)
	}
	claimed, err := firstService.queue.Claim([]string{JobTypeMediaLibraryStructureDiagnosis})
	if err != nil || claimed == nil {
		t.Fatalf("claim second diagnosis=%+v err=%v", claimed, err)
	}
	if result := NewMediaLibraryStructureDiagnosisWorker(firstService).Run(context.Background(), fastScanTestRuntime{}, *claimed); result.ErrorCode != "" {
		t.Fatalf("second diagnosis=%+v", result)
	}
	if err := firstService.queue.Complete(claimed.Job.ID, claimed.LeaseToken); err != nil {
		t.Fatal(err)
	}
	secondDiagnostics, err := firstService.Diagnostics(context.Background(), firstActor, secondLibrary.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := firstService.PreviewSelectionRepair(context.Background(), firstActor, secondLibrary.ID, MediaLibraryStructureSelectionInput{Revision: secondDiagnostics.Revision, Selections: []MediaLibraryStructureSelection{{IssueToken: page.List[0].Token, Action: StructureSelectionSkip}}}); ErrorCode(err) != CodeConflict {
		t.Fatalf("cross-library issue token was accepted: %v", err)
	}
}
