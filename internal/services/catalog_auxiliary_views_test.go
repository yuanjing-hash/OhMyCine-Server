package services

import (
	"context"
	"errors"
	"image/color"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/metadata/tmdb"
)

func TestCatalogAuxiliaryDashboardAndCoverageUseEffectiveAuthorizedFacts(t *testing.T) {
	_, store, library, rec, entries, actor := playerSnapshotFixture(t)
	c, token := catalogCandidate(t, store, library, "delta", 1)
	tombstone := CatalogEntryFromLegacy(entries[0], &rec)
	tombstone.Tombstone = true
	newID := int64(99)
	rec.TMDBID = &newID
	if err := store.AppendBatch(context.Background(), c.ID, token, CatalogFactBatch{Entries: []models.CatalogEntryFact{tombstone}, Recognitions: []models.CatalogRecognitionFact{CatalogRecognitionFromLegacy(rec)}}); err != nil {
		t.Fatal(err)
	}
	catalogPublish(t, store, c, token)
	admin := NewAdminService(store.writeDB, nil, nil, nil)
	admin.SetCatalogSnapshotStore(store)
	section := admin.DashboardOperations(context.Background(), actor)["media-summary"]
	if section.Status != "ok" || len(section.List) != 3 || *section.List[0].Value != 1 || *section.List[1].Value != 1 || *section.List[2].Value != 1 {
		t.Fatalf("effective dashboard=%+v", section)
	}
	coverage := NewMediaCoverageService(store.writeDB, nil)
	coverage.SetCatalogSnapshotStore(store)
	libs, found, _, _, err := coverage.coverageCatalog(context.Background(), actor, "tv", 99)
	if err != nil || len(libs) != 1 || len(found) != 1 || found[0].ID != entries[1].ID || *found[0].Season != 2 {
		t.Fatalf("effective coverage=%+v %+v %v", libs, found, err)
	}
	_, old, _, _, err := coverage.coverageCatalog(context.Background(), actor, "tv", 42)
	if err != nil || len(old) != 0 {
		t.Fatalf("stale TMDB recognition resurrected: %+v %v", old, err)
	}
	denied := actor
	denied.ResourceRules = []AuthorizationRule{{PermissionCode: authz.PermissionMediaLibrariesRead, Effect: models.AuthorizationEffectDeny, ResourceType: models.AuthorizationResourceMediaLibrary, ResourceID: strconv.FormatUint(uint64(library.ID), 10)}}
	libs, found, _, _, err = coverage.coverageCatalog(context.Background(), denied, "tv", 99)
	if err != nil || len(libs) != 0 || len(found) != 0 {
		t.Fatalf("denied coverage=%+v %+v %v", libs, found, err)
	}
	section = admin.DashboardOperations(context.Background(), denied)["media-summary"]
	if section.Status != "ok" || *section.List[0].Value != 0 || *section.List[2].Value != 0 {
		t.Fatalf("denied dashboard=%+v", section)
	}
	if err := store.writeDB.Model(&models.Storage{}).Where("id=?", library.StorageID).Update("enabled", false).Error; err != nil {
		t.Fatal(err)
	}
	libs, found, _, _, err = coverage.coverageCatalog(context.Background(), actor, "tv", 99)
	if err != nil || len(libs) != 0 || len(found) != 0 {
		t.Fatal("disabled source remained visible")
	}
	legacyAdmin := NewAdminService(store.writeDB, nil, nil, nil)
	if legacyAdmin.DashboardOperations(context.Background(), actor)["media-summary"].Status != "unavailable" {
		t.Fatal("dashboard silently used versioned anchors")
	}
	if _, _, _, _, err := NewMediaCoverageService(store.writeDB, nil).coverageCatalog(context.Background(), actor, "tv", 42); err == nil {
		t.Fatal("coverage silently used versioned anchors")
	}
}

func TestCatalogAuxiliaryCoverageReleasesReadBeforeSeasonNetwork(t *testing.T) {
	_, store, library, _, _, actor := playerSnapshotFixture(t)
	actor.Permissions[authz.PermissionDiscoveryRead] = struct{}{}
	now := time.Now().UTC()
	if err := store.writeDB.Model(&library).Updates(map[string]any{"baseline_generation": 1, "last_successful_scan_at": now}).Error; err != nil {
		t.Fatal(err)
	}
	pool, _ := store.readDB.DB()
	inUse := make(chan int, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inUse <- pool.Stats().InUse
		switch r.URL.Path {
		case "/tv/42":
			_, _ = io.WriteString(w, `{"id":42,"name":"Show","seasons":[{"season_number":2,"name":"Second","episode_count":1}]}`)
		case "/tv/42/season/2":
			_, _ = io.WriteString(w, `{"season_number":2,"episodes":[{"season_number":2,"episode_number":1,"name":"One","air_date":"2020-01-01"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	metadata := NewMetadataSettingsService(store.writeDB, NewAuditService(store.writeDB), nil, tmdb.Credential{Kind: tmdb.CredentialKindReadAccessToken, Value: "fixture"})
	metadata.clientFactory = func(c tmdb.Credential, _, _ string) (*tmdb.Client, error) {
		return tmdb.NewForTest(c.Value, upstream.URL, upstream.Client())
	}
	s := NewMediaCoverageService(store.writeDB, metadata)
	s.SetCatalogSnapshotStore(store)
	result, err := s.Coverage(context.Background(), actor, "tv", 42)
	if err != nil || result.TV == nil || result.TV.Counts.Present != 1 {
		t.Fatalf("coverage=%+v err=%v", result, err)
	}
	for i := 0; i < 2; i++ {
		select {
		case count := <-inUse:
			if count != 0 {
				t.Fatal("network retained read snapshot")
			}
		default:
			t.Fatal("missing upstream request")
		}
	}
}

func TestCatalogAuxiliaryArtworkUsesEffectiveBothSidesAndStableCompactionKey(t *testing.T) {
	_, store, library, rec, _, _ := playerSnapshotFixture(t)
	metadata := NewMetadataSettingsService(store.writeDB, NewAuditService(store.writeDB), nil, tmdb.Credential{Kind: tmdb.CredentialKindReadAccessToken, Value: "fixture"})
	s := NewLibraryArtworkService(store.writeDB, metadata, nil, nil, zerolog.Nop(), WithLibraryArtworkRoot(t.TempDir()))
	s.SetCatalogSnapshotStore(store)
	c, token := catalogCandidate(t, store, library, "delta", 1)
	rec.CategoryName = "Effective"
	rec.MetadataJSON = playerSnapshotMetadata(t, "Show", "/current.jpg")
	if err := store.AppendBatch(context.Background(), c.ID, token, CatalogFactBatch{Recognitions: []models.CatalogRecognitionFact{CatalogRecognitionFromLegacy(rec)}}); err != nil {
		t.Fatal(err)
	}
	catalogPublish(t, store, c, token)
	current, err := s.mediaCategoryCandidates(library.ID, "Effective", "series")
	if err != nil || len(current) != 1 || current[0].key != "tmdb:/current.jpg" {
		t.Fatalf("effective candidates=%+v %v", current, err)
	}
	old, err := s.mediaCategoryCandidates(library.ID, "Series", "series")
	if err != nil || len(old) != 0 {
		t.Fatalf("anchor category resurrected=%+v %v", old, err)
	}
	key := artworkGenerationKey("Effective", normalizeArtworkCandidates(current))
	loads := 0
	pool, _ := store.readDB.DB()
	s.categoryCandidates = func(uint, string, string) ([]artworkCandidate, error) {
		return []artworkCandidate{{key: current[0].key, load: func(ctx context.Context) ([]byte, error) {
			loads++
			if pool.Stats().InUse != 0 {
				t.Fatal("image IO retained read snapshot")
			}
			return solidArtworkLoader(color.RGBA{R: 90, G: 130, B: 180, A: 255})(ctx)
		}}}, nil
	}
	if err := s.ReconcileMediaLibrary(context.Background(), library.ID, true); err != nil {
		t.Fatal(err)
	}
	var first models.MediaCategoryArtwork
	if err := store.writeDB.Where("library_id=?", library.ID).First(&first).Error; err != nil {
		t.Fatal(err)
	}
	if first.CategoryName != "Effective" || first.GenerationKey != key || loads != 1 {
		t.Fatalf("artwork=%+v loads=%d", first, loads)
	}
	// Re-materialize identical effective facts as a new physical base.
	effective := catalogReadEntries(t, store, library.ID, "")
	c, token = catalogCandidate(t, store, library, "base", 2)
	batch := CatalogFactBatch{Recognitions: []models.CatalogRecognitionFact{CatalogRecognitionFromLegacy(rec)}}
	for _, entry := range effective {
		batch.Entries = append(batch.Entries, CatalogEntryFromLegacy(entry, &rec))
	}
	if err := store.AppendBatch(context.Background(), c.ID, token, batch); err != nil {
		t.Fatal(err)
	}
	catalogPublish(t, store, c, token)
	if err := s.ReconcileMediaLibrary(context.Background(), library.ID, true); err != nil {
		t.Fatal(err)
	}
	var after models.MediaCategoryArtwork
	if err := store.writeDB.First(&after, first.ID).Error; err != nil {
		t.Fatal(err)
	}
	if after.GenerationKey != first.GenerationKey || after.ContentHash != first.ContentHash || after.Revision != first.Revision || loads != 1 {
		t.Fatal("physical compaction invalidated content-addressed artwork")
	}
	legacy := NewLibraryArtworkService(store.writeDB, metadata, nil, nil, zerolog.Nop())
	if _, err := legacy.mediaCategoryCandidates(library.ID, "Series", "series"); err == nil {
		t.Fatal("artwork silently read versioned anchors")
	}
}

func TestCatalogAuxiliaryArtworkFencesPublicationDuringImageWork(t *testing.T) {
	_, store, library, rec, _, _ := playerSnapshotFixture(t)
	s := NewLibraryArtworkService(store.writeDB, nil, nil, nil, zerolog.Nop(), WithLibraryArtworkRoot(t.TempDir()))
	s.SetCatalogSnapshotStore(store)
	s.categoryCandidates = func(uint, string, string) ([]artworkCandidate, error) {
		return []artworkCandidate{{key: "old", load: solidArtworkLoader(color.RGBA{R: 20, G: 80, B: 90, A: 255})}}, nil
	}
	if err := s.ReconcileMediaLibrary(context.Background(), library.ID, true); err != nil {
		t.Fatal(err)
	}
	var first models.MediaCategoryArtwork
	if err := store.writeDB.Where("library_id=?", library.ID).First(&first).Error; err != nil {
		t.Fatal(err)
	}
	s.categoryCandidates = func(uint, string, string) ([]artworkCandidate, error) {
		return []artworkCandidate{{key: "stale", load: func(ctx context.Context) ([]byte, error) {
			c, token := catalogCandidate(t, store, library, "delta", 1)
			rec.MetadataJSON = playerSnapshotMetadata(t, "Corrected", "/new.jpg")
			if err := store.AppendBatch(ctx, c.ID, token, CatalogFactBatch{Recognitions: []models.CatalogRecognitionFact{CatalogRecognitionFromLegacy(rec)}}); err != nil {
				return nil, err
			}
			catalogPublish(t, store, c, token)
			return solidArtworkLoader(color.RGBA{R: 200, G: 40, B: 90, A: 255})(ctx)
		}}}, nil
	}
	if err := s.ReconcileMediaLibrary(context.Background(), library.ID, true); !errors.Is(err, ErrCatalogFence) {
		t.Fatalf("stale generation succeeded: %v", err)
	}
	var after models.MediaCategoryArtwork
	if err := store.writeDB.First(&after, first.ID).Error; err != nil {
		t.Fatal(err)
	}
	if after.ContentHash != first.ContentHash || after.Revision != first.Revision {
		t.Fatal("stale image replaced current cover")
	}
}
