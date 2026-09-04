package services

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/database"
	serverlog "github.com/yuanjing-hash/OhMyCine-Server/internal/logging"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/medialibrary"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/metadata/tmdb"
	"gorm.io/gorm"
)

func mediaLibraryTestService(t *testing.T) (*MediaLibraryService, *gorm.DB, Actor, models.Storage, models.MediaClassificationProfile) {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "libraries.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(db); err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	user := models.User{Username: "library-test", UsernameNormalized: "library-test", DisplayName: "Library Test", PasswordHash: "unused", Status: models.UserStatusActive}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	actor := Actor{User: user, Permissions: map[string]struct{}{
		authz.PermissionMediaLibrariesRead: {}, authz.PermissionMediaLibrariesCreate: {},
		authz.PermissionMediaLibrariesUpdate: {}, authz.PermissionMediaLibrariesDelete: {},
		authz.PermissionMediaLibrariesScan: {},
	}}
	var profile models.MediaClassificationProfile
	if err := db.Where("code = ?", "default-v1").First(&profile).Error; err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	storage := models.Storage{Name: "Test storage", NameNormalized: "test storage", Type: models.StorageTypeLocal, RootPath: root, RootPathNormalized: strings.ToLower(root), Enabled: true, Capabilities: `{}`}
	if err := db.Create(&storage).Error; err != nil {
		t.Fatal(err)
	}
	service := NewMediaLibraryService(db, NewAuditService(db), zerolog.Nop())
	t.Cleanup(service.Close)
	return service, db, actor, storage, profile
}

func TestNormalizeSourceAssetExtraExtensions(t *testing.T) {
	valid, err := normalizeSourceAssetExtraExtensions([]string{"png", "xml", "png"})
	if err != nil || !reflect.DeepEqual(valid, []string{"png", "xml"}) {
		t.Fatalf("valid=%v err=%v", valid, err)
	}
	invalid := [][]string{{".png"}, {"PNG"}, {"sub title"}, {"../xml"}, {"srt"}, {"mkv"}, {"toolongvalue"}}
	for _, values := range invalid {
		if _, err := normalizeSourceAssetExtraExtensions(values); err == nil {
			t.Fatalf("expected rejection for %q", values)
		}
	}
	tooMany := make([]string, maxSourceAssetExtraExtensions+1)
	for index := range tooMany {
		tooMany[index] = fmt.Sprintf("x%d", index)
	}
	if _, err := normalizeSourceAssetExtraExtensions(tooMany); err == nil {
		t.Fatal("expected extension count limit")
	}
}

func TestMediaRecognitionProjectionChangedTracksUserVisibleMetadata(t *testing.T) {
	year, tmdbID, confidence, rule := 2024, int64(42), 0.91, "rule-1"
	record := models.MediaLibraryRecognition{
		Status: "matched", MediaType: "movie", Title: "Example", ReleaseYear: &year,
		TMDBID: &tmdbID, Confidence: &confidence, CategoryName: "Movies", MatchedRuleID: &rule,
		MetadataJSON: `{"title":"Example"}`,
	}
	result := MediaRecognitionResult{
		Status: "matched", MediaType: "movie", Title: "Example", ReleaseYear: &year,
		TMDBID: &tmdbID, Confidence: &confidence, CategoryName: "Movies", MatchedRuleID: &rule,
	}
	if mediaRecognitionProjectionChanged(record, result, record.MetadataJSON, false) {
		t.Fatal("identical recognition projection must remain unchanged")
	}
	if !mediaRecognitionProjectionChanged(record, result, `{"title":"Example","overview":"updated"}`, false) {
		t.Fatal("metadata-only changes must be observable")
	}
}

func TestMediaLibraryEntryProjectionChangedIgnoresPhysicalFacts(t *testing.T) {
	season, episode, tmdbID, year, confidence, rule := 1, 2, int64(42), 2024, 0.91, "rule-1"
	before := models.MediaLibraryEntry{
		MediaType: "series", Title: "Episode", SeriesTitle: "Example", Season: &season, Episode: &episode,
		WorkKey: "tmdb:tv:42", MatchStatus: "matched", TMDBID: &tmdbID, ReleaseYear: &year,
		MatchConfidence: &confidence, CategoryName: "TV", MatchedRuleID: &rule,
	}
	after := before
	after.Size++
	after.ModifiedAt = time.Now().UTC()
	if mediaLibraryEntryProjectionChanged(before, after) {
		t.Fatal("physical file facts are counted separately from catalog projection")
	}
	after.Title = "Updated episode title"
	if !mediaLibraryEntryProjectionChanged(before, after) {
		t.Fatal("entry metadata changes must be observable")
	}
}

func TestMediaLibraryNoOpRoutineScanDoesNotRequireArtifactGeneration(t *testing.T) {
	run := models.MediaLibraryScanRun{Status: "success", Partial: false}
	for _, kind := range []string{"event", "incremental", "full"} {
		if mediaLibraryArtifactGenerationRequired(kind, run, false) {
			t.Fatalf("complete no-op %s scan scheduled artifacts", kind)
		}
	}
	run.Added = 1
	if !mediaLibraryArtifactGenerationRequired("event", run, false) {
		t.Fatal("catalog change did not schedule artifacts")
	}
	run.Added = 0
	if !mediaLibraryArtifactGenerationRequired("event", run, true) || !mediaLibraryArtifactGenerationRequired("catch_up", run, false) {
		t.Fatal("metadata or policy reconciliation did not schedule artifacts")
	}
}

func TestMediaLibraryScanUsesSharedTMDBRecognitionAndPersistentCache(t *testing.T) {
	service, db, actor, storage, profile := mediaLibraryTestService(t)
	if err := os.WriteFile(filepath.Join(storage.RootPath, "Seven.Samurai.1954.1080p.BluRay.mkv"), []byte("media"), 0o600); err != nil {
		t.Fatal(err)
	}
	requests := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests++
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/search/movie":
			_, _ = response.Write([]byte(`{"results":[{"id":346,"title":"七武士","original_title":"Seven Samurai","original_language":"ja","genre_ids":[18],"release_date":"1954-04-26"}]}`))
		case "/movie/346":
			_, _ = response.Write([]byte(`{"id":346,"title":"七武士","original_language":"ja","release_date":"1954-04-26","genres":[{"id":18}],"production_countries":[{"iso_3166_1":"JP"}]}`))
		default:
			http.NotFound(response, request)
		}
	}))
	defer upstream.Close()
	client, err := tmdb.NewForTest("test-token", upstream.URL, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	metadata := NewMetadataSettingsService(db, NewAuditService(db), nil, tmdb.Credential{Kind: tmdb.CredentialKindReadAccessToken, Value: "deployment-token"})
	metadata.clientFactory = func(tmdb.Credential, string, string) (*tmdb.Client, error) { return client, nil }
	service.SetMetadataSettingsService(metadata)

	created, err := service.Create(context.Background(), actor, testLibraryInput("Recognized library", storage, profile, false), RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	first, err := service.ScanNow(context.Background(), actor, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if first.Matched != 1 || first.Unrecognized != 0 || first.CacheHits != 0 {
		t.Fatalf("first run=%+v", first)
	}
	var entry models.MediaLibraryEntry
	if err := db.Where("library_id = ?", created.ID).First(&entry).Error; err != nil {
		t.Fatal(err)
	}
	if entry.MatchStatus != "matched" || entry.TMDBID == nil || *entry.TMDBID != 346 || entry.Title != "七武士" || !strings.HasPrefix(entry.WorkKey, "movie:tmdb:") {
		t.Fatalf("entry=%+v", entry)
	}
	var recognition models.MediaLibraryRecognition
	if err := db.First(&recognition, entry.RecognitionID).Error; err != nil {
		t.Fatal(err)
	}
	_, snapshot, err := decodeRecognitionMetadata(recognition.MetadataJSON)
	if err != nil || snapshot.TMDBID != 346 || snapshot.Title != "七武士" || snapshot.OriginalTitle != "Seven Samurai" {
		t.Fatalf("snapshot=%+v err=%v", snapshot, err)
	}
	requestCount := requests
	second, err := service.ScanNow(context.Background(), actor, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if second.CacheHits != 1 || requests != requestCount {
		t.Fatalf("second=%+v requests=%d want=%d", second, requests, requestCount)
	}
	page, err := service.Recognitions(actor, created.ID, MediaPageQuery{Page: 1, PageSize: 20}, "matched")
	if err != nil || page.Total != 1 || page.List[0].TMDBID == nil || *page.List[0].TMDBID != 346 || strings.Contains(page.List[0].SourceSummary, storage.RootPath) {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	year := 1954
	candidates, err := service.RecognitionCandidates(context.Background(), actor, created.ID, page.List[0].Token, "Seven Samurai", "movie", &year)
	if err != nil || len(candidates) != 1 || candidates[0].ID != 346 {
		t.Fatalf("candidates=%+v err=%v", candidates, err)
	}
	overridden, err := service.OverrideRecognition(context.Background(), actor, created.ID, page.List[0].Token, MediaRecognitionOverrideInput{TMDBID: 346, MediaType: "movie"}, RequestContext{})
	if err != nil || !overridden.ManualOverride || overridden.Status != "matched" {
		t.Fatalf("overridden=%+v err=%v", overridden, err)
	}
	cleared, err := service.ClearRecognitionOverride(context.Background(), actor, created.ID, page.List[0].Token, RequestContext{})
	if err != nil || cleared.ManualOverride || cleared.Status != "matched" {
		t.Fatalf("cleared=%+v err=%v", cleared, err)
	}
}

func TestMediaLibraryTenEpisodeScanRepairsRegressedChangeRevision(t *testing.T) {
	service, db, actor, storage, profile := mediaLibraryTestService(t)
	changes := NewMediaChangeService(db)
	service.SetMediaChangeService(changes)

	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/search/tv":
			_, _ = response.Write([]byte(`{"results":[{"id":1001,"name":"Example Show","original_name":"Example Show","original_language":"en","genre_ids":[18],"first_air_date":"2024-01-01","origin_country":["US"]}]}`))
		case "/tv/1001":
			_, _ = response.Write([]byte(`{"id":1001,"name":"Example Show","original_name":"Example Show","original_language":"en","first_air_date":"2024-01-01","genres":[{"id":18}],"origin_country":["US"],"number_of_seasons":1,"number_of_episodes":10,"seasons":[{"season_number":1,"episode_count":10,"air_date":"2024-01-01"}]}`))
		default:
			http.NotFound(response, request)
		}
	}))
	defer upstream.Close()
	client, err := tmdb.NewForTest("test-token", upstream.URL, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	metadata := NewMetadataSettingsService(db, NewAuditService(db), nil, tmdb.Credential{Kind: tmdb.CredentialKindReadAccessToken, Value: "deployment-token"})
	metadata.clientFactory = func(tmdb.Credential, string, string) (*tmdb.Client, error) { return client, nil }
	service.SetMetadataSettingsService(metadata)

	input := testLibraryInput("Ten episode revision repair", storage, profile, false)
	created, err := service.Create(context.Background(), actor, input, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	var automaticDiagnosisState models.MediaLibraryStructureAutoState
	if err := db.First(&automaticDiagnosisState, "library_id = ?", created.ID).Error; err != nil || automaticDiagnosisState.SourceRevision != 1 || automaticDiagnosisState.DiagnosedRevision != 0 || automaticDiagnosisState.Status != "pending" {
		t.Fatalf("new source automatic diagnosis state=%+v err=%v", automaticDiagnosisState, err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		_, recordErr := changes.RecordTx(tx, created.ID, 0, models.MediaLibraryChangeCatalog, true)
		return recordErr
	}); err != nil {
		t.Fatal(err)
	}
	// Reproduce the durable state produced by older Update code: an existing
	// revision-1 outbox row while media_libraries.content_revision was reset.
	if err := db.Model(&models.MediaLibrary{}).Where("id = ?", created.ID).Update("content_revision", 0).Error; err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(storage.RootPath, "Example.Show.S01.2024")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	for episode := 1; episode <= 10; episode++ {
		name := fmt.Sprintf("Example.Show.S01E%02d.1080p.mkv", episode)
		if err := os.WriteFile(filepath.Join(directory, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	run, err := service.reconcile(context.Background(), created.ID, "event")
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != "success" || run.Added != 10 || run.Matched != 1 || run.Generation != 1 {
		t.Fatalf("event run=%+v", run)
	}
	var entryCount, recognitionCount int64
	if err := db.Model(&models.MediaLibraryEntry{}).Where("library_id = ?", created.ID).Count(&entryCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.MediaLibraryRecognition{}).Where("library_id = ?", created.ID).Count(&recognitionCount).Error; err != nil {
		t.Fatal(err)
	}
	var library models.MediaLibrary
	if err := db.First(&library, created.ID).Error; err != nil {
		t.Fatal(err)
	}
	if entryCount != 10 || recognitionCount != 1 || library.DirtyGeneration != 1 || library.BaselineGeneration != 1 || library.ContentRevision != 2 {
		t.Fatalf("entries=%d recognitions=%d library=%+v", entryCount, recognitionCount, library)
	}

	input.IncrementalMinutes = 20
	const callbackName = "test:advance_content_revision_before_library_update"
	injectedRevisionAdvance := false
	if err := db.Callback().Update().Before("gorm:update").Register(callbackName, func(tx *gorm.DB) {
		if injectedRevisionAdvance || tx.Statement.Schema == nil || tx.Statement.Schema.Table != "media_libraries" {
			return
		}
		injectedRevisionAdvance = true
		tx.Exec("UPDATE media_libraries SET content_revision = ? WHERE id = ?", 9, created.ID)
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Callback().Update().Remove(callbackName) })
	updated, err := service.Update(context.Background(), actor, created.ID, input, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if !injectedRevisionAdvance || updated.ContentRevision != 9 {
		t.Fatalf("revision advance injected=%v response revision=%d", injectedRevisionAdvance, updated.ContentRevision)
	}
	if err := db.First(&library, created.ID).Error; err != nil || library.ContentRevision != 9 {
		t.Fatalf("updated library revision=%d err=%v", library.ContentRevision, err)
	}
}

func TestMediaLibraryPersistenceFailureLogsSafeStageAndRollsBack(t *testing.T) {
	service, db, actor, storage, profile := mediaLibraryTestService(t)
	var logs bytes.Buffer
	service.log = zerolog.New(&logs)
	const secretTitle = "SECRET-MEDIA-TITLE"
	if err := os.WriteFile(filepath.Join(storage.RootPath, secretTitle+".mkv"), []byte("media"), 0o600); err != nil {
		t.Fatal(err)
	}
	created, err := service.Create(context.Background(), actor, testLibraryInput("Persistence failure", storage, profile, false), RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE TRIGGER fail_media_entry_insert BEFORE INSERT ON media_library_entries BEGIN SELECT RAISE(ABORT, 'SECRET SQL path'); END`).Error; err != nil {
		t.Fatal(err)
	}
	run, err := service.ScanNow(context.Background(), actor, created.ID)
	if ErrorCode(err) != CodeMediaLibraryScanFailed || run.Status != "failed" {
		t.Fatalf("run=%+v code=%q err=%v", run, ErrorCode(err), err)
	}
	var entries, recognitions int64
	if err := db.Model(&models.MediaLibraryEntry{}).Where("library_id = ?", created.ID).Count(&entries).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.MediaLibraryRecognition{}).Where("library_id = ?", created.ID).Count(&recognitions).Error; err != nil {
		t.Fatal(err)
	}
	var library models.MediaLibrary
	if err := db.First(&library, created.ID).Error; err != nil {
		t.Fatal(err)
	}
	if entries != 0 || recognitions != 0 || library.DirtyGeneration != 0 || library.BaselineGeneration != 0 {
		t.Fatalf("partial commit: entries=%d recognitions=%d library=%+v", entries, recognitions, library)
	}
	text := logs.String()
	if !strings.Contains(text, `"persistence_stage":"persist_entries"`) || !strings.Contains(text, `"database_error_class":"constraint"`) {
		t.Fatalf("missing safe diagnostics: %s", text)
	}
	if strings.Contains(text, secretTitle) || strings.Contains(text, "SECRET SQL path") || strings.Contains(text, storage.RootPath) {
		t.Fatalf("sensitive persistence cause leaked: %s", text)
	}
}

func TestMediaLibraryRecognitionHonorsConfiguredConcurrencyAndRateGate(t *testing.T) {
	service, db, actor, storage, profile := mediaLibraryTestService(t)
	for _, name := range []string{"Alpha.2020.mkv", "Beta.2020.mkv", "Gamma.2020.mkv"} {
		if err := os.WriteFile(filepath.Join(storage.RootPath, name), []byte("media"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var active, maximum atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/search/movie" {
			current := active.Add(1)
			defer active.Add(-1)
			for {
				previous := maximum.Load()
				if current <= previous || maximum.CompareAndSwap(previous, current) {
					break
				}
			}
			time.Sleep(80 * time.Millisecond)
			title := request.URL.Query().Get("query")
			_, _ = response.Write([]byte(`{"results":[{"id":100,"title":` + strconv.Quote(title) + `,"original_title":` + strconv.Quote(title) + `,"original_language":"en","genre_ids":[18],"release_date":"2020-01-01"}]}`))
			return
		}
		if request.URL.Path == "/movie/100" {
			_, _ = response.Write([]byte(`{"id":100,"title":"Concurrent title","original_title":"Concurrent title","original_language":"en","release_date":"2020-01-01","genres":[{"id":18}],"production_countries":[{"iso_3166_1":"US"}]}`))
			return
		}
		http.NotFound(response, request)
	}))
	defer upstream.Close()
	client, err := tmdb.NewForTest("test-token", upstream.URL, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	metadata := NewMetadataSettingsService(db, NewAuditService(db), nil, tmdb.Credential{Kind: tmdb.CredentialKindReadAccessToken, Value: "deployment-token"})
	metadata.clientFactory = func(tmdb.Credential, string, string) (*tmdb.Client, error) { return client, nil }
	service.SetMetadataSettingsService(metadata)

	input := testLibraryInput("Concurrent recognition", storage, profile, false)
	input.MetadataConcurrency = 3
	input.MetadataRatePerSecond = 100
	created, err := service.Create(context.Background(), actor, input, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	run, err := service.ScanNow(context.Background(), actor, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Matched != 3 || maximum.Load() < 2 {
		t.Fatalf("run=%+v maximum concurrent TMDB searches=%d, want at least 2", run, maximum.Load())
	}
}

func testLibraryInput(name string, storage models.Storage, profile models.MediaClassificationProfile, enabled bool) MediaLibraryInput {
	return MediaLibraryInput{Name: name, StorageID: storage.ID, ProfileID: profile.ID, RelativeRoot: "/", Enabled: enabled, Recursive: true}
}

type recordingLibraryArtworkScheduler struct {
	libraryID uint
	complete  bool
	calls     int
}

func (s *recordingLibraryArtworkScheduler) ScheduleGeneration(libraryID uint, complete bool) error {
	s.libraryID, s.complete = libraryID, complete
	s.calls++
	return nil
}

func TestSuccessfulMediaLibraryScanSchedulesCategoryArtworkAfterCommit(t *testing.T) {
	service, _, actor, storage, profile := mediaLibraryTestService(t)
	recorder := &recordingLibraryArtworkScheduler{}
	service.SetLibraryArtworkScheduler(recorder)
	created, err := service.Create(context.Background(), actor, testLibraryInput("Artwork scheduling", storage, profile, false), RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	run, err := service.ScanNow(context.Background(), actor, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != "success" || recorder.calls != 1 || recorder.libraryID != created.ID || !recorder.complete {
		t.Fatalf("run=%+v artwork schedule=%+v", run, recorder)
	}
}

func TestMediaLibraryUnrelatedUpdatePreservesUnifiedSchedule(t *testing.T) {
	service, db, actor, storage, profile := mediaLibraryTestService(t)
	input := testLibraryInput("Schedule library", storage, profile, false)
	created, err := service.Create(context.Background(), actor, input, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	managedKey := managedScheduleKey("media_library_scan", "media_library", uintID(created.ID))
	if err := db.Model(&models.ScheduleDefinition{}).Where("managed_key = ?", managedKey).Updates(map[string]any{
		"cron_expression": "15 4 * * 2",
		"timezone":        "UTC",
		"overlap_policy":  "queue",
	}).Error; err != nil {
		t.Fatal(err)
	}
	input.Name = "Schedule library renamed"
	if _, err := service.Update(context.Background(), actor, created.ID, input, RequestContext{}); err != nil {
		t.Fatal(err)
	}
	var schedule models.ScheduleDefinition
	if err := db.First(&schedule, "managed_key = ?", managedKey).Error; err != nil {
		t.Fatal(err)
	}
	if schedule.CronExpression != "15 4 * * 2" || schedule.Timezone != "UTC" || schedule.OverlapPolicy != "queue" {
		t.Fatalf("unrelated media library update overwrote unified schedule: %+v", schedule)
	}
}

func waitForLibrary(t *testing.T, db *gorm.DB, id uint, condition func(models.MediaLibrary) bool) models.MediaLibrary {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		var library models.MediaLibrary
		if err := db.First(&library, id).Error; err == nil && condition(library) {
			return library
		}
		time.Sleep(40 * time.Millisecond)
	}
	var library models.MediaLibrary
	_ = db.First(&library, id).Error
	t.Fatalf("library %d did not reach expected state; last=%+v", id, library)
	return models.MediaLibrary{}
}

func TestMediaLibraryAutomaticallyBuildsBaselineThenListens(t *testing.T) {
	service, db, actor, storage, profile := mediaLibraryTestService(t)
	files := []string{"Movie.mp4", filepath.Join("Movies", "Nested.mkv"), filepath.Join("Series", "Show.S01E01.mp4"), filepath.Join("Series", "Show.S01E02.mp4")}
	for _, relative := range files {
		path := filepath.Join(storage.RootPath, relative)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(relative), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	created, err := service.Create(context.Background(), actor, testLibraryInput("Local library", storage, profile, true), RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	library := waitForLibrary(t, db, created.ID, func(item models.MediaLibrary) bool { return item.Status == models.MediaLibraryStatusListening })
	if library.BaselineGeneration < 2 || library.DirtyGeneration < 2 {
		t.Fatalf("baseline/catch-up generations were not committed: %+v", library)
	}
	entries, err := service.Entries(actor, created.ID, 20)
	if err != nil || len(entries) != len(files) {
		t.Fatalf("entries=%+v err=%v", entries, err)
	}
	var kinds []string
	if err := db.Model(&models.MediaLibraryScanRun{}).Where("library_id = ? AND status = ?", created.ID, "success").Order("id").Pluck("kind", &kinds).Error; err != nil {
		t.Fatal(err)
	}
	if len(kinds) < 2 || kinds[0] != "initial" || kinds[1] != "catch_up" {
		t.Fatalf("scan kinds=%v, want initial then catch_up", kinds)
	}
	payload, err := json.Marshal(created)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), storage.RootPath) {
		t.Fatalf("physical storage root leaked in media library response: %s", payload)
	}
}

func TestPan115InitialScanDoesNotRepeatUnchangedCatchUp(t *testing.T) {
	modified := time.Now().UTC().Truncate(time.Second)
	driver := &fakeCloudDriver{
		items:    map[string]cloud.Item{"cloud-root": {ID: "cloud-root", ParentID: "0", Name: "媒体", IsDir: true}},
		children: map[string][]cloud.Item{"cloud-root": {{ID: "video-id", ParentID: "cloud-root", Name: "Once.2026.mkv", Size: 128, ModifiedAt: modified}}},
	}
	db, _, connections, actor := newConnectionTestService(t, driver)
	for _, permission := range []string{authz.PermissionMediaLibrariesRead, authz.PermissionMediaLibrariesCreate, authz.PermissionMediaLibrariesScan} {
		actor.Permissions[permission] = struct{}{}
	}
	connection, err := connections.Create(actor, ConnectionInput{Name: "115 initial account", Provider: cloud.ProviderPan115, Cookie: testPan115Cookie, Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	storageService := NewStorageService(db, NewAuditService(db))
	storageService.SetConnectionService(connections)
	storage, err := storageService.CreateContext(context.Background(), actor, StorageInput{Name: "115 initial root", Type: models.StorageTypePan115, RootPath: "cloud-root", RootDisplayPath: "/媒体", ConnectionID: &connection.ID, Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	var profile models.MediaClassificationProfile
	if err := db.Order("id").First(&profile).Error; err != nil {
		t.Fatal(err)
	}
	service := NewMediaLibraryService(db, NewAuditService(db), zerolog.Nop())
	service.SetConnectionService(connections)
	t.Cleanup(service.Close)
	library, err := service.Create(context.Background(), actor, MediaLibraryInput{Name: "115 initial once", StorageID: storage.ID, ProfileID: profile.ID, RelativeRoot: "/", ProviderRootID: storage.RootPath, Enabled: true, Recursive: true, VideoExtensions: []string{".mkv"}, TransferMode: models.MediaLibraryTransferCopy}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	waitForLibrary(t, db, library.ID, func(item models.MediaLibrary) bool { return item.Status == models.MediaLibraryStatusListening })
	time.Sleep(150 * time.Millisecond)
	var kinds []string
	if err := db.Model(&models.MediaLibraryScanRun{}).Where("library_id = ?", library.ID).Order("id").Pluck("kind", &kinds).Error; err != nil {
		t.Fatal(err)
	}
	if len(kinds) != 1 || kinds[0] != "initial" {
		t.Fatalf("115 initialization enumerated more than once without an event: kinds=%v", kinds)
	}
}

func TestDisabledLibraryWaitsUntilEnabledAndWatcherReconcilesChanges(t *testing.T) {
	service, db, actor, storage, profile := mediaLibraryTestService(t)
	changes := NewMediaChangeService(db)
	service.SetMediaChangeService(changes)
	var notifiedRevision atomic.Uint64
	changes.SetReadyHandler(func(_ uint, revision uint64) { notifiedRevision.Store(revision) })
	input := testLibraryInput("Deferred library", storage, profile, false)
	created, err := service.Create(context.Background(), actor, input, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	var runCount int64
	if err := db.Model(&models.MediaLibraryScanRun{}).Where("library_id = ?", created.ID).Count(&runCount).Error; err != nil || runCount != 0 {
		t.Fatalf("disabled library run count=%d err=%v", runCount, err)
	}
	input.Enabled = true
	if _, err := service.Update(context.Background(), actor, created.ID, input, RequestContext{}); err != nil {
		t.Fatal(err)
	}
	waitForLibrary(t, db, created.ID, func(item models.MediaLibrary) bool { return item.Status == models.MediaLibraryStatusListening })

	mediaPath := filepath.Join(storage.RootPath, "Arrived.S02E03.mp4")
	if err := os.WriteFile(mediaPath, []byte("video"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitForLibrary(t, db, created.ID, func(models.MediaLibrary) bool {
		var count int64
		return db.Model(&models.MediaLibraryEntry{}).Where("library_id = ? AND relative_path = ?", created.ID, "/Arrived.S02E03.mp4").Count(&count).Error == nil && count == 1 && notifiedRevision.Load() > 0
	})
	var addedChange models.MediaLibraryChange
	if err := db.Where("library_id = ? AND state = ?", created.ID, models.MediaLibraryChangeReady).Order("revision DESC").First(&addedChange).Error; err != nil {
		t.Fatal(err)
	}
	if addedChange.Kind != models.MediaLibraryChangeCatalog || addedChange.ReadyAt == nil || notifiedRevision.Load() != addedChange.Revision {
		t.Fatalf("added change=%+v notified_revision=%d", addedChange, notifiedRevision.Load())
	}
	if err := os.Remove(mediaPath); err != nil {
		t.Fatal(err)
	}
	waitForLibrary(t, db, created.ID, func(models.MediaLibrary) bool {
		var count int64
		return db.Model(&models.MediaLibraryEntry{}).Where("library_id = ? AND relative_path = ?", created.ID, "/Arrived.S02E03.mp4").Count(&count).Error == nil && count == 0 && notifiedRevision.Load() > addedChange.Revision
	})
	var removedChange models.MediaLibraryChange
	if err := db.Where("library_id = ? AND state = ?", created.ID, models.MediaLibraryChangeReady).Order("revision DESC").First(&removedChange).Error; err != nil {
		t.Fatal(err)
	}
	if removedChange.Kind != models.MediaLibraryChangeRemoval || removedChange.Revision <= addedChange.Revision || removedChange.ReadyAt == nil || notifiedRevision.Load() != removedChange.Revision {
		t.Fatalf("removed change=%+v added_revision=%d notified_revision=%d", removedChange, addedChange.Revision, notifiedRevision.Load())
	}
}

func TestMediaLibrarySourceChangeClearsOldCatalogAndBuildsNewBaseline(t *testing.T) {
	service, db, actor, storage, profile := mediaLibraryTestService(t)
	oldRoot := filepath.Join(storage.RootPath, "old")
	newRoot := filepath.Join(storage.RootPath, "new")
	if err := os.MkdirAll(oldRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(newRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldRoot, "Old.Movie.mp4"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldRoot, "Old.Movie.srt"), []byte("subtitle"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(newRoot, "New.Movie.mp4"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}

	input := testLibraryInput("Changing source", storage, profile, true)
	input.RelativeRoot = "/old"
	created, err := service.Create(context.Background(), actor, input, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	var automaticDiagnosisState models.MediaLibraryStructureAutoState
	if err := db.First(&automaticDiagnosisState, "library_id = ?", created.ID).Error; err != nil || automaticDiagnosisState.SourceRevision != 1 || automaticDiagnosisState.DiagnosedRevision != 0 || automaticDiagnosisState.Status != "pending" {
		t.Fatalf("new source automatic diagnosis state=%+v err=%v", automaticDiagnosisState, err)
	}
	waitForLibrary(t, db, created.ID, func(item models.MediaLibrary) bool {
		var count, assetCount int64
		return item.Status == models.MediaLibraryStatusListening &&
			db.Model(&models.MediaLibraryEntry{}).Where("library_id = ? AND relative_path = ?", created.ID, "/Old.Movie.mp4").Count(&count).Error == nil && count == 1 &&
			db.Model(&models.MediaLibrarySourceAsset{}).Where("library_id = ? AND relative_path = ?", created.ID, "/Old.Movie.srt").Count(&assetCount).Error == nil && assetCount == 1
	})
	cleanupAt := time.Now().UTC().Truncate(time.Millisecond)
	if err := db.Model(&models.MediaLibrary{}).Where("id = ?", created.ID).Updates(map[string]any{"artifact_cleanup_removed": 3, "artifact_cleanup_error": "cleanup_test", "artifact_cleanup_at": cleanupAt}).Error; err != nil {
		t.Fatal(err)
	}
	staleIssue := models.MediaLibraryStructureIssue{Token: "old-source-issue", LibraryID: created.ID, DiagnosisJobID: "old-source-diagnosis", Generation: 1, Code: "path_mismatch", Kind: "video", State: "pending_repair", Repairable: true, CurrentPath: "Old.Movie.mp4", ExpectedPath: "电影/Old Movie/Old Movie.mp4", CreatedAt: cleanupAt, UpdatedAt: cleanupAt}
	if err := db.Create(&staleIssue).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.MediaLibraryStructureIssueMember{IssueID: staleIssue.ID, Token: "old-source-member", SourcePath: "Old.Movie.mp4", CreatedAt: cleanupAt}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.MediaLibraryStructureRepairDraft{ID: "old-source-draft", OwnerID: actor.User.ID, LibraryID: created.ID, DiagnosisJobID: "old-source-diagnosis", SourceRevision: 1, Generation: 1, RuleFingerprint: "old", PlanHash: "old", SelectionsJSON: `{}`, ExpiresAt: cleanupAt.Add(time.Hour), CreatedAt: cleanupAt}).Error; err != nil {
		t.Fatal(err)
	}

	input.RelativeRoot = "/new"
	input.Enabled = false
	updated, err := service.Update(context.Background(), actor, created.ID, input, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if updated.BaselineGeneration != 0 || updated.DirtyGeneration != 0 || updated.LastScanAt != nil || updated.LastSuccessfulScanAt != nil {
		t.Fatalf("source change retained old scan state: %+v", updated.MediaLibrary)
	}
	if updated.ArtifactCleanupRemoved != 3 || updated.ArtifactCleanupError != "cleanup_test" || updated.ArtifactCleanupAt == nil {
		t.Fatalf("source change lost cleanup observability: %+v", updated.MediaLibrary)
	}
	if err := db.First(&automaticDiagnosisState, "library_id = ?", created.ID).Error; err != nil || automaticDiagnosisState.SourceRevision != 2 || automaticDiagnosisState.Status != "pending" {
		t.Fatalf("source change automatic diagnosis state=%+v err=%v", automaticDiagnosisState, err)
	}
	var entryCount, runCount, recognitionCount, assetCount, issueCount, memberCount, draftCount int64
	if err := db.Model(&models.MediaLibraryEntry{}).Where("library_id = ?", created.ID).Count(&entryCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.MediaLibraryScanRun{}).Where("library_id = ?", created.ID).Count(&runCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.MediaLibraryRecognition{}).Where("library_id = ?", created.ID).Count(&recognitionCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.MediaLibrarySourceAsset{}).Where("library_id = ?", created.ID).Count(&assetCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.MediaLibraryStructureIssue{}).Where("library_id = ?", created.ID).Count(&issueCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.MediaLibraryStructureIssueMember{}).Where("issue_id = ?", staleIssue.ID).Count(&memberCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.MediaLibraryStructureRepairDraft{}).Where("library_id = ?", created.ID).Count(&draftCount).Error; err != nil {
		t.Fatal(err)
	}
	if entryCount != 0 || runCount != 0 || recognitionCount != 0 || assetCount != 0 || issueCount != 0 || memberCount != 0 || draftCount != 0 {
		t.Fatalf("source change retained source-bound records: entries=%d runs=%d recognitions=%d assets=%d issues=%d members=%d drafts=%d", entryCount, runCount, recognitionCount, assetCount, issueCount, memberCount, draftCount)
	}

	input.Enabled = true
	if _, err := service.Update(context.Background(), actor, created.ID, input, RequestContext{}); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&automaticDiagnosisState, "library_id = ?", created.ID).Error; err != nil || automaticDiagnosisState.SourceRevision != 2 {
		t.Fatalf("non-source update advanced automatic diagnosis revision: state=%+v err=%v", automaticDiagnosisState, err)
	}
	waitForLibrary(t, db, created.ID, func(item models.MediaLibrary) bool {
		var newCount, oldCount int64
		newErr := db.Model(&models.MediaLibraryEntry{}).Where("library_id = ? AND relative_path = ?", created.ID, "/New.Movie.mp4").Count(&newCount).Error
		oldErr := db.Model(&models.MediaLibraryEntry{}).Where("library_id = ? AND relative_path = ?", created.ID, "/Old.Movie.mp4").Count(&oldCount).Error
		return item.Status == models.MediaLibraryStatusListening && newErr == nil && oldErr == nil && newCount == 1 && oldCount == 0
	})

	var kinds []string
	if err := db.Model(&models.MediaLibraryScanRun{}).Where("library_id = ? AND status = ?", created.ID, "success").Order("id").Pluck("kind", &kinds).Error; err != nil {
		t.Fatal(err)
	}
	if len(kinds) < 2 || kinds[0] != "initial" || kinds[1] != "catch_up" {
		t.Fatalf("new source scan kinds=%v, want initial then catch_up", kinds)
	}
}

func TestMediaLibrarySourceIdentityIncludesProviderRoot(t *testing.T) {
	existing := models.MediaLibrary{StorageID: 7, RelativeRoot: "/Movies", ProviderRootID: "provider-old"}
	tests := []struct {
		name        string
		replacement models.MediaLibrary
		changed     bool
	}{
		{name: "same source", replacement: existing, changed: false},
		{name: "storage changed", replacement: models.MediaLibrary{StorageID: 8, RelativeRoot: "/Movies", ProviderRootID: "provider-old"}, changed: true},
		{name: "relative root changed", replacement: models.MediaLibrary{StorageID: 7, RelativeRoot: "/TV", ProviderRootID: "provider-old"}, changed: true},
		{name: "provider root changed", replacement: models.MediaLibrary{StorageID: 7, RelativeRoot: "/Movies", ProviderRootID: "provider-new"}, changed: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if actual := mediaLibrarySourceChanged(existing, test.replacement); actual != test.changed {
				t.Fatalf("mediaLibrarySourceChanged()=%v, want %v", actual, test.changed)
			}
		})
	}
}

func TestMediaLibraryRejectsOverlappingRootsAndLocalSTRM(t *testing.T) {
	service, _, actor, storage, profile := mediaLibraryTestService(t)
	if err := os.MkdirAll(filepath.Join(storage.RootPath, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	first := testLibraryInput("First", storage, profile, false)
	first.RelativeRoot = "/nested"
	if _, err := service.Create(context.Background(), actor, first, RequestContext{}); err != nil {
		t.Fatal(err)
	}
	second := testLibraryInput("Second", storage, profile, false)
	if _, err := service.Create(context.Background(), actor, second, RequestContext{}); ErrorCode(err) != CodeMediaLibraryOverlap {
		t.Fatalf("overlap code=%q err=%v", ErrorCode(err), err)
	}
	strm := testLibraryInput("STRM", storage, profile, false)
	strm.StorageID = storage.ID
	strm.STRMEnabled = true
	if _, err := service.Create(context.Background(), actor, strm, RequestContext{}); ErrorCode(err) != CodeInvalidRequest {
		t.Fatalf("local STRM code=%q err=%v", ErrorCode(err), err)
	}
}

func TestPan115MediaLibraryArtifactPolicyRequiresCapabilitiesAndProjectionRoot(t *testing.T) {
	driver := &fakeCloudDriver{
		signedProxy: true,
		items: map[string]cloud.Item{
			"cloud-root": {ID: "cloud-root", ParentID: "0", Name: "媒体", IsDir: true},
		},
	}
	db, _, connections, actor := newConnectionTestService(t, driver)
	actor.Permissions[authz.PermissionMediaLibrariesRead] = struct{}{}
	actor.Permissions[authz.PermissionMediaLibrariesCreate] = struct{}{}
	connection, err := connections.Create(actor, ConnectionInput{Name: "115 artifact account", Provider: cloud.ProviderPan115, Cookie: testPan115Cookie, Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	storageService := NewStorageService(db, NewAuditService(db))
	storageService.SetConnectionService(connections)
	storage, err := storageService.CreateContext(context.Background(), actor, StorageInput{Name: "115 artifact root", Type: models.StorageTypePan115, RootPath: "cloud-root", RootDisplayPath: "/媒体", ConnectionID: &connection.ID, Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	var profile models.MediaClassificationProfile
	if err := db.Where("code = ?", "default-v1").First(&profile).Error; err != nil {
		t.Fatal(err)
	}
	service := NewMediaLibraryService(db, NewAuditService(db), zerolog.Nop())
	service.SetConnectionService(connections)
	t.Cleanup(service.Close)
	projection := t.TempDir()
	metadata := true
	input := MediaLibraryInput{Name: "115 STRM", StorageID: storage.ID, ProfileID: profile.ID, RelativeRoot: "/", ProviderRootID: storage.RootPath, Enabled: false, Recursive: true, STRMEnabled: true, STRMLocalRoot: projection, MetadataArtifactsEnabled: &metadata, TransferMode: models.MediaLibraryTransferCopy}
	created, err := service.Create(context.Background(), actor, input, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if !created.STRMEnabled || !created.SignedProxyEnabled || !created.MetadataArtifactsEnabled || created.STRMLocalPath != filepath.Clean(projection) || created.UploadSidecars {
		t.Fatalf("artifact policy=%+v", created.MediaLibrary)
	}
	listener := models.Downloader{ID: "pan115-listener", OwnerID: actor.User.ID, Name: "Listener", NameNormalized: "pan115-listener", Type: models.DownloaderTypePan115Offline, StorageID: &storage.ID, ProviderDirectoryID: "listener-root", AutoListenLifeEvents: true, Enabled: true, CapabilitiesJSON: `{}`, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := db.Create(&listener).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.MediaLibrary{}).Where("id = ?", created.ID).Update("enabled", true).Error; err != nil {
		t.Fatal(err)
	}
	input.Enabled = true
	driver.statCalls = 0
	if _, err := service.validateInput(context.Background(), created.ID, actor, input); err != nil {
		t.Fatalf("ordinary enabled-library edit failed: %v", err)
	}
	if driver.statCalls != 0 {
		t.Fatalf("ordinary media-library edit repeated provider validation: %d stat calls", driver.statCalls)
	}
	input.Enabled = false

	input.Name = "Missing projection"
	input.RelativeRoot = "/other"
	input.ProviderRootID = "cloud-root"
	input.STRMLocalRoot = ""
	if _, err := service.Create(context.Background(), actor, input, RequestContext{}); ErrorCode(err) != CodeMediaLibraryPathInvalid {
		t.Fatalf("missing projection code=%q err=%v", ErrorCode(err), err)
	}

	input.Name = "Upload conflict"
	input.STRMLocalRoot = projection
	input.UploadSidecars = true
	if _, err := service.Create(context.Background(), actor, input, RequestContext{}); ErrorCode(err) != CodeInvalidRequest {
		t.Fatalf("STRM upload conflict code=%q err=%v", ErrorCode(err), err)
	}
}

func TestMediaLibraryImportPolicyAndOrder(t *testing.T) {
	service, db, actor, storage, profile := mediaLibraryTestService(t)
	firstInput := testLibraryInput("First", storage, profile, false)
	firstInput.TransferMode = models.MediaLibraryTransferCopy
	firstInput.ConflictPolicy = models.MediaLibraryConflictRename
	first, err := service.Create(context.Background(), actor, firstInput, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	secondStorage := models.Storage{Name: "Second storage", NameNormalized: "second storage", Type: models.StorageTypeLocal, RootPath: root, RootPathNormalized: strings.ToLower(root), Enabled: true, Capabilities: `{}`}
	if err := db.Create(&secondStorage).Error; err != nil {
		t.Fatal(err)
	}
	second, err := service.Create(context.Background(), actor, testLibraryInput("Second", secondStorage, profile, false), RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if first.TransferMode != models.MediaLibraryTransferCopy || first.ConflictPolicy != models.MediaLibraryConflictRename || first.SortOrder >= second.SortOrder {
		t.Fatalf("unexpected import/order details: first=%+v second=%+v", first.MediaLibrary, second.MediaLibrary)
	}
	ordered, err := service.Reorder(actor, []uint{second.ID, first.ID}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if len(ordered) != 2 || ordered[0].ID != second.ID || ordered[0].SortOrder != 1 || ordered[1].ID != first.ID {
		t.Fatalf("ordered=%+v", ordered)
	}
	invalid := testLibraryInput("Second", secondStorage, profile, false)
	invalid.MovieDirectoryTemplate = "../{title}"
	if _, err := service.Update(context.Background(), actor, second.ID, invalid, RequestContext{}); ErrorCode(err) != CodeInvalidRequest {
		t.Fatalf("invalid template code=%q err=%v", ErrorCode(err), err)
	}
}

func TestMediaLibraryReferenceAndProfileRevisionContracts(t *testing.T) {
	service, db, actor, storage, profile := mediaLibraryTestService(t)
	created, err := service.Create(context.Background(), actor, testLibraryInput("Referenced", storage, profile, false), RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if refs, err := service.References(profile.ID); err != nil || len(refs) != 1 || refs[0] != created.Name {
		t.Fatalf("profile refs=%v err=%v", refs, err)
	}
	if refs, err := service.StorageReferences(storage.ID); err != nil || len(refs) != 1 || refs[0] != created.Name {
		t.Fatalf("storage refs=%v err=%v", refs, err)
	}
	if err := service.ProfileRevisionChanged(profile.ID, profile.Revision+1); err != nil {
		t.Fatal(err)
	}
	var library models.MediaLibrary
	if err := db.First(&library, created.ID).Error; err != nil || !library.ReclassificationDue {
		t.Fatalf("library=%+v err=%v", library, err)
	}
}

func TestLiveMediaLibraryRootIsReadOnly(t *testing.T) {
	root := os.Getenv("OMC_LIVE_LIBRARY_ROOT")
	if root == "" {
		t.Skip("set OMC_LIVE_LIBRARY_ROOT for the opt-in local acceptance test")
	}
	before := snapshotTree(t, root)
	service, db, actor, storage, profile := mediaLibraryTestService(t)
	storage.RootPath = root
	storage.RootPathNormalized = strings.ToLower(filepath.Clean(root))
	if err := db.Save(&storage).Error; err != nil {
		t.Fatal(err)
	}
	created, err := service.Create(context.Background(), actor, testLibraryInput("Live acceptance", storage, profile, true), RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	waitForLibrary(t, db, created.ID, func(item models.MediaLibrary) bool { return item.Status == models.MediaLibraryStatusListening })
	entries, err := service.Entries(actor, created.ID, 500)
	if err != nil {
		t.Fatal(err)
	}
	mp4Count := 0
	for _, entry := range entries {
		if strings.EqualFold(filepath.Ext(entry.RelativePath), ".mp4") {
			mp4Count++
		}
		if filepath.IsAbs(entry.RelativePath) || strings.Contains(entry.RelativePath, root) {
			t.Fatalf("entry exposes a physical absolute path")
		}
	}
	if mp4Count != 4 {
		t.Fatalf("discovered %d MP4 entries, want 4", mp4Count)
	}
	after := snapshotTree(t, root)
	if len(before) != len(after) {
		t.Fatalf("source tree item count changed: before=%d after=%d", len(before), len(after))
	}
	for relative, want := range before {
		if got, ok := after[relative]; !ok || got != want {
			t.Fatalf("source item changed during scan: %q", relative)
		}
	}
}

type treeItemSnapshot struct {
	Size    int64
	Mode    os.FileMode
	ModTime int64
}

func snapshotTree(t *testing.T, root string) map[string]treeItemSnapshot {
	t.Helper()
	items := map[string]treeItemSnapshot{}
	if err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		items[relative] = treeItemSnapshot{Size: info.Size(), Mode: info.Mode(), ModTime: info.ModTime().UnixNano()}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return items
}

func TestPan115MediaLibraryScanKeepsFileIdentityAcrossRename(t *testing.T) {
	modified := time.Now().UTC().Truncate(time.Second)
	driver := &fakeCloudDriver{
		signedProxy: true,
		items:       map[string]cloud.Item{"cloud-root": {ID: "cloud-root", ParentID: "0", Name: "媒体", IsDir: true}},
		children:    map[string][]cloud.Item{"cloud-root": {{ID: "video-id", ParentID: "cloud-root", Name: "Before.2026.mkv", Size: 128, ModifiedAt: modified}}},
	}
	db, _, connections, actor := newConnectionTestService(t, driver)
	for _, permission := range []string{authz.PermissionMediaLibrariesRead, authz.PermissionMediaLibrariesCreate, authz.PermissionMediaLibrariesScan} {
		actor.Permissions[permission] = struct{}{}
	}
	connection, err := connections.Create(actor, ConnectionInput{Name: "115 scan account", Provider: cloud.ProviderPan115, Cookie: testPan115Cookie, Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	storageService := NewStorageService(db, NewAuditService(db))
	storageService.SetConnectionService(connections)
	storage, err := storageService.CreateContext(context.Background(), actor, StorageInput{Name: "115 scan root", Type: models.StorageTypePan115, RootPath: "cloud-root", RootDisplayPath: "/媒体", ConnectionID: &connection.ID, Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	var profile models.MediaClassificationProfile
	if err := db.Order("id").First(&profile).Error; err != nil {
		t.Fatal(err)
	}
	service := NewMediaLibraryService(db, NewAuditService(db), zerolog.Nop())
	service.SetConnectionService(connections)
	queue := NewQueueService(db, NewAuditService(db))
	service.SetQueueService(queue)
	artifacts := NewMediaArtifactService(db, queue, &SignedProxyService{}, zerolog.Nop())
	service.SetArtifactService(artifacts)
	structure := NewMediaLibraryStructureService(db, NewAuditService(db), queue, connections, zerolog.Nop())
	service.SetStructureService(structure)
	t.Cleanup(service.Close)
	metadataArtifacts := true
	library, err := service.Create(context.Background(), actor, MediaLibraryInput{Name: "115 media", StorageID: storage.ID, ProfileID: profile.ID, RelativeRoot: "/", ProviderRootID: storage.RootPath, Enabled: false, Recursive: true, VideoExtensions: []string{".mkv"}, STRMEnabled: true, STRMLocalRoot: t.TempDir(), MetadataArtifactsEnabled: &metadataArtifacts, TransferMode: models.MediaLibraryTransferCopy}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	first, err := service.ScanNow(context.Background(), actor, library.ID)
	if err != nil || first.Added != 1 {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	if first.Status != "catalog_ready" || first.Phase != "recognition_queued" || first.Persisted != 1 || first.RecognitionTotal != 1 {
		t.Fatalf("fast 115 scan did not publish before recognition: %+v", first)
	}
	var recognitionJob models.Job
	if err := db.First(&recognitionJob, "job_type = ?", JobTypeMediaLibraryRecognition).Error; err != nil {
		t.Fatalf("recognition job was not queued: %v", err)
	}
	if recognitionJob.ResourceKey != mediaArtifactResourceKey(library.ID) {
		t.Fatalf("recognition and artifact work were not serialized: resource=%q", recognitionJob.ResourceKey)
	}
	initialDiagnosis, err := queue.Claim([]string{JobTypeMediaLibraryStructureDiagnosis})
	if err != nil || initialDiagnosis != nil {
		t.Fatalf("automatic structure diagnosis ran before recognition convergence: claimed=%+v err=%v", initialDiagnosis, err)
	}
	claimed, err := queue.Claim([]string{JobTypeMediaLibraryRecognition})
	if err != nil || claimed == nil {
		t.Fatalf("recognition job was not claimable: claimed=%+v err=%v", claimed, err)
	}
	workerResult := NewMediaLibraryRecognitionWorker(service).Run(context.Background(), fastScanTestRuntime{}, *claimed)
	if workerResult.ErrorCode != "" {
		t.Fatalf("recognition worker failed: %+v", workerResult)
	}
	var diagnosisJobs int64
	if err := db.Model(&models.Job{}).Where("job_type = ?", JobTypeMediaLibraryStructureDiagnosis).Count(&diagnosisJobs).Error; err != nil || diagnosisJobs != 1 {
		t.Fatalf("recognition convergence did not enqueue exactly one diagnosis: count=%d err=%v", diagnosisJobs, err)
	}
	refreshedDiagnosis, err := queue.Claim([]string{JobTypeMediaLibraryStructureDiagnosis})
	if err != nil || refreshedDiagnosis == nil {
		t.Fatalf("post-recognition structure diagnosis was not claimable: claimed=%+v err=%v", refreshedDiagnosis, err)
	}
	if result := NewMediaLibraryStructureDiagnosisWorker(structure).Run(context.Background(), fastScanTestRuntime{}, *refreshedDiagnosis); result.ErrorCode != "" {
		t.Fatalf("post-recognition structure diagnosis failed: %+v", result)
	}
	if err := queue.Complete(refreshedDiagnosis.Job.ID, refreshedDiagnosis.LeaseToken); err != nil {
		t.Fatal(err)
	}
	completedDiagnostics, err := structure.Diagnostics(context.Background(), actor, library.ID)
	if err != nil {
		t.Fatal(err)
	}
	if completedDiagnostics.Unrecognized != 1 || completedDiagnostics.Classifications.Unrecognized != 1 {
		t.Fatalf("completed no-match recognition was not exposed for manual recovery: %+v", completedDiagnostics)
	}
	if err := queue.Complete(claimed.Job.ID, claimed.LeaseToken); err != nil {
		t.Fatal(err)
	}
	var completedRun models.MediaLibraryScanRun
	if err := db.First(&completedRun, first.ID).Error; err != nil || completedRun.Status != "success" || completedRun.Phase != "completed" || completedRun.RecognitionCompleted != 1 {
		t.Fatalf("recognition did not complete scan run: run=%+v err=%v", completedRun, err)
	}
	unchanged, err := service.ScanNow(context.Background(), actor, library.ID)
	if err != nil || unchanged.Status != "success" || unchanged.Phase != "completed" || unchanged.RecognitionTotal != 0 || unchanged.CacheHits != 1 {
		t.Fatalf("unchanged scan did not reuse recognition: run=%+v err=%v", unchanged, err)
	}
	var recognitionJobs int64
	if err := db.Model(&models.Job{}).Where("job_type = ?", JobTypeMediaLibraryRecognition).Count(&recognitionJobs).Error; err != nil || recognitionJobs != 1 {
		t.Fatalf("unchanged scan queued recognition work: count=%d err=%v", recognitionJobs, err)
	}
	var firstArtifactRun models.MediaArtifactRun
	if err := db.Where("library_id = ? AND generation = ?", library.ID, first.Generation).First(&firstArtifactRun).Error; err != nil || firstArtifactRun.Status != models.MediaArtifactStatusQueued {
		t.Fatalf("115 scan did not immediately schedule artifact generation: run=%+v err=%v", firstArtifactRun, err)
	}
	var firstArtifactJob models.Job
	if err := db.First(&firstArtifactJob, "job_type = ?", JobTypeMediaArtifact).Error; err != nil || firstArtifactJob.ResourceKey != mediaArtifactResourceKey(library.ID) {
		t.Fatalf("115 scan artifact job=%+v err=%v", firstArtifactJob, err)
	}
	driver.children["cloud-root"] = []cloud.Item{{ID: "video-id", ParentID: "cloud-root", Name: "After.2026.mkv", Size: 128, ModifiedAt: modified}}
	second, err := service.ScanNow(context.Background(), actor, library.ID)
	if err != nil {
		t.Fatal(err)
	}
	if second.Added != 0 || second.Updated != 1 || second.Removed != 0 {
		t.Fatalf("rename run=%+v", second)
	}
	var entries []models.MediaLibraryEntry
	if err := db.Where("library_id = ?", library.ID).Find(&entries).Error; err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].ProviderID != "video-id" || entries[0].RelativePath != "/After.2026.mkv" {
		t.Fatalf("entries=%+v", entries)
	}
}

func TestPan115FastScanPublishes12171EntriesBeforeRecognition(t *testing.T) {
	modified := time.Now().UTC().Truncate(time.Second)
	items := make([]cloud.Item, 12171)
	for index := range items {
		name := fmt.Sprintf("Movie.%05d.2026.mkv", index)
		items[index] = cloud.Item{ID: "video-" + strconv.Itoa(index), ParentID: "cloud-root", Name: name, Size: int64(1024 + index), ModifiedAt: modified}
	}
	driver := &fakeCloudDriver{
		items:    map[string]cloud.Item{"cloud-root": {ID: "cloud-root", ParentID: "0", Name: "媒体", IsDir: true}},
		children: map[string][]cloud.Item{"cloud-root": items},
	}
	db, _, connections, actor := newConnectionTestService(t, driver)
	for _, permission := range []string{authz.PermissionMediaLibrariesRead, authz.PermissionMediaLibrariesCreate, authz.PermissionMediaLibrariesScan} {
		actor.Permissions[permission] = struct{}{}
	}
	connection, err := connections.Create(actor, ConnectionInput{Name: "115 performance account", Provider: cloud.ProviderPan115, Cookie: testPan115Cookie, Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	storageService := NewStorageService(db, NewAuditService(db))
	storageService.SetConnectionService(connections)
	storage, err := storageService.CreateContext(context.Background(), actor, StorageInput{Name: "115 performance root", Type: models.StorageTypePan115, RootPath: "cloud-root", RootDisplayPath: "/媒体", ConnectionID: &connection.ID, Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	var profile models.MediaClassificationProfile
	if err := db.Order("id").First(&profile).Error; err != nil {
		t.Fatal(err)
	}
	service := NewMediaLibraryService(db, NewAuditService(db), zerolog.Nop())
	service.SetConnectionService(connections)
	service.SetQueueService(NewQueueService(db, NewAuditService(db)))
	t.Cleanup(service.Close)
	library, err := service.Create(context.Background(), actor, MediaLibraryInput{Name: "115 performance library", StorageID: storage.ID, ProfileID: profile.ID, RelativeRoot: "/", ProviderRootID: storage.RootPath, Enabled: false, Recursive: true, VideoExtensions: []string{".mkv"}, TransferMode: models.MediaLibraryTransferCopy}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	run, err := service.ScanNow(context.Background(), actor, library.ID)
	elapsed := time.Since(started)
	if err != nil || run.Status != "catalog_ready" || run.Added != len(items) || run.Persisted != len(items) {
		t.Fatalf("run=%+v elapsed=%s err=%v", run, elapsed, err)
	}
	if elapsed >= 60*time.Second {
		t.Fatalf("12171-entry catalog publication took %s", elapsed)
	}
	var count int64
	if err := db.Model(&models.MediaLibraryEntry{}).Where("library_id = ?", library.ID).Count(&count).Error; err != nil || count != int64(len(items)) {
		t.Fatalf("published entries=%d err=%v", count, err)
	}
}

func TestFastScanStagingResumesCommittedCheckpoint(t *testing.T) {
	service, db, actor, storage, profile := mediaLibraryTestService(t)
	library, err := service.Create(context.Background(), actor, testLibraryInput("Staging resume", storage, profile, false), RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	run := models.MediaLibraryScanRun{LibraryID: library.ID, Kind: "full", Status: "running", Phase: "staging", Generation: 1, CheckpointJSON: `{"next_row":5,"total":7}`, StartedAt: time.Now().UTC()}
	if err := db.Create(&run).Error; err != nil {
		t.Fatal(err)
	}
	created := time.Now().UTC().Add(-time.Hour)
	staged := make([]models.MediaLibraryScanStaging, 5)
	files := make([]medialibrary.File, 7)
	for index := range files {
		path := fmt.Sprintf("/Movie.%02d.mkv", index)
		files[index] = medialibrary.File{RelativePath: path, ProviderID: fmt.Sprintf("provider-%02d", index), ProviderIDStable: true, Size: int64(index + 1), ModifiedAt: created}
		if index < len(staged) {
			staged[index] = models.MediaLibraryScanStaging{RunID: run.ID, LibraryID: library.ID, ItemKind: "video", RelativePath: path, ProviderID: files[index].ProviderID, Size: files[index].Size, ModifiedAt: created, RowOffset: index, CreatedAt: created, UpdatedAt: created}
		}
	}
	if err := db.Create(&staged).Error; err != nil {
		t.Fatal(err)
	}
	if err := service.stageFastMediaLibraryScan(context.Background(), &run, medialibrary.Result{Files: files}, serverlog.OperationLibraryFullScan); err != nil {
		t.Fatal(err)
	}
	var rows []models.MediaLibraryScanStaging
	if err := db.Where("run_id = ?", run.ID).Order("row_offset").Find(&rows).Error; err != nil || len(rows) != len(files) {
		t.Fatalf("rows=%d err=%v", len(rows), err)
	}
	if !rows[0].CreatedAt.Equal(created) {
		t.Fatalf("checkpoint rows were rewritten: got=%s want=%s", rows[0].CreatedAt, created)
	}
	if run.Persisted != len(files) || !strings.Contains(run.CheckpointJSON, `"next_row":7`) {
		t.Fatalf("run checkpoint=%s persisted=%d", run.CheckpointJSON, run.Persisted)
	}
}

func TestFastScanStagingRejectsNonPrefixCheckpoint(t *testing.T) {
	service, db, actor, storage, profile := mediaLibraryTestService(t)
	library, err := service.Create(context.Background(), actor, testLibraryInput("Staging prefix guard", storage, profile, false), RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	run := models.MediaLibraryScanRun{LibraryID: library.ID, Kind: "full", Status: "running", Phase: "staging", Generation: 1, CheckpointJSON: `{"next_row":2,"total":3}`, StartedAt: time.Now().UTC()}
	if err := db.Create(&run).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	// These rows prove only that two facts exist, not that rows 0 and 1 were
	// committed. A count-only resume check would incorrectly skip the prefix.
	stale := []models.MediaLibraryScanStaging{
		{RunID: run.ID, LibraryID: library.ID, ItemKind: "video", RelativePath: "/stale-2.mkv", ProviderID: "stale-2", ModifiedAt: now, RowOffset: 2, CreatedAt: now, UpdatedAt: now},
		{RunID: run.ID, LibraryID: library.ID, ItemKind: "video", RelativePath: "/stale-3.mkv", ProviderID: "stale-3", ModifiedAt: now, RowOffset: 3, CreatedAt: now, UpdatedAt: now},
	}
	if err := db.Create(&stale).Error; err != nil {
		t.Fatal(err)
	}
	files := []medialibrary.File{
		{RelativePath: "/Movie.00.mkv", ProviderID: "provider-0", ProviderIDStable: true, ModifiedAt: now},
		{RelativePath: "/Movie.01.mkv", ProviderID: "provider-1", ProviderIDStable: true, ModifiedAt: now},
		{RelativePath: "/Movie.02.mkv", ProviderID: "provider-2", ProviderIDStable: true, ModifiedAt: now},
	}
	if err := service.stageFastMediaLibraryScan(context.Background(), &run, medialibrary.Result{Files: files}, serverlog.OperationLibraryFullScan); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/Movie.00.mkv", "/Movie.01.mkv", "/Movie.02.mkv"} {
		var count int64
		if err := db.Model(&models.MediaLibraryScanStaging{}).Where("run_id = ? AND relative_path = ?", run.ID, path).Count(&count).Error; err != nil || count != 1 {
			t.Fatalf("path=%s count=%d err=%v", path, count, err)
		}
	}
}

func TestFastScanStagingRejectsDuplicateOffsetCheckpoint(t *testing.T) {
	service, db, actor, storage, profile := mediaLibraryTestService(t)
	library, err := service.Create(context.Background(), actor, testLibraryInput("Staging duplicate prefix guard", storage, profile, false), RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	run := models.MediaLibraryScanRun{LibraryID: library.ID, Kind: "full", Status: "running", Phase: "staging", Generation: 1, CheckpointJSON: `{"next_row":2,"total":3}`, StartedAt: time.Now().UTC()}
	if err := db.Create(&run).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	stale := []models.MediaLibraryScanStaging{
		{RunID: run.ID, LibraryID: library.ID, ItemKind: "video", RelativePath: "/stale-a.mkv", ProviderID: "stale-a", ModifiedAt: now, RowOffset: 0, CreatedAt: now, UpdatedAt: now},
		{RunID: run.ID, LibraryID: library.ID, ItemKind: "video", RelativePath: "/stale-b.mkv", ProviderID: "stale-b", ModifiedAt: now, RowOffset: 0, CreatedAt: now, UpdatedAt: now},
	}
	if err := db.Create(&stale).Error; err != nil {
		t.Fatal(err)
	}
	files := []medialibrary.File{
		{RelativePath: "/Movie.00.mkv", ProviderID: "provider-0", ProviderIDStable: true, ModifiedAt: now},
		{RelativePath: "/Movie.01.mkv", ProviderID: "provider-1", ProviderIDStable: true, ModifiedAt: now},
		{RelativePath: "/Movie.02.mkv", ProviderID: "provider-2", ProviderIDStable: true, ModifiedAt: now},
	}
	if err := service.stageFastMediaLibraryScan(context.Background(), &run, medialibrary.Result{Files: files}, serverlog.OperationLibraryFullScan); err != nil {
		t.Fatal(err)
	}
	var newPrefix int64
	if err := db.Model(&models.MediaLibraryScanStaging{}).Where("run_id = ? AND relative_path IN ?", run.ID, []string{"/Movie.00.mkv", "/Movie.01.mkv"}).Count(&newPrefix).Error; err != nil {
		t.Fatal(err)
	}
	if newPrefix != 2 {
		t.Fatalf("duplicate offsets incorrectly authorized checkpoint resume: new_prefix=%d", newPrefix)
	}
}

func TestFastPartialScanPreservesUnseenRecognition(t *testing.T) {
	service, db, actor, storage, profile := mediaLibraryTestService(t)
	queue := NewQueueService(db, NewAuditService(db))
	service.SetQueueService(queue)
	created, err := service.Create(context.Background(), actor, testLibraryInput("Partial recognition guard", storage, profile, false), RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	if err := db.Model(&models.MediaLibrary{}).Where("id = ?", created.ID).Updates(map[string]any{"dirty_generation": 1, "baseline_generation": 1}).Error; err != nil {
		t.Fatal(err)
	}
	var library models.MediaLibrary
	if err := db.First(&library, created.ID).Error; err != nil {
		t.Fatal(err)
	}
	oldRecognition := models.MediaLibraryRecognition{
		LibraryID: library.ID, SourceKey: "old-source", InputFingerprint: "old-fingerprint", ProfileID: profile.ID, ProfileRevision: profile.Revision,
		Status: mediaRecognitionStatusMatched, MediaType: "movie", Title: "保留作品", MetadataJSON: "{}", LastGeneration: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&oldRecognition).Error; err != nil {
		t.Fatal(err)
	}
	oldEntry := models.MediaLibraryEntry{
		LibraryID: library.ID, RelativePath: "/Old.2025.mkv", ProviderID: "old-provider", RecognitionID: &oldRecognition.ID,
		MediaType: "movie", Title: "保留作品", WorkKey: "tmdb:movie:1", MatchStatus: mediaRecognitionStatusMatched,
		LastGeneration: 1, ModifiedAt: now, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&oldEntry).Error; err != nil {
		t.Fatal(err)
	}
	run := models.MediaLibraryScanRun{LibraryID: library.ID, Kind: "event", Status: "running", Phase: "enumerating", Generation: 2, SourceFingerprint: mediaLibraryScanSourceFingerprint(library, storage, profile), CheckpointJSON: "{}", StartedAt: now}
	if err := db.Create(&run).Error; err != nil {
		t.Fatal(err)
	}
	result := medialibrary.Result{Partial: true, Enumerated: 1, Files: []medialibrary.File{{RelativePath: "/New.2026.mkv", ProviderID: "new-provider", ProviderIDStable: true, Size: 10, ModifiedAt: now}}}
	published, err := service.publishFastPan115Scan(context.Background(), library, storage, profile, run, result, time.Now(), serverlog.OperationLibraryEventScan)
	if err != nil || published.Status != "catalog_ready" || !published.Partial {
		t.Fatalf("published=%+v err=%v", published, err)
	}
	var oldRecognitionCount, oldEntryCount int64
	if err := db.Model(&models.MediaLibraryRecognition{}).Where("id = ?", oldRecognition.ID).Count(&oldRecognitionCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.MediaLibraryEntry{}).Where("id = ? AND recognition_id = ?", oldEntry.ID, oldRecognition.ID).Count(&oldEntryCount).Error; err != nil {
		t.Fatal(err)
	}
	if oldRecognitionCount != 1 || oldEntryCount != 1 || published.Removed != 0 {
		t.Fatalf("unseen partial state was pruned: recognition=%d entry=%d removed=%d", oldRecognitionCount, oldEntryCount, published.Removed)
	}
	claimed, err := queue.Claim([]string{JobTypeMediaLibraryRecognition})
	if err != nil || claimed == nil {
		t.Fatalf("recognition job was not claimable: claimed=%+v err=%v", claimed, err)
	}
	if result := NewMediaLibraryRecognitionWorker(service).Run(context.Background(), fastScanTestRuntime{}, *claimed); result.ErrorCode != "" {
		t.Fatalf("partial recognition worker failed: %+v", result)
	}
	if err := queue.Complete(claimed.Job.ID, claimed.LeaseToken); err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.MediaLibraryRecognition{}).Where("id = ?", oldRecognition.ID).Count(&oldRecognitionCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.MediaLibraryEntry{}).Where("id = ? AND recognition_id = ?", oldEntry.ID, oldRecognition.ID).Count(&oldEntryCount).Error; err != nil {
		t.Fatal(err)
	}
	if oldRecognitionCount != 1 || oldEntryCount != 1 {
		t.Fatalf("background recognition pruned unseen partial state: recognition=%d entry=%d", oldRecognitionCount, oldEntryCount)
	}
}

func TestFastMixedScanAdvancesReusedRecognitionGeneration(t *testing.T) {
	service, db, actor, storage, profile := mediaLibraryTestService(t)
	service.SetQueueService(NewQueueService(db, NewAuditService(db)))
	created, err := service.Create(context.Background(), actor, testLibraryInput("Mixed recognition generation", storage, profile, false), RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.MediaLibrary{}).Where("id = ?", created.ID).Updates(map[string]any{"dirty_generation": 1, "baseline_generation": 1}).Error; err != nil {
		t.Fatal(err)
	}
	var library models.MediaLibrary
	if err := db.First(&library, created.ID).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	cachedFile := medialibrary.File{RelativePath: "/Cached.2025.mkv", ProviderID: "cached-provider", ProviderIDStable: true, Size: 20, ModifiedAt: now}
	cachedUnit := medialibrary.GroupRecognitionUnits([]medialibrary.File{cachedFile})[0]
	recognition := models.MediaLibraryRecognition{
		LibraryID: library.ID, SourceKey: cachedUnit.SourceKey, InputFingerprint: cachedUnit.InputFingerprint, ProfileID: profile.ID, ProfileRevision: profile.Revision,
		Status: mediaRecognitionStatusMatched, MediaType: "movie", Title: "缓存作品", MetadataJSON: currentRecognitionMetadataJSON(t), LastGeneration: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&recognition).Error; err != nil {
		t.Fatal(err)
	}
	entry := models.MediaLibraryEntry{
		LibraryID: library.ID, RelativePath: cachedFile.RelativePath, ProviderID: cachedFile.ProviderID, RecognitionID: &recognition.ID,
		Size: cachedFile.Size, ModifiedAt: cachedFile.ModifiedAt, MediaType: "movie", Title: recognition.Title, WorkKey: "tmdb:movie:1",
		MatchStatus: mediaRecognitionStatusMatched, LastGeneration: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&entry).Error; err != nil {
		t.Fatal(err)
	}
	run := models.MediaLibraryScanRun{LibraryID: library.ID, Kind: "full", Status: "running", Phase: "enumerating", Generation: 2, SourceFingerprint: mediaLibraryScanSourceFingerprint(library, storage, profile), CheckpointJSON: "{}", StartedAt: now}
	if err := db.Create(&run).Error; err != nil {
		t.Fatal(err)
	}
	newFile := medialibrary.File{RelativePath: "/New.2026.mkv", ProviderID: "new-provider", ProviderIDStable: true, Size: 10, ModifiedAt: now}
	published, err := service.publishFastPan115Scan(context.Background(), library, storage, profile, run, medialibrary.Result{Files: []medialibrary.File{cachedFile, newFile}, Enumerated: 2}, time.Now(), serverlog.OperationLibraryFullScan)
	if err != nil || published.RecognitionTotal != 1 || published.CacheHits != 1 {
		t.Fatalf("published=%+v err=%v", published, err)
	}
	if err := db.First(&recognition, recognition.ID).Error; err != nil {
		t.Fatal(err)
	}
	if recognition.LastGeneration != run.Generation {
		t.Fatalf("reused recognition generation=%d want=%d", recognition.LastGeneration, run.Generation)
	}
}

func TestFastScanInvalidatesAutomaticProjectionFromOlderRecognitionEngine(t *testing.T) {
	service, db, actor, storage, profile := mediaLibraryTestService(t)
	service.SetQueueService(NewQueueService(db, NewAuditService(db)))
	created, err := service.Create(context.Background(), actor, testLibraryInput("Recognition engine refresh", storage, profile, false), RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.MediaLibrary{}).Where("id = ?", created.ID).Updates(map[string]any{"dirty_generation": 1, "baseline_generation": 1}).Error; err != nil {
		t.Fatal(err)
	}
	var library models.MediaLibrary
	if err := db.First(&library, created.ID).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	file := medialibrary.File{RelativePath: "/电影/吉卜力工作室特别短片合辑/吉卜力工作室特别短片合辑.mp4", ProviderID: "old-engine-provider", ProviderIDStable: true, Size: 20, ModifiedAt: now}
	unit := medialibrary.GroupRecognitionUnits([]medialibrary.File{file})[0]
	wrongID := int64(1)
	stale := models.MediaLibraryRecognition{
		LibraryID: library.ID, SourceKey: unit.SourceKey, InputFingerprint: unit.InputFingerprint, ProfileID: profile.ID, ProfileRevision: profile.Revision,
		Status: mediaRecognitionStatusMatched, MediaType: "movie", Title: "电影人", TMDBID: &wrongID,
		MetadataJSON: `{"version":1,"engine_version":"nextgen-domain-v10","classification":{"MediaType":"movie"}}`, LastGeneration: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&stale).Error; err != nil {
		t.Fatal(err)
	}
	entry := models.MediaLibraryEntry{
		LibraryID: library.ID, RelativePath: file.RelativePath, ProviderID: file.ProviderID, RecognitionID: &stale.ID,
		Size: file.Size, ModifiedAt: file.ModifiedAt, MediaType: "movie", Title: stale.Title, WorkKey: "movie:tmdb:1",
		MatchStatus: mediaRecognitionStatusMatched, TMDBID: &wrongID, LastGeneration: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&entry).Error; err != nil {
		t.Fatal(err)
	}
	run := models.MediaLibraryScanRun{LibraryID: library.ID, Kind: "full", Status: "running", Phase: "enumerating", Generation: 2, SourceFingerprint: mediaLibraryScanSourceFingerprint(library, storage, profile), CheckpointJSON: "{}", StartedAt: now}
	if err := db.Create(&run).Error; err != nil {
		t.Fatal(err)
	}
	published, err := service.publishFastPan115Scan(context.Background(), library, storage, profile, run, medialibrary.Result{Files: []medialibrary.File{file}, Enumerated: 1}, time.Now(), serverlog.OperationLibraryFullScan)
	if err != nil || published.RecognitionTotal != 1 || published.CacheHits != 0 || published.Status != "catalog_ready" {
		t.Fatalf("stale projection was reused: published=%+v err=%v", published, err)
	}
	var refreshed models.MediaLibraryEntry
	if err := db.First(&refreshed, entry.ID).Error; err != nil {
		t.Fatal(err)
	}
	if refreshed.MatchStatus != mediaRecognitionStatusPending || refreshed.RecognitionID != nil || refreshed.TMDBID != nil || refreshed.Title == stale.Title {
		t.Fatalf("old engine identity remained in published catalog: entry=%+v", refreshed)
	}
}

func TestFastTVScanAddsEpisodeWithoutReidentifyingWork(t *testing.T) {
	service, db, actor, storage, profile := mediaLibraryTestService(t)
	created, err := service.Create(context.Background(), actor, testLibraryInput("Stable TV work identity", storage, profile, false), RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.MediaLibrary{}).Where("id = ?", created.ID).Updates(map[string]any{"dirty_generation": 1, "baseline_generation": 1}).Error; err != nil {
		t.Fatal(err)
	}
	var library models.MediaLibrary
	if err := db.First(&library, created.ID).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	first := medialibrary.File{RelativePath: "/哆啦A梦 (2005)/Season 01/哆啦A梦 0001.mp4", ProviderID: "episode-provider-1", ProviderIDStable: true, Size: 10, ModifiedAt: now}
	second := medialibrary.File{RelativePath: "/哆啦A梦 (2005)/Season 01/哆啦A梦 0002.mp4", ProviderID: "episode-provider-0", ProviderIDStable: true, Size: 20, ModifiedAt: now}
	unparsed := medialibrary.File{RelativePath: "/哆啦A梦 (2005)/Season 01/特别篇.mp4", ProviderID: "episode-provider-special", ProviderIDStable: true, Size: 30, ModifiedAt: now}
	tmdbID := int64(65733)
	recognition := models.MediaLibraryRecognition{
		LibraryID: library.ID, SourceKey: "legacy-provider-anchor", InputFingerprint: "legacy-whole-series-fingerprint", ProfileID: profile.ID, ProfileRevision: profile.Revision,
		Status: mediaRecognitionStatusMatched, MediaType: "tv", Title: "哆啦A梦", TMDBID: &tmdbID, MetadataJSON: "{}", ManualOverride: true,
		LastGeneration: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&recognition).Error; err != nil {
		t.Fatal(err)
	}
	season, episode := 1, 1
	entry := models.MediaLibraryEntry{
		LibraryID: library.ID, RelativePath: first.RelativePath, ProviderID: first.ProviderID, RecognitionID: &recognition.ID,
		Size: first.Size, ModifiedAt: first.ModifiedAt, MediaType: "tv", Title: recognition.Title, SeriesTitle: recognition.Title,
		Season: &season, Episode: &episode, WorkKey: recognitionWorkKey(MediaRecognitionResult{Status: recognition.Status, MediaType: recognition.MediaType, Title: recognition.Title, TMDBID: recognition.TMDBID}, recognition.SourceKey),
		MatchStatus: mediaRecognitionStatusMatched, TMDBID: &tmdbID, LastGeneration: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&entry).Error; err != nil {
		t.Fatal(err)
	}
	run := models.MediaLibraryScanRun{LibraryID: library.ID, Kind: "full", Status: "running", Phase: "enumerating", Generation: 2, SourceFingerprint: mediaLibraryScanSourceFingerprint(library, storage, profile), CheckpointJSON: "{}", StartedAt: now}
	if err := db.Create(&run).Error; err != nil {
		t.Fatal(err)
	}
	published, err := service.publishFastPan115Scan(context.Background(), library, storage, profile, run, medialibrary.Result{Files: []medialibrary.File{first, second, unparsed}, Enumerated: 3}, time.Now(), serverlog.OperationLibraryFullScan)
	if err != nil || published.Status != "success" || published.RecognitionTotal != 0 || published.CacheHits != 1 {
		t.Fatalf("published=%+v err=%v", published, err)
	}
	var added models.MediaLibraryEntry
	if err := db.Where("library_id = ? AND relative_path = ?", library.ID, second.RelativePath).First(&added).Error; err != nil {
		t.Fatal(err)
	}
	if added.RecognitionID == nil || *added.RecognitionID != recognition.ID || added.Season == nil || *added.Season != 1 || added.Episode == nil || *added.Episode != 2 || added.Title != "哆啦A梦" {
		t.Fatalf("added entry=%+v", added)
	}
	var pendingEpisode models.MediaLibraryEntry
	if err := db.Where("library_id = ? AND relative_path = ?", library.ID, unparsed.RelativePath).First(&pendingEpisode).Error; err != nil {
		t.Fatal(err)
	}
	if pendingEpisode.RecognitionID == nil || *pendingEpisode.RecognitionID != recognition.ID || pendingEpisode.MatchStatus != mediaRecognitionStatusMatched || pendingEpisode.Season == nil || *pendingEpisode.Season != 1 || pendingEpisode.Episode != nil {
		t.Fatalf("unparsed episode downgraded work recognition: %+v", pendingEpisode)
	}
	var persisted models.MediaLibraryRecognition
	if err := db.First(&persisted, recognition.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !persisted.ManualOverride || persisted.LastGeneration != run.Generation {
		t.Fatalf("recognition=%+v", persisted)
	}
	var recognitionCount int64
	if err := db.Model(&models.MediaLibraryRecognition{}).Where("library_id = ?", library.ID).Count(&recognitionCount).Error; err != nil {
		t.Fatal(err)
	}
	if recognitionCount != 1 {
		t.Fatalf("recognition count=%d", recognitionCount)
	}
}

func TestFastScanRejectsChangedConfigurationBeforeCatalogPublish(t *testing.T) {
	service, db, actor, storage, profile := mediaLibraryTestService(t)
	created, err := service.Create(context.Background(), actor, testLibraryInput("Configuration guard", storage, profile, false), RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	var library models.MediaLibrary
	if err := db.First(&library, created.ID).Error; err != nil {
		t.Fatal(err)
	}
	run := models.MediaLibraryScanRun{LibraryID: library.ID, Kind: "full", Status: "running", Phase: "enumerating", Generation: library.DirtyGeneration + 1, SourceFingerprint: mediaLibraryScanSourceFingerprint(library, storage, profile), CheckpointJSON: "{}", StartedAt: time.Now().UTC()}
	if err := db.Create(&run).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&library).Update("ignore_patterns_json", `["changed-during-scan"]`).Error; err != nil {
		t.Fatal(err)
	}
	result := medialibrary.Result{Files: []medialibrary.File{{RelativePath: "/Guard.2026.mkv", ProviderID: "guard-provider", ProviderIDStable: true, Size: 10, ModifiedAt: time.Now().UTC()}}}
	failed, err := service.publishFastPan115Scan(context.Background(), library, storage, profile, run, result, time.Now(), serverlog.OperationLibraryFullScan)
	if err == nil || failed.Status != "failed" || failed.DatabaseErrorClass != mediaLibraryDatabaseErrorConfigurationChanged {
		t.Fatalf("failed run=%+v err=%v", failed, err)
	}
	var catalogCount, stagingCount int64
	if countErr := db.Model(&models.MediaLibraryEntry{}).Where("library_id = ?", library.ID).Count(&catalogCount).Error; countErr != nil {
		t.Fatal(countErr)
	}
	if countErr := db.Model(&models.MediaLibraryScanStaging{}).Where("run_id = ?", run.ID).Count(&stagingCount).Error; countErr != nil {
		t.Fatal(countErr)
	}
	if catalogCount != 0 || stagingCount != 1 {
		t.Fatalf("catalog=%d staging=%d", catalogCount, stagingCount)
	}
}

func TestFastScanKeepsCatalogUsableWhenRecognitionEnqueueFails(t *testing.T) {
	service, db, actor, storage, profile := mediaLibraryTestService(t)
	created, err := service.Create(context.Background(), actor, testLibraryInput("Queue failure", storage, profile, false), RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	var library models.MediaLibrary
	if err := db.First(&library, created.ID).Error; err != nil {
		t.Fatal(err)
	}
	run := models.MediaLibraryScanRun{LibraryID: library.ID, Kind: "full", Status: "running", Phase: "enumerating", Generation: 1, SourceFingerprint: mediaLibraryScanSourceFingerprint(library, storage, profile), CheckpointJSON: "{}", StartedAt: time.Now().UTC()}
	if err := db.Create(&run).Error; err != nil {
		t.Fatal(err)
	}
	result := medialibrary.Result{Files: []medialibrary.File{{RelativePath: "/Queue.2026.mkv", ProviderID: "queue-provider", ProviderIDStable: true, Size: 10, ModifiedAt: time.Now().UTC()}}}
	published, err := service.publishFastPan115Scan(context.Background(), library, storage, profile, run, result, time.Now(), serverlog.OperationLibraryFullScan)
	if err != nil || published.Status != "catalog_ready" || published.Phase != "recognition_enqueue_failed" || published.ErrorCode != "media_library_recognition_enqueue_failed" {
		t.Fatalf("published run=%+v err=%v", published, err)
	}
	var count int64
	if err := db.Model(&models.MediaLibraryEntry{}).Where("library_id = ?", library.ID).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("catalog count=%d err=%v", count, err)
	}
}

func TestFastScanStartupRecoversFailedRecognitionPhase(t *testing.T) {
	service, db, actor, storage, profile := mediaLibraryTestService(t)
	queue := NewQueueService(db, NewAuditService(db))
	service.SetQueueService(queue)
	library, err := service.Create(context.Background(), actor, testLibraryInput("Recognition recovery", storage, profile, false), RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	run := models.MediaLibraryScanRun{LibraryID: library.ID, Kind: "full", Status: "catalog_ready", Phase: "recognition_failed", Generation: 1, RecognitionTotal: 1, StartedAt: time.Now().UTC()}
	if err := db.Create(&run).Error; err != nil {
		t.Fatal(err)
	}
	if err := service.recoverMediaLibraryRecognitionJobs(); err != nil {
		t.Fatal(err)
	}
	var job models.Job
	if err := db.First(&job, "job_type = ?", JobTypeMediaLibraryRecognition).Error; err != nil {
		t.Fatal(err)
	}
	if job.ResourceKey != mediaArtifactResourceKey(library.ID) {
		t.Fatalf("recovery job=%+v", job)
	}
}

func TestFastScanPublicStateAndMediaDisplayNameAreSafe(t *testing.T) {
	longName := strings.Repeat("电影", 100)
	clean := safeMediaDisplayName("\x00\r\n  " + longName + "  ")
	if strings.ContainsAny(clean, "\x00\r\n") || len([]rune(clean)) != 161 || !strings.HasSuffix(clean, "…") {
		t.Fatalf("unsafe display name %q", clean)
	}
	for _, private := range []string{`C:\Media\Secret.mkv`, `/mnt/private/Secret.mkv`, `https://cdn.example.test/video.mkv?token=secret`, `file:///C:/Media/Secret.mkv`, `magnet:?xt=urn:btih:secret`} {
		if got := safeMediaDisplayName(private); got != "未命名媒体" {
			t.Fatalf("private locator was rendered as media name: input=%q got=%q", private, got)
		}
	}
	run := models.MediaLibraryScanRun{LibraryID: 7, SourceFingerprint: "private-source", CheckpointJSON: `{"provider_id":"private-provider"}`}
	payload, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), "private-source") || strings.Contains(string(payload), "private-provider") || strings.Contains(string(payload), "checkpoint") || strings.Contains(string(payload), "source_fingerprint") {
		t.Fatalf("private scan state leaked: %s", payload)
	}
}

func TestFastScanOperationLogsNeverIncludeRawFailureOrLocator(t *testing.T) {
	service, _, _, _, _ := mediaLibraryTestService(t)
	var output bytes.Buffer
	service.log = zerolog.New(&output).Level(zerolog.DebugLevel)
	run := models.MediaLibraryScanRun{ID: 9, LibraryID: 7, Kind: "full", Generation: 3, StartedAt: time.Now().UTC()}
	logFastScanMediaAction(service.log, serverlog.OperationLibraryFullScan, run, "processing", "discovered", `file:///C:/Media/Secret.mkv`, "movie")
	_, _ = service.failFastScanPersistence(run, serverlog.OperationLibraryFullScan, time.Now(), mediaLibraryPersistenceStageEntries,
		errors.New(`Cookie=secret-cookie https://provider.example/private C:\Media\Secret.mkv INSERT INTO private VALUES ('upstream-body')`))
	logged := output.String()
	for _, forbidden := range []string{"secret-cookie", "provider.example", `C:\\Media`, "INSERT INTO", "upstream-body", "file:///"} {
		if strings.Contains(logged, forbidden) {
			t.Fatalf("private diagnostic leaked into operation log: forbidden=%q log=%s", forbidden, logged)
		}
	}
	if !strings.Contains(logged, "未命名媒体") || !strings.Contains(logged, mediaLibraryDatabaseErrorUnknown) {
		t.Fatalf("safe diagnostics missing: %s", logged)
	}
}

type fastScanTestRuntime struct{}

func (fastScanTestRuntime) Heartbeat(*float64, *int64, *int64, *float64, *int64) error { return nil }
func (fastScanTestRuntime) Checkpoint(any) error                                       { return nil }

func currentRecognitionMetadataJSON(t *testing.T) string {
	t.Helper()
	raw, err := marshalRecognitionMetadata(MediaRecognitionResult{})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
