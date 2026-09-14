package database

import "testing"

func TestMigrationV105AddsRemoteTransferFileCheckpoints(t *testing.T) {
	db, err := Open(t.TempDir() + "/migration-v105.db")
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	applyMigrationsThrough(t, db, 104)

	if db.Migrator().HasTable("remote_transfer_files") {
		t.Fatal("v105 table exists before migration")
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	if !db.Migrator().HasTable("remote_transfer_files") {
		t.Fatal("v105 remote transfer file table missing")
	}
	for _, index := range []string{"idx_remote_transfer_file_token", "idx_remote_transfer_file_path", "idx_remote_transfer_files_download", "idx_remote_transfer_files_status"} {
		if !db.Migrator().HasIndex("remote_transfer_files", index) {
			t.Fatalf("v105 index %s missing", index)
		}
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("repeat migration: %v", err)
	}
	var count int64
	if err := db.Table("schema_migrations").Where("version = ?", 105).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("v105 migration count=%d err=%v", count, err)
	}
}
