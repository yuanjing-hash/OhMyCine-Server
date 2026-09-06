package database

import "gorm.io/gorm"

// Additive only: zero is the current revision of each existing manifest. No
// backfill or scan of media_managed_items is required.
func migrateManagedManifestRevision(tx *gorm.DB) error {
	if !tx.Migrator().HasColumn("transfer_tasks", "managed_revision") {
		if err := tx.Exec("ALTER TABLE transfer_tasks ADD COLUMN managed_revision INTEGER NOT NULL DEFAULT 0").Error; err != nil {
			return err
		}
	}
	// SQLite overflows integer arithmetic to REAL. Fail the enclosing mutation
	// atomically instead of losing monotonic CAS precision at that boundary.
	bump := `managed_revision=CASE WHEN typeof(managed_revision)='integer' AND managed_revision>=0 AND managed_revision<9223372036854775807 THEN managed_revision+1 ELSE RAISE(ABORT,'managed manifest revision exhausted') END`
	statements := []string{
		`CREATE TRIGGER IF NOT EXISTS managed_revision_insert AFTER INSERT ON media_managed_items BEGIN UPDATE transfer_tasks SET ` + bump + ` WHERE id=NEW.transfer_task_id; END`,
		`CREATE TRIGGER IF NOT EXISTS managed_revision_delete AFTER DELETE ON media_managed_items BEGIN UPDATE transfer_tasks SET ` + bump + ` WHERE id=OLD.transfer_task_id; END`,
		`CREATE TRIGGER IF NOT EXISTS managed_revision_update AFTER UPDATE OF transfer_task_id,library_id,download_task_id,identity_revision,kind,relative_path,provider_item_id,provider_parent_id,size,managed,active ON media_managed_items WHEN OLD.transfer_task_id IS NOT NEW.transfer_task_id OR OLD.library_id IS NOT NEW.library_id OR OLD.download_task_id IS NOT NEW.download_task_id OR OLD.identity_revision IS NOT NEW.identity_revision OR OLD.kind IS NOT NEW.kind OR OLD.relative_path IS NOT NEW.relative_path OR OLD.provider_item_id IS NOT NEW.provider_item_id OR OLD.provider_parent_id IS NOT NEW.provider_parent_id OR OLD.size IS NOT NEW.size OR OLD.managed IS NOT NEW.managed OR OLD.active IS NOT NEW.active BEGIN UPDATE transfer_tasks SET ` + bump + ` WHERE id IN (OLD.transfer_task_id,NEW.transfer_task_id); END`,
	}
	for _, statement := range statements {
		if err := tx.Exec(statement).Error; err != nil {
			return err
		}
	}
	return nil
}
