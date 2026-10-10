package database

import "gorm.io/gorm"

func migratePluginCatalogueSnapshots(db *gorm.DB) error {
	statements := []string{
		`ALTER TABLE plugin_feed_caches ADD COLUMN scope_key TEXT NOT NULL DEFAULT ''`,
		`CREATE TABLE plugin_catalogue_snapshots (id TEXT PRIMARY KEY, library_id TEXT NOT NULL, scope_key TEXT NOT NULL, kind TEXT NOT NULL, selector TEXT NOT NULL, depth INTEGER NOT NULL DEFAULT 0, ancestors_json TEXT NOT NULL DEFAULT '[]', response_json TEXT NOT NULL, refresh_session TEXT NOT NULL DEFAULT '', fresh_until DATETIME NOT NULL, stale_until DATETIME NOT NULL, retry_at DATETIME NOT NULL, failures INTEGER NOT NULL DEFAULT 0, updated_at DATETIME NOT NULL, FOREIGN KEY(library_id) REFERENCES plugin_online_libraries(id) ON DELETE CASCADE)`,
		`CREATE INDEX idx_plugin_catalogue_library ON plugin_catalogue_snapshots(library_id)`,
		`CREATE INDEX idx_plugin_catalogue_scope ON plugin_catalogue_snapshots(scope_key)`,
		`CREATE INDEX idx_plugin_catalogue_refresh ON plugin_catalogue_snapshots(fresh_until,retry_at)`,
		`CREATE INDEX idx_plugin_catalogue_stale ON plugin_catalogue_snapshots(stale_until)`,
		`CREATE TABLE plugin_online_media_identities (id TEXT PRIMARY KEY, library_id TEXT NOT NULL, scope_key TEXT NOT NULL, item_token TEXT NOT NULL, metadata_json TEXT NOT NULL, expires_at DATETIME NOT NULL, updated_at DATETIME NOT NULL, FOREIGN KEY(library_id) REFERENCES plugin_online_libraries(id) ON DELETE CASCADE)`,
		`CREATE INDEX idx_plugin_online_identity_scope ON plugin_online_media_identities(library_id,scope_key,item_token)`,
		`CREATE INDEX idx_plugin_online_identity_expiry ON plugin_online_media_identities(expires_at)`,
		`CREATE TABLE plugin_online_playback_receipts (user_id INTEGER NOT NULL, identity_key TEXT NOT NULL, scope_key TEXT NOT NULL, expires_at DATETIME NOT NULL, PRIMARY KEY(user_id,identity_key), FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE)`,
		`CREATE INDEX idx_plugin_online_receipt_expiry ON plugin_online_playback_receipts(expires_at)`,
		// Old feed rows may contain expired provider-image leases. They cannot
		// become durable catalogue snapshots on upgrade.
		`DELETE FROM plugin_feed_caches`,
	}
	for _, statement := range statements {
		if err := db.Exec(statement).Error; err != nil {
			return err
		}
	}
	return nil
}
