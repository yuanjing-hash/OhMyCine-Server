package services

import (
	"context"
	"errors"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

func TestCatalogBoundReaderKeepsExactFactsAndRequiresDurableOwner(t *testing.T) {
	store, library, rec, entries := catalogFixture(t)
	catalogConvert(t, store, library, rec, entries)
	var binding CatalogSnapshotBinding
	if err := store.writeDB.Transaction(func(tx *gorm.DB) error {
		var err error
		binding, err = CaptureCatalogBindingTx(tx, library.ID, "diagnosis", "test-owner")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	c, token := catalogCandidate(t, store, library, "delta", 1)
	changed := rec
	changed.Title = "new title"
	if err := store.AppendBatch(context.Background(), c.ID, token, CatalogFactBatch{Recognitions: []models.CatalogRecognitionFact{CatalogRecognitionFromLegacy(changed)}, Entries: []models.CatalogEntryFact{{MediaLibraryEntry: entries[0], Tombstone: true}}}); err != nil {
		t.Fatal(err)
	}
	catalogPublish(t, store, c, token)
	if rows := catalogReadEntries(t, store, library.ID, ""); len(rows) != 1 {
		t.Fatalf("current count=%d", len(rows))
	}
	if err := store.ReadBoundCatalog(context.Background(), binding, "diagnosis", "test-owner", func(r *CatalogReader) error {
		var got []models.MediaLibraryEntry
		if err := r.Entries().Order("id").Find(&got).Error; err != nil {
			return err
		}
		if len(got) != 2 || got[0].Title != entries[0].Title {
			t.Fatalf("bound facts=%+v", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.ReadBoundCatalog(context.Background(), binding, "diagnosis", "wrong-owner", func(*CatalogReader) error { return nil }); !errors.Is(err, ErrCatalogFence) {
		t.Fatalf("wrong owner=%v", err)
	}
	if err := store.writeDB.Transaction(func(tx *gorm.DB) error { return ReleaseCatalogBindingTx(tx, binding, "diagnosis", "test-owner") }); err != nil {
		t.Fatal(err)
	}
	if err := store.ReadBoundCatalog(context.Background(), binding, "diagnosis", "test-owner", func(*CatalogReader) error { return nil }); !errors.Is(err, ErrCatalogFence) {
		t.Fatalf("released owner=%v", err)
	}
}
