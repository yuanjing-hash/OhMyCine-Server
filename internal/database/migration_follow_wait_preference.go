package database

import "gorm.io/gorm"

// Additive only: keep frozen subscriptions, runs, receipts and subtitle policy
// bytes intact. Historical runs deliberately have no inferred retry deadline.
func migrateFollowWaitPreference(db *gorm.DB) error {
	for _, statement := range []string{
		`ALTER TABLE follow_runs ADD COLUMN retry_not_before DATETIME`,
		`ALTER TABLE follow_runs ADD COLUMN retry_code TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE follow_policy_defaults ADD COLUMN acquisition_preference TEXT NOT NULL DEFAULT 'transfer_first'`,
	} {
		if err := db.Exec(statement).Error; err != nil {
			return err
		}
	}
	return nil
}
