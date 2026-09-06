package database

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"gorm.io/gorm"
)

func TestCatalogReadOnlyPoolIsDeferredAndSnapshotConsistent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "read-pool.db")
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	sqlWriter, _ := writer.DB()
	t.Cleanup(func() { _ = sqlWriter.Close() })
	if err := writer.Exec("CREATE TABLE probe(id INTEGER PRIMARY KEY,value INTEGER NOT NULL)").Error; err != nil {
		t.Fatal(err)
	}
	if err := writer.Exec("INSERT INTO probe(id,value) VALUES(1,1)").Error; err != nil {
		t.Fatal(err)
	}
	reader, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	sqlReader, _ := reader.DB()
	t.Cleanup(func() { _ = sqlReader.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err = reader.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var before, after int
		if err := tx.Raw("SELECT value FROM probe WHERE id=1").Scan(&before).Error; err != nil {
			return err
		}
		// This must complete while the pinned reader is still open. An immediate
		// read pool would reserve the writer and fail this actual concurrency test.
		if err := writer.WithContext(ctx).Exec("UPDATE probe SET value=2 WHERE id=1").Error; err != nil {
			return err
		}
		if err := tx.Raw("SELECT value FROM probe WHERE id=1").Scan(&after).Error; err != nil {
			return err
		}
		if before != 1 || after != 1 {
			t.Fatalf("reader mixed snapshots: %d -> %d", before, after)
		}
		if err := tx.Exec("UPDATE probe SET value=3 WHERE id=1").Error; err == nil {
			t.Fatal("query_only transaction accepted writes")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var latest int
	if err := reader.Raw("SELECT value FROM probe WHERE id=1").Scan(&latest).Error; err != nil || latest != 2 {
		t.Fatalf("latest=%d err=%v", latest, err)
	}
	if err := reader.Exec("CREATE TABLE forbidden(id INTEGER)").Error; err == nil {
		t.Fatal("read pool accepted schema mutation")
	}
}

func TestCatalogReadOnlyPoolDoesNotCreateMissingDatabase(t *testing.T) {
	if db, err := OpenReadOnly(filepath.Join(t.TempDir(), "missing", "catalog.db")); err == nil {
		sqlDB, _ := db.DB()
		_ = sqlDB.Close()
		t.Fatal("read-only open created missing database")
	}
}

func TestCatalogSnapshotMigrationV75IsAdditiveAndDisabled(t *testing.T) {
	db := structureMigrationDB(t, 74)
	for i := 0; i < 2; i++ {
		if err := Migrate(db); err != nil {
			t.Fatal(err)
		}
	}
	var floor int
	if err := db.Raw("SELECT format FROM catalog_format_floor WHERE id=1").Scan(&floor).Error; err != nil || floor != 0 {
		t.Fatalf("floor=%d err=%v", floor, err)
	}
	for _, table := range []string{"catalog_heads", "catalog_head_layers", "catalog_snapshots", "catalog_identities", "catalog_entry_facts", "catalog_recognition_facts", "catalog_source_asset_facts", "catalog_snapshot_references"} {
		var count int64
		if err := db.Table(table).Count(&count).Error; err != nil || count != 0 {
			t.Fatalf("%s count=%d err=%v", table, count, err)
		}
	}
	var count int64
	if err := db.Table("schema_migrations").Where("version=75").Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("migration count=%d err=%v", count, err)
	}
}

func TestReadCatalogFormatFailsClosedForMalformedFloor(t *testing.T) {
	db := structureMigrationDB(t, 74)
	if format, err := ReadCatalogFormat(context.Background(), db); err != nil || format != 0 {
		t.Fatalf("legacy format=%d err=%v", format, err)
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("UPDATE catalog_format_floor SET format=99 WHERE id=1").Error; err != nil {
		t.Fatal(err)
	}
	if format, err := ReadCatalogFormat(context.Background(), db); err != nil || format != 99 {
		t.Fatalf("future format hidden=%d err=%v", format, err)
	}
	if err := db.Exec("DELETE FROM catalog_format_floor WHERE id=1").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := ReadCatalogFormat(context.Background(), db); err == nil {
		t.Fatal("missing floor treated as legacy")
	}
}
