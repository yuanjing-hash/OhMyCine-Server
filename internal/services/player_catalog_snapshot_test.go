package services

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/metadata/tmdb"
	"gorm.io/gorm"
)

func playerSnapshotFixture(t *testing.T) (*MediaLibraryService, *CatalogSnapshotStore, models.MediaLibrary, models.MediaLibraryRecognition, []models.MediaLibraryEntry, Actor) {
	t.Helper()
	store, library, rec, entries := catalogFixture(t)
	library.Enabled = true
	if err := store.writeDB.Model(&library).Update("enabled", true).Error; err != nil {
		t.Fatal(err)
	}
	rec.CategoryName = "Series"
	rec.MetadataJSON = playerSnapshotMetadata(t, "Show", "/old.jpg")
	for i := range entries {
		entries[i].CategoryName = "Series"
	}
	catalogConvert(t, store, library, rec, entries)
	s := NewMediaLibraryService(store.writeDB, NewAuditService(store.writeDB), zerolog.Nop())
	s.SetCatalogSnapshotStore(store)
	actor := Actor{Permissions: map[string]struct{}{authz.PermissionMediaLibrariesRead: {}}}
	return s, store, library, rec, entries, actor
}

func playerSnapshotMetadata(t *testing.T, title, poster string) string {
	t.Helper()
	value, err := marshalRecognitionMetadata(MediaRecognitionResult{Snapshot: tmdb.Snapshot{Version: 1, TMDBID: 42, MediaType: "tv", Title: title, PosterPath: poster}})
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func TestPlayerSnapshotPublicViewsAndPinnedDecoration(t *testing.T) {
	s, store, library, rec, entries, actor := playerSnapshotFixture(t)
	err := s.withCatalogRead(context.Background(), []uint{library.ID}, func(tx *gorm.DB, reader *CatalogReader) error {
		before, err := s.playerCatalogTx(tx, reader, actor, library.ID, MediaPageQuery{Page: 1, PageSize: 20})
		if err != nil {
			return err
		}
		if len(before.List) != 1 || before.List[0].PosterPath != "/old.jpg" {
			t.Fatalf("old projection: %+v", before)
		}
		c, token := catalogCandidate(t, store, library, "delta", 1)
		rec.Title, rec.ManualOverride, rec.MetadataJSON = "Corrected", true, playerSnapshotMetadata(t, "Corrected", "/new.jpg")
		if err := store.AppendBatch(context.Background(), c.ID, token, CatalogFactBatch{Recognitions: []models.CatalogRecognitionFact{CatalogRecognitionFromLegacy(rec)}}); err != nil {
			return err
		}
		catalogPublish(t, store, c, token)
		after, err := s.playerCatalogTx(tx, reader, actor, library.ID, MediaPageQuery{Page: 1, PageSize: 20})
		if err != nil {
			return err
		}
		if len(after.List) != 1 || after.List[0].Title != before.List[0].Title || after.List[0].PosterPath != "/old.jpg" {
			t.Fatalf("mixed pinned projection: %+v", after)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	libraries, err := s.PlayerLibraries(actor)
	if err != nil || len(libraries) != 1 || libraries[0].EntryCount != 2 || libraries[0].WorkCount != 1 {
		t.Fatalf("libraries=%+v err=%v", libraries, err)
	}
	categories, err := s.PlayerCategories(actor, library.ID)
	if err != nil || len(categories) != 1 || categories[0].Name != "Series" || categories[0].ItemCount != 1 {
		t.Fatalf("categories=%+v err=%v", categories, err)
	}
	page, err := s.PlayerSearch(actor, MediaPageQuery{Query: "Corrected", Page: 1, PageSize: 20})
	if err != nil || len(page.List) != 1 || page.List[0].Title != "Corrected" || page.List[0].PosterPath != "/new.jpg" {
		t.Fatalf("search=%+v err=%v", page, err)
	}
	detail, err := s.PlayerCatalogDetail(context.Background(), actor, library.ID, encodeCatalogToken(entries[0].WorkKey))
	if err != nil || len(detail.Versions) != 2 || detail.Item.PosterPath != "/new.jpg" {
		t.Fatalf("detail=%+v err=%v", detail, err)
	}
	for _, v := range detail.Versions {
		if v.Season == nil || *v.Season != 2 || v.Episode == nil || *v.Episode != 1 || v.PosterPath != "/new.jpg" {
			t.Fatalf("version facts lost: %+v", v)
		}
	}
	if detail.Versions[0].HistoryIdentity != detail.Versions[1].HistoryIdentity || detail.Versions[0].ID == detail.Versions[1].ID {
		t.Fatal("physical versions lost canonical episode grouping")
	}
}

func TestPlayerSnapshotStreamUsesEffectivePathAndRejectsTombstone(t *testing.T) {
	_, store, library, rec, entries, actor := playerSnapshotFixture(t)
	var storage models.Storage
	if err := store.writeDB.First(&storage, library.StorageID).Error; err != nil {
		t.Fatal(err)
	}
	entry := entries[0]
	entry.RelativePath = "current.mkv"
	if err := os.WriteFile(filepath.Join(storage.RootPath, entry.RelativePath), []byte("current-version"), 0600); err != nil {
		t.Fatal(err)
	}
	c, token := catalogCandidate(t, store, library, "delta", 1)
	if err := store.AppendBatch(context.Background(), c.ID, token, CatalogFactBatch{Entries: []models.CatalogEntryFact{CatalogEntryFromLegacy(entry, &rec)}}); err != nil {
		t.Fatal(err)
	}
	catalogPublish(t, store, c, token)
	proxy := &SignedProxyService{db: store.writeDB}
	proxy.SetCatalogSnapshotStore(store)
	resolved, err := proxy.ResolvePlayerEntry(context.Background(), actor, entry.ID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(resolved.File)
	_ = resolved.File.Close()
	if err != nil || string(data) != "current-version" {
		t.Fatalf("stream=%q err=%v", data, err)
	}
	stale, err := proxy.playerStreamSource(context.Background(), actor, entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	c, token = catalogCandidate(t, store, library, "delta", 2)
	tombstone := CatalogEntryFromLegacy(entry, &rec)
	tombstone.Tombstone = true
	if err := store.AppendBatch(context.Background(), c.ID, token, CatalogFactBatch{Entries: []models.CatalogEntryFact{tombstone}}); err != nil {
		t.Fatal(err)
	}
	catalogPublish(t, store, c, token)
	if _, err := proxy.ResolvePlayerEntry(context.Background(), actor, entry.ID, "", ""); ErrorCode(err) != CodeNotFound {
		t.Fatalf("tombstone played: %v", err)
	}
	if err := proxy.revalidatePlayerStreamSource(context.Background(), actor, stale); err == nil {
		t.Fatal("retired locator passed revalidation")
	}
	legacyProxy := &SignedProxyService{db: store.writeDB}
	if _, err := legacyProxy.ResolvePlayerEntry(context.Background(), actor, entries[1].ID, "", ""); err == nil {
		t.Fatal("versioned anchor played through legacy fallback")
	}
}

func capturePlayerEpisodeSource(t *testing.T, s *MediaLibraryService, library models.MediaLibrary, work string) playerEpisodeMetadataSource {
	t.Helper()
	var source playerEpisodeMetadataSource
	if err := s.withCatalogRead(context.Background(), []uint{library.ID}, func(tx *gorm.DB, r *CatalogReader) error {
		var err error
		source, err = playerEpisodeMetadataSourceTx(tx, r, library.ID, work)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return source
}

func TestPlayerSnapshotEpisodeEnrichmentUsesRecognitionDelta(t *testing.T) {
	s, store, library, rec, entries, _ := playerSnapshotFixture(t)
	source := capturePlayerEpisodeSource(t, s, library, entries[0].WorkKey)
	updated := playerSnapshotMetadata(t, "Show", "/enriched.jpg")
	if err := s.persistPlayerEpisodeMetadata(context.Background(), source, updated); err != nil {
		t.Fatal(err)
	}
	current := capturePlayerEpisodeSource(t, s, library, entries[0].WorkKey)
	if current.Head.Revision != 2 || current.Recognition.MetadataJSON != updated || !current.Recognition.UpdatedAt.Equal(source.Recognition.UpdatedAt) {
		t.Fatalf("enrichment not normalized: %+v", current)
	}
	var anchor models.MediaLibraryRecognition
	if err := store.writeDB.First(&anchor, rec.ID).Error; err != nil {
		t.Fatal(err)
	}
	if anchor.MetadataJSON != "{}" {
		t.Fatal("versioned enrichment mutated identity anchor")
	}
	if err := s.persistPlayerEpisodeMetadata(context.Background(), source, source.Recognition.MetadataJSON); !errors.Is(err, ErrCatalogFence) {
		t.Fatalf("stale head overwrote enrichment: %v", err)
	}
}

func TestPlayerSnapshotEpisodeEnrichmentFencesSourceAndDeltaBudget(t *testing.T) {
	for _, scenario := range []string{"source", "profile", "disabled", "budget"} {
		t.Run(scenario, func(t *testing.T) {
			s, store, library, rec, entries, _ := playerSnapshotFixture(t)
			if scenario == "budget" {
				for revision := uint64(1); revision <= CatalogMaxDeltas; revision++ {
					c, token := catalogCandidate(t, store, library, "delta", revision)
					catalogPublish(t, store, c, token)
				}
			}
			source := capturePlayerEpisodeSource(t, s, library, entries[0].WorkKey)
			var err error
			switch scenario {
			case "source":
				err = store.writeDB.Model(&library).Update("relative_root", "/changed").Error
			case "profile":
				err = store.writeDB.Model(&models.MediaClassificationProfile{}).Where("id=?", library.ProfileID).Update("revision", gorm.Expr("revision+1")).Error
			case "disabled":
				err = store.writeDB.Model(&library).Update("enabled", false).Error
			}
			if err != nil {
				t.Fatal(err)
			}
			err = s.persistPlayerEpisodeMetadata(context.Background(), source, playerSnapshotMetadata(t, "Wrong", "/wrong.jpg"))
			if err == nil {
				t.Fatal("unsafe enrichment accepted")
			}
			if scenario == "budget" && !errors.Is(err, ErrCatalogBudget) {
				t.Fatalf("budget error=%v", err)
			}
			var anchor models.MediaLibraryRecognition
			if err := store.writeDB.First(&anchor, rec.ID).Error; err != nil {
				t.Fatal(err)
			}
			if anchor.MetadataJSON != "{}" {
				t.Fatal("failed enrichment mutated anchor")
			}
			var head models.CatalogHead
			if err := store.writeDB.First(&head, "library_id=?", library.ID).Error; err != nil {
				t.Fatal(err)
			}
			if head.Revision != source.Head.Revision {
				t.Fatal("failed enrichment advanced head")
			}
		})
	}
}

func TestPlayerSnapshotEpisodeFetchReleasesReadTransaction(t *testing.T) {
	s, store, library, _, entries, actor := playerSnapshotFixture(t)
	readPool, err := store.readDB.DB()
	if err != nil {
		t.Fatal(err)
	}
	inUse := make(chan int, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inUse <- readPool.Stats().InUse
		_, _ = io.WriteString(w, `{"season_number":2,"episodes":[{"id":4201,"name":"Fetched second season","episode_number":1,"season_number":2,"still_path":"/s02e01.jpg"}]}`)
	}))
	defer server.Close()
	metadata := NewMetadataSettingsService(store.writeDB, NewAuditService(store.writeDB), nil, tmdb.Credential{Kind: tmdb.CredentialKindReadAccessToken, Value: "test-token"})
	metadata.clientFactory = func(credential tmdb.Credential, _, _ string) (*tmdb.Client, error) {
		return tmdb.NewForTest(credential.Value, server.URL+"/3", server.Client())
	}
	s.SetMetadataSettingsService(metadata)
	detail, err := s.PlayerCatalogDetail(context.Background(), actor, library.ID, encodeCatalogToken(entries[0].WorkKey))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case count := <-inUse:
		if count != 0 {
			t.Fatalf("network fetch retained %d read connections", count)
		}
	default:
		t.Fatal("TMDB season fetch did not run")
	}
	if len(detail.Versions) != 2 || detail.Versions[0].EpisodeTitle != "Fetched second season" {
		t.Fatalf("fetched metadata lost: %+v", detail)
	}
	current := capturePlayerEpisodeSource(t, s, library, entries[0].WorkKey)
	if current.Head.Revision != 2 {
		t.Fatal("fetched metadata was not committed as delta")
	}
}

func TestPlayerSnapshotLegacyEnrichmentCASProtectsManualCorrection(t *testing.T) {
	for _, field := range []string{"metadata_json", "manual_override", "input_fingerprint", "profile_revision"} {
		t.Run(field, func(t *testing.T) {
			s, library, _ := createCatalogTestLibrary(t)
			if err := s.db.Model(&library).Update("enabled", true).Error; err != nil {
				t.Fatal(err)
			}
			rec := models.MediaLibraryRecognition{LibraryID: library.ID, SourceKey: "show", ProfileID: library.ProfileID, ProfileRevision: library.ProfileRevision, MetadataJSON: "{}"}
			if err := s.db.Create(&rec).Error; err != nil {
				t.Fatal(err)
			}
			entry := models.MediaLibraryEntry{LibraryID: library.ID, RelativePath: "show.mkv", WorkKey: "series:test", RecognitionID: &rec.ID}
			if err := s.db.Create(&entry).Error; err != nil {
				t.Fatal(err)
			}
			source := capturePlayerEpisodeSource(t, s, library, entry.WorkKey)
			value := map[string]any{"metadata_json": "{\"corrected\":true}", "manual_override": true, "input_fingerprint": "corrected", "profile_revision": rec.ProfileRevision + 1}[field]
			if err := s.db.Model(&rec).UpdateColumn(field, value).Error; err != nil {
				t.Fatal(err)
			}
			if err := s.persistPlayerEpisodeMetadata(context.Background(), source, "{\"stale\":true}"); !errors.Is(err, ErrCatalogFence) {
				t.Fatalf("%s correction overwritten: %v", field, err)
			}
			var current models.MediaLibraryRecognition
			if err := s.db.First(&current, rec.ID).Error; err != nil {
				t.Fatal(err)
			}
			if current.MetadataJSON == "{\"stale\":true}" {
				t.Fatal("stale metadata persisted")
			}
		})
	}
}
