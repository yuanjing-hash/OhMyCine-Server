package database

import (
	"reflect"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestMigrationReleased118To122PreservesAuthorizationHistoryAndConfiguration(t *testing.T) {
	db, err := Open(t.TempDir() + "/released118-upgrade.db")
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	applyMigrationsThrough(t, db, 118)
	if err := seedAuthorization(db); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	user := models.User{Username: "upgrade-user", UsernameNormalized: "upgrade-user", DisplayName: "Existing user", PasswordHash: "existing-password-hash", Status: "active", CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	history := models.PlayerPlaybackHistory{UserID: user.ID, SyncKey: "existing-sync-key", HistoryIdentity: "server|existing-media", SourceKind: "server", SourceID: "existing-server", LibraryID: "existing-library", ItemToken: "existing-media", MediaIdentity: "existing-media", Title: "Existing history", Position: 123, ClientUpdatedAt: now.UnixMilli(), Revision: 4, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&history).Error; err != nil {
		t.Fatal(err)
	}
	site := models.Site{Name: "Existing Jackett", NameNormalized: "existing jackett", Kind: "torznab", BaseURL: "http://jackett:9117", CredentialCiphertext: "preserved-encrypted-api-key", TimeoutSeconds: 12, Enabled: true, Revision: 8, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&site).Error; err != nil {
		t.Fatal(err)
	}
	// Authorization seeds intentionally refresh timestamps and rebuild the
	// builtin grant rows. Compare their effective grants, not those timestamps.
	columns := map[string]string{
		"users": "*", "player_playback_history": "*", "sites": "*",
		"roles":            "id,code,name,description,kind,protected,active",
		"permissions":      "code,module,name,description,risk",
		"role_permissions": "role_id,permission_code",
	}
	before := map[string][]map[string]any{}
	for table, selectColumns := range columns {
		var rows []map[string]any
		if err := db.Table(table).Select(selectColumns).Order("1,2").Find(&rows).Error; err != nil {
			t.Fatal(err)
		}
		before[table] = rows
	}
	for attempt := range 2 {
		if err := Migrate(db); err != nil {
			t.Fatal(err)
		}
		for table, want := range before {
			var got []map[string]any
			if err := db.Table(table).Select(columns[table]).Order("1,2").Find(&got).Error; err != nil || !reflect.DeepEqual(want, got) {
				t.Fatalf("upgrade/restart %d changed %s: %v", attempt, table, err)
			}
		}
		var versions []int
		if err := db.Table("schema_migrations").Where("version >= ?", 116).Order("version").Pluck("version", &versions).Error; err != nil || !reflect.DeepEqual(versions, []int{116, 117, 118, 119, 120, 121, 122}) {
			t.Fatalf("released migration identities lost or new ones repeated: %v %v", versions, err)
		}
		for _, model := range []any{&models.ProviderObservationScope{}, &models.ProviderObservationBatch{}, &models.ProviderObservationItem{}} {
			if !db.Migrator().HasTable(model) {
				t.Fatalf("Quark observation schema missing: %T", model)
			}
		}
	}
}

func TestMigrationQuarkPreservesLegacyRowsConstraintsAndIdentity(t *testing.T) {
	db, err := Open(t.TempDir() + "/quark-upgrade.db")
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	applyMigrationsThrough(t, db, 118)
	connection := models.Connection{Name: "legacy", NameNormalized: "legacy", Provider: models.ConnectionProviderPan115, CredentialCiphertext: "unchanged-encrypted115", Enabled: true, Revision: 9}
	if err := db.Create(&connection).Error; err != nil {
		t.Fatal(err)
	}
	storage := models.Storage{Name: "legacy", NameNormalized: "legacy", Type: models.StorageTypePan115, RootPath: "42", RootPathNormalized: "115:1:42", ConnectionID: &connection.ID, Enabled: true, Capabilities: "{}"}
	if err := db.Create(&storage).Error; err != nil {
		t.Fatal(err)
	}
	var before models.Storage
	if err := db.First(&before, storage.ID).Error; err != nil {
		t.Fatal(err)
	}
	// Retain identities already issued even if the highest row was removed.
	if err := db.Exec("UPDATE sqlite_sequence SET seq=100 WHERE name='storages'").Error; err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := Migrate(db); err != nil {
			t.Fatal(err)
		}
	}
	var after models.Storage
	if err := db.First(&after, storage.ID).Error; err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("legacy storage changed: %v", err)
	}
	var gotConnection models.Connection
	if err := db.First(&gotConnection, connection.ID).Error; err != nil || gotConnection.CredentialCiphertext != connection.CredentialCiphertext || gotConnection.Revision != 9 {
		t.Fatalf("legacy credential/revision changed: %v", err)
	}
	quark := models.Storage{Name: "quark", NameNormalized: "quark", Type: models.StorageTypeQuark, RootPath: "tv:root", RootPathNormalized: "quark:1:root", ConnectionID: &connection.ID, Enabled: true, Capabilities: "{}"}
	if err := db.Create(&quark).Error; err != nil || quark.ID <= 100 {
		t.Fatalf("new provider/identity: %d %v", quark.ID, err)
	}
	if err := db.Model(&quark).Update("type", "unrecognized").Error; err == nil {
		t.Fatal("unknown storage accepted")
	}
	if err := db.Delete(&connection).Error; err == nil {
		t.Fatal("connection foreign key lost")
	}
	duplicate := quark
	duplicate.ID = 0
	if err := db.Create(&duplicate).Error; err == nil {
		t.Fatal("unique storage identity lost")
	}
	var violations int64
	if err := db.Raw("SELECT COUNT(*) FROM pragma_foreign_key_check").Scan(&violations).Error; err != nil || violations != 0 {
		t.Fatalf("foreign key violations %d: %v", violations, err)
	}
}

func TestMigrationQuarkRetainsSequenceAfterAllRowsWereDeleted(t *testing.T) {
	db, err := Open(t.TempDir() + "/empty-upgrade.db")
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	applyMigrationsThrough(t, db, 118)
	if err := db.Exec("INSERT INTO sqlite_sequence(name,seq) VALUES ('storages',812)").Error; err != nil {
		t.Fatal(err)
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	row := models.Storage{Name: "local", NameNormalized: "local", Type: models.StorageTypeLocal, RootPath: "/media", RootPathNormalized: "/media", Enabled: true, Capabilities: "{}"}
	if err := db.Create(&row).Error; err != nil || row.ID <= 812 {
		t.Fatalf("retired ID reused: %d %v", row.ID, err)
	}
}
