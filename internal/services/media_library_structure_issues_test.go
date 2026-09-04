package services

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

func TestStructureIssuesPersistAndPageBeyondLegacySampleLimit(t *testing.T) {
	service, actor, library := structureConfirmationFixture(t)
	issues := make([]StructureIssue, 0, 227)
	for index := 1; index <= 225; index++ {
		issues = append(issues, StructureIssue{
			Code:         "path_mismatch",
			Kind:         "video",
			Title:        fmt.Sprintf("作品 %03d", index),
			CurrentPath:  fmt.Sprintf("待整理/作品 %03d.mkv", index),
			ExpectedPath: fmt.Sprintf("电影/作品 %03d/作品 %03d.mkv", index, index),
			Repairable:   true,
		})
	}
	issues = append(issues,
		StructureIssue{
			Code:                "duplicate_target",
			Kind:                "video",
			Title:               "冲突作品",
			CurrentPath:         "冲突/电影.mkv",
			ExpectedPath:        "电影/冲突作品/冲突作品.mkv",
			ConflictSourceCount: 3,
			AllConflictSources:  []string{"冲突/电影.mkv", "冲突/电影 (1).mkv", "冲突/电影 copy.mkv"},
		},
		StructureIssue{Code: "missing_season_episode", Kind: "video", CurrentPath: "剧集/未知.mkv"},
	)
	now := time.Now().UTC()
	if err := service.db.Transaction(func(tx *gorm.DB) error {
		return insertStructureIssuesTx(tx, library.ID, "diagnosis-job", library.BaselineGeneration, issues, now, "")
	}); err != nil {
		t.Fatal(err)
	}

	first, err := service.StructureIssues(context.Background(), actor, library.ID, MediaLibraryStructureIssueQuery{Page: 1, PageSize: 100, Actionable: true})
	if err != nil {
		t.Fatal(err)
	}
	if first.Total != 226 || len(first.List) != 100 || first.Page != 1 || first.PageSize != 100 {
		t.Fatalf("first page=%+v", first)
	}
	third, err := service.StructureIssues(context.Background(), actor, library.ID, MediaLibraryStructureIssueQuery{Page: 3, PageSize: 100, Actionable: true})
	if err != nil {
		t.Fatal(err)
	}
	if third.Total != 226 || len(third.List) != 26 {
		t.Fatalf("third page total=%d list=%d", third.Total, len(third.List))
	}

	conflicts, err := service.StructureIssues(context.Background(), actor, library.ID, MediaLibraryStructureIssueQuery{Page: 1, PageSize: 50, Code: "duplicate_target", Actionable: true})
	if err != nil {
		t.Fatal(err)
	}
	if conflicts.Total != 1 || len(conflicts.List) != 1 || len(conflicts.List[0].Members) != 3 {
		t.Fatalf("conflicts=%+v", conflicts)
	}
	recommended := 0
	for _, member := range conflicts.List[0].Members {
		if member.Recommended {
			recommended++
			if member.Token != conflicts.List[0].RecommendedMemberToken || member.SourcePath != "冲突/电影.mkv" {
				t.Fatalf("unexpected recommendation: issue=%+v member=%+v", conflicts.List[0], member)
			}
		}
	}
	if recommended != 1 {
		t.Fatalf("recommended members=%d", recommended)
	}
}

func TestStructureIssuesDefensivelyRedactsInvalidPersistedPaths(t *testing.T) {
	service, actor, library := structureConfirmationFixture(t)
	now := time.Now().UTC()
	issue := models.MediaLibraryStructureIssue{Token: "private-path-issue", LibraryID: library.ID, DiagnosisJobID: "diagnosis", Generation: 1, Code: "duplicate_target", Kind: "video", State: "needs_attention", CurrentPath: `C:\\Users\\owner\\movie.mkv`, ExpectedPath: `../outside/movie.mkv`, ConflictSourceCount: 1, RecommendedMemberToken: "private-path-member", CreatedAt: now, UpdatedAt: now}
	if err := service.db.Create(&issue).Error; err != nil {
		t.Fatal(err)
	}
	if err := service.db.Create(&models.MediaLibraryStructureIssueMember{IssueID: issue.ID, Token: "private-path-member", SourcePath: `C:\\Users\\owner\\movie.mkv`, Recommended: true, CreatedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	page, err := service.StructureIssues(context.Background(), actor, library.ID, MediaLibraryStructureIssueQuery{Page: 1, PageSize: 10, Actionable: true})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, forbidden := range []string{"Users", "owner", "outside"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("private path leaked through issue DTO: %s", text)
		}
	}
	if len(page.List) != 1 || page.List[0].CurrentPath != "" || page.List[0].ExpectedPath != "" || len(page.List[0].Members) != 0 || page.List[0].RecommendedMemberToken != "" {
		t.Fatalf("invalid paths were not redacted: %+v", page.List)
	}
}

func TestRefreshRecognitionProjectionPersistsIdentityWithoutMovingFilesOrEnqueueingDiagnosis(t *testing.T) {
	service, actor, library := structureConfirmationFixture(t)
	now := time.Now().UTC()
	year, tmdbID, confidence := 2005, int64(12345), 1.0
	recognition := models.MediaLibraryRecognition{
		LibraryID: library.ID, SourceKey: "manual-identity", InputFingerprint: "manual-identity-fingerprint",
		ProfileID: library.ProfileID, ProfileRevision: library.ProfileRevision, Status: mediaRecognitionStatusMatched,
		MediaType: "tv", Title: "哆啦A梦", ReleaseYear: &year, TMDBID: &tmdbID, Confidence: &confidence,
		ManualOverride: true, MetadataJSON: `{"version":1,"snapshot":{"poster_path":"/poster.jpg"},"classification":{}}`,
		LastGeneration: library.BaselineGeneration, CreatedAt: now, UpdatedAt: now,
	}
	if err := service.db.Create(&recognition).Error; err != nil {
		t.Fatal(err)
	}
	entry := models.MediaLibraryEntry{
		LibraryID: library.ID, RecognitionID: &recognition.ID, RelativePath: "/哆啦A梦 (2005)/Season 01/01.mkv",
		MediaType: "tv", Title: "哆啦A梦", WorkKey: "tv:tmdb:12345", MatchStatus: mediaRecognitionStatusMatched,
		TMDBID: &tmdbID, ReleaseYear: &year, Season: intPointer(1), Episode: intPointer(1), LastGeneration: library.BaselineGeneration,
		ModifiedAt: now, CreatedAt: now, UpdatedAt: now,
	}
	if err := service.db.Create(&entry).Error; err != nil {
		t.Fatal(err)
	}
	var storage models.Storage
	if err := service.db.First(&storage, library.StorageID).Error; err != nil {
		t.Fatal(err)
	}
	physical := filepath.Join(storage.RootPath, "哆啦A梦 (2005)", "Season 01", "01.mkv")
	if err := os.MkdirAll(filepath.Dir(physical), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(physical, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	job := enqueueFake(t, service.queue, actor, "Manual projection", "manual-projection")
	diagnosis := models.MediaLibraryStructureDiagnosis{LibraryID: library.ID, JobID: job.ID, Generation: library.BaselineGeneration, ScanKind: "manual", Status: models.MediaLibraryStructureIssues, IssuesJSON: "[]", CreatedAt: now, UpdatedAt: now}
	if err := service.db.Create(&diagnosis).Error; err != nil {
		t.Fatal(err)
	}
	oldIssue := models.MediaLibraryStructureIssue{Token: "old-issue", LibraryID: library.ID, DiagnosisJobID: diagnosis.JobID, Generation: library.BaselineGeneration, Code: "media_unrecognized", Kind: "video", State: "unrecognized", CurrentPath: "哆啦A梦 (2005)/Season 01/01.mkv", RecognitionID: &recognition.ID, CreatedAt: now, UpdatedAt: now}
	if err := service.db.Create(&oldIssue).Error; err != nil {
		t.Fatal(err)
	}
	if err := service.db.Create(&models.MediaLibraryStructureIssueMember{IssueID: oldIssue.ID, Token: "old-member", SourcePath: oldIssue.CurrentPath, CreatedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	var jobsBefore int64
	if err := service.db.Model(&models.Job{}).Count(&jobsBefore).Error; err != nil {
		t.Fatal(err)
	}
	if err := service.RefreshRecognitionProjection(context.Background(), library.ID, recognition.ID); err != nil {
		t.Fatal(err)
	}
	var projected []models.MediaLibraryStructureIssue
	if err := service.db.Where("library_id = ? AND recognition_id = ?", library.ID, recognition.ID).Find(&projected).Error; err != nil {
		t.Fatal(err)
	}
	if len(projected) == 0 {
		t.Fatal("manual identity projection was not persisted")
	}
	for _, issue := range projected {
		if issue.State != "manual_identity_resolved" || issue.Title != "哆啦A梦" || issue.Token == oldIssue.Token {
			t.Fatalf("projected issue=%+v", issue)
		}
	}
	var jobsAfter int64
	if err := service.db.Model(&models.Job{}).Count(&jobsAfter).Error; err != nil {
		t.Fatal(err)
	}
	if jobsAfter != jobsBefore {
		t.Fatalf("targeted projection enqueued diagnosis: before=%d after=%d", jobsBefore, jobsAfter)
	}
	content, err := os.ReadFile(physical)
	if err != nil || string(content) != "unchanged" {
		t.Fatalf("manual identity changed file before repair: content=%q err=%v", content, err)
	}
}

func TestRefreshRecognitionProjectionKeepsCrossWorkTargetConflict(t *testing.T) {
	service, actor, library, _ := prepareStructureSelectionConflicts(t, 1)
	now := time.Now().UTC()
	year, tmdbID, confidence := 2024, int64(9000), 1.0
	recognition := models.MediaLibraryRecognition{LibraryID: library.ID, SourceKey: "manual-conflict", InputFingerprint: "manual-conflict", ProfileID: library.ProfileID, ProfileRevision: library.ProfileRevision, Status: mediaRecognitionStatusMatched, MediaType: "movie", Title: "冲突影片", ReleaseYear: &year, TMDBID: &tmdbID, Confidence: &confidence, ManualOverride: true, LastGeneration: library.BaselineGeneration, CreatedAt: now, UpdatedAt: now}
	if err := service.db.Create(&recognition).Error; err != nil {
		t.Fatal(err)
	}
	if err := service.db.Model(&models.MediaLibraryEntry{}).Where("library_id = ? AND provider_id = ?", library.ID, "primary-冲突影片").Update("recognition_id", recognition.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := service.RefreshRecognitionProjection(context.Background(), library.ID, recognition.ID); err != nil {
		t.Fatal(err)
	}
	page, err := service.StructureIssues(context.Background(), actor, library.ID, MediaLibraryStructureIssueQuery{Page: 1, PageSize: 10, Code: "duplicate_target", Actionable: true})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || len(page.List) != 1 || len(page.List[0].Members) != 2 || page.List[0].State != "manual_identity_resolved" {
		t.Fatalf("manual projection lost the authoritative conflict: %+v", page)
	}
}

func intPointer(value int) *int { return &value }
