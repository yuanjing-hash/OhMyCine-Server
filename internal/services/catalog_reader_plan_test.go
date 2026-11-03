package services

import (
	"context"

	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestCatalogReaderRecognitionTombstoneDoesNotResurrectOlderMetadata(t *testing.T) {
	store, lib, rec, entries := catalogFixture(t)
	catalogConvert(t, store, lib, rec, entries)
	candidate, token := catalogCandidate(t, store, lib, "delta", 1)
	rec.Title = "Deleted recognition must not decorate files"
	fact := CatalogRecognitionFromLegacy(rec)
	fact.Tombstone = true
	batch := CatalogFactBatch{Recognitions: []models.CatalogRecognitionFact{fact}}
	for _, entry := range entries {
		entry.RecognitionID = nil
		batch.Entries = append(batch.Entries, CatalogEntryFromLegacy(entry, nil))
	}
	if err := store.AppendBatch(context.Background(), candidate.ID, token, batch); err != nil {
		t.Fatal(err)
	}
	catalogPublish(t, store, candidate, token)
	rows := catalogReadEntries(t, store, lib.ID, "")
	if len(rows) != len(entries) {
		t.Fatal("recognition tombstone removed physical versions")
	}
	for i := range rows {
		if rows[i].Title != entries[i].Title || rows[i].ID != entries[i].ID {
			t.Fatalf("entry fallback after recognition tombstone: %+v", rows[i])
		}
	}
	if err := store.Read(context.Background(), []uint{lib.ID}, func(reader *CatalogReader) error {
		var count int64
		if err := reader.Recognitions().Where("id = ?", rec.ID).Count(&count).Error; err != nil {
			return err
		}
		if count != 0 {
			t.Fatal("old recognition resurrected")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
