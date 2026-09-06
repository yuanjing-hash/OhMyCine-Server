package services

import (
	"context"
	"errors"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

func TestCatalogStablePagesRestartDiscardsAllOldRowsWithoutDurableRefs(t *testing.T) {
	s, library, rec, entries := catalogFixture(t)
	catalogConvert(t, s, library, rec, entries)
	reads, resets := 0, 0
	var rows []models.MediaLibraryEntry
	read := func(callback func(*gorm.DB, *CatalogReader) error) error {
		err := s.Read(context.Background(), []uint{library.ID}, func(reader *CatalogReader) error { return callback(reader.tx, reader) })
		reads++
		// Publish after the first page transaction has actually closed.
		if reads == 2 {
			candidate, token := catalogCandidate(t, s, library, "delta", 1)
			rec.Title = "New generation"
			if err := s.AppendBatch(context.Background(), candidate.ID, token, CatalogFactBatch{Recognitions: []models.CatalogRecognitionFact{CatalogRecognitionFromLegacy(rec)}}); err != nil {
				t.Fatal(err)
			}
			catalogPublish(t, s, candidate, token)
		}
		return err
	}
	err := readStableCatalogPages(read, library.ID, func(*gorm.DB, *CatalogReader) error {
		resets++
		rows = nil
		return nil
	}, func(page func(func(*CatalogReader) error) error) error {
		for _, entry := range entries {
			var row models.MediaLibraryEntry
			if err := page(func(reader *CatalogReader) error { return reader.Entries().Where("id=?", entry.ID).First(&row).Error }); err != nil {
				return err
			}
			rows = append(rows, row)
		}
		return nil
	})
	if err != nil || resets != 2 || len(rows) != 2 || rows[0].Title != "New generation" || rows[1].Title != "Per-file override" {
		t.Fatalf("mixed or duplicated generations: resets=%d rows=%+v err=%v", resets, rows, err)
	}
	var refs int64
	if err := s.writeDB.Model(&models.CatalogSnapshotReference{}).Count(&refs).Error; err != nil || refs != 0 {
		t.Fatalf("transient loader leaked references: %d %v", refs, err)
	}
}

func TestCatalogStablePagesChurnStopsAfterThreeAttempts(t *testing.T) {
	s, library, rec, entries := catalogFixture(t)
	catalogConvert(t, s, library, rec, entries)
	resets := 0
	read := func(callback func(*gorm.DB, *CatalogReader) error) error {
		return s.Read(context.Background(), []uint{library.ID}, func(reader *CatalogReader) error { return callback(reader.tx, reader) })
	}
	err := readStableCatalogPages(read, library.ID, func(*gorm.DB, *CatalogReader) error { resets++; return nil }, func(page func(func(*CatalogReader) error) error) error {
		candidate, token := catalogCandidate(t, s, library, "delta", uint64(resets))
		catalogPublish(t, s, candidate, token)
		return page(func(*CatalogReader) error { t.Fatal("changed generation reached page callback"); return nil })
	})
	if !errors.Is(err, ErrCatalogFence) || resets != 3 {
		t.Fatalf("unbounded retry or stale acceptance: resets=%d err=%v", resets, err)
	}
}
