package services

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/medialibrary"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/metadata/tmdb"
)

func installHydrationMetadata(t *testing.T, s *MediaLibraryService, handler http.HandlerFunc) {
	t.Helper()
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)
	client, err := tmdb.NewForTest("fixture", upstream.URL, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	metadata := NewMetadataSettingsService(s.db, NewAuditService(s.db), nil, tmdb.Credential{Kind: tmdb.CredentialKindReadAccessToken, Value: "fixture"})
	metadata.clientFactory = func(tmdb.Credential, string, string) (*tmdb.Client, error) { return client, nil }
	s.SetMetadataSettingsService(metadata)
}

func TestTransferMetadataHydrationPreservesIdentityAndEpisodes(t *testing.T) {
	s, _, _, _, profile := mediaLibraryTestService(t)
	var calls atomic.Int32
	installHydrationMetadata(t, s, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tv/42" {
			t.Errorf("unexpected identity search: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":42,"name":"Verified Show","overview":"Details","poster_path":"/poster.jpg","backdrop_path":"/back.jpg"}`)
	})
	id := int64(42)
	library := models.MediaLibrary{MetadataConcurrency: 4}
	ctx := context.WithValue(context.Background(), transferBatchContextKey{}, transferCatalogBatch{Identity: MediaIdentitySnapshot{TMDBID: &id, MediaType: "tv", Title: "Known Show"}})
	units := []medialibrary.RecognitionUnit{{SourceKey: "one"}, {SourceKey: "two"}}
	compact := MediaRecognitionResult{Status: mediaRecognitionStatusMatched, TMDBID: &id, MediaType: "tv", Title: "Manual Name", Snapshot: tmdb.Snapshot{Version: 1, TMDBID: id, MediaType: "tv", Title: "Manual Name", EpisodeLanguage: "zh-CN", EpisodeSeasons: []int{2}, EpisodeSnapshots: []tmdb.EpisodeSnapshot{{SeasonNumber: 2, EpisodeNumber: 1, Name: "Episode"}}}}
	raw, _ := marshalRecognitionMetadata(compact)
	existing := []models.MediaLibraryRecognition{{SourceKey: "one", Status: compact.Status, TMDBID: &id, MediaType: "tv", Title: compact.Title, MetadataJSON: raw, ManualOverride: true}}
	results, err := s.recognizeLibraryUnitsWithExisting(ctx, library, profile, units, existing)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("detail calls=%d, want one per identity", calls.Load())
	}
	if !results[0].Manual || results[0].Result.Title != "Manual Name" || len(results[0].Result.Snapshot.EpisodeSnapshots) != 1 || results[0].Result.Snapshot.EpisodeSeasons[0] != 2 {
		t.Fatalf("manual/episode data lost: %+v", results[0])
	}
	for _, result := range results {
		if !result.Result.Snapshot.DetailsFetched || result.Result.Snapshot.PosterPath != "/poster.jpg" {
			t.Fatalf("incomplete: %+v", result)
		}
	}
	// Subsequent episodes reuse the completed stored show, without a detail call.
	raw, _ = marshalRecognitionMetadata(results[1].Result)
	existing = []models.MediaLibraryRecognition{{SourceKey: "two", Status: compact.Status, TMDBID: &id, MediaType: "tv", MetadataJSON: raw}}
	if _, err = s.recognizeLibraryUnitsWithExisting(ctx, library, profile, units[1:], existing); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("re-fetched complete show")
	}
}

func TestTransferMetadataHydrationMissingImagesAndRetry(t *testing.T) {
	s, _, _, _, profile := mediaLibraryTestService(t)
	var calls atomic.Int32
	installHydrationMetadata(t, s, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":42,"name":"No images"}`)
	})
	id := int64(42)
	ctx := context.WithValue(context.Background(), transferBatchContextKey{}, transferCatalogBatch{Identity: MediaIdentitySnapshot{TMDBID: &id, MediaType: "tv", Title: "Known"}})
	unit := []medialibrary.RecognitionUnit{{SourceKey: "one"}}
	library := models.MediaLibrary{MetadataConcurrency: 1}
	if _, err := s.recognizeLibraryUnitsWithExisting(ctx, library, profile, unit, nil); err == nil {
		t.Fatal("failed details reported success")
	}
	results, err := s.recognizeLibraryUnitsWithExisting(ctx, library, profile, unit, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !results[0].Result.Snapshot.DetailsFetched || results[0].Result.Snapshot.PosterPath != "" {
		t.Fatal("successful empty images not marked fetched")
	}
	raw, _ := marshalRecognitionMetadata(results[0].Result)
	if _, err = s.recognizeLibraryUnitsWithExisting(ctx, library, profile, unit, []models.MediaLibraryRecognition{{SourceKey: "one", Status: mediaRecognitionStatusMatched, TMDBID: &id, MediaType: "tv", MetadataJSON: raw}}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("empty-artwork refetched: %d", calls.Load())
	}
}

func TestMetadataHydrationRetryGeneratesOnlyWorkArtifacts(t *testing.T) {
	s, db, actor, storage, profile := mediaLibraryTestService(t)
	now := time.Now().UTC()
	library := models.MediaLibrary{Name: "Hydration", NameNormalized: "hydration", StorageID: storage.ID, ProfileID: profile.ID, ProfileRevision: profile.Revision, RelativeRoot: "/", Enabled: true, MetadataArtifactsEnabled: true, Status: models.MediaLibraryStatusListening, DirtyGeneration: 1, VideoExtensionsJSON: `[".mkv"]`, IgnorePatternsJSON: `[]`, CreatedAt: now, UpdatedAt: now}
	if err := db.Select("*").Create(&library).Error; err != nil {
		t.Fatal(err)
	}
	id := int64(42)
	raw, _ := marshalRecognitionMetadata(MediaRecognitionResult{Snapshot: tmdb.Snapshot{Version: 1, TMDBID: id, MediaType: "movie", Title: "Known"}})
	record := models.MediaLibraryRecognition{LibraryID: library.ID, SourceKey: strings.Repeat("a", 64), InputFingerprint: strings.Repeat("b", 64), ProfileID: profile.ID, ProfileRevision: profile.Revision, Status: mediaRecognitionStatusMatched, TMDBID: &id, MediaType: "movie", Title: "Known", MetadataJSON: raw, LastGeneration: 1, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&record).Error; err != nil {
		t.Fatal(err)
	}
	entry := models.MediaLibraryEntry{LibraryID: library.ID, RecognitionID: &record.ID, RelativePath: "/Known.mkv", WorkKey: "movie:tmdb:42", MediaType: "movie", TMDBID: &id, MatchStatus: mediaRecognitionStatusMatched, LastGeneration: 1, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&entry).Error; err != nil {
		t.Fatal(err)
	}
	other := record
	other.ID = 0
	other.SourceKey = strings.Repeat("c", 64)
	if err := db.Create(&other).Error; err != nil {
		t.Fatal(err)
	}
	installHydrationMetadata(t, s, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/movie/42" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"id":42,"title":"Known","overview":"Full details","poster_path":"/poster.jpg","backdrop_path":"/back.jpg"}`)
			return
		}
		if strings.HasSuffix(r.URL.Path, ".jpg") {
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write([]byte{0xff, 0xd8, 0xff, 0xdb, 1, 2, 3})
			return
		}
		t.Errorf("unexpected fuzzy search %s", r.URL.Path)
		http.NotFound(w, r)
	})
	queue := NewQueueService(db, NewAuditService(db))
	artifacts := NewMediaArtifactService(db, queue, nil, zerolog.Nop())
	artifacts.SetMetadataSettingsService(s.metadata)
	s.artifacts = artifacts
	// Enqueue fails after metadata commits. The normal pending follow-up must
	// retain exact scope and recover without fetching/identifying the show again.
	artifacts.queue = nil
	if _, err := s.RetryRecognition(context.Background(), actor, library.ID, encodeRecognitionToken(record.ID), RequestContext{}); err == nil {
		t.Fatal("queue failure hidden")
	}
	var pending models.MediaLibraryScanRun
	if err := db.Where("library_id = ? AND kind = ?", library.ID, "metadata").First(&pending).Error; err != nil {
		t.Fatal(err)
	}
	var checkpoint batchArtifactCheckpoint
	if err := json.Unmarshal([]byte(pending.CheckpointJSON), &checkpoint); err != nil {
		t.Fatal(err)
	}
	if !checkpoint.Pending || len(checkpoint.EntryIDs) != 1 || checkpoint.EntryIDs[0] != entry.ID {
		t.Fatalf("lost/broadened checkpoint: %+v", checkpoint)
	}
	artifacts.queue = queue
	if err := s.recoverBatchArtifactFollowups(context.Background(), library.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&pending, pending.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(pending.CheckpointJSON), &checkpoint); err != nil || checkpoint.Pending {
		t.Fatalf("checkpoint still pending: %+v %v", checkpoint, err)
	}
	if err := db.Model(&models.MediaLibraryRecognition{}).Where("id = ?", record.ID).Update("manual_override", true).Error; err != nil {
		t.Fatal(err)
	}
	// An explicit detail recovery does not clear a user's manual identity.
	s.artifacts = nil
	if result, err := s.RetryRecognition(context.Background(), actor, library.ID, encodeRecognitionToken(record.ID), RequestContext{}); err != nil || !result.ManualOverride || result.TMDBID == nil || *result.TMDBID != id {
		t.Fatalf("manual recovery: %+v %v", result, err)
	}
	s.artifacts = artifacts
	var unchanged models.MediaLibraryRecognition
	if err := db.First(&unchanged, other.ID).Error; err != nil {
		t.Fatal(err)
	}
	if unchanged.LastGeneration != 1 || !unchanged.UpdatedAt.Equal(other.UpdatedAt) {
		t.Fatal("unrelated work mutated")
	}
	claimed, err := queue.Claim([]string{JobTypeMediaArtifact})
	if err != nil || claimed == nil {
		t.Fatalf("claim %v %v", claimed, err)
	}
	result := NewMediaArtifactWorker(artifacts).Run(context.Background(), &providerWakeRuntime{}, *claimed)
	if result.ErrorCode != "" {
		t.Fatalf("artifact result %+v", result)
	}
	for _, name := range []string{"Known.nfo", "Known-poster.jpg", "Known-fanart.jpg"} {
		data, err := os.ReadFile(filepath.Join(storage.RootPath, name))
		if err != nil || len(data) == 0 {
			t.Fatalf("%s: %v", name, err)
		}
		if strings.HasSuffix(name, ".nfo") && !strings.Contains(string(data), "Full details") {
			t.Fatal("NFO missing details")
		}
	}
}

func TestTransferMetadataTVFirstImportGeneratesArtwork(t *testing.T) {
	s, db, _, storage, profile := mediaLibraryTestService(t)
	now := time.Now().UTC()
	library := models.MediaLibrary{Name: "TV import", NameNormalized: "tv-import", StorageID: storage.ID, ProfileID: profile.ID, ProfileRevision: profile.Revision, RelativeRoot: "/", Enabled: true, Recursive: true, MetadataArtifactsEnabled: true, Status: models.MediaLibraryStatusListening, MetadataConcurrency: 2, VideoExtensionsJSON: `[".mkv"]`, IgnorePatternsJSON: `[]`, CreatedAt: now, UpdatedAt: now}
	if err := db.Select("*").Create(&library).Error; err != nil {
		t.Fatal(err)
	}
	installHydrationMetadata(t, s, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/tv/42" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"id":42,"name":"Show","overview":"Show detail","poster_path":"/poster.jpg","backdrop_path":"/back.jpg","seasons":[{"id":43,"season_number":1,"poster_path":"/season.jpg"}]}`)
			return
		}
		if strings.HasSuffix(r.URL.Path, ".jpg") {
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write([]byte{0xff, 0xd8, 0xff, 0xdb, 1, 2, 3})
			return
		}
		http.NotFound(w, r)
	})
	queue := NewQueueService(db, NewAuditService(db))
	artifacts := NewMediaArtifactService(db, queue, nil, zerolog.Nop())
	artifacts.SetMetadataSettingsService(s.metadata)
	s.artifacts = artifacts
	id := int64(42)
	batch := transferCatalogBatch{TaskID: "fixture-tv-import", Identity: MediaIdentitySnapshot{TMDBID: &id, MediaType: "tv", Title: "Show"}, Result: medialibrary.Result{Files: []medialibrary.File{{RelativePath: "/Show/Season 01/Show.S01E01.mkv", ProviderID: "fixture-file", Size: 100, ModifiedAt: now}}}}
	ctx := context.WithValue(context.Background(), transferBatchContextKey{}, batch)
	if _, err := s.reconcile(ctx, library.ID, "transfer_batch"); err != nil {
		t.Fatal(err)
	}
	claimed, err := queue.Claim([]string{JobTypeMediaArtifact})
	if err != nil || claimed == nil {
		t.Fatalf("claim %v %v", claimed, err)
	}
	result := NewMediaArtifactWorker(artifacts).Run(context.Background(), &providerWakeRuntime{}, *claimed)
	if result.ErrorCode != "" {
		t.Fatalf("artifact: %+v", result)
	}
	for _, name := range []string{"tvshow.nfo", "poster.jpg", "fanart.jpg", "season01-poster.jpg"} {
		data, err := os.ReadFile(filepath.Join(storage.RootPath, "Show", name))
		if err != nil || len(data) == 0 {
			t.Fatalf("%s: %v", name, err)
		}
		if name == "tvshow.nfo" && !strings.Contains(string(data), "Show detail") {
			t.Fatal("TV detail missing")
		}
	}
}
