package database

import (
	"github.com/google/uuid"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"strings"
	"testing"
	"time"
)

func TestMigrationV117CatalogueCachePreservesAccountsAndHistory(t *testing.T) {
	db, err := Open(t.TempDir() + "/migration-v117.db")
	if err != nil {
		t.Fatal(err)
	}
	connection, _ := db.DB()
	t.Cleanup(func() { connection.Close() })
	applyMigrationsThrough(t, db, 116)
	now := time.Now().UTC()
	pkg := models.PluginPackage{PluginID: "org.example.fixture", Version: "0.1.0", ManifestJSON: `{}`, PackageSHA256: strings.Repeat("a", 64), CreatedAt: now, VerifiedAt: now}
	if err = db.Create(&pkg).Error; err != nil {
		t.Fatal(err)
	}
	if err = db.Create(&models.PluginInstallation{PluginID: pkg.PluginID, ActivePackageID: pkg.ID, Status: models.PluginInstallationEnabled, Revision: 1, RuntimeGeneration: 1, InstalledAt: now, UpdatedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	account := models.PluginConnection{ID: id, PluginID: pkg.PluginID, Name: "Fixture", CredentialCiphertext: "preserved-ciphertext", CredentialVersion: 7, ConfigJSON: `{}`, Enabled: true, Revision: 3, CreatedAt: now, UpdatedAt: now}
	if err = db.Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	if err = db.Create(&models.PluginOnlineLibrary{ID: id, PluginID: pkg.PluginID, ConnectionID: id, Name: "Fixture", ExternalKey: "default", Enabled: true, Revision: 1, CreatedAt: now, UpdatedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	if err = db.Exec(`INSERT INTO plugin_feed_caches(library_id,route_key,cursor_key,refresh_session,response_json,expires_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`, id, "route", "cursor", "session", `[{"posterUrl":"https://provider.invalid?token=old"}]`, now.Add(time.Hour), now, now).Error; err != nil {
		t.Fatal(err)
	}
	user := models.User{Username: "migration-history", UsernameNormalized: "migration-history", DisplayName: "Fixture", PasswordHash: "synthetic", Status: "active", CreatedAt: now, UpdatedAt: now}
	if err = db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	priorHistory := []models.PlayerPlaybackHistory{
		{UserID: user.ID, SyncKey: strings.Repeat("d", 64), HistoryIdentity: "online-version|" + id + "|work|main|full", SourceKind: "server", SourceID: id, LibraryID: "online-library|" + id, ItemToken: "online-version|" + id + "|work|main|full", MediaIdentity: "main", Title: "Preserved watch", DisplaySubtitle: "Official main", Position: 120, ClientUpdatedAt: now.UnixMilli(), Revision: 1, CreatedAt: now, UpdatedAt: now},
		{UserID: user.ID, SyncKey: strings.Repeat("e", 64), HistoryIdentity: "online-version|" + id + "|work|bonus|full", SourceKind: "server", SourceID: id, LibraryID: "online-library|" + id, ItemToken: "online-version|" + id + "|work|bonus|full", MediaIdentity: "bonus", Title: "Preserved deletion", DisplaySubtitle: "Official bonus", Position: 1000, Completed: true, Deleted: true, ClientUpdatedAt: now.Add(time.Second).UnixMilli(), Revision: 2, CreatedAt: now, UpdatedAt: now},
	}
	if err = db.Create(&priorHistory).Error; err != nil {
		t.Fatal(err)
	}
	assertHistoryPreserved := func() {
		t.Helper()
		var rows []models.PlayerPlaybackHistory
		if err := db.Where("user_id = ?", user.ID).Order("sync_key ASC").Find(&rows).Error; err != nil {
			t.Fatal(err)
		}
		if len(rows) != len(priorHistory) {
			t.Fatal("migration changed durable history row count")
		}
		for i, row := range rows {
			want := priorHistory[i]
			if row.SyncKey != want.SyncKey || row.HistoryIdentity != want.HistoryIdentity || row.ItemToken != want.ItemToken || row.SourceID != want.SourceID || row.LibraryID != want.LibraryID || row.Title != want.Title || row.DisplaySubtitle != want.DisplaySubtitle || row.Position != want.Position || row.Completed != want.Completed || row.Deleted != want.Deleted || row.ClientUpdatedAt != want.ClientUpdatedAt || row.Revision != want.Revision {
				t.Fatal("migration changed durable identity, progress, metadata, or tombstone")
			}
		}
	}
	if err = Migrate(db); err != nil {
		t.Fatal(err)
	}
	assertHistoryPreserved()
	var accountAfter models.PluginConnection
	if err = db.First(&accountAfter, "id = ?", id).Error; err != nil || accountAfter.CredentialCiphertext != "preserved-ciphertext" || accountAfter.CredentialVersion != 7 || accountAfter.Revision != 3 {
		t.Fatal("migration changed account")
	}
	var count int64
	if err = db.Model(&models.PluginFeedCache{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatal("legacy private image lease migrated into persistent cache")
	}
	snapshot := models.PluginCatalogueSnapshot{ID: strings.Repeat("b", 64), LibraryID: id, ScopeKey: strings.Repeat("c", 64), Kind: "navigation", ResponseJSON: `[]`, FreshUntil: now.Add(time.Hour), StaleUntil: now.Add(24 * time.Hour), RetryAt: now, UpdatedAt: now}
	if err = db.Create(&snapshot).Error; err != nil {
		t.Fatal(err)
	}
	if err = Migrate(db); err != nil {
		t.Fatal(err)
	}
	assertHistoryPreserved()
	if err = db.Model(&models.PluginCatalogueSnapshot{}).Count(&count).Error; err != nil || count != 1 {
		t.Fatal("restart migration erased snapshots")
	}
}
