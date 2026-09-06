package database

import (
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestNotificationMigrationV74UpgradeRepeatAndCascade(t *testing.T) {
	db := structureMigrationDB(t, 73)
	for i := 0; i < 2; i++ {
		if err := Migrate(db); err != nil {
			t.Fatal(err)
		}
	}
	user := models.User{Username: "receipt", UsernameNormalized: "receipt", PasswordHash: "unused", Status: models.UserStatusActive}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	job := models.Job{ID: "migration-job", OwnerID: &user.ID, JobType: "fake", Status: "failed", Generation: 1, PayloadJSON: "{}"}
	if err := db.Create(&job).Error; err != nil {
		t.Fatal(err)
	}
	receipt := models.NotificationReceipt{UserID: user.ID, JobID: job.ID, Occurrence: 1, ReadAt: time.Now().UTC()}
	if err := db.Create(&receipt).Error; err != nil {
		t.Fatal(err)
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := db.Model(&models.NotificationReceipt{}).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("receipt=%d error=%v", count, err)
	}
	if err := db.Delete(&job).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.NotificationReceipt{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("orphan receipt=%d error=%v", count, err)
	}
}
