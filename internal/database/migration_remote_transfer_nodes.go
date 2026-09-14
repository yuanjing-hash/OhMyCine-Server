package database

import "gorm.io/gorm"

// migrateRemoteTransferNodes adds the Server-side identity, binding and
// checkpoint projections for ohmycine-node. Existing downloads remain bound
// to the Server by default; no task is silently moved to a node.
func migrateRemoteTransferNodes(db *gorm.DB) error {
	statements := []string{
		`CREATE TABLE transfer_nodes (id TEXT PRIMARY KEY, owner_id INTEGER NOT NULL, name TEXT NOT NULL, name_normalized TEXT NOT NULL UNIQUE, api_url TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'pending', platform TEXT NOT NULL DEFAULT '', architecture TEXT NOT NULL DEFAULT '', protocol_min INTEGER NOT NULL DEFAULT 1, protocol_max INTEGER NOT NULL DEFAULT 1, agent_version TEXT NOT NULL DEFAULT '', public_key_fingerprint TEXT NOT NULL DEFAULT '', encryption_public_key TEXT NOT NULL DEFAULT '', capabilities_json TEXT NOT NULL DEFAULT '{}', free_bytes INTEGER, last_error_code TEXT NOT NULL DEFAULT '', last_heartbeat_at DATETIME, revocation_epoch INTEGER NOT NULL DEFAULT 1, revision INTEGER NOT NULL DEFAULT 1, created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL, FOREIGN KEY(owner_id) REFERENCES users(id) ON DELETE RESTRICT)`,
		`CREATE INDEX idx_transfer_nodes_status ON transfer_nodes(status)`,
		`CREATE INDEX idx_transfer_nodes_owner ON transfer_nodes(owner_id)`,
		`CREATE TABLE node_enrollments (id TEXT PRIMARY KEY, node_id TEXT NOT NULL, token_hash TEXT NOT NULL UNIQUE, token_ciphertext TEXT NOT NULL, platform TEXT NOT NULL DEFAULT '', architecture TEXT NOT NULL DEFAULT '', expires_at DATETIME NOT NULL, consumed_at DATETIME, created_at DATETIME NOT NULL, FOREIGN KEY(node_id) REFERENCES transfer_nodes(id) ON DELETE CASCADE)`,
		`CREATE INDEX idx_node_enrollments_expiry ON node_enrollments(expires_at)`,
		`CREATE TABLE node_downloader_bindings (id TEXT PRIMARY KEY, downloader_id TEXT NOT NULL UNIQUE, node_id TEXT NOT NULL, base_url TEXT NOT NULL, username_ciphertext TEXT NOT NULL DEFAULT '', password_ciphertext TEXT NOT NULL DEFAULT '', downloader_save_root TEXT NOT NULL DEFAULT '', node_mount_root TEXT NOT NULL DEFAULT '', last_test_code TEXT NOT NULL DEFAULT '', last_tested_at DATETIME, revision INTEGER NOT NULL DEFAULT 1, created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL, FOREIGN KEY(downloader_id) REFERENCES downloaders(id) ON DELETE CASCADE, FOREIGN KEY(node_id) REFERENCES transfer_nodes(id) ON DELETE RESTRICT)`,
		`CREATE INDEX idx_node_downloader_bindings_node ON node_downloader_bindings(node_id)`,
		`CREATE TABLE remote_operations (id INTEGER PRIMARY KEY AUTOINCREMENT, operation_key TEXT NOT NULL UNIQUE, task_id TEXT NOT NULL, node_id TEXT NOT NULL, plan_digest TEXT NOT NULL, plan_revision INTEGER NOT NULL, lease_epoch INTEGER NOT NULL DEFAULT 1, phase TEXT NOT NULL DEFAULT 'accepted', status TEXT NOT NULL DEFAULT 'pending', progress REAL, checkpoint_digest TEXT NOT NULL DEFAULT '', error_code TEXT NOT NULL DEFAULT '', created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL, FOREIGN KEY(node_id) REFERENCES transfer_nodes(id) ON DELETE RESTRICT)`,
		`CREATE INDEX idx_remote_operations_task ON remote_operations(task_id)`,
		`CREATE INDEX idx_remote_operations_node_status ON remote_operations(node_id,status)`,
		`CREATE TABLE node_credential_grants (id TEXT PRIMARY KEY, node_id TEXT NOT NULL, task_id TEXT NOT NULL, operation_key TEXT NOT NULL, storage_id INTEGER, downloader_id TEXT, credential_revision INTEGER NOT NULL DEFAULT 1, expires_at DATETIME NOT NULL, revoked_at DATETIME, created_at DATETIME NOT NULL, FOREIGN KEY(node_id) REFERENCES transfer_nodes(id) ON DELETE RESTRICT, FOREIGN KEY(storage_id) REFERENCES storages(id) ON DELETE SET NULL, FOREIGN KEY(downloader_id) REFERENCES downloaders(id) ON DELETE SET NULL)`,
		`CREATE INDEX idx_node_credential_grants_expiry ON node_credential_grants(expires_at)`,
		`CREATE TABLE node_controller_identities (id INTEGER PRIMARY KEY CHECK(id=1), server_id TEXT NOT NULL UNIQUE, certificate_ciphertext TEXT NOT NULL, private_key_ciphertext TEXT NOT NULL, certificate_fingerprint TEXT NOT NULL UNIQUE, created_at DATETIME NOT NULL, expires_at DATETIME NOT NULL)`,
		`CREATE TABLE transfer_node_settings (id INTEGER PRIMARY KEY CHECK(id=1), default_node_id TEXT, revision INTEGER NOT NULL DEFAULT 1, created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL, FOREIGN KEY(default_node_id) REFERENCES transfer_nodes(id) ON DELETE SET NULL)`,
		`INSERT INTO transfer_node_settings(id, default_node_id, revision, created_at, updated_at) VALUES(1, NULL, 1, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		`ALTER TABLE downloaders ADD COLUMN execution_location TEXT NOT NULL DEFAULT 'server'`,
		`ALTER TABLE downloaders ADD COLUMN node_id TEXT`,
		`ALTER TABLE downloaders ADD COLUMN node_name TEXT NOT NULL DEFAULT ''`,
		`CREATE INDEX idx_downloaders_execution_location ON downloaders(execution_location)`,
		`ALTER TABLE download_tasks ADD COLUMN execution_location TEXT NOT NULL DEFAULT 'server'`,
		`ALTER TABLE download_tasks ADD COLUMN node_id TEXT`,
		`ALTER TABLE download_tasks ADD COLUMN node_name TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE download_tasks ADD COLUMN protocol_version INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE download_tasks ADD COLUMN route_plan_revision INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE download_tasks ADD COLUMN route_plan_digest TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE transfer_tasks ADD COLUMN execution_location TEXT NOT NULL DEFAULT 'server'`,
		`ALTER TABLE transfer_tasks ADD COLUMN node_id TEXT`,
		`ALTER TABLE transfer_tasks ADD COLUMN node_name TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE seeding_tasks ADD COLUMN execution_location TEXT NOT NULL DEFAULT 'server'`,
		`ALTER TABLE seeding_tasks ADD COLUMN node_id TEXT`,
		`ALTER TABLE seeding_tasks ADD COLUMN node_name TEXT NOT NULL DEFAULT ''`,
		`CREATE INDEX idx_seeding_tasks_execution_location ON seeding_tasks(execution_location)`,
	}
	for _, statement := range statements {
		if err := db.Exec(statement).Error; err != nil {
			return err
		}
	}
	return nil
}
