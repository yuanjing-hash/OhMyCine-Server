package database

import "testing"

func TestMigrationV107AddsPluginResourceSiteContracts(t *testing.T) {
	db, err := Open(t.TempDir() + "/migration-v107.db")
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	applyMigrationsThrough(t, db, 106)

	if db.Migrator().HasTable("plugin_resource_claims") {
		t.Fatal("v107 claim table exists before migration")
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	if !db.Migrator().HasTable("plugin_resource_claims") {
		t.Fatal("v107 plugin resource claim table missing")
	}
	for _, table := range []string{"plugin_connections", "sites", "download_tasks"} {
		if !db.Migrator().HasTable(table) {
			t.Fatalf("required table %s missing", table)
		}
	}
	for _, index := range []struct {
		table string
		name  string
	}{
		{"plugin_resource_claims", "idx_plugin_resource_claim_owner_expiry"},
		{"plugin_resource_claims", "idx_plugin_resource_claim_connection"},
		{"plugin_resource_claims", "idx_plugin_resource_claim_consumed"},
		{"sites", "idx_sites_source_type"},
		{"sites", "idx_sites_plugin_id"},
		{"sites", "idx_sites_plugin_connection_id"},
		{"download_tasks", "idx_download_tasks_plugin_resource_claim_id"},
	} {
		if !db.Migrator().HasIndex(index.table, index.name) {
			t.Fatalf("v107 index %s on %s missing", index.name, index.table)
		}
	}
	for _, column := range []struct {
		table string
		name  string
	}{
		{"plugin_connections", "resource_type"},
		{"plugin_connections", "entry_origin"},
		{"plugin_connections", "login_account_label"},
		{"plugin_connections", "credential_version"},
		{"sites", "source_type"},
		{"sites", "plugin_id"},
		{"sites", "plugin_connection_id"},
		{"download_tasks", "plugin_resource_claim_id"},
	} {
		if !db.Migrator().HasColumn(column.table, column.name) {
			t.Fatalf("v107 column %s.%s missing", column.table, column.name)
		}
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("repeat migration: %v", err)
	}
	var count int64
	if err := db.Table("schema_migrations").Where("version = ?", 107).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("v107 migration count=%d err=%v", count, err)
	}
}
