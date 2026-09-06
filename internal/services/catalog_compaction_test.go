package services

import (
	"context"
	"errors"
	"math"
	"reflect"
	"strconv"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

func TestCatalogCompactionActualManualDirtySuffixSurvivesGuard(t *testing.T) {
	store, library, rec, entries := catalogFixture(t)
	catalogConvert(t, store, library, rec, entries)
	delta, token := catalogCandidate(t, store, library, "delta", 1)
	rec.Title, rec.ManualOverride = "New manual suffix", true
	if err := store.AppendBatch(context.Background(), delta.ID, token, CatalogFactBatch{Recognitions: []models.CatalogRecognitionFact{CatalogRecognitionFromLegacy(rec)}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Seal(context.Background(), delta.ID, token); err != nil {
		t.Fatal(err)
	}
	checks := 0
	store.spaceAvailable = func(context.Context) (uint64, error) {
		checks++
		if checks == 2 {
			// Compaction has pinned its prefix, but has not finished copying.
			// Publish already-prepared manual data and its dirty/artifact bump.
			if err := store.writeDB.Transaction(func(tx *gorm.DB) error {
				if _, err := store.PublishTx(tx, delta.ID, token, 1, func(*gorm.DB) error { return nil }); err != nil {
					return err
				}
				return tx.Model(&models.MediaLibrary{}).Where("id=?", library.ID).Update("dirty_generation", library.DirtyGeneration+1).Error
			}); err != nil {
				t.Fatal(err)
			}
		}
		return math.MaxUint64, nil
	}
	compacted, err := store.Compact(context.Background(), CatalogCompactionInput{LibraryID: library.ID, Force: true})
	if err != nil || !compacted {
		t.Fatalf("manual dirty invalidated compaction: %v %v", compacted, err)
	}
	rows := catalogReadEntries(t, store, library.ID, "")
	if rows[0].Title != "New manual suffix" {
		t.Fatal("new manual suffix lost")
	}
}

func TestCatalogCompactionDueCursorAdvancesPastResourceFailure(t *testing.T) {
	store, library, rec, entries := catalogFixture(t)
	catalogConvert(t, store, library, rec, entries)
	second := library
	second.ID, second.Name, second.NameNormalized = 0, "second-compaction", "second-compaction"
	if err := store.writeDB.Create(&second).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.writeDB.Transaction(func(tx *gorm.DB) error {
		return StartCatalogConversionTx(tx, models.CatalogHead{LibraryID: second.ID, SourceEpoch: 1, SourceFingerprint: "source-a", ConfigFingerprint: "config-a"}, func(*gorm.DB) error { return nil })
	}); err != nil {
		t.Fatal(err)
	}
	base, bt := catalogCandidate(t, store, second, "base", 0)
	catalogPublish(t, store, base, bt)
	for _, lib := range []models.MediaLibrary{library, second} {
		for revision := uint64(1); revision <= CatalogMaxDeltas/2; revision++ {
			candidate, token := catalogCandidate(t, store, lib, "delta", revision)
			catalogPublish(t, store, candidate, token)
		}
	}
	store.spaceAvailable = func(context.Context) (uint64, error) { return 1, nil }
	after, compacted, err := store.CompactNextDue(context.Background(), 0)
	if after != library.ID || compacted || !errors.Is(err, ErrCatalogBudget) {
		t.Fatalf("first due: %d %v %v", after, compacted, err)
	}
	store.spaceAvailable = nil
	after, compacted, err = store.CompactNextDue(context.Background(), after)
	if after != second.ID || !compacted || err != nil {
		t.Fatalf("failed library starved next: %d %v %v", after, compacted, err)
	}
	after, compacted, err = store.CompactNextDue(context.Background(), after)
	if after != 0 || compacted || err != nil {
		t.Fatalf("cursor end: %d %v %v", after, compacted, err)
	}
}

func TestCatalogCompactionHalfBudgetPreservesRawOverrideAndSemanticState(t *testing.T) {
	store, library, rec, entries := catalogFixture(t)
	catalogConvert(t, store, library, rec, entries)
	if compacted, err := store.Compact(context.Background(), CatalogCompactionInput{LibraryID: library.ID}); err != nil || compacted {
		t.Fatalf("premature compaction %v %v", compacted, err)
	}
	for revision := uint64(1); revision <= 4; revision++ {
		candidate, token := catalogCandidate(t, store, library, "delta", revision)
		rec.Title = "Per-file override"
		if err := store.AppendBatch(context.Background(), candidate.ID, token, CatalogFactBatch{Recognitions: []models.CatalogRecognitionFact{CatalogRecognitionFromLegacy(rec)}}); err != nil {
			t.Fatal(err)
		}
		catalogPublish(t, store, candidate, token)
	}
	var before models.MediaLibrary
	if err := store.writeDB.First(&before, library.ID).Error; err != nil {
		t.Fatal(err)
	}
	compacted, err := store.Compact(context.Background(), CatalogCompactionInput{LibraryID: library.ID})
	if err != nil || !compacted {
		t.Fatalf("compaction=%v %v", compacted, err)
	}
	var after models.MediaLibrary
	if err := store.writeDB.First(&after, library.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("compaction changed logical scan/content state")
	}
	var layers []models.CatalogHeadLayer
	if err := store.writeDB.Where("library_id=?", library.ID).Order("rank").Find(&layers).Error; err != nil {
		t.Fatal(err)
	}
	if len(layers) != 1 {
		t.Fatalf("layers=%+v", layers)
	}
	var raw models.CatalogEntryFact
	if err := store.writeDB.First(&raw, "snapshot_id=? AND id=?", layers[0].SnapshotID, entries[1].ID).Error; err != nil {
		t.Fatal(err)
	}
	if raw.SharedOverrideMask&(1<<1) == 0 {
		t.Fatal("compaction reconstructed equality and lost explicit override")
	}
	candidate, token := catalogCandidate(t, store, library, "delta", 6)
	rec.Title = "Later title"
	if err := store.AppendBatch(context.Background(), candidate.ID, token, CatalogFactBatch{Recognitions: []models.CatalogRecognitionFact{CatalogRecognitionFromLegacy(rec)}}); err != nil {
		t.Fatal(err)
	}
	catalogPublish(t, store, candidate, token)
	rows := catalogReadEntries(t, store, library.ID, "")
	if len(rows) != 2 || rows[0].Title != "Later title" || rows[1].Title != "Per-file override" || *rows[0].Season != 2 {
		t.Fatalf("raw mask semantics lost: %+v", rows)
	}
}

func TestCatalogCompactionKeepsVerifiedConcurrentSuffixAndReleasesReferences(t *testing.T) {
	store, library, rec, entries := catalogFixture(t)
	base := catalogConvert(t, store, library, rec, entries)
	delta, dt := catalogCandidate(t, store, library, "delta", 1)
	rec.Title = "Prefix title"
	if err := store.AppendBatch(context.Background(), delta.ID, dt, CatalogFactBatch{Recognitions: []models.CatalogRecognitionFact{CatalogRecognitionFromLegacy(rec)}}); err != nil {
		t.Fatal(err)
	}
	catalogPublish(t, store, delta, dt)
	var prefix []models.CatalogHeadLayer
	if err := store.writeDB.Where("library_id=?", library.ID).Order("rank").Find(&prefix).Error; err != nil {
		t.Fatal(err)
	}
	compact, token := catalogCandidate(t, store, library, "base", 2)
	if err := store.writeCatalogBatch(context.Background(), func(tx *gorm.DB) error {
		for _, layer := range prefix {
			if err := AcquireCatalogReferenceTx(tx, layer.SnapshotID, "compaction", compact.ID+":"+strconv.Itoa(layer.Rank)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	suffix, st := catalogCandidate(t, store, library, "delta", 2)
	rec.Title = "Newer suffix"
	if err := store.AppendBatch(context.Background(), suffix.ID, st, CatalogFactBatch{Recognitions: []models.CatalogRecognitionFact{CatalogRecognitionFromLegacy(rec)}}); err != nil {
		t.Fatal(err)
	}
	catalogPublish(t, store, suffix, st)
	if err := store.copyCatalogCompactionFacts(context.Background(), compact, token, prefix); err != nil {
		t.Fatal(err)
	}
	if err := store.Seal(context.Background(), compact.ID, token); err != nil {
		t.Fatal(err)
	}
	if err := store.writeCatalogBatch(context.Background(), func(tx *gorm.DB) error {
		if _, err := store.PublishTx(tx, compact.ID, token, 2, func(*gorm.DB) error { return nil }); !errors.Is(err, ErrCatalogInvalid) {
			t.Fatalf("ordinary publisher accepted compaction: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var revision uint64
	if err := store.writeCatalogBatch(context.Background(), func(tx *gorm.DB) error {
		var err error
		revision, err = store.PublishCompactionTx(tx, compact.ID, token, func(*gorm.DB) error { return nil })
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if revision != 4 {
		t.Fatalf("physical revision=%d", revision)
	}
	rows := catalogReadEntries(t, store, library.ID, "")
	if len(rows) != 2 || rows[0].Title != "Newer suffix" {
		t.Fatalf("suffix lost: %+v", rows)
	}
	var layers []models.CatalogHeadLayer
	if err := store.writeDB.Where("library_id=?", library.ID).Order("rank").Find(&layers).Error; err != nil {
		t.Fatal(err)
	}
	if len(layers) != 2 || layers[0].SnapshotID != compact.ID || layers[1].SnapshotID != suffix.ID {
		t.Fatalf("suffix chain=%+v", layers)
	}
	if err := store.writeCatalogBatch(context.Background(), func(tx *gorm.DB) error {
		again, err := store.PublishCompactionTx(tx, compact.ID, token, func(*gorm.DB) error { return nil })
		if err == nil && again != revision {
			t.Fatal("replay advanced revision")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var refs int64
	if err := store.writeDB.Model(&models.CatalogSnapshotReference{}).Where("owner_kind=? AND owner_id IN ?", "compaction", catalogCompactionOwnerIDs(compact.ID)).Count(&refs).Error; err != nil {
		t.Fatal(err)
	}
	if refs != 0 {
		t.Fatal("committed compaction leaked references")
	}
	if err := store.writeCatalogBatch(context.Background(), func(tx *gorm.DB) error { return MarkCatalogGCTx(tx, base.ID) }); err != nil {
		t.Fatal(err)
	}
	for {
		done, err := store.CollectBatch(context.Background(), base.ID)
		if err != nil {
			t.Fatal(err)
		}
		if done {
			break
		}
	}
	if rows = catalogReadEntries(t, store, library.ID, ""); len(rows) != 2 || rows[0].Title != "Newer suffix" {
		t.Fatal("GC damaged compacted chain")
	}
}

func TestCatalogCompactionMalformedPrefixDoesNotRelaxOrdinaryCandidateCAS(t *testing.T) {
	store, library, rec, entries := catalogFixture(t)
	base := catalogConvert(t, store, library, rec, entries)
	candidate, token := catalogCandidate(t, store, library, "base", 1)
	if err := store.writeCatalogBatch(context.Background(), func(tx *gorm.DB) error {
		return AcquireCatalogReferenceTx(tx, base.ID, "compaction", candidate.ID+":1")
	}); err != nil {
		t.Fatal(err)
	}
	delta, dt := catalogCandidate(t, store, library, "delta", 1)
	rec.Title = "Manual"
	if err := store.AppendBatch(context.Background(), delta.ID, dt, CatalogFactBatch{Recognitions: []models.CatalogRecognitionFact{CatalogRecognitionFromLegacy(rec)}}); err != nil {
		t.Fatal(err)
	}
	catalogPublish(t, store, delta, dt)
	if err := store.AppendBatch(context.Background(), candidate.ID, token, CatalogFactBatch{Recognitions: []models.CatalogRecognitionFact{CatalogRecognitionFromLegacy(rec)}}); !errors.Is(err, ErrCatalogInvalid) {
		t.Fatalf("invalid prefix accepted %v", err)
	}
	if err := store.Abandon(context.Background(), candidate.ID, token); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := store.writeDB.Model(&models.CatalogSnapshotReference{}).Where("owner_kind=? AND owner_id IN ?", "compaction", catalogCompactionOwnerIDs(candidate.ID)).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("abandon failed to release owned compaction references")
	}
}
