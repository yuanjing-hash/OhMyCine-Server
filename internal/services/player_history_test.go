package services

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/metadata/tmdb"
)

func TestPlayerHistorySyncIsUserScopedAndRejectsOlderProgress(t *testing.T) {
	queue, actor, _ := queueFixture(t)
	service := NewPlayerHistoryService(queue.db)
	base := PlayerHistoryChange{SyncKey: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", SourceKind: "server", SourceLocator: "http://127.0.0.1:3000", SourceID: "server-a", MediaIdentity: "entry|1|work|2", Title: "Movie", Position: 120, Duration: floatPointer(1000), UpdatedAt: 2_000}
	first, err := service.Sync(actor, 0, []PlayerHistoryChange{base})
	if err != nil || first.Cursor == 0 || len(first.Changes) != 1 || first.Changes[0].Position != 120 {
		t.Fatalf("first sync=%+v err=%v", first, err)
	}
	older := base
	older.Position, older.UpdatedAt = 30, 1_000
	second, err := service.Sync(actor, first.Cursor, []PlayerHistoryChange{older})
	if err != nil || len(second.Changes) != 0 || second.Cursor != first.Cursor {
		t.Fatalf("older sync=%+v err=%v", second, err)
	}
	tie := base
	tie.Completed = true
	tie.Position = 1000
	third, err := service.Sync(actor, first.Cursor, []PlayerHistoryChange{tie})
	if err != nil || len(third.Changes) != 1 || !third.Changes[0].Completed || third.Changes[0].Position != 1000 {
		t.Fatalf("completion tie=%+v err=%v", third, err)
	}
	fourth, err := service.Sync(actor, third.Cursor, []PlayerHistoryChange{tie})
	if err != nil || len(fourth.Changes) != 0 || fourth.Cursor != third.Cursor {
		t.Fatalf("idempotent retry=%+v err=%v", fourth, err)
	}

	page, err := service.List(actor, 1, 24, "server")
	if err != nil || page.Total != 1 || len(page.List) != 1 || !page.List[0].Completed {
		t.Fatalf("history page=%+v err=%v", page, err)
	}
	foreign := Actor{User: models.User{ID: actor.User.ID + 99}}
	foreignPage, err := service.List(foreign, 1, 24, "server")
	if err != nil || foreignPage.Total != 0 || len(foreignPage.List) != 0 {
		t.Fatalf("foreign history page=%+v err=%v", foreignPage, err)
	}
}

func floatPointer(value float64) *float64 { return &value }

type playerHistoryCatalogFixture struct {
	libraries  *MediaLibraryService
	history    *PlayerHistoryService
	actor      Actor
	libraryID  uint
	movie      []models.MediaLibraryEntry
	episodes   []models.MediaLibraryEntry
	movieWork  string
	seriesWork string
}

func newPlayerHistoryCatalogFixture(t *testing.T) playerHistoryCatalogFixture {
	t.Helper()
	libraries, library, actor := createCatalogTestLibrary(t)
	if err := libraries.db.Model(&models.MediaLibrary{}).Where("id = ?", library.ID).Update("enabled", true).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	movieSnapshot := tmdb.Snapshot{Version: 1, TMDBID: 200, MediaType: "movie", Title: "权威电影", PosterPath: "/movie-poster.jpg", BackdropPath: "/movie-backdrop.jpg"}
	movieMetadata, err := marshalRecognitionMetadata(MediaRecognitionResult{Snapshot: movieSnapshot})
	if err != nil {
		t.Fatal(err)
	}
	movieRecognition := models.MediaLibraryRecognition{LibraryID: library.ID, SourceKey: "history-movie", InputFingerprint: strings.Repeat("a", 64), ProfileID: library.ProfileID, ProfileRevision: library.ProfileRevision, Status: "matched", MediaType: "movie", Title: movieSnapshot.Title, TMDBID: &movieSnapshot.TMDBID, MetadataJSON: movieMetadata, LastGeneration: 1, CreatedAt: now, UpdatedAt: now}
	if err := libraries.db.Create(&movieRecognition).Error; err != nil {
		t.Fatal(err)
	}
	movieWork := encodeCatalogToken("movie:tmdb:200")
	movie := []models.MediaLibraryEntry{
		{LibraryID: library.ID, RelativePath: "/Movie.1080p.mkv", ProviderID: "movie-1080", RecognitionID: &movieRecognition.ID, Size: 1, ModifiedAt: now, MediaType: "movie", Title: movieSnapshot.Title, WorkKey: "movie:tmdb:200", MatchStatus: "matched", TMDBID: &movieSnapshot.TMDBID, CategoryName: "电影", LastGeneration: 1, CreatedAt: now, UpdatedAt: now},
		{LibraryID: library.ID, RelativePath: "/Movie.2160p.mkv", ProviderID: "movie-2160", RecognitionID: &movieRecognition.ID, Size: 2, ModifiedAt: now, MediaType: "movie", Title: movieSnapshot.Title, WorkKey: "movie:tmdb:200", MatchStatus: "matched", TMDBID: &movieSnapshot.TMDBID, CategoryName: "电影", LastGeneration: 1, CreatedAt: now, UpdatedAt: now},
	}
	if err := libraries.db.Create(&movie).Error; err != nil {
		t.Fatal(err)
	}

	seriesSnapshot := tmdb.Snapshot{Version: 1, TMDBID: 300, MediaType: "tv", Title: "权威剧名", PosterPath: "/series-poster.jpg", BackdropPath: "/series-backdrop.jpg", EpisodeLanguage: "zh-CN", EpisodeSeasons: []int{1}, EpisodeSnapshots: []tmdb.EpisodeSnapshot{{SeasonNumber: 1, EpisodeNumber: 1, Name: "第一集", StillPath: "/episode-1.jpg"}, {SeasonNumber: 1, EpisodeNumber: 2, Name: "第二集", StillPath: "/episode-2.jpg"}}}
	seriesMetadata, err := marshalRecognitionMetadata(MediaRecognitionResult{Snapshot: seriesSnapshot})
	if err != nil {
		t.Fatal(err)
	}
	seriesRecognition := models.MediaLibraryRecognition{LibraryID: library.ID, SourceKey: "history-series", InputFingerprint: strings.Repeat("b", 64), ProfileID: library.ProfileID, ProfileRevision: library.ProfileRevision, Status: "matched", MediaType: "tv", Title: seriesSnapshot.Title, TMDBID: &seriesSnapshot.TMDBID, MetadataJSON: seriesMetadata, LastGeneration: 1, CreatedAt: now, UpdatedAt: now}
	if err := libraries.db.Create(&seriesRecognition).Error; err != nil {
		t.Fatal(err)
	}
	season, episodeOne, episodeTwo := 1, 1, 2
	seriesWork := encodeCatalogToken("series:tmdb:300")
	episodes := []models.MediaLibraryEntry{
		{LibraryID: library.ID, RelativePath: "/Series/Season 01/Series.S01E01.1080p.mkv", ProviderID: "e1-1080", RecognitionID: &seriesRecognition.ID, Size: 1, ModifiedAt: now, MediaType: "tv", Title: seriesSnapshot.Title, SeriesTitle: seriesSnapshot.Title, WorkKey: "series:tmdb:300", Season: &season, Episode: &episodeOne, MatchStatus: "matched", TMDBID: &seriesSnapshot.TMDBID, CategoryName: "电视剧", LastGeneration: 1, CreatedAt: now, UpdatedAt: now},
		{LibraryID: library.ID, RelativePath: "/Series/Season 01/Series.S01E01.2160p.mkv", ProviderID: "e1-2160", RecognitionID: &seriesRecognition.ID, Size: 2, ModifiedAt: now, MediaType: "tv", Title: seriesSnapshot.Title, SeriesTitle: seriesSnapshot.Title, WorkKey: "series:tmdb:300", Season: &season, Episode: &episodeOne, MatchStatus: "matched", TMDBID: &seriesSnapshot.TMDBID, CategoryName: "电视剧", LastGeneration: 1, CreatedAt: now, UpdatedAt: now},
		{LibraryID: library.ID, RelativePath: "/Series/Season 01/Series.S01E02.mkv", ProviderID: "e2", RecognitionID: &seriesRecognition.ID, Size: 3, ModifiedAt: now, MediaType: "tv", Title: seriesSnapshot.Title, SeriesTitle: seriesSnapshot.Title, WorkKey: "series:tmdb:300", Season: &season, Episode: &episodeTwo, MatchStatus: "matched", TMDBID: &seriesSnapshot.TMDBID, CategoryName: "电视剧", LastGeneration: 1, CreatedAt: now, UpdatedAt: now},
	}
	if err := libraries.db.Create(&episodes).Error; err != nil {
		t.Fatal(err)
	}
	return playerHistoryCatalogFixture{libraries: libraries, history: NewPlayerHistoryService(libraries.db, libraries), actor: actor, libraryID: library.ID, movie: movie, episodes: episodes, movieWork: movieWork, seriesWork: seriesWork}
}

func (fixture playerHistoryCatalogFixture) change(key, sourceID, locator, itemToken string, position float64, updatedAt int64) PlayerHistoryChange {
	return PlayerHistoryChange{SyncKey: key, SourceKind: "server", SourceLocator: locator, SourceID: sourceID, LibraryID: strconv.FormatUint(uint64(fixture.libraryID), 10), ItemID: itemToken, MediaIdentity: itemToken, Title: "客户端伪造标题", PosterURL: "https://attacker.invalid/poster.jpg", Position: position, Duration: floatPointer(1000), UpdatedAt: updatedAt}
}

func TestPlayerHistoryCanonicalizesMovieAcrossOriginEntryAndPhysicalVersions(t *testing.T) {
	fixture := newPlayerHistoryCatalogFixture(t)
	legacy := NewPlayerHistoryService(fixture.libraries.db)
	firstToken := playerHistoryEntryToken(fixture.libraryID, fixture.movieWork, fixture.movie[0].ID)
	secondToken := playerHistoryEntryToken(fixture.libraryID, fixture.movieWork, fixture.movie[1].ID)
	first := fixture.change(strings.Repeat("1", 64), "device-a", "https://server-a.example.test", firstToken, 120, 2_000)
	second := fixture.change(strings.Repeat("2", 64), "device-b", "https://server-b.example.test", secondToken, 360, 3_000)
	if _, err := legacy.Sync(fixture.actor, 0, []PlayerHistoryChange{first, second}); err != nil {
		t.Fatal(err)
	}
	incoming := fixture.change(strings.Repeat("3", 64), "device-c", "https://server-c.example.test", firstToken, 240, 2_500)
	result, err := fixture.history.Sync(fixture.actor, 0, []PlayerHistoryChange{incoming})
	if err != nil {
		t.Fatal(err)
	}
	wantIdentity := playerHistoryCanonicalIdentity(fixture.libraryID, fixture.movieWork, "movie", nil, nil, 0)
	wantKey := playerHistoryCanonicalSyncKey(wantIdentity)
	var canonical *PlayerHistoryChange
	for index := range result.Changes {
		if result.Changes[index].SyncKey == wantKey {
			canonical = &result.Changes[index]
		}
	}
	if canonical == nil || canonical.HistoryIdentity != wantIdentity || canonical.Position != 360 || canonical.Title != "权威电影" || canonical.DisplayTitle != "权威电影" || canonical.PosterPath != "/movie-poster.jpg" || canonical.BackdropPath != "/movie-backdrop.jpg" || canonical.PosterURL != "" || canonical.MediaIdentity != wantIdentity {
		t.Fatalf("canonical=%+v changes=%+v", canonical, result.Changes)
	}
	page, err := fixture.history.List(fixture.actor, 1, 100, "server")
	if err != nil || page.Total != 1 || len(page.List) != 1 || page.List[0].SyncKey != wantKey || page.List[0].Position != 360 {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	var activeLegacy int64
	if err := fixture.libraries.db.Model(&models.PlayerPlaybackHistory{}).Where("user_id = ? AND sync_key <> ? AND deleted = ?", fixture.actor.User.ID, wantKey, false).Count(&activeLegacy).Error; err != nil || activeLegacy != 0 {
		t.Fatalf("active legacy=%d err=%v", activeLegacy, err)
	}
}

func TestPlayerHistoryCanonicalizesEpisodesAndKeepsSeriesPresentation(t *testing.T) {
	fixture := newPlayerHistoryCatalogFixture(t)
	firstToken := playerHistoryEntryToken(fixture.libraryID, fixture.seriesWork, fixture.episodes[0].ID)
	secondVersionToken := playerHistoryEntryToken(fixture.libraryID, fixture.seriesWork, fixture.episodes[1].ID)
	episodeTwoToken := playerHistoryEntryToken(fixture.libraryID, fixture.seriesWork, fixture.episodes[2].ID)
	first := fixture.change(strings.Repeat("4", 64), "phone", "https://phone.example.test", firstToken, 60, 1_000)
	if _, err := fixture.history.Sync(fixture.actor, 0, []PlayerHistoryChange{first}); err != nil {
		t.Fatal(err)
	}
	second := fixture.change(strings.Repeat("5", 64), "desktop", "https://desktop.example.test", secondVersionToken, 180, 2_000)
	secondResult, err := fixture.history.Sync(fixture.actor, 0, []PlayerHistoryChange{second})
	if err != nil {
		t.Fatal(err)
	}
	wantEpisodeOne := playerHistoryCanonicalIdentity(fixture.libraryID, fixture.seriesWork, "series", intPointerTest(1), intPointerTest(1), 0)
	var episodeOne PlayerHistoryChange
	for _, change := range secondResult.Changes {
		if change.SyncKey == playerHistoryCanonicalSyncKey(wantEpisodeOne) {
			episodeOne = change
		}
	}
	if episodeOne.HistoryIdentity != wantEpisodeOne || episodeOne.Position != 180 || episodeOne.Title != "权威剧名" || episodeOne.DisplayTitle != "权威剧名" || episodeOne.DisplaySubtitle != "S01E01 · 第一集" || episodeOne.SeriesTitle != "权威剧名" || episodeOne.EpisodeTitle != "第一集" || episodeOne.MediaType != "episode" || episodeOne.PosterPath != "/series-poster.jpg" || episodeOne.BackdropPath != "/series-backdrop.jpg" || episodeOne.EpisodeStillPath != "/episode-1.jpg" {
		t.Fatalf("episode one=%+v", episodeOne)
	}
	episodeTwo := fixture.change(strings.Repeat("6", 64), "tablet", "https://tablet.example.test", episodeTwoToken, 30, 2_100)
	if _, err := fixture.history.Sync(fixture.actor, 0, []PlayerHistoryChange{episodeTwo}); err != nil {
		t.Fatal(err)
	}
	page, err := fixture.history.List(fixture.actor, 1, 100, "server")
	if err != nil || page.Total != 2 || len(page.List) != 2 || page.List[0].DisplaySubtitle != "S01E02 · 第二集" || page.List[1].DisplaySubtitle != "S01E01 · 第一集" {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	completion := second
	completion.SyncKey = strings.Repeat("7", 64)
	completion.Completed = true
	completion.Position = 1000
	completion.UpdatedAt = 2_000
	completed, err := fixture.history.Sync(fixture.actor, 0, []PlayerHistoryChange{completion})
	if err != nil {
		t.Fatal(err)
	}
	canonicalKey := playerHistoryCanonicalSyncKey(wantEpisodeOne)
	if len(completed.Changes) == 0 {
		t.Fatal("completion produced no change")
	}
	refreshed, err := fixture.history.List(fixture.actor, 1, 100, "server")
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.List[1].SyncKey != canonicalKey || !refreshed.List[1].Completed || refreshed.List[1].Position != 1000 {
		t.Fatalf("completed page=%+v", refreshed)
	}
	retry, err := fixture.history.Sync(fixture.actor, completed.Cursor, []PlayerHistoryChange{completion})
	if err != nil || len(retry.Changes) != 0 || retry.Cursor != completed.Cursor {
		t.Fatalf("retry=%+v err=%v", retry, err)
	}
}

func TestPlayerHistoryRejectsForgedCatalogItemAndPreservesUnresolvableLegacy(t *testing.T) {
	fixture := newPlayerHistoryCatalogFixture(t)
	legacy := NewPlayerHistoryService(fixture.libraries.db)
	unresolved := PlayerHistoryChange{SyncKey: strings.Repeat("8", 64), SourceKind: "server", SourceLocator: "https://legacy.example.test", SourceID: "legacy", MediaIdentity: "entry|999|invalid|1", Title: "Legacy", Position: 10, UpdatedAt: 1_000}
	if _, err := legacy.Sync(fixture.actor, 0, []PlayerHistoryChange{unresolved}); err != nil {
		t.Fatal(err)
	}
	validToken := playerHistoryEntryToken(fixture.libraryID, fixture.movieWork, fixture.movie[0].ID)
	valid := fixture.change(strings.Repeat("9", 64), "valid", "https://valid.example.test", validToken, 20, 2_000)
	if _, err := fixture.history.Sync(fixture.actor, 0, []PlayerHistoryChange{valid}); err != nil {
		t.Fatal(err)
	}
	page, err := fixture.history.List(fixture.actor, 1, 100, "server")
	if err != nil || page.Total != 2 {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	forged := valid
	forged.SyncKey = strings.Repeat("a", 64)
	forged.LibraryID = strconv.FormatUint(uint64(fixture.libraryID+1), 10)
	if _, err := fixture.history.Sync(fixture.actor, 0, []PlayerHistoryChange{forged}); ErrorCode(err) != CodeInvalidRequest {
		t.Fatalf("forged library err=%v", err)
	}
	forged = valid
	forged.SyncKey = strings.Repeat("b", 64)
	forged.ItemID = playerHistoryEntryToken(fixture.libraryID, fixture.movieWork, fixture.episodes[0].ID)
	forged.MediaIdentity = forged.ItemID
	if _, err := fixture.history.Sync(fixture.actor, 0, []PlayerHistoryChange{forged}); ErrorCode(err) != CodeNotFound {
		t.Fatalf("forged entry/work err=%v", err)
	}
	denied := fixture.actor
	denied.ResourceRules = append(denied.ResourceRules, AuthorizationRule{PermissionCode: authz.PermissionMediaLibrariesRead, Effect: models.AuthorizationEffectDeny, ResourceType: models.AuthorizationResourceMediaLibrary, ResourceID: strconv.FormatUint(uint64(fixture.libraryID), 10)})
	forged = valid
	forged.SyncKey = strings.Repeat("c", 64)
	if _, err := fixture.history.Sync(denied, 0, []PlayerHistoryChange{forged}); ErrorCode(err) != CodePermissionDenied {
		t.Fatalf("denied actor err=%v", err)
	}
}

func TestPlayerCatalogPublishesHistoryIdentityAndItemToken(t *testing.T) {
	fixture := newPlayerHistoryCatalogFixture(t)
	moviePage, err := fixture.libraries.PlayerCatalog(fixture.actor, fixture.libraryID, MediaPageQuery{Page: 1, PageSize: 100, MediaType: "movie"})
	if err != nil || len(moviePage.List) != 1 {
		t.Fatalf("movie page=%+v err=%v", moviePage, err)
	}
	wantMovie := playerHistoryCanonicalIdentity(fixture.libraryID, fixture.movieWork, "movie", nil, nil, 0)
	if moviePage.List[0].ItemToken != playerHistoryWorkToken(fixture.libraryID, fixture.movieWork) || moviePage.List[0].HistoryIdentity != wantMovie {
		t.Fatalf("movie item=%+v", moviePage.List[0])
	}
	detail, err := fixture.libraries.PlayerCatalogDetail(context.Background(), fixture.actor, fixture.libraryID, fixture.seriesWork)
	if err != nil || len(detail.Versions) != 3 {
		t.Fatalf("series detail=%+v err=%v", detail, err)
	}
	if detail.Item.ItemToken != playerHistoryWorkToken(fixture.libraryID, fixture.seriesWork) || detail.Item.HistoryIdentity != "" {
		t.Fatalf("series item=%+v", detail.Item)
	}
	version := detail.Versions[0]
	if version.ItemToken == "" || version.HistoryIdentity == "" || version.DisplayTitle != "权威剧名" || version.DisplaySubtitle != "S01E01 · 第一集" || version.PosterPath != "/series-poster.jpg" || version.EpisodeStillPath != "/episode-1.jpg" {
		t.Fatalf("series version=%+v", version)
	}
}
