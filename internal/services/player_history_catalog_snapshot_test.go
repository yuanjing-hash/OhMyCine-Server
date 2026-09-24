package services

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/database"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/metadata/tmdb"
	"gorm.io/gorm"
)

// Conversion remains test-only. Real consumers must receive the separate
// query-only pool; the default constructor is not a rollout switch.
func historySnapshotFixture(t *testing.T) (playerHistoryCatalogFixture, *CatalogSnapshotStore, models.MediaLibrary) {
	t.Helper()
	f := newPlayerHistoryCatalogFixture(t)
	var library models.MediaLibrary
	if err := f.libraries.db.First(&library, f.libraryID).Error; err != nil {
		t.Fatal(err)
	}
	var location struct{ File string }
	if err := f.libraries.db.Raw("PRAGMA database_list").Scan(&location).Error; err != nil {
		t.Fatal(err)
	}
	readDB, err := database.OpenReadOnly(location.File)
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := readDB.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	store := NewCatalogSnapshotStore(f.libraries.db, readDB)
	f.libraries.SetCatalogSnapshotStore(store)
	if err := f.libraries.db.Transaction(func(tx *gorm.DB) error {
		return StartCatalogConversionTx(tx, models.CatalogHead{LibraryID: f.libraryID, SourceEpoch: 1, SourceFingerprint: "source-a", ConfigFingerprint: "config-a"}, func(*gorm.DB) error { return nil })
	}); err != nil {
		t.Fatal(err)
	}
	candidate, token := catalogCandidate(t, store, library, "base", 0)
	var records []models.MediaLibraryRecognition
	if err := f.libraries.db.Where("library_id = ?", f.libraryID).Find(&records).Error; err != nil {
		t.Fatal(err)
	}
	batch := CatalogFactBatch{}
	requests := make([]CatalogIdentityRequest, 0)
	byID := make(map[uint]models.MediaLibraryRecognition)
	for _, record := range records {
		byID[record.ID] = record
		requests = append(requests, CatalogIdentityRequest{Kind: "recognition", SourceKey: record.SourceKey, ExistingID: record.ID})
		batch.Recognitions = append(batch.Recognitions, CatalogRecognitionFromLegacy(record))
	}
	for _, entry := range append(append([]models.MediaLibraryEntry{}, f.movie...), f.episodes...) {
		record := byID[*entry.RecognitionID]
		requests = append(requests, CatalogIdentityRequest{Kind: "entry", SourceKey: entry.RelativePath, ExistingID: entry.ID})
		batch.Entries = append(batch.Entries, CatalogEntryFromLegacy(entry, &record))
	}
	if _, err := store.ResolveIdentities(context.Background(), candidate.ID, token, requests); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendBatch(context.Background(), candidate.ID, token, batch); err != nil {
		t.Fatal(err)
	}
	catalogPublish(t, store, candidate, token)
	// Stable anchors deliberately no longer carry the current title or path.
	if err := f.libraries.db.Model(&models.MediaLibraryRecognition{}).Where("library_id = ?", f.libraryID).Updates(map[string]any{"title": "stale anchor", "metadata_json": "{}"}).Error; err != nil {
		t.Fatal(err)
	}
	return f, store, library
}

func TestPlayerHistoryCatalogSnapshotSyncReconcileAndRecognitionDelta(t *testing.T) {
	f, store, library := historySnapshotFixture(t)
	first := f.change(strings.Repeat("1a", 32), "phone", "https://phone.example.test", playerHistoryEntryToken(f.libraryID, f.seriesWork, f.episodes[0].ID), 40, 1000)
	second := f.change(strings.Repeat("2b", 32), "desktop", "https://desktop.example.test", playerHistoryEntryToken(f.libraryID, f.seriesWork, f.episodes[1].ID), 90, 2000)
	// Seed pre-canonical relay rows, then reconcile with no incoming changes.
	legacy := NewPlayerHistoryService(f.libraries.db)
	if _, err := legacy.Sync(f.actor, 0, []PlayerHistoryChange{first, second}); err != nil {
		t.Fatal(err)
	}
	result, err := f.history.Sync(f.actor, 0, nil)
	if err != nil || len(result.Changes) != 3 {
		t.Fatalf("reconcile=%+v err=%v", result, err)
	}
	page, err := f.history.List(f.actor, 1, 10, "server")
	if err != nil || page.Total != 1 || page.List[0].Position != 90 || page.List[0].PosterPath != "/series-poster.jpg" || page.List[0].DisplayTitle != "权威剧名" || page.List[0].DisplaySubtitle != "S01E01 · 第一集" {
		t.Fatalf("effective series=%+v err=%v", page, err)
	}
	var record models.MediaLibraryRecognition
	if err := store.Read(context.Background(), []uint{library.ID}, func(r *CatalogReader) error {
		return r.Recognitions().First(&record, *f.episodes[0].RecognitionID).Error
	}); err != nil {
		t.Fatal(err)
	}
	record.Title = "修正剧名"
	record.MetadataJSON, err = marshalRecognitionMetadata(MediaRecognitionResult{Snapshot: tmdb.Snapshot{Version: 1, TMDBID: 300, MediaType: "tv", Title: record.Title, PosterPath: "/corrected-series.jpg"}})
	if err != nil {
		t.Fatal(err)
	}
	c, token := catalogCandidate(t, store, library, "delta", 1)
	if err := store.AppendBatch(context.Background(), c.ID, token, CatalogFactBatch{Recognitions: []models.CatalogRecognitionFact{CatalogRecognitionFromLegacy(record)}}); err != nil {
		t.Fatal(err)
	}
	catalogPublish(t, store, c, token)
	second.UpdatedAt, second.Position = 3000, 150
	if _, err := f.history.Sync(f.actor, result.Cursor, []PlayerHistoryChange{second}); err != nil {
		t.Fatal(err)
	}
	page, err = f.history.List(f.actor, 1, 10, "server")
	if err != nil || page.Total != 1 || page.List[0].Title != "修正剧名" || page.List[0].PosterPath != "/corrected-series.jpg" || page.List[0].Position != 150 {
		t.Fatalf("recognition delta=%+v err=%v", page, err)
	}
	second.Deleted, second.UpdatedAt = true, 4000
	if _, err := f.history.Sync(f.actor, 0, []PlayerHistoryChange{second}); err != nil {
		t.Fatal(err)
	}
	page, err = f.history.List(f.actor, 1, 10, "server")
	if err != nil || page.Total != 0 {
		t.Fatalf("deletion winner=%+v err=%v", page, err)
	}
}

func TestPlayerHistoryCatalogSnapshotTombstonesMixedSourcesAndAvailability(t *testing.T) {
	f, store, library := historySnapshotFixture(t)
	other := library
	other.ID, other.Name, other.NameNormalized, other.RelativeRoot = 0, "Legacy library", "legacy library", "/other"
	if err := f.libraries.db.Create(&other).Error; err != nil {
		t.Fatal(err)
	}
	legacyEntry := f.movie[0]
	legacyEntry.ID, legacyEntry.LibraryID, legacyEntry.RecognitionID = 0, other.ID, nil
	if err := f.libraries.db.Create(&legacyEntry).Error; err != nil {
		t.Fatal(err)
	}
	current := f.change(strings.Repeat("a1", 32), "server", "https://server.example.test", playerHistoryEntryToken(f.libraryID, f.movieWork, f.movie[0].ID), 100, 2000)
	otherChange := f.change(strings.Repeat("b2", 32), "legacy", "https://server.example.test", playerHistoryEntryToken(other.ID, f.movieWork, legacyEntry.ID), 110, 3000)
	otherChange.LibraryID = fmt.Sprint(other.ID)
	external := PlayerHistoryChange{SyncKey: strings.Repeat("c3", 32), SourceKind: "emby", SourceID: "emby-a", SourceLocator: "https://emby.example.test", MediaIdentity: "emby-item", Title: "外部电影", Position: 120, Duration: floatPointer(1000), UpdatedAt: 4000}
	if _, err := f.history.Sync(f.actor, 0, []PlayerHistoryChange{current, otherChange, external}); err != nil {
		t.Fatal(err)
	}
	page, err := f.history.List(f.actor, 1, 1, "server")
	if err != nil || page.Total != 2 || !page.HasMore || page.List[0].LibraryID != fmt.Sprint(other.ID) {
		t.Fatalf("mixed page=%+v err=%v", page, err)
	}
	c, token := catalogCandidate(t, store, library, "delta", 1)
	batch := CatalogFactBatch{}
	for _, entry := range f.movie {
		batch.Entries = append(batch.Entries, models.CatalogEntryFact{MediaLibraryEntry: entry, Tombstone: true})
	}
	if err := store.AppendBatch(context.Background(), c.ID, token, batch); err != nil {
		t.Fatal(err)
	}
	catalogPublish(t, store, c, token)
	current.UpdatedAt = 5000
	external.UpdatedAt = 5000
	result, err := f.history.Sync(f.actor, 0, []PlayerHistoryChange{current, external})
	if err != nil || len(result.Rejected) != 1 || result.Rejected[0].Code != CodeNotFound {
		t.Fatalf("partial tombstone rejection=%+v err=%v", result, err)
	}
	page, err = f.history.List(f.actor, 1, 1, "server")
	if err != nil || page.Total != 1 || page.HasMore || page.List[0].LibraryID != fmt.Sprint(other.ID) {
		t.Fatalf("deleted version not hidden=%+v err=%v", page, err)
	}
	browser, err := f.history.BrowserList(f.actor, 1, 10)
	if err != nil || browser.Total != 1 || browser.List[0].SourceKind != "server" {
		t.Fatalf("browser must show only Server history=%+v err=%v", browser, err)
	}
	items, more, err := f.history.ServerContinueWatching(f.actor, 1, map[uint]struct{}{library.ID: {}, other.ID: {}})
	if err != nil || more || len(items) != 1 || items[0].LibraryID != fmt.Sprint(other.ID) {
		t.Fatalf("continue eligibility=%+v more=%v err=%v", items, more, err)
	}
	denied := Actor{User: f.actor.User, Permissions: map[string]struct{}{}}
	if denied.CanResource(authz.PermissionMediaLibrariesRead, models.AuthorizationResourceMediaLibrary, uintID(other.ID)) {
		t.Fatal("denied actor unexpectedly authorized")
	}
	page, err = f.history.List(denied, 1, 10, "server")
	if err != nil || page.Total != 0 {
		t.Fatalf("authorization=%+v err=%v", page, err)
	}
	if err := f.libraries.db.Model(&models.Storage{}).Where("id = ?", library.StorageID).Update("enabled", false).Error; err != nil {
		t.Fatal(err)
	}
	page, err = f.history.List(f.actor, 1, 10, "server")
	if err != nil || page.Total != 0 {
		t.Fatalf("disabled storage=%+v err=%v", page, err)
	}
}

func TestPlayerHistoryCatalogSnapshotPinsAllKeysetPagesWithoutBlockingWriter(t *testing.T) {
	f, store, library := historySnapshotFixture(t)
	server := f.change(strings.Repeat("ef", 32), "server", "https://server.example.test", playerHistoryEntryToken(library.ID, f.movieWork, f.movie[0].ID), 10, 1000)
	if _, err := f.history.Sync(f.actor, 0, []PlayerHistoryChange{server}); err != nil {
		t.Fatal(err)
	}
	rows := make([]models.PlayerPlaybackHistory, 500)
	for i := range rows {
		rows[i] = playerHistoryRecord(f.actor.User.ID, PlayerHistoryChange{SyncKey: fmt.Sprintf("%064x", i+1), SourceKind: "emby", SourceID: "emby", MediaIdentity: fmt.Sprint(i), Title: "External", UpdatedAt: 2000})
	}
	if err := f.libraries.db.CreateInBatches(rows, 100).Error; err != nil {
		t.Fatal(err)
	}
	c, token := catalogCandidate(t, store, library, "delta", 1)
	batch := CatalogFactBatch{}
	for _, entry := range f.movie {
		batch.Entries = append(batch.Entries, models.CatalogEntryFact{MediaLibraryEntry: entry, Tombstone: true})
	}
	if err := store.AppendBatch(context.Background(), c.ID, token, batch); err != nil {
		t.Fatal(err)
	}
	if err := store.Seal(context.Background(), c.ID, token); err != nil {
		t.Fatal(err)
	}
	visited, sawServer := 0, false
	err := f.history.visitAvailableHistory(f.actor, false, false, func(change PlayerHistoryChange) bool {
		visited++
		if visited == 1 {
			publishCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := store.writeDB.WithContext(publishCtx).Transaction(func(tx *gorm.DB) error {
				_, err := store.PublishTx(tx, c.ID, token, 1, func(*gorm.DB) error { return nil })
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
		sawServer = sawServer || change.SourceKind == "server"
		return true
	})
	if err != nil || visited != 501 || !sawServer {
		t.Fatalf("mixed snapshots across pages: visited=%d server=%v err=%v", visited, sawServer, err)
	}
	page, err := f.history.List(f.actor, 1, 10, "server")
	if err != nil || page.Total != 0 {
		t.Fatalf("new request missed new snapshot=%+v err=%v", page, err)
	}
}

func TestPlayerHistoryCatalogSnapshotRecognitionFallbackAndSourceReplacement(t *testing.T) {
	f, store, library := historySnapshotFixture(t)
	c, token := catalogCandidate(t, store, library, "delta", 1)
	entry := f.movie[0]
	entry.RecognitionID = nil
	if err := store.AppendBatch(context.Background(), c.ID, token, CatalogFactBatch{Entries: []models.CatalogEntryFact{CatalogEntryFromLegacy(entry, nil)}}); err != nil {
		t.Fatal(err)
	}
	catalogPublish(t, store, c, token)
	change := f.change(strings.Repeat("a4", 32), "server", "https://server.example.test", playerHistoryEntryToken(library.ID, f.movieWork, entry.ID), 100, 1000)
	if _, err := f.history.Sync(f.actor, 0, []PlayerHistoryChange{change}); err != nil {
		t.Fatal(err)
	}
	page, err := f.history.List(f.actor, 1, 10, "server")
	if err != nil || page.Total != 1 || page.List[0].PosterPath != "/movie-poster.jpg" || page.List[0].Title != "权威电影" {
		t.Fatalf("fallback read stale recognition anchors=%+v err=%v", page, err)
	}
	// Replace the complete catalog with an empty base. The test source epoch
	// then advances atomically; physical identity anchors deliberately remain.
	c, token = catalogCandidate(t, store, library, "base", 2)
	catalogPublish(t, store, c, token)
	if err := store.writeDB.Transaction(func(tx *gorm.DB) error {
		values := map[string]any{"source_epoch": 2, "source_fingerprint": "replacement-source"}
		if err := tx.Model(&models.CatalogHead{}).Where("library_id = ?", library.ID).Updates(values).Error; err != nil {
			return err
		}
		return tx.Model(&models.CatalogSnapshot{}).Where("id = ?", c.ID).Updates(values).Error
	}); err != nil {
		t.Fatal(err)
	}
	page, err = f.history.List(f.actor, 1, 10, "server")
	if err != nil || page.Total != 0 {
		t.Fatalf("old source resurfaced=%+v err=%v", page, err)
	}
	change.UpdatedAt = 2000
	if _, err := f.history.Sync(f.actor, 0, []PlayerHistoryChange{change}); ErrorCode(err) != CodeNotFound {
		t.Fatalf("stale source upload should be not-found, got %v", err)
	}
	var count int64
	if err := store.writeDB.Model(&models.MediaLibraryEntry{}).Where("id = ?", entry.ID).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("test did not retain identity anchor: count=%d err=%v", count, err)
	}
}

func TestPlayerHistoryCatalogSnapshotFailureRollsBackWholeSync(t *testing.T) {
	f, store, library := historySnapshotFixture(t)
	if err := store.writeDB.Model(&models.CatalogHead{}).Where("library_id = ?", library.ID).Update("source_fingerprint", "drifted").Error; err != nil {
		t.Fatal(err)
	}
	external := PlayerHistoryChange{SyncKey: strings.Repeat("a5", 32), SourceKind: "emby", SourceID: "emby", MediaIdentity: "external", Title: "External", UpdatedAt: 1000}
	server := f.change(strings.Repeat("b6", 32), "server", "https://server.example.test", playerHistoryEntryToken(library.ID, f.movieWork, f.movie[0].ID), 10, 1000)
	if _, err := f.history.Sync(f.actor, 0, []PlayerHistoryChange{external, server}); err != ErrCatalogInvalid {
		t.Fatalf("infrastructure failure was treated as row rejection: %v", err)
	}
	var count int64
	if err := store.writeDB.Model(&models.PlayerPlaybackHistory{}).Where("user_id = ?", f.actor.User.ID).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("failed transaction partially committed: count=%d err=%v", count, err)
	}
}
