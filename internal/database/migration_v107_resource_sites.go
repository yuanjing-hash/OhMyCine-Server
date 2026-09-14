package database

import "gorm.io/gorm"

func migratePluginResourceSites(db *gorm.DB) error {
	statements := []string{
		`ALTER TABLE plugin_connections ADD COLUMN resource_type TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE plugin_connections ADD COLUMN entry_origin TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE plugin_connections ADD COLUMN login_account_label TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE plugin_connections ADD COLUMN credential_version INTEGER NOT NULL DEFAULT 0`,
		`CREATE INDEX idx_plugin_connections_resource_type ON plugin_connections(resource_type)`,
		`ALTER TABLE sites ADD COLUMN source_type TEXT NOT NULL DEFAULT 'builtin'`,
		`ALTER TABLE sites ADD COLUMN plugin_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE sites ADD COLUMN plugin_connection_id TEXT NOT NULL DEFAULT ''`,
		`CREATE INDEX idx_sites_source_type ON sites(source_type)`,
		`CREATE INDEX idx_sites_plugin_id ON sites(plugin_id)`,
		`CREATE UNIQUE INDEX idx_sites_plugin_connection_id ON sites(plugin_connection_id) WHERE plugin_connection_id <> ''`,
		`CREATE TABLE plugin_resource_claims (id TEXT PRIMARY KEY, token_hash TEXT NOT NULL UNIQUE, owner_id INTEGER NOT NULL, site_id INTEGER NOT NULL, plugin_id TEXT NOT NULL, plugin_version TEXT NOT NULL, plugin_connection_id TEXT NOT NULL, resource_id TEXT NOT NULL, title TEXT NOT NULL, subtitle TEXT NOT NULL DEFAULT '', media_type_hint TEXT NOT NULL DEFAULT '', expires_at DATETIME NOT NULL, consumed_at DATETIME, created_at DATETIME NOT NULL, FOREIGN KEY(owner_id) REFERENCES users(id) ON DELETE CASCADE, FOREIGN KEY(site_id) REFERENCES sites(id) ON DELETE CASCADE, FOREIGN KEY(plugin_connection_id) REFERENCES plugin_connections(id) ON DELETE CASCADE)`,
		`CREATE INDEX idx_plugin_resource_claim_owner_expiry ON plugin_resource_claims(owner_id, expires_at)`,
		`CREATE INDEX idx_plugin_resource_claim_site ON plugin_resource_claims(site_id)`,
		`CREATE INDEX idx_plugin_resource_claim_plugin ON plugin_resource_claims(plugin_id)`,
		`CREATE INDEX idx_plugin_resource_claim_connection ON plugin_resource_claims(plugin_connection_id)`,
		`CREATE INDEX idx_plugin_resource_claim_consumed ON plugin_resource_claims(consumed_at)`,
		`ALTER TABLE download_tasks ADD COLUMN plugin_resource_claim_id TEXT NOT NULL DEFAULT ''`,
		`CREATE INDEX idx_download_tasks_plugin_resource_claim_id ON download_tasks(plugin_resource_claim_id)`,
	}
	for _, statement := range statements {
		if err := db.Exec(statement).Error; err != nil {
			return err
		}
	}
	return nil
}
