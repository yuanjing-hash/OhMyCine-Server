package services

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func structureConfirmationFixture(t *testing.T) (*MediaLibraryStructureService, Actor, models.MediaLibrary) {
	t.Helper()
	queue, actor, _ := queueFixture(t)
	actor.Permissions[authz.PermissionMediaLibrariesRead] = struct{}{}
	actor.Permissions[authz.PermissionMediaLibrariesScan] = struct{}{}
	var profile models.MediaClassificationProfile
	if err := queue.db.Where("code = ?", "default-v1").First(&profile).Error; err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	storage := models.Storage{Name: "Local", NameNormalized: "structure-local", Type: models.StorageTypeLocal, RootPath: root, RootDisplayPath: root, RootPathNormalized: root, Enabled: true, Capabilities: "{}"}
	if err := queue.db.Create(&storage).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	library := models.MediaLibrary{Name: "Structure", NameNormalized: "structure", StorageID: storage.ID, ProfileID: profile.ID, ProfileRevision: profile.Revision, RelativeRoot: "/", Enabled: true, Recursive: true, VideoExtensionsJSON: `[".mkv"]`, STRMAssetExtraExtensionsJSON: `[]`, IgnorePatternsJSON: `[]`, BaselineGeneration: 1, StructureStatus: models.MediaLibraryStructurePending, CreatedAt: now, UpdatedAt: now}
	if err := queue.db.Create(&library).Error; err != nil {
		t.Fatal(err)
	}
	return NewMediaLibraryStructureService(queue.db, queue.audit, queue, nil, zerolog.Nop()), actor, library
}

func diagnoseStructureSynchronouslyForTest(t *testing.T, service *MediaLibraryStructureService, libraryID uint) MediaLibraryStructureDiagnostics {
	t.Helper()
	plan, library, err := service.buildPlan(context.Background(), libraryID, "")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	status := models.MediaLibraryStructureHealthy
	if plan.IssueCount > 0 {
		status = models.MediaLibraryStructureIssues
	}
	return diagnosticsFromPlan(library, plan, status, nil, "manual", &now)
}

func TestStructureDiagnosisIncludesOwnedHistorical115RootItems(t *testing.T) {
	service, actor, library := structureConfirmationFixture(t)
	if err := service.db.Model(&models.MediaLibrary{}).Where("id = ?", library.ID).Update("provider_root_id", "library-root").Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	downloadJob := enqueueFake(t, service.queue, actor, "Historical 115 download", "historical-115-download")
	download := models.DownloadTask{ID: uuid.NewString(), OwnerID: actor.User.ID, JobID: downloadJob.ID, DownloaderName: "115 Offline", ProviderType: models.DownloaderTypePan115Offline, SourceCiphertext: "encrypted", DisplayName: "Historical 115 media", Phase: models.DownloadTaskStatusCompleted, TargetLibraryID: &library.ID, CreatedAt: now, UpdatedAt: now}
	if err := service.db.Create(&download).Error; err != nil {
		t.Fatal(err)
	}
	transferJob := enqueueFake(t, service.queue, actor, "Historical 115 transfer", "historical-115-transfer")
	transfer := models.TransferTask{ID: uuid.NewString(), OwnerID: actor.User.ID, JobID: transferJob.ID, DownloadTaskID: download.ID, LibraryID: library.ID, LibraryName: library.Name, ManifestJSON: `{}`, Phase: models.TransferTaskStatusCompleted, CreatedAt: now, UpdatedAt: now}
	if err := service.db.Create(&transfer).Error; err != nil {
		t.Fatal(err)
	}
	managed := models.MediaManagedItem{OpaqueID: uuid.NewString(), LibraryID: library.ID, TransferTaskID: transfer.ID, DownloadTaskID: download.ID, IdentityRevision: 1, Kind: models.MediaManagedItemKindVideo, RelativePath: "电影/动画/示例/示例.mkv", ProviderItemID: "provider-video", ProviderParentID: "0", Size: 5, Managed: true, Active: true, CreatedAt: now, UpdatedAt: now}
	if err := service.db.Create(&managed).Error; err != nil {
		t.Fatal(err)
	}
	diagnostics := diagnoseStructureSynchronouslyForTest(t, service, library.ID)
	if diagnostics.IssueCount != 1 || len(diagnostics.Issues) != 1 || diagnostics.Issues[0].Code != "cloud_transfer_root_misplaced" || !diagnostics.Issues[0].Repairable {
		t.Fatalf("diagnostics=%+v", diagnostics)
	}
	if err := service.db.Model(&models.MediaLibrary{}).Where("id = ?", library.ID).Updates(map[string]any{
		"movie_directory_template": "电影/{category}/{title}",
		"movie_filename_template":  "{title}",
	}).Error; err != nil {
		t.Fatal(err)
	}
	year, tmdbID := 2026, int64(115)
	entry := models.MediaLibraryEntry{LibraryID: library.ID, RelativePath: "/待整理/示例.mkv", ProviderID: "catalog-video", MediaType: "movie", Title: "示例", WorkKey: "movie:tmdb:115", MatchStatus: mediaRecognitionStatusMatched, TMDBID: &tmdbID, ReleaseYear: &year, CategoryName: "动画"}
	if err := service.db.Create(&entry).Error; err != nil {
		t.Fatal(err)
	}
	diagnostics = diagnoseStructureSynchronouslyForTest(t, service, library.ID)
	if diagnostics.RepairableCount != 0 || diagnostics.Classifications.DuplicateTarget != 2 || diagnostics.IssueCount != 2 {
		t.Fatalf("conflicting historical target escaped isolation: %+v", diagnostics)
	}
	for _, issue := range diagnostics.Issues {
		if issue.Repairable {
			t.Fatalf("conflicting issue remained repairable: %+v", issue)
		}
	}
	assertStructureConflictSources(t, diagnostics.Issues, 2)
	if err := service.EnqueueDiagnosis(context.Background(), library.ID, 0, library.BaselineGeneration, "manual"); err != nil {
		t.Fatal(err)
	}
	claimed, err := service.queue.Claim([]string{JobTypeMediaLibraryStructureDiagnosis})
	if err != nil || claimed == nil {
		t.Fatalf("claim=%+v err=%v", claimed, err)
	}
	if result := NewMediaLibraryStructureDiagnosisWorker(service).Run(context.Background(), fastScanTestRuntime{}, *claimed); result.ErrorCode != "" {
		t.Fatalf("worker=%+v", result)
	}
	if err := service.queue.Complete(claimed.Job.ID, claimed.LeaseToken); err != nil {
		t.Fatal(err)
	}
	diagnostics, err = service.Diagnostics(context.Background(), actor, library.ID)
	if err != nil || diagnostics.RepairableCount != 0 || diagnostics.Classifications.DuplicateTarget != 1 || diagnostics.CheckedAt == nil {
		t.Fatalf("persisted async diagnostics=%+v err=%v", diagnostics, err)
	}
	assertStructureConflictSources(t, diagnostics.Issues, 2)
}

func assertStructureConflictSources(t *testing.T, issues []StructureIssue, want int) {
	t.Helper()
	found := false
	for _, issue := range issues {
		if issue.Code != "duplicate_target" {
			continue
		}
		found = true
		if issue.ConflictSourceCount != want || len(issue.ConflictSources) != want {
			t.Fatalf("conflict sources did not survive projection: %+v", issue)
		}
	}
	if !found {
		t.Fatal("duplicate target issue missing")
	}
}

func TestHistoricalRootPlanRebuildRetainsTitlesBeyondIssueSample(t *testing.T) {
	service, _, library := structureConfirmationFixture(t)
	if err := service.db.Model(&models.MediaLibrary{}).Where("id = ?", library.ID).Updates(map[string]any{
		"provider_root_id":         "library-root",
		"movie_directory_template": "电影/{category}/{title} ({year})",
		"movie_filename_template":  "{title} ({year})",
	}).Error; err != nil {
		t.Fatal(err)
	}
	year := 2026
	entries := make([]models.MediaLibraryEntry, 0, maxStructureIssueSamples+1)
	for index := 0; index < maxStructureIssueSamples; index++ {
		title := fmt.Sprintf("浅层影片 %03d", index)
		tmdbID := int64(index + 1)
		entries = append(entries, models.MediaLibraryEntry{LibraryID: library.ID, RelativePath: fmt.Sprintf("/a/%03d.mkv", index), ProviderID: fmt.Sprintf("shallow-%03d", index), MediaType: "movie", Title: title, WorkKey: fmt.Sprintf("movie:tmdb:%d", tmdbID), MatchStatus: mediaRecognitionStatusMatched, TMDBID: &tmdbID, ReleaseYear: &year, CategoryName: "剧情"})
	}
	deepTMDBID := int64(9999)
	entries = append(entries, models.MediaLibraryEntry{LibraryID: library.ID, RelativePath: "/z/更深/机械之声.mkv", ProviderID: "deep-title", MediaType: "movie", Title: "机械之声的传奇", WorkKey: "movie:tmdb:9999", MatchStatus: mediaRecognitionStatusMatched, TMDBID: &deepTMDBID, ReleaseYear: &year, CategoryName: "动画"})
	if err := service.db.CreateInBatches(entries, 50).Error; err != nil {
		t.Fatal(err)
	}
	plan, _, err := service.buildPlan(context.Background(), library.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, issue := range plan.Issues {
		if issue.CurrentPath != "z/更深/机械之声.mkv" {
			continue
		}
		found = true
		if issue.Title != "机械之声的传奇" {
			t.Fatalf("title was lost while rebuilding the full plan: %+v", issue)
		}
	}
	if !found {
		t.Fatalf("deep rebuilt issue missing from bounded sample: %+v", plan.Issues)
	}
}

func TestMediaLibraryStructureConfirmationRejectsTamperAndBoundaryChanges(t *testing.T) {
	service, actor, library := structureConfirmationFixture(t)
	diagnostics := diagnoseStructureSynchronouslyForTest(t, service, library.ID)
	preview, err := service.PreviewRepair(context.Background(), actor, library.ID, "", diagnostics.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.EnqueueConfirmedRepair(context.Background(), actor, library.ID, "", preview.ConfirmationToken+"x", RequestContext{}); ErrorCode(err) != CodeInvalidRequest {
		t.Fatalf("tampered token err=%v", err)
	}
	if err := service.db.Model(&models.MediaLibrary{}).Where("id = ?", library.ID).Update("baseline_generation", 2).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := service.EnqueueConfirmedRepair(context.Background(), actor, library.ID, "", preview.ConfirmationToken, RequestContext{}); ErrorCode(err) != CodeConflict {
		t.Fatalf("changed boundary err=%v", err)
	}
}

func TestMediaLibraryStructureResourceDenyBlocksServiceBypass(t *testing.T) {
	service, actor, library := structureConfirmationFixture(t)
	diagnostics := diagnoseStructureSynchronouslyForTest(t, service, library.ID)
	actor.ResourceRules = append(actor.ResourceRules, AuthorizationRule{PermissionCode: authz.PermissionMediaLibrariesScan, Effect: models.AuthorizationEffectDeny, ResourceType: models.AuthorizationResourceMediaLibrary, ResourceID: uintID(library.ID)})
	if _, err := service.PreviewRepair(context.Background(), actor, library.ID, "", diagnostics.Revision); ErrorCode(err) != CodePermissionDenied {
		t.Fatalf("scoped deny bypassed: %v", err)
	}
}
