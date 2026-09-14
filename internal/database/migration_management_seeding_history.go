package database

import "gorm.io/gorm"

// v99 extends the shared management-history retirement contract to the
// independently visible seeding-management list. It does not remove provider
// tasks, downloads, or seeding facts.
func migrateManagementSeedingHistory(db *gorm.DB) error {
	for _, statement := range []string{
		`ALTER TABLE seeding_tasks ADD COLUMN history_cleared_at DATETIME`,
		`CREATE INDEX idx_seeding_tasks_history_cleared ON seeding_tasks(history_cleared_at)`,
	} {
		if err := db.Exec(statement).Error; err != nil {
			return err
		}
	}
	return nil
}
