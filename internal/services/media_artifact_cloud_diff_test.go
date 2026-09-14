package services

import (
	"context"
	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/classification"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/metadata/tmdb"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestLegacyFullArtifactWorkerSkipsHealthyCloudSidecarAndUpdatesChangedSource(t *testing.T) {
	var calls atomic.Int64
	assetBody := "first subtitle"
	assetServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/subtitle" {
			http.NotFound(response, request)
			return
		}
		calls.Add(1)
		response.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = response.Write([]byte(assetBody))
	}))
	defer assetServer.Close()
	driver := &fakeCloudDriver{signedProxy: true, directURL: assetServer.URL + "/subtitle", items: map[string]cloud.Item{
		"movie-file":    {ID: "movie-file", ParentID: "library-root", Name: "Movie.mkv", PickCode: "movie-pickcode", Size: 100},
		"disc-file":     {ID: "disc-file", ParentID: "library-root", Name: "Disc.iso", PickCode: "disc-pickcode", Size: 200},
		"subtitle-file": {ID: "subtitle-file", ParentID: "movie-dir", Name: "Movie.zh-CN.srt", PickCode: "subtitle-pickcode", Size: 42},
	}}
	db, store, connections, actor := newConnectionTestService(t, driver)
	connection, err := connections.Create(actor, ConnectionInput{Name: "Artifact account", Provider: cloud.ProviderPan115, Cookie: testPan115Cookie, Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	storage := models.Storage{Name: "Artifact cloud", NameNormalized: "artifact-cloud", Type: models.StorageTypePan115, RootPath: "storage-root", RootDisplayPath: "/媒体", RootPathNormalized: "pan115:artifact", ConnectionID: &connection.ID, Enabled: true, Capabilities: `{"temporary_direct_url":true,"signed_proxy":true}`, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&storage).Error; err != nil {
		t.Fatal(err)
	}
	var profile models.MediaClassificationProfile
	if err := db.Where("code = ?", "default-v1").First(&profile).Error; err != nil {
		t.Fatal(err)
	}
	projection := t.TempDir()
	library := models.MediaLibrary{Name: "Artifact library", NameNormalized: "artifact-library", StorageID: storage.ID, ProfileID: profile.ID, ProfileRevision: profile.Revision, RelativeRoot: "/", ProviderRootID: "library-root", Enabled: true, Recursive: true, VideoExtensionsJSON: `[".mkv",".iso"]`, IgnorePatternsJSON: `[]`, STRMEnabled: true, STRMLocalRoot: projection, SignedProxyEnabled: true, MetadataArtifactsEnabled: true, Status: models.MediaLibraryStatusListening, ArtifactStatus: models.MediaArtifactStatusIdle, DirtyGeneration: 1, CreatedAt: now, UpdatedAt: now}
	if err := db.Select("*").Create(&library).Error; err != nil {
		t.Fatal(err)
	}
	metadataJSON, err := marshalRecognitionMetadata(MediaRecognitionResult{Metadata: classification.Metadata{MediaType: classification.MediaTypeMovie, OriginalLanguage: "en"}, Snapshot: tmdb.Snapshot{Version: 1, TMDBID: 42, MediaType: "movie", Title: "Movie", OriginalTitle: "Movie", ReleaseDate: "2026-01-01"}})
	if err != nil {
		t.Fatal(err)
	}
	tmdbID, confidence := int64(42), .98
	recognition := models.MediaLibraryRecognition{LibraryID: library.ID, SourceKey: strings.Repeat("a", 64), InputFingerprint: strings.Repeat("b", 64), ProfileID: profile.ID, ProfileRevision: profile.Revision, Status: mediaRecognitionStatusMatched, MediaType: "movie", Title: "Movie", TMDBID: &tmdbID, Confidence: &confidence, MetadataJSON: metadataJSON, LastGeneration: 2, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&recognition).Error; err != nil {
		t.Fatal(err)
	}
	entries := []models.MediaLibraryEntry{
		{LibraryID: library.ID, RelativePath: "/Movies/Movie.mkv", ProviderID: "movie-file", RecognitionID: &recognition.ID, Size: 100, ModifiedAt: now, MediaType: "movie", Title: "Movie", MatchStatus: "matched", LastGeneration: 1, CreatedAt: now, UpdatedAt: now},
		{LibraryID: library.ID, RelativePath: "/Discs/Disc.iso", ProviderID: "disc-file", Size: 200, ModifiedAt: now, MediaType: "movie", Title: "Disc", MatchStatus: "unrecognized", LastGeneration: 1, CreatedAt: now, UpdatedAt: now},
	}
	if err := db.Create(&entries).Error; err != nil {
		t.Fatal(err)
	}
	sourceAsset := models.MediaLibrarySourceAsset{LibraryID: library.ID, Generation: 2, ProviderID: "subtitle-file", ParentProviderID: "movie-dir", RelativePath: "/Movies/Movie.zh-CN.srt", Name: "Movie.zh-CN.srt", Extension: ".srt", Size: 42, ModifiedAt: now, Active: true, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&sourceAsset).Error; err != nil {
		t.Fatal(err)
	}

	queue := NewQueueService(db, NewAuditService(db))
	proxy, err := NewSignedProxyService(db, store, connections, "https://media.example.test", zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	artifacts := NewMediaArtifactService(db, queue, proxy, zerolog.Nop())
	artifacts.SetConnectionService(connections)
	execute := func(g uint64) models.MediaArtifactRun {
		t.Helper()
		if err := db.Model(&recognition).Update("last_generation", g).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Model(&sourceAsset).Update("generation", g).Error; err != nil {
			t.Fatal(err)
		}
		finished := time.Now().UTC()
		scan := models.MediaLibraryScanRun{LibraryID: library.ID, Generation: g, Kind: "full", Status: "success", StartedAt: finished, FinishedAt: &finished}
		if err := db.Create(&scan).Error; err != nil {
			t.Fatal(err)
		}
		if err := artifacts.ScheduleGeneration(library.ID, g); err != nil {
			t.Fatal(err)
		}
		claim, err := queue.Claim([]string{JobTypeMediaArtifact})
		if err != nil || claim == nil {
			t.Fatalf("claim=%v err=%v", claim, err)
		}
		result := NewMediaArtifactWorker(artifacts).Run(context.Background(), &providerWakeRuntime{}, *claim)
		if result.ErrorCode != "" {
			t.Fatalf("worker=%+v", result)
		}
		if err := queue.Complete(claim.Job.ID, claim.LeaseToken); err != nil {
			t.Fatal(err)
		}
		var run models.MediaArtifactRun
		if err := db.Where("library_id = ? AND generation = ?", library.ID, g).First(&run).Error; err != nil {
			t.Fatal(err)
		}
		return run
	}
	execute(2)
	target := filepath.Join(projection, "Movies", "Movie.zh-CN.srt")
	before, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	healthy := execute(3)
	after, _ := os.Stat(target)
	if calls.Load() != 1 || healthy.ExpectedCount != 0 || !before.ModTime().Equal(after.ModTime()) {
		t.Fatalf("healthy triggered I/O: calls=%d run=%+v", calls.Load(), healthy)
	}
	assetBody = "updated subtitle"
	if err := db.Model(&sourceAsset).Update("hash_hint", "cloud-updated").Error; err != nil {
		t.Fatal(err)
	}
	changed := execute(4)
	body, err := os.ReadFile(target)
	if calls.Load() != 2 || changed.ExpectedCount != 1 || string(body) != assetBody || err != nil {
		t.Fatalf("changed not refreshed calls=%d run=%+v body=%s err=%v", calls.Load(), changed, body, err)
	}
	execute(5)
	if calls.Load() != 2 {
		t.Fatalf("stable retry downloaded sidecar again: %d", calls.Load())
	}
}
