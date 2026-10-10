package database

import (
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestMigrationV115ResourceAccessDefaultAndCascade(t *testing.T) {
	db, err := Open(t.TempDir() + "/migration-v115.db")
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	applyMigrationsThrough(t, db, 114)
	user := models.User{Username: "existing", UsernameNormalized: "existing", DisplayName: "Existing", PasswordHash: "unused", Status: models.UserStatusActive, AuthzVersion: 9}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := Migrate(db); err != nil {
			t.Fatal(err)
		}
	}
	var count int64
	if err := db.Model(&models.UserResourceAccessPolicy{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("migration imposed resource lists: %d %v", count, err)
	}
	if err := db.First(&user, user.ID).Error; err != nil || user.AuthzVersion != 9 {
		t.Fatalf("legacy authority revision changed: %+v %v", user, err)
	}
	now := time.Now().UTC()
	row := models.UserResourceAccessPolicy{UserID: user.ID, Scope: models.ResourceAccessScopeSiteSearch, Mode: models.ResourceAccessModeAllowlist, ResourceIDsJSON: `[]`, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&row).Error; err == nil {
		t.Fatal("duplicate user/scope policy accepted")
	}
	if err := db.Delete(&user).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.UserResourceAccessPolicy{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("user deletion did not cascade: %d %v", count, err)
	}
}
