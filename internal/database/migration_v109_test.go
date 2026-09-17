package database

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestMigrationV109BrowserStateFreshAndUpgrade(t *testing.T) {
	for _, previous := range []int{0, 108} {
		name := "fresh"
		if previous != 0 {
			name = "upgrade"
		}
		t.Run(name, func(t *testing.T) {
			db, err := Open(filepath.Join(t.TempDir(), "browser-state.db"))
			if err != nil {
				t.Fatal(err)
			}
			sqlDB, _ := db.DB()
			t.Cleanup(func() { _ = sqlDB.Close() })
			if previous != 0 {
				applyMigrationsThrough(t, db, previous)
			}
			if db.Migrator().HasTable("plugin_browser_states") {
				t.Fatal("browser state table exists before v109")
			}
			if err := Migrate(db); err != nil {
				t.Fatal(err)
			}
			for _, column := range []string{"connection_id", "plugin_id", "owner_id", "fingerprint", "ciphertext", "updated_at"} {
				if !db.Migrator().HasColumn("plugin_browser_states", column) {
					t.Fatalf("missing browser column %s", column)
				}
			}
			for _, index := range []string{"idx_plugin_browser_states_plugin_id", "idx_plugin_browser_states_owner_id"} {
				if !db.Migrator().HasIndex("plugin_browser_states", index) {
					t.Fatalf("missing index %s", index)
				}
			}
			if err := Migrate(db); err != nil {
				t.Fatal(err)
			}
			var count int64
			if err := db.Table("schema_migrations").Where("version = ?", 109).Count(&count).Error; err != nil || count != 1 {
				t.Fatalf("migration applied %d times: %v", count, err)
			}
		})
	}
}

func TestBrowserStateCascadeAndPrivateSerialization(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "browser-cascade.db"))
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	// Minimal parent tables isolate the new migration's actual SQLite constraints.
	for _, statement := range []string{
		"CREATE TABLE users (id INTEGER PRIMARY KEY)",
		"CREATE TABLE plugin_connections (id TEXT PRIMARY KEY)",
		"INSERT INTO users(id) VALUES (1)",
		"INSERT INTO plugin_connections(id) VALUES ('a'), ('b')",
	} {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := migratePluginBrowserState(db); err != nil {
		t.Fatal(err)
	}
	state := models.PluginBrowserState{ConnectionID: "a", PluginID: "example", OwnerID: 1, Fingerprint: "binding", Ciphertext: "encrypted-test-only", UpdatedAt: time.Now().UTC()}
	if err := db.Create(&state).Error; err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(state)
	if err != nil || string(encoded) != "{}" {
		t.Fatal("private browser state must not enter JSON projections")
	}
	state.ConnectionID = "b"
	if err := db.Create(&state).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("DELETE FROM plugin_connections WHERE id = 'a'").Error; err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := db.Model(&models.PluginBrowserState{}).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("connection cleanup count=%d err=%v", count, err)
	}
	if err := db.Exec("DELETE FROM users WHERE id = 1").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.PluginBrowserState{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("owner cleanup count=%d err=%v", count, err)
	}
	if err := db.Create(&state).Error; err == nil {
		t.Fatal("orphan browser state accepted")
	}
}
