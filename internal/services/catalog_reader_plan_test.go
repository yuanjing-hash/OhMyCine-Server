package services

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

func TestCatalogReaderIndexedRecognitionLookup(t *testing.T) {
	store, lib, rec, entries := catalogFixture(t)
	catalogConvert(t, store, lib, rec, entries)
	if err := store.Read(context.Background(), []uint{lib.ID}, func(r *CatalogReader) error {
		for _, mode := range []string{"page", "ids", "count"} {
			var rows []models.MediaLibraryEntry
			var count int64
			var q *gorm.DB
			switch mode {
			case "page":
				q = r.Entries().Session(&gorm.Session{DryRun: true}).Where("id > ?", 1).Order("id").Limit(250).Find(&rows)
			case "ids":
				q = r.Entries().Session(&gorm.Session{DryRun: true}).Where("id IN ?", []uint{1, 2}).Find(&rows)
			default:
				q = r.Entries().Session(&gorm.Session{DryRun: true}).Count(&count)
			}
			var plan []struct{ Detail string }
			if err := r.tx.Raw("EXPLAIN QUERY PLAN "+q.Statement.SQL.String(), q.Statement.Vars...).Scan(&plan).Error; err != nil {
				return err
			}
			for _, row := range plan {
				if os.Getenv("OMC_RUN_PERFORMANCE") == "1" {
					t.Log(mode, row.Detail)
				}
				if strings.Contains(row.Detail, "MATERIALIZE r") || strings.Contains(row.Detail, "COMPOUND QUERY") {
					t.Errorf("%s rebuilds the complete recognition catalog/empty UNION for a bounded read: %s", mode, row.Detail)
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCatalogReaderDeltaPagingUsesOrderedIndexes(t *testing.T) {
	store, lib, rec, entries := catalogFixture(t)
	catalogConvert(t, store, lib, rec, entries)
	candidate, token := catalogCandidate(t, store, lib, "delta", 1)
	rec.Title = "Latest shared title"
	if err := store.AppendBatch(context.Background(), candidate.ID, token, CatalogFactBatch{Recognitions: []models.CatalogRecognitionFact{CatalogRecognitionFromLegacy(rec)}}); err != nil {
		t.Fatal(err)
	}
	catalogPublish(t, store, candidate, token)
	if err := store.Read(context.Background(), []uint{lib.ID}, func(r *CatalogReader) error {
		var rows []models.MediaLibraryEntry
		q := r.Entries().Session(&gorm.Session{DryRun: true}).Where("id > ?", 1).Order("id").Limit(250).Find(&rows)
		var plan []struct{ Detail string }
		if err := r.tx.Raw("EXPLAIN QUERY PLAN "+q.Statement.SQL.String(), q.Statement.Vars...).Scan(&plan).Error; err != nil {
			return err
		}
		merged := false
		for _, row := range plan {
			merged = merged || strings.Contains(row.Detail, "MERGE (UNION ALL)")
			if strings.Contains(row.Detail, "USE TEMP B-TREE FOR ORDER BY") {
				t.Errorf("keyset page sorts the remaining full catalog: %s", row.Detail)
			}
		}
		if !merged {
			t.Error("delta page must merge snapshot ID indexes")
		}
		identityQuery := r.entryIdentities().Session(&gorm.Session{DryRun: true}).Where("id IN ?", []uint{entries[0].ID}).Find(&rows)
		if strings.Contains(identityQuery.Statement.SQL.String(), "catalog_recognition_facts") {
			t.Error("physical path/ID validation must not read recognition metadata")
		}
		// Compaction pins explicit layers without a head map. Its raw E/R/A
		// pages need the same ordered plan; otherwise every 250-row copy sorts
		// the rest of the entire library again.
		private := &CatalogReader{tx: r.tx, layers: r.layers, privateLayers: true}
		for _, table := range []string{"catalog_entry_facts", "catalog_recognition_facts", "catalog_source_asset_facts"} {
			sql, args := private.effectiveSQL(table)
			q := r.tx.Table("(?) AS f", r.tx.Raw(sql, args...)).Session(&gorm.Session{DryRun: true}).Where("id > ? AND tombstone=0", 1).Order("id").Limit(250).Find(&rows)
			var privatePlan []struct{ Detail string }
			if err := r.tx.Raw("EXPLAIN QUERY PLAN "+q.Statement.SQL.String(), q.Statement.Vars...).Scan(&privatePlan).Error; err != nil {
				return err
			}
			for _, step := range privatePlan {
				if strings.Contains(step.Detail, "USE TEMP B-TREE FOR ORDER BY") {
					t.Errorf("%s private page sorts full suffix: %s", table, step.Detail)
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

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

func TestCatalogReaderPhysicalEntryCountMatchesEffectiveVersions(t *testing.T) {
	store, lib, rec, entries := catalogFixture(t)
	check := func() {
		t.Helper()
		if err := store.Read(context.Background(), []uint{lib.ID}, func(reader *CatalogReader) error {
			var expected int64
			if err := reader.Entries().Count(&expected).Error; err != nil {
				return err
			}
			count, err := reader.EntryCount()
			if err == nil && count != expected {
				t.Errorf("physical total %d != effective versions %d", count, expected)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	check() // converting: legacy remains readable
	catalogConvert(t, store, lib, rec, entries)
	check()
	c, token := catalogCandidate(t, store, lib, "delta", 1)
	fact := CatalogEntryFromLegacy(entries[0], &rec)
	fact.Tombstone = true
	if err := store.AppendBatch(context.Background(), c.ID, token, CatalogFactBatch{Entries: []models.CatalogEntryFact{fact}}); err != nil {
		t.Fatal(err)
	}
	catalogPublish(t, store, c, token)
	check()
}
