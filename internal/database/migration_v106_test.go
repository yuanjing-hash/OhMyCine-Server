package database

import "testing"

func TestMigrationV106AddsUnboundedRemoteUploadPlanRows(t *testing.T) {
	db, err := Open(t.TempDir() + "/migration-v106.db")
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	applyMigrationsThrough(t, db, 105)

	if db.Migrator().HasTable("remote_upload_files") {
		t.Fatal("v106 table exists before migration")
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	if !db.Migrator().HasTable("remote_upload_files") {
		t.Fatal("v106 remote upload plan table missing")
	}
	for _, index := range []string{"idx_remote_upload_ordinal", "idx_remote_upload_source", "idx_remote_upload_download", "idx_remote_upload_status"} {
		if !db.Migrator().HasIndex("remote_upload_files", index) {
			t.Fatalf("v106 index %s missing", index)
		}
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("repeat migration: %v", err)
	}
	var count int64
	if err := db.Table("schema_migrations").Where("version = ?", 106).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("v106 migration count=%d err=%v", count, err)
	}
}
