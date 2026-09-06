package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/database"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

func catalogFixture(t *testing.T) (*CatalogSnapshotStore, models.MediaLibrary, models.MediaLibraryRecognition, []models.MediaLibraryEntry) {
	t.Helper()
	service, library, _ := createCatalogTestLibrary(t)
	var location struct{ File string }
	if err := service.db.Raw("PRAGMA database_list").Scan(&location).Error; err != nil {
		t.Fatal(err)
	}
	reader, err := database.OpenReadOnly(location.File)
	if err != nil {
		t.Fatal(err)
	}
	sqlReader, _ := reader.DB()
	t.Cleanup(func() { _ = sqlReader.Close() })
	store := NewCatalogSnapshotStore(service.db, reader)
	tmdbID := int64(42)
	season, episode := 2, 1
	rec := models.MediaLibraryRecognition{LibraryID: library.ID, SourceKey: "show", ProfileID: library.ProfileID, ProfileRevision: library.ProfileRevision, Status: "matched", MediaType: "tv", Title: "Show", TMDBID: &tmdbID, MetadataJSON: "{}"}
	if err := service.db.Create(&rec).Error; err != nil {
		t.Fatal(err)
	}
	entries := []models.MediaLibraryEntry{
		{LibraryID: library.ID, RelativePath: "Show/Season 02/01.h264.mkv", ProviderID: "file-a", RecognitionID: &rec.ID, MediaType: "tv", Title: "Show", SeriesTitle: "Show", WorkKey: "series:tmdb:42", Season: &season, Episode: &episode, MatchStatus: "matched", TMDBID: &tmdbID},
		{LibraryID: library.ID, RelativePath: "Show/Season 02/01.h265.mkv", ProviderID: "file-b", RecognitionID: &rec.ID, MediaType: "tv", Title: "Per-file override", SeriesTitle: "Show", WorkKey: "series:tmdb:42", Season: &season, Episode: &episode, MatchStatus: "matched", TMDBID: &tmdbID},
	}
	if err := service.db.Create(&entries).Error; err != nil {
		t.Fatal(err)
	}
	if err := service.db.Transaction(func(tx *gorm.DB) error {
		return StartCatalogConversionTx(tx, models.CatalogHead{LibraryID: library.ID, SourceEpoch: 1, SourceFingerprint: "source-a", ConfigFingerprint: "config-a"}, func(*gorm.DB) error { return nil })
	}); err != nil {
		t.Fatal(err)
	}
	return store, library, rec, entries
}

func catalogCandidate(t *testing.T, s *CatalogSnapshotStore, library models.MediaLibrary, kind string, revision uint64) (models.CatalogSnapshot, string) {
	t.Helper()
	c, token, err := s.BeginCandidate(context.Background(), CatalogCandidateInput{LibraryID: library.ID, Kind: kind, ExpectedRevision: revision, SourceEpoch: 1, SourceFingerprint: "source-a", ConfigFingerprint: "config-a", LeaseDuration: 5 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	return c, token
}

func catalogConvert(t *testing.T, s *CatalogSnapshotStore, library models.MediaLibrary, rec models.MediaLibraryRecognition, entries []models.MediaLibraryEntry) models.CatalogSnapshot {
	t.Helper()
	c, token := catalogCandidate(t, s, library, "base", 0)
	requests := []CatalogIdentityRequest{{Kind: "recognition", SourceKey: rec.SourceKey, ExistingID: rec.ID}}
	for _, e := range entries {
		requests = append(requests, CatalogIdentityRequest{Kind: "entry", SourceKey: e.RelativePath, ExistingID: e.ID})
	}
	if _, err := s.ResolveIdentities(context.Background(), c.ID, token, requests); err != nil {
		t.Fatal(err)
	}
	batch := CatalogFactBatch{Recognitions: []models.CatalogRecognitionFact{CatalogRecognitionFromLegacy(rec)}}
	for _, e := range entries {
		batch.Entries = append(batch.Entries, CatalogEntryFromLegacy(e, &rec))
	}
	if err := s.AppendBatch(context.Background(), c.ID, token, batch); err != nil {
		t.Fatal(err)
	}
	catalogPublish(t, s, c, token)
	return c
}

func catalogPublish(t *testing.T, s *CatalogSnapshotStore, c models.CatalogSnapshot, token string) {
	t.Helper()
	if err := s.Seal(context.Background(), c.ID, token); err != nil {
		t.Fatal(err)
	}
	if err := s.writeDB.Transaction(func(tx *gorm.DB) error {
		_, err := s.PublishTx(tx, c.ID, token, c.ParentRevision, func(*gorm.DB) error { return nil })
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func catalogReadEntries(t *testing.T, s *CatalogSnapshotStore, libraryID uint, filter string, args ...any) []models.MediaLibraryEntry {
	t.Helper()
	var rows []models.MediaLibraryEntry
	if err := s.Read(context.Background(), []uint{libraryID}, func(r *CatalogReader) error {
		q := r.Entries().Order("id")
		if filter != "" {
			q = q.Where(filter, args...)
		}
		return q.Find(&rows).Error
	}); err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestCatalogSnapshotNormalizedDeltaLatestWinnerAndTombstone(t *testing.T) {
	s, library, rec, entries := catalogFixture(t)
	catalogConvert(t, s, library, rec, entries)
	rows := catalogReadEntries(t, s, library.ID, "")
	if len(rows) != 2 || rows[0].ID != entries[0].ID || *rows[0].Season != 2 {
		t.Fatalf("identity/versions lost: %+v", rows)
	}
	c, token := catalogCandidate(t, s, library, "delta", 1)
	rec.Title = "Corrected"
	rec.ManualOverride = true
	if err := s.AppendBatch(context.Background(), c.ID, token, CatalogFactBatch{Recognitions: []models.CatalogRecognitionFact{CatalogRecognitionFromLegacy(rec)}}); err != nil {
		t.Fatal(err)
	}
	if rows := catalogReadEntries(t, s, library.ID, "title=?", "Corrected"); len(rows) != 0 {
		t.Fatal("unpublished recognition escaped")
	}
	catalogPublish(t, s, c, token)
	rows = catalogReadEntries(t, s, library.ID, "")
	if len(rows) != 2 || rows[0].Title != "Corrected" || rows[1].Title != "Per-file override" || rows[0].SeriesTitle != "Corrected" {
		t.Fatalf("shared/override precedence: %+v", rows)
	}
	if rows := catalogReadEntries(t, s, library.ID, "title=?", "Show"); len(rows) != 0 {
		t.Fatal("old title resurrected before winner selection")
	}
	c, token = catalogCandidate(t, s, library, "delta", 2)
	tombstone := CatalogEntryFromLegacy(entries[0], &rec)
	tombstone.Tombstone = true
	if err := s.AppendBatch(context.Background(), c.ID, token, CatalogFactBatch{Entries: []models.CatalogEntryFact{tombstone}}); err != nil {
		t.Fatal(err)
	}
	catalogPublish(t, s, c, token)
	if rows := catalogReadEntries(t, s, library.ID, "id=?", entries[0].ID); len(rows) != 0 {
		t.Fatal("tombstone resurrected base row")
	}
	var anchor models.MediaLibraryEntry
	if err := s.writeDB.First(&anchor, entries[0].ID).Error; err != nil {
		t.Fatal("GC/deletion removed stable anchor")
	}
}

func TestCatalogSnapshotTVToMovieClearsOnlySharedSeriesTitle(t *testing.T) {
	store, library, rec, entries := catalogFixture(t)
	entries[1].SeriesTitle = "Explicit per-file series"
	catalogConvert(t, store, library, rec, entries)
	candidate, token := catalogCandidate(t, store, library, "delta", 1)
	rec.MediaType = "movie"
	if err := store.AppendBatch(context.Background(), candidate.ID, token, CatalogFactBatch{Recognitions: []models.CatalogRecognitionFact{CatalogRecognitionFromLegacy(rec)}}); err != nil {
		t.Fatal(err)
	}
	catalogPublish(t, store, candidate, token)
	rows := catalogReadEntries(t, store, library.ID, "")
	if len(rows) != 2 || rows[0].MediaType != "movie" || rows[0].SeriesTitle != "" || rows[1].SeriesTitle != "Explicit per-file series" {
		t.Fatalf("shared movie title/override: %+v", rows)
	}
	movieEntry := models.MediaLibraryEntry{SeriesTitle: "Movie file override"}
	fact := CatalogEntryFromLegacy(movieEntry, &rec)
	if fact.SharedOverrideMask&(1<<3) == 0 {
		t.Fatal("movie nonempty series title not captured as explicit file override")
	}
}

func TestCatalogSnapshotCASRollbackAndImmutableSeal(t *testing.T) {
	s, library, rec, entries := catalogFixture(t)
	catalogConvert(t, s, library, rec, entries)
	a, at := catalogCandidate(t, s, library, "delta", 1)
	b, bt := catalogCandidate(t, s, library, "delta", 1)
	for _, candidate := range []struct {
		c models.CatalogSnapshot
		t string
	}{{a, at}, {b, bt}} {
		if err := s.Seal(context.Background(), candidate.c.ID, candidate.t); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AppendBatch(context.Background(), a.ID, at, CatalogFactBatch{Entries: []models.CatalogEntryFact{CatalogEntryFromLegacy(entries[0], &rec)}}); !errors.Is(err, ErrCatalogFence) {
		t.Fatalf("sealed mutation=%v", err)
	}
	blocked := errors.New("injected durable marker failure")
	err := s.writeDB.Transaction(func(tx *gorm.DB) error {
		_, err := s.PublishTx(tx, a.ID, at, 1, func(*gorm.DB) error { return blocked })
		return err
	})
	if !errors.Is(err, blocked) {
		t.Fatal(err)
	}
	var head models.CatalogHead
	_ = s.writeDB.First(&head, "library_id=?", library.ID).Error
	if head.Revision != 1 {
		t.Fatal("failed commit advanced head")
	}
	if err := s.writeDB.Transaction(func(tx *gorm.DB) error {
		_, err := s.PublishTx(tx, a.ID, at, 1, func(*gorm.DB) error { return nil })
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.writeDB.Transaction(func(tx *gorm.DB) error {
		_, err := s.PublishTx(tx, b.ID, bt, 1, func(*gorm.DB) error { return nil })
		return err
	}); !errors.Is(err, ErrCatalogFence) {
		t.Fatalf("stale CAS=%v", err)
	}
	if err := s.writeDB.Transaction(func(tx *gorm.DB) error {
		revision, err := s.PublishTx(tx, a.ID, at, 1, func(*gorm.DB) error { t.Fatal("retry repeated side effects"); return nil })
		if revision != 2 {
			t.Fatalf("retry revision=%d", revision)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCatalogSnapshotIdentityAllocationNeverRebindsAnotherEpoch(t *testing.T) {
	s, library, rec, entries := catalogFixture(t)
	catalogConvert(t, s, library, rec, entries)
	c, token := catalogCandidate(t, s, library, "delta", 1)
	ids, err := s.ResolveIdentities(context.Background(), c.ID, token, []CatalogIdentityRequest{{Kind: "entry", SourceKey: entries[0].RelativePath, ExistingID: entries[0].ID}, {Kind: "entry", SourceKey: "new.mkv"}})
	if err != nil {
		t.Fatal(err)
	}
	if ids[0] != entries[0].ID || ids[1] == ids[0] {
		t.Fatalf("ids=%v", ids)
	}
	if len(catalogReadEntries(t, s, library.ID, "")) != 2 {
		t.Fatal("new private anchor leaked")
	}
	// Simulate the surrounding root-replacement transaction's source fence.
	if err := s.writeDB.Model(&models.CatalogHead{}).Where("library_id=?", library.ID).Updates(map[string]any{"source_epoch": 2, "source_fingerprint": "source-b"}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveIdentities(context.Background(), c.ID, token, []CatalogIdentityRequest{{Kind: "entry", SourceKey: "new2.mkv"}}); !errors.Is(err, ErrCatalogFence) {
		t.Fatalf("old source worker resumed: %v", err)
	}
	if err := s.writeDB.Create(&models.CatalogIdentity{LibraryID: library.ID, SourceEpoch: 2, EntityKind: "entry", SourceKey: entries[0].RelativePath, AnchorID: entries[0].ID, CreatedAt: time.Now().UTC()}).Error; err == nil {
		t.Fatal("old identity rebound across source epochs")
	}
}

func TestCatalogSnapshotGCReferenceRaceAndBoundedDeletion(t *testing.T) {
	s, library, rec, entries := catalogFixture(t)
	old := catalogConvert(t, s, library, rec, entries)
	if err := s.writeDB.Transaction(func(tx *gorm.DB) error { return MarkCatalogGCTx(tx, old.ID) }); !errors.Is(err, ErrCatalogFence) {
		t.Fatal("current head marked GC")
	}
	if err := s.writeDB.Transaction(func(tx *gorm.DB) error { return AcquireCatalogReferenceTx(tx, old.ID, "artifact", "run-1") }); err != nil {
		t.Fatal(err)
	}
	base, token := catalogCandidate(t, s, library, "base", 1)
	catalogPublish(t, s, base, token)
	if err := s.writeDB.Transaction(func(tx *gorm.DB) error { return MarkCatalogGCTx(tx, old.ID) }); !errors.Is(err, ErrCatalogFence) {
		t.Fatal("referenced snapshot marked GC")
	}
	if err := s.writeDB.Transaction(func(tx *gorm.DB) error {
		if err := ReleaseCatalogReferenceTx(tx, old.ID, "artifact", "run-1"); err != nil {
			return err
		}
		return MarkCatalogGCTx(tx, old.ID)
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.writeDB.Transaction(func(tx *gorm.DB) error { return AcquireCatalogReferenceTx(tx, old.ID, "artifact", "late") }); !errors.Is(err, ErrCatalogFence) {
		t.Fatal("GC snapshot gained reference")
	}
	for i := 0; i < 5; i++ {
		done, err := s.CollectBatch(context.Background(), old.ID)
		if err != nil {
			t.Fatal(err)
		}
		if done {
			var count int64
			if err := s.writeDB.Model(&models.MediaLibraryEntry{}).Count(&count).Error; err != nil || count != 2 {
				t.Fatalf("anchors=%d err=%v", count, err)
			}
			return
		}
	}
	t.Fatal("GC did not finish")
}

func TestCatalogReaderLegacyScopedRelationsCanBeJoined(t *testing.T) {
	store, library, rec, entries := catalogFixture(t)
	// A converting library still reads its legacy facts until atomic conversion.
	// Both E and R have library_id; the reader's scope must qualify its relation.
	if err := store.Read(context.Background(), []uint{library.ID}, func(r *CatalogReader) error {
		var ids []uint
		if err := r.Recognitions().Joins("JOIN (?) AS media_library_entries ON media_library_entries.recognition_id=media_library_recognitions.id", r.Entries()).Select("media_library_recognitions.id").Pluck("media_library_recognitions.id", &ids).Error; err != nil {
			return err
		}
		if len(ids) != len(entries) {
			t.Fatalf("joined membership=%v", ids)
		}
		for _, id := range ids {
			if id != rec.ID {
				t.Fatal("joined foreign recognition")
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCatalogSnapshotMixedLibrariesAndTransactionBoundFilters(t *testing.T) {
	s, library, rec, entries := catalogFixture(t)
	second := models.MediaLibrary{StorageID: library.StorageID, ProfileID: library.ProfileID, ProfileRevision: library.ProfileRevision, Name: "legacy", NameNormalized: "legacy", RelativeRoot: "legacy"}
	if err := s.writeDB.Create(&second).Error; err != nil {
		t.Fatal(err)
	}
	other := models.MediaLibraryEntry{LibraryID: second.ID, RelativePath: "other.mkv", Title: "Legacy", MediaType: "movie", WorkKey: "movie:legacy", MatchStatus: "matched"}
	if err := s.writeDB.Create(&other).Error; err != nil {
		t.Fatal(err)
	}
	catalogConvert(t, s, library, rec, entries)
	if err := s.writeDB.Transaction(func(tx *gorm.DB) error {
		r, err := PinCatalogTx(tx, []uint{library.ID, second.ID, second.ID})
		if err != nil {
			return err
		}
		var count int64
		if err := r.Entries().Count(&count).Error; err != nil {
			return err
		}
		if count != 3 {
			t.Fatalf("mixed count=%d", count)
		}
		var filtered []models.MediaLibraryEntry
		if err := r.Entries().Where("title=?", "Legacy").Limit(1).Find(&filtered).Error; err != nil {
			return err
		}
		if len(filtered) != 1 || filtered[0].LibraryID != second.ID {
			t.Fatalf("mixed filter=%+v", filtered)
		}
		var recognized []models.MediaLibraryRecognition
		if err := r.Recognitions().Find(&recognized).Error; err != nil {
			return err
		}
		if len(recognized) != 1 || recognized[0].SourceKey != "show" {
			t.Fatalf("recognitions=%+v", recognized)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := PinCatalogTx(s.writeDB, []uint{library.ID}); err == nil {
		t.Fatal("unpinned queries accepted")
	}
	if err := s.writeDB.Where("library_id=?", library.ID).Delete(&models.CatalogHeadLayer{}).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.Read(context.Background(), []uint{library.ID}, func(*CatalogReader) error { t.Fatal("corrupt head fell back to legacy"); return nil }); !errors.Is(err, ErrCatalogInvalid) {
		t.Fatalf("corrupt head=%v", err)
	}
}

func TestCatalogSnapshotOwnershipJobCancellationAndDeltaBudget(t *testing.T) {
	s, library, rec, entries := catalogFixture(t)
	catalogConvert(t, s, library, rec, entries)
	expires := time.Now().UTC().Add(time.Minute)
	job := models.Job{ID: "snapshot-worker", CreatedByKind: "system", JobType: "fake", Status: "running", LeaseTokenHash: "lease-hash", LeaseExpiresAt: &expires, Generation: 1, PayloadJSON: "{}"}
	if err := s.writeDB.Create(&job).Error; err != nil {
		t.Fatal(err)
	}
	c, token, err := s.BeginCandidate(context.Background(), CatalogCandidateInput{LibraryID: library.ID, Kind: "delta", ExpectedRevision: 1, SourceEpoch: 1, SourceFingerprint: "source-a", ConfigFingerprint: "config-a", JobID: &job.ID, JobLeaseHash: job.LeaseTokenHash, LeaseDuration: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	batch := CatalogFactBatch{Recognitions: []models.CatalogRecognitionFact{CatalogRecognitionFromLegacy(rec)}}
	if err := s.AppendBatch(context.Background(), c.ID, "wrong-owner", batch); !errors.Is(err, ErrCatalogFence) {
		t.Fatalf("wrong owner=%v", err)
	}
	if err := s.writeDB.Model(&job).Update("cancellation_asked", true).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.AppendBatch(context.Background(), c.ID, token, batch); !errors.Is(err, ErrCatalogFence) {
		t.Fatalf("cancelled job wrote=%v", err)
	}
	if err := s.Abandon(context.Background(), c.ID, token); err != nil {
		t.Fatal(err)
	}
	for revision := uint64(1); revision <= CatalogMaxDeltas; revision++ {
		c, token := catalogCandidate(t, s, library, "delta", revision)
		catalogPublish(t, s, c, token)
	}
	c, token = catalogCandidate(t, s, library, "delta", CatalogMaxDeltas+1)
	if err := s.Seal(context.Background(), c.ID, token); !errors.Is(err, ErrCatalogBudget) {
		t.Fatalf("unbounded delta depth=%v", err)
	}
	var floor int
	if err := s.writeDB.Raw("SELECT format FROM catalog_format_floor WHERE id=1").Scan(&floor).Error; err != nil || floor != 1 {
		t.Fatalf("format=%d err=%v", floor, err)
	}
}

func TestCatalogSnapshotSourceAssetLatestWinnerAndIdentityScope(t *testing.T) {
	s, library, rec, entries := catalogFixture(t)
	asset := models.MediaLibrarySourceAsset{LibraryID: library.ID, RelativePath: "Show/poster.jpg", Name: "poster.jpg", Extension: ".jpg", ProviderID: "poster", Active: true}
	if err := s.writeDB.Create(&asset).Error; err != nil {
		t.Fatal(err)
	}
	c, token := catalogCandidate(t, s, library, "base", 0)
	requests := []CatalogIdentityRequest{{Kind: "recognition", SourceKey: rec.SourceKey, ExistingID: rec.ID}, {Kind: "asset", SourceKey: asset.RelativePath, ExistingID: asset.ID}}
	for _, e := range entries {
		requests = append(requests, CatalogIdentityRequest{Kind: "entry", SourceKey: e.RelativePath, ExistingID: e.ID})
	}
	if _, err := s.ResolveIdentities(context.Background(), c.ID, token, requests); err != nil {
		t.Fatal(err)
	}
	batch := CatalogFactBatch{Recognitions: []models.CatalogRecognitionFact{CatalogRecognitionFromLegacy(rec)}, SourceAssets: []models.CatalogSourceAssetFact{{MediaLibrarySourceAsset: asset}}}
	for _, e := range entries {
		batch.Entries = append(batch.Entries, CatalogEntryFromLegacy(e, &rec))
	}
	if err := s.AppendBatch(context.Background(), c.ID, token, batch); err != nil {
		t.Fatal(err)
	}
	catalogPublish(t, s, c, token)
	c, token = catalogCandidate(t, s, library, "delta", 1)
	asset.RelativePath = "Show/changed.jpg"
	asset.Name = "changed.jpg"
	if err := s.AppendBatch(context.Background(), c.ID, token, CatalogFactBatch{SourceAssets: []models.CatalogSourceAssetFact{{MediaLibrarySourceAsset: asset}}}); err != nil {
		t.Fatal(err)
	}
	catalogPublish(t, s, c, token)
	if err := s.Read(context.Background(), []uint{library.ID}, func(r *CatalogReader) error {
		var rows []models.MediaLibrarySourceAsset
		if err := r.SourceAssets().Where("name=?", "poster.jpg").Find(&rows).Error; err != nil {
			return err
		}
		if len(rows) != 0 {
			t.Fatal("old asset matched after update")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	c, token = catalogCandidate(t, s, library, "delta", 2)
	asset.LibraryID++
	if err := s.AppendBatch(context.Background(), c.ID, token, CatalogFactBatch{SourceAssets: []models.CatalogSourceAssetFact{{MediaLibrarySourceAsset: asset}}}); !errors.Is(err, ErrCatalogInvalid) {
		t.Fatalf("foreign-library fact accepted=%v", err)
	}
}
