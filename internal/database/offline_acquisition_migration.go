package database

import "gorm.io/gorm"

func migrateOfflineAcquisitionPolicy(db *gorm.DB) error {
	statements := []string{
		`ALTER TABLE downloaders ADD COLUMN max_concurrent_downloads INTEGER NOT NULL DEFAULT 2 CHECK(max_concurrent_downloads BETWEEN 1 AND 64)`,
		`ALTER TABLE download_tasks ADD COLUMN offline_kind TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE download_tasks ADD COLUMN offline_account_namespace TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE download_tasks ADD COLUMN offline_resource_digest TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE download_tasks ADD COLUMN offline_required_digest TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE download_tasks ADD COLUMN offline_required_intent_json TEXT NOT NULL DEFAULT '{}'`,
		`ALTER TABLE download_tasks ADD COLUMN offline_history_revision INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE download_tasks ADD COLUMN offline_started_at DATETIME`,
		`ALTER TABLE download_tasks ADD COLUMN offline_start_provenance TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE download_tasks ADD COLUMN offline_deadline_at DATETIME`,
		`ALTER TABLE download_tasks ADD COLUMN offline_ready_at DATETIME`,
		`ALTER TABLE download_tasks ADD COLUMN offline_completion_mode TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE download_tasks ADD COLUMN offline_stop_pending INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE download_tasks ADD COLUMN offline_settled_at DATETIME`,
		`ALTER TABLE download_tasks ADD COLUMN offline_clock_state TEXT NOT NULL DEFAULT ''`,
		`CREATE INDEX idx_download_offline_deadline ON download_tasks(offline_deadline_at) WHERE offline_ready_at IS NULL AND offline_started_at IS NOT NULL`,
		`CREATE TABLE download_admissions (acquisition_key TEXT PRIMARY KEY, downloader_id TEXT NOT NULL, task_id TEXT NOT NULL, state TEXT NOT NULL CHECK(state IN ('held','released')), created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL, released_at DATETIME)`,
		`CREATE INDEX idx_download_admissions_downloader ON download_admissions(downloader_id,state)`,
		`CREATE TABLE offline_failures (id INTEGER PRIMARY KEY AUTOINCREMENT, task_id TEXT NOT NULL UNIQUE, owner_id INTEGER NOT NULL, account_namespace TEXT NOT NULL, resource_digest TEXT NOT NULL, required_digest TEXT NOT NULL, reason TEXT NOT NULL, started_at DATETIME NOT NULL, deadline_at DATETIME NOT NULL, created_at DATETIME NOT NULL, resolved_at DATETIME)`,
		`CREATE INDEX idx_offline_failure_scope ON offline_failures(owner_id,account_namespace,resource_digest,required_digest,id)`,
		`CREATE TABLE offline_confirmations (token_hash TEXT PRIMARY KEY, owner_id INTEGER NOT NULL, request_digest TEXT NOT NULL, intake_digest TEXT NOT NULL DEFAULT '', authz_version INTEGER NOT NULL DEFAULT 0, source_site_id INTEGER NOT NULL DEFAULT 0, source_site_revision INTEGER NOT NULL DEFAULT 0, history_revision INTEGER NOT NULL, accepted_task_id TEXT NOT NULL DEFAULT '', created_at DATETIME NOT NULL, expires_at DATETIME NOT NULL)`,
		`CREATE INDEX idx_offline_confirmation_expiry ON offline_confirmations(expires_at)`,
		// Adopt existing live/ambiguous work without inventing clocks or killing
		// acquisitions above the new default. Failed tracking is not proof of stop.
		`INSERT OR IGNORE INTO download_admissions(acquisition_key,downloader_id,task_id,state,created_at,updated_at) SELECT CASE WHEN bt_selection_group_id <> '' THEN 'bt:' || bt_selection_group_id ELSE 'task:' || id END, downloader_id, id, 'held', created_at, updated_at FROM download_tasks WHERE downloader_id IS NOT NULL AND (provider_task_id <> '' OR phase = 'submitting' OR last_error_code IN ('downloader_submission_unconfirmed','downloader_submit_unknown','downloader_selection_result_unknown','downloader_share_result_unknown')) AND phase <> 'completed' AND (phase <> 'cancelled' OR last_error_code = 'downloader_cancellation_pending') AND (phase NOT IN ('failed','cancelled') OR provider_task_id <> '' OR last_error_code IN ('downloader_submission_unconfirmed','downloader_submit_unknown','downloader_selection_result_unknown','downloader_share_result_unknown','downloader_cancellation_pending')) ORDER BY created_at,id`,
	}
	for _, statement := range statements {
		if err := db.Exec(statement).Error; err != nil {
			return err
		}
	}
	return nil
}
