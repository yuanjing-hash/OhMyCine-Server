package database

import "gorm.io/gorm"

func migratePluginBrowserState(db *gorm.DB) error {
	for _, statement := range []string{
		`CREATE TABLE plugin_browser_states (
			connection_id TEXT PRIMARY KEY NOT NULL, plugin_id TEXT NOT NULL,
			owner_id INTEGER NOT NULL, fingerprint TEXT NOT NULL,
			ciphertext TEXT NOT NULL, updated_at DATETIME NOT NULL,
			FOREIGN KEY(connection_id) REFERENCES plugin_connections(id) ON DELETE CASCADE,
			FOREIGN KEY(owner_id) REFERENCES users(id) ON DELETE CASCADE
		)`,
		`CREATE INDEX idx_plugin_browser_states_plugin_id ON plugin_browser_states(plugin_id)`,
		`CREATE INDEX idx_plugin_browser_states_owner_id ON plugin_browser_states(owner_id)`,
	} {
		if err := db.Exec(statement).Error; err != nil {
			return err
		}
	}
	return nil
}
