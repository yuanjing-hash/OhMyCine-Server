package database

import "gorm.io/gorm"

func migrateNotificationReceipts(db *gorm.DB) error {
	return db.Exec(`CREATE TABLE notification_receipts (user_id INTEGER NOT NULL, job_id TEXT NOT NULL, occurrence INTEGER NOT NULL, read_at DATETIME NOT NULL, PRIMARY KEY(user_id,job_id), FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE, FOREIGN KEY(job_id) REFERENCES jobs(id) ON DELETE CASCADE)`).Error
}
