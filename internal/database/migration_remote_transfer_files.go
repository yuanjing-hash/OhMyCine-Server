package database

import "gorm.io/gorm"

// migrateRemoteTransferFiles adds compact, per-file chunk completion state for
// Node -> Server local-library transfers. It contains no absolute path and is
// deleted with the owning TransferTask.
func migrateRemoteTransferFiles(db *gorm.DB) error {
	statements := []string{
		`CREATE TABLE remote_transfer_files (id INTEGER PRIMARY KEY AUTOINCREMENT, transfer_task_id TEXT NOT NULL, download_task_id TEXT NOT NULL, operation_key TEXT NOT NULL, manifest_digest TEXT NOT NULL, file_token TEXT NOT NULL, relative_path TEXT NOT NULL, size INTEGER NOT NULL, sha256 TEXT NOT NULL, chunk_size INTEGER NOT NULL, chunk_count INTEGER NOT NULL, completed_bitmap BLOB NOT NULL, status TEXT NOT NULL DEFAULT 'pending', created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL, FOREIGN KEY(transfer_task_id) REFERENCES transfer_tasks(id) ON DELETE CASCADE, FOREIGN KEY(download_task_id) REFERENCES download_tasks(id) ON DELETE CASCADE)`,
		`CREATE UNIQUE INDEX idx_remote_transfer_file_token ON remote_transfer_files(transfer_task_id,file_token)`,
		`CREATE UNIQUE INDEX idx_remote_transfer_file_path ON remote_transfer_files(transfer_task_id,relative_path)`,
		`CREATE INDEX idx_remote_transfer_files_download ON remote_transfer_files(download_task_id)`,
		`CREATE INDEX idx_remote_transfer_files_status ON remote_transfer_files(status)`,
	}
	for _, statement := range statements {
		if err := db.Exec(statement).Error; err != nil {
			return err
		}
	}
	return nil
}
