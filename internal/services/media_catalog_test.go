package services

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/database"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

func createCatalogTestLibrary(t *testing.T) (*MediaLibraryService, models.MediaLibrary, Actor) {
	t.Helper()
	service, _, actor, storage, profile := mediaLibraryTestService(t)
	detail, err := service.Create(context.Background(), actor, testLibraryInput("Catalog library", storage, profile, false), RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	return service, detail.MediaLibrary, actor
}

func TestMediaCatalogRecentOrderUsesIngestionTimeBeforePaging(t *testing.T) {
	for _, mode := range []string{"legacy", "versioned"} {
		t.Run(mode, func(t *testing.T) {
			service, library, actor := createCatalogTestLibrary(t)
			if err := service.db.Model(&library).Update("enabled", true).Error; err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
			rec := models.MediaLibraryRecognition{LibraryID: library.ID, SourceKey: "recent-fixture", ProfileID: library.ProfileID, ProfileRevision: library.ProfileRevision, Status: "unrecognized", MediaType: "movie", Title: "Fixture", MetadataJSON: "{}"}
			if err := service.db.Create(&rec).Error; err != nil {
				t.Fatal(err)
			}
			entries := make([]models.MediaLibraryEntry, 0, 34)
			for index := 0; index < 32; index++ {
				entries = append(entries, models.MediaLibraryEntry{LibraryID: library.ID, RecognitionID: &rec.ID,
					RelativePath: fmt.Sprintf("/Movie%02d.mkv", index), ProviderID: fmt.Sprintf("movie-%02d", index), WorkKey: fmt.Sprintf("movie:%03d", index),
					MediaType: "movie", Title: fmt.Sprintf("A movie %02d", index), MatchStatus: "unrecognized", Size: 1,
					ModifiedAt: now.Add(time.Duration(100-index) * time.Hour), CreatedAt: now.Add(time.Duration(index) * time.Minute), UpdatedAt: now})
			}
			// The newest movie is outside the title-ordered first page and has an
			// older source mtime. A tie must use the same work key in SQL and merge.
			entries[31].Title, entries[31].ModifiedAt = "Z newest movie", now.Add(-24*time.Hour)
			entries[30].CreatedAt = entries[31].CreatedAt
			season, firstEpisode, secondEpisode := 1, 1, 2
			entries = append(entries,
				models.MediaLibraryEntry{LibraryID: library.ID, RecognitionID: &rec.ID, RelativePath: "/Series/S01E01.mkv", ProviderID: "episode-1", WorkKey: "series:recent", MediaType: "tv", Title: "Z series", SeriesTitle: "Z series", Season: &season, Episode: &firstEpisode, MatchStatus: "unrecognized", Size: 1, ModifiedAt: now.Add(-48 * time.Hour), CreatedAt: now.Add(-24 * time.Hour), UpdatedAt: now},
				models.MediaLibraryEntry{LibraryID: library.ID, RecognitionID: &rec.ID, RelativePath: "/Series/S01E02.mkv", ProviderID: "episode-2", WorkKey: "series:recent", MediaType: "tv", Title: "Z series", SeriesTitle: "Z series", Season: &season, Episode: &secondEpisode, MatchStatus: "unrecognized", Size: 1, ModifiedAt: now.Add(-48 * time.Hour), CreatedAt: now.Add(time.Hour), UpdatedAt: now})
			if err := service.db.Create(&entries).Error; err != nil {
				t.Fatal(err)
			}
			if mode == "versioned" {
				var location struct{ File string }
				if err := service.db.Raw("PRAGMA database_list").Scan(&location).Error; err != nil {
					t.Fatal(err)
				}
				readDB, err := database.OpenReadOnly(location.File)
				if err != nil {
					t.Fatal(err)
				}
				pool, _ := readDB.DB()
				t.Cleanup(func() { _ = pool.Close() })
				store := NewCatalogSnapshotStore(service.db, readDB)
				service.SetCatalogSnapshotStore(store)
				if err := service.db.Transaction(func(tx *gorm.DB) error {
					return StartCatalogConversionTx(tx, models.CatalogHead{LibraryID: library.ID, SourceEpoch: 1, SourceFingerprint: "source-a", ConfigFingerprint: "config-a"}, func(*gorm.DB) error { return nil })
				}); err != nil {
					t.Fatal(err)
				}
				catalogConvert(t, store, library, rec, entries)
				candidate, token := catalogCandidate(t, store, library, "delta", 1)
				hidden := entries[0]
				hidden.CreatedAt = now.Add(24 * time.Hour)
				if err := store.AppendBatch(context.Background(), candidate.ID, token, CatalogFactBatch{Entries: []models.CatalogEntryFact{CatalogEntryFromLegacy(hidden, &rec)}}); err != nil {
					t.Fatal(err)
				}
			}
			ordinary, err := service.PlayerCatalog(actor, library.ID, MediaPageQuery{Page: 1, PageSize: 20})
			if err != nil || len(ordinary.List) != 20 || ordinary.List[0].Title != "A movie 00" {
				t.Fatalf("ordinary order changed: %+v err=%v", ordinary, err)
			}
			query := MediaPageQuery{Page: 1, PageSize: playerOverviewRecentlyAddedLimit, order: mediaCatalogOrderAdded}
			recent, err := service.PlayerCatalog(actor, library.ID, query)
			if err != nil || recent.Total != 33 || len(recent.List) != playerOverviewRecentlyAddedLimit {
				t.Fatalf("recent page=%+v err=%v", recent, err)
			}
			if recent.List[0].ID != encodeCatalogToken("series:recent") || !recent.List[0].AddedAt.Equal(entries[33].CreatedAt) || recent.List[0].FileCount != 2 || recent.List[1].ID != encodeCatalogToken("movie:030") || recent.List[2].ID != encodeCatalogToken("movie:031") {
				t.Fatalf("recent ingestion/episode/tie order=%+v", recent.List[:3])
			}
			if !recent.List[2].ModifiedAt.Equal(entries[31].ModifiedAt) || !browserMediaItem(recent.List[2]).AddedAt.Equal(entries[31].CreatedAt) {
				t.Fatalf("timestamp projection=%+v", recent.List[2])
			}
			query.Page = 2
			second, err := service.PlayerCatalog(actor, library.ID, query)
			if err != nil || len(second.List) != 9 || !playerOverviewCatalogItemBefore(recent.List[23], second.List[0]) {
				t.Fatalf("recent pagination=%+v err=%v", second, err)
			}
			if _, err := service.PlayerCatalog(actor, library.ID, MediaPageQuery{Page: 1, PageSize: playerOverviewRecentlyAddedLimit}); ErrorCode(err) != CodeInvalidRequest {
				t.Fatalf("ordinary public page size changed: %v", err)
			}
			if _, err := service.ExcludeCatalogWork(context.Background(), actor, library.ID, encodeCatalogToken("series:recent"), RequestContext{}); err != nil {
				t.Fatal(err)
			}
			query.Page = 1
			recent, err = service.PlayerCatalog(actor, library.ID, query)
			if err != nil || recent.Total != 32 || recent.List[0].ID != encodeCatalogToken("movie:030") {
				t.Fatalf("recent exclusion=%+v err=%v", recent, err)
			}
			denied := actor
			denied.ResourceRules = append(append([]AuthorizationRule{}, actor.ResourceRules...), AuthorizationRule{PermissionCode: authz.PermissionMediaLibrariesRead, Effect: models.AuthorizationEffectDeny, ResourceType: models.AuthorizationResourceMediaLibrary, ResourceID: uintID(library.ID)})
			if _, err := service.PlayerCatalog(denied, library.ID, query); ErrorCode(err) != CodePermissionDenied {
				t.Fatalf("recent authorization=%v", err)
			}
		})
	}
}

func TestAggregateMediaCatalogMergesOnlyTrustedTMDBIdentityBeforePaging(t *testing.T) {
	service, library, actor := createCatalogTestLibrary(t)
	var storage models.Storage
	if err := service.db.First(&storage, library.StorageID).Error; err != nil {
		t.Fatal(err)
	}
	var profile models.MediaClassificationProfile
	if err := service.db.First(&profile, library.ProfileID).Error; err != nil {
		t.Fatal(err)
	}
	secondRoot := t.TempDir()
	secondStorage := models.Storage{Name: "Second storage", NameNormalized: "second storage", Type: models.StorageTypeLocal, RootPath: secondRoot, RootPathNormalized: strings.ToLower(secondRoot), Enabled: true, Capabilities: `{}`}
	if err := service.db.Create(&secondStorage).Error; err != nil {
		t.Fatal(err)
	}
	second, err := service.Create(context.Background(), actor, testLibraryInput("Second catalog", secondStorage, profile, false), RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.db.Model(&models.MediaLibrary{}).Where("id IN ?", []uint{library.ID, second.ID}).Update("enabled", true).Error; err != nil {
		t.Fatal(err)
	}
	now, tmdbID := time.Now().UTC(), int64(42)
	entries := []models.MediaLibraryEntry{
		{LibraryID: library.ID, RelativePath: "/A.mkv", ProviderID: "a", Size: 10, ModifiedAt: now, MediaType: "movie", Title: "Same", WorkKey: "movie:first", MatchStatus: "matched", TMDBID: &tmdbID, CategoryName: "电影"},
		{LibraryID: second.ID, RelativePath: "/B.mkv", ProviderID: "b", Size: 20, ModifiedAt: now, MediaType: "movie", Title: "Same localized", WorkKey: "movie:second", MatchStatus: "matched", TMDBID: &tmdbID, CategoryName: "电影"},
		{LibraryID: second.ID, RelativePath: "/Same.mkv", ProviderID: "c", Size: 30, ModifiedAt: now, MediaType: "movie", Title: "Same", WorkKey: "movie:unmatched", MatchStatus: "unrecognized", CategoryName: "电影"},
		{LibraryID: library.ID, RelativePath: "/Stale-A.mkv", ProviderID: "stale-a", Size: 40, ModifiedAt: now, MediaType: "movie", Title: "Stale A", WorkKey: "movie:stale-a", MatchStatus: "unrecognized", TMDBID: &tmdbID, CategoryName: "纪录片"},
		{LibraryID: second.ID, RelativePath: "/Stale-B.mkv", ProviderID: "stale-b", Size: 50, ModifiedAt: now, MediaType: "movie", Title: "Stale B", WorkKey: "movie:stale-b", MatchStatus: "unrecognized", TMDBID: &tmdbID, CategoryName: "纪录片"},
	}
	if err := service.db.Create(&entries).Error; err != nil {
		t.Fatal(err)
	}
	page, err := service.AggregateCatalog(actor, MediaPageQuery{Page: 1, PageSize: 20})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 4 || len(page.List) != 4 || !slices.Equal(page.Categories, []string{"电影", "纪录片"}) {
		t.Fatalf("aggregate=%+v", page)
	}
	var merged *MediaCatalogItem
	for index := range page.List {
		if page.List[index].TMDBID != nil && page.List[index].MatchStatus == mediaRecognitionStatusMatched {
			merged = &page.List[index]
		}
	}
	if merged == nil || len(merged.LibraryWorks) != 2 || merged.FileCount != 2 || merged.Size != 30 {
		t.Fatalf("merged=%+v", merged)
	}
}

func TestMediaCatalogLocalDeletionPreviewConfirmAndReplay(t *testing.T) {
	service, library, actor := createCatalogTestLibrary(t)
	var storage models.Storage
	if err := service.db.First(&storage, library.StorageID).Error; err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(storage.RootPath, "Delete.mkv")
	if err := os.WriteFile(path, []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	entry := models.MediaLibraryEntry{LibraryID: library.ID, RelativePath: "/Delete.mkv", ProviderID: "Delete.mkv", Size: 7, ModifiedAt: now, MediaType: "movie", Title: "Delete", WorkKey: "movie:delete", MatchStatus: "unrecognized", CategoryName: "电影"}
	if err := service.db.Create(&entry).Error; err != nil {
		t.Fatal(err)
	}
	run := models.MediaArtifactRun{ID: "catalog-delete-artifact-run", LibraryID: library.ID, Generation: 1, Status: models.MediaArtifactStatusCompleted, CreatedAt: now, UpdatedAt: now}
	if err := service.db.Create(&run).Error; err != nil {
		t.Fatal(err)
	}
	artifact := models.MediaArtifact{OpaqueID: "catalog-delete-strm", RunID: run.ID, LibraryID: library.ID, SourceIdentity: fmt.Sprintf("entry:%d", entry.ID), Kind: models.MediaArtifactKindSTRM, TargetKind: models.MediaArtifactTargetLocalProjection, RelativePath: "/Delete.strm", Managed: true, Active: true, Status: models.MediaArtifactStatusCompleted, CreatedAt: now, UpdatedAt: now}
	if err := service.db.Create(&artifact).Error; err != nil {
		t.Fatal(err)
	}
	token := encodeCatalogToken(entry.WorkKey)
	if _, err := service.PreviewCatalogDeletion(context.Background(), actor, library.ID, token, RequestContext{}); ErrorCode(err) != CodePermissionDenied {
		t.Fatalf("preview without media-delete permission err=%v", err)
	}
	actor.Permissions[authz.PermissionMediaLibrariesMediaDelete] = struct{}{}
	preview, err := service.PreviewCatalogDeletion(context.Background(), actor, library.ID, token, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if preview.FileCount != 1 || preview.Title != "Delete" || preview.ConfirmationToken == "" || len(preview.RelativePaths) != 1 || preview.STRMImpactCount != 1 {
		t.Fatalf("preview=%+v", preview)
	}
	result, err := service.ConfirmCatalogDeletion(context.Background(), actor, library.ID, token, preview.ConfirmationToken, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Deleted || result.RemovedFiles != 1 {
		t.Fatalf("result=%+v", result)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file still exists err=%v", err)
	}
	var count int64
	if err := service.db.Model(&models.MediaLibraryEntry{}).Where("id = ?", entry.ID).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("entry count=%d err=%v", count, err)
	}
	if _, err := service.ConfirmCatalogDeletion(context.Background(), actor, library.ID, token, preview.ConfirmationToken, RequestContext{}); ErrorCode(err) != CodeMediaCatalogDeletionExpired {
		t.Fatalf("replay err=%v", err)
	}
}

func TestMediaCatalogLocalDeletionRejectsSymlinkedAncestor(t *testing.T) {
	service, library, actor := createCatalogTestLibrary(t)
	actor.Permissions[authz.PermissionMediaLibrariesMediaDelete] = struct{}{}
	var storage models.Storage
	if err := service.db.First(&storage, library.StorageID).Error; err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "outside.mkv")
	if err := os.WriteFile(outsideFile, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(storage.RootPath, "Linked")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink boundary test unavailable: %v", err)
	}
	now := time.Now().UTC()
	entry := models.MediaLibraryEntry{LibraryID: library.ID, RelativePath: "/Linked/outside.mkv", ProviderID: "outside", Size: 7, ModifiedAt: now, MediaType: "movie", Title: "Outside", WorkKey: "movie:outside", MatchStatus: "unrecognized", CategoryName: "电影", CreatedAt: now, UpdatedAt: now}
	if err := service.db.Create(&entry).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := service.PreviewCatalogDeletion(context.Background(), actor, library.ID, encodeCatalogToken(entry.WorkKey), RequestContext{}); ErrorCode(err) != CodeMediaCatalogDeletionChanged {
		t.Fatalf("symlinked ancestor accepted: %v", err)
	}
	if _, err := os.Stat(outsideFile); err != nil {
		t.Fatalf("outside file changed: %v", err)
	}
}

func TestMediaLibraryEntryPageReturnsDatabaseTotalAndEmptyOutOfRangePage(t *testing.T) {
	service, library, actor := createCatalogTestLibrary(t)
	now := time.Now().UTC()
	entries := make([]models.MediaLibraryEntry, 0, 12099)
	for index := 0; index < 12099; index++ {
		entries = append(entries, models.MediaLibraryEntry{
			LibraryID: library.ID, RelativePath: fmt.Sprintf("/movie/%05d.mkv", index), ProviderID: fmt.Sprintf("p-%d", index),
			Size: int64(index), ModifiedAt: now, MediaType: "movie", Title: fmt.Sprintf("Movie %05d", index),
			WorkKey: fmt.Sprintf("movie:%032x", index), MatchStatus: "unmatched", CategoryName: "电影", LastGeneration: 1,
			CreatedAt: now, UpdatedAt: now,
		})
	}
	if err := service.db.CreateInBatches(entries, 250).Error; err != nil {
		t.Fatal(err)
	}

	page, err := service.EntryPage(actor, library.ID, MediaPageQuery{Page: 2, PageSize: 50})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 12099 || len(page.List) != 50 || page.Page != 2 || page.List[0].RelativePath != "/movie/00050.mkv" {
		t.Fatalf("entry page=%+v first=%+v", page, page.List[0])
	}
	outOfRange, err := service.EntryPage(actor, library.ID, MediaPageQuery{Page: 999, PageSize: 50})
	if err != nil || outOfRange.Total != 12099 || len(outOfRange.List) != 0 {
		t.Fatalf("out-of-range page=%+v err=%v", outOfRange, err)
	}
}

func TestMediaCatalogGroupsSeriesBeforePagingAndBuildsSeasonDetail(t *testing.T) {
	service, library, actor := createCatalogTestLibrary(t)
	actor.Permissions[authz.PermissionJobsControlOwn] = struct{}{}
	now := time.Now().UTC()
	season1, season2, episode1, episode2 := 1, 2, 1, 2
	entries := []models.MediaLibraryEntry{
		{LibraryID: library.ID, RelativePath: "/莉可丽丝/Season 02/E02.mkv", ProviderID: "s2e2", Size: 22, ModifiedAt: now, MediaType: "tv", Title: "莉可丽丝", SeriesTitle: "莉可丽丝", WorkKey: "series:lycoris", Season: &season2, Episode: &episode2, MatchStatus: "matched", CategoryName: "动画", LastGeneration: 1},
		{LibraryID: library.ID, RelativePath: "/莉可丽丝/Season 01/E02.mkv", ProviderID: "s1e2", Size: 12, ModifiedAt: now, MediaType: "tv", Title: "莉可丽丝", SeriesTitle: "莉可丽丝", WorkKey: "series:lycoris", Season: &season1, Episode: &episode2, MatchStatus: "matched", CategoryName: "动画", LastGeneration: 1},
		{LibraryID: library.ID, RelativePath: "/莉可丽丝/Season 01/E01.mkv", ProviderID: "s1e1", Size: 11, ModifiedAt: now, MediaType: "tv", Title: "莉可丽丝", SeriesTitle: "莉可丽丝", WorkKey: "series:lycoris", Season: &season1, Episode: &episode1, MatchStatus: "matched", CategoryName: "动画", LastGeneration: 1},
		{LibraryID: library.ID, RelativePath: "/Movie.mkv", ProviderID: "movie", Size: 50, ModifiedAt: now, MediaType: "movie", Title: "Movie", WorkKey: "movie:movie", MatchStatus: "unmatched", CategoryName: "电影", LastGeneration: 1},
	}
	if err := service.db.Create(&entries).Error; err != nil {
		t.Fatal(err)
	}

	page, err := service.Catalog(actor, library.ID, MediaPageQuery{Page: 1, PageSize: 20})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 2 || len(page.List) != 2 {
		t.Fatalf("catalog page=%+v", page)
	}
	seriesPage, err := service.Catalog(actor, library.ID, MediaPageQuery{Page: 1, PageSize: 20, Query: "莉可", MediaType: "series"})
	if err != nil || seriesPage.Total != 1 || len(seriesPage.List) != 1 || seriesPage.List[0].FileCount != 3 || seriesPage.List[0].SeasonCount != 2 || seriesPage.List[0].ModifiedAt.IsZero() {
		t.Fatalf("series page=%+v err=%v", seriesPage, err)
	}
	detail, err := service.CatalogDetail(actor, library.ID, seriesPage.List[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Seasons) != 2 || detail.Seasons[0].Number != 1 || len(detail.Seasons[0].Episodes) != 2 || *detail.Seasons[0].Episodes[0].Episode != 1 || detail.Seasons[1].Number != 2 {
		t.Fatalf("catalog detail=%+v", detail)
	}
	ownerID := actor.User.ID
	jobs := []models.Job{
		{ID: "catalog-download-job", OwnerID: &ownerID, JobType: "download", Status: models.JobStatusCompleted, DisplayName: "catalog download"},
		{ID: "catalog-managed-job", OwnerID: &ownerID, JobType: "transfer", Status: models.JobStatusCompleted, DisplayName: "catalog transfer"},
	}
	if err := service.db.Create(&jobs).Error; err != nil {
		t.Fatal(err)
	}
	download := models.DownloadTask{ID: "catalog-managed-download", OwnerID: ownerID, JobID: jobs[0].ID, DownloaderName: "fixture", ProviderType: "fixture", SourceCiphertext: "fixture", DisplayName: "catalog download", Phase: models.DownloadTaskStatusCompleted, IdentityRevision: 4}
	if err := service.db.Create(&download).Error; err != nil {
		t.Fatal(err)
	}
	transfer := models.TransferTask{ID: "catalog-managed-transfer", OwnerID: ownerID, JobID: jobs[1].ID, DownloadTaskID: download.ID, LibraryID: library.ID, LibraryName: library.Name, ManifestJSON: "{}", Phase: models.TransferTaskStatusCompleted}
	if err := service.db.Create(&transfer).Error; err != nil {
		t.Fatal(err)
	}
	managed := []models.MediaManagedItem{
		{OpaqueID: "catalog-managed-one", LibraryID: library.ID, TransferTaskID: transfer.ID, DownloadTaskID: transfer.DownloadTaskID, IdentityRevision: 4, Kind: models.MediaManagedItemKindVideo, RelativePath: entries[1].RelativePath, Size: entries[1].Size, Managed: true, Active: true},
		{OpaqueID: "catalog-managed-two", LibraryID: library.ID, TransferTaskID: transfer.ID, DownloadTaskID: transfer.DownloadTaskID, IdentityRevision: 4, Kind: models.MediaManagedItemKindVideo, RelativePath: entries[2].RelativePath, Size: entries[2].Size, Managed: true, Active: true},
		{OpaqueID: "catalog-unmanaged", LibraryID: library.ID, TransferTaskID: transfer.ID, DownloadTaskID: transfer.DownloadTaskID, IdentityRevision: 4, Kind: models.MediaManagedItemKindVideo, RelativePath: "/not-in-work.mkv", Size: 1, Managed: false, Active: true},
	}
	if err := service.db.Create(&managed).Error; err != nil {
		t.Fatal(err)
	}
	detail, err = service.CatalogDetail(actor, library.ID, seriesPage.List[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.ReorganizableTransfers) != 1 || detail.ReorganizableTransfers[0].TransferTaskID != transfer.ID || detail.ReorganizableTransfers[0].DownloadTaskID != transfer.DownloadTaskID || detail.ReorganizableTransfers[0].IdentityRevision != 4 || detail.ReorganizableTransfers[0].FileCount != 2 {
		t.Fatalf("managed correction projection=%+v", detail.ReorganizableTransfers)
	}
	if _, err := service.Catalog(actor, library.ID, MediaPageQuery{Page: -1, PageSize: 50}); ErrorCode(err) != CodeInvalidRequest {
		t.Fatalf("invalid page error=%v", err)
	}
}
