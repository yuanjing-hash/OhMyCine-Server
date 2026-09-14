package database

import "gorm.io/gorm"

// migrateRemoteUploadFiles keeps the full Server-authored target plan outside
// queue payloads and bounded Node wire batches. It stores no credential or
// absolute path and is removed with the owning TransferTask.
func migrateRemoteUploadFiles(db *gorm.DB) error {
	statements := []string{
		`CREATE TABLE remote_upload_files (id INTEGER PRIMARY KEY AUTOINCREMENT, transfer_task_id TEXT NOT NULL, download_task_id TEXT NOT NULL, ordinal INTEGER NOT NULL, source_file_token TEXT NOT NULL, source_relative TEXT NOT NULL, target_relative TEXT NOT NULL, size INTEGER NOT NULL, sha256 TEXT NOT NULL, conflict_action TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'pending', target_parent_id TEXT NOT NULL DEFAULT '', target_item_id TEXT NOT NULL DEFAULT '', target_sha1 TEXT NOT NULL DEFAULT '', created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL, FOREIGN KEY(transfer_task_id) REFERENCES transfer_tasks(id) ON DELETE CASCADE, FOREIGN KEY(download_task_id) REFERENCES download_tasks(id) ON DELETE CASCADE)`,
		`CREATE UNIQUE INDEX idx_remote_upload_ordinal ON remote_upload_files(transfer_task_id,ordinal)`,
		`CREATE UNIQUE INDEX idx_remote_upload_source ON remote_upload_files(transfer_task_id,source_file_token)`,
		`CREATE INDEX idx_remote_upload_download ON remote_upload_files(download_task_id)`,
		`CREATE INDEX idx_remote_upload_status ON remote_upload_files(status)`,
	}
	for _, statement := range statements {
		if err := db.Exec(statement).Error; err != nil {
			return err
		}
	}
	return nil
}
