package database

import (
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestStructureRepairCheckpointsV92MigrateAndCascade(t *testing.T) {
	db := structureMigrationDB(t, 91)
	library := seedStructureMigrationLibrary(t, db, 91, "repair-v92", "issues", 1, 1)
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{"succeeded_items", "failed_items", "blocked_items"} {
		if !db.Migrator().HasColumn(&models.MediaLibraryStructureRepair{}, column) {
			t.Fatalf("missing repair column %s", column)
		}
	}
	if !db.Migrator().HasTable(&models.MediaLibraryStructureRepairItem{}) {
		t.Fatal("missing repair item table")
	}
	now := time.Now().UTC()
	user := models.User{Username: "repair-v92", UsernameNormalized: "repair-v92", DisplayName: "Repair", PasswordHash: "unused", Status: models.UserStatusActive, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	repair := models.MediaLibraryStructureRepair{ID: "repair-v92", OwnerID: user.ID, LibraryID: library.ID, Scope: "full", RuleFingerprint: "rule", Generation: 1, PlanJSON: "{}", StateJSON: "{}", Phase: "failed", TotalItems: 1, FailedItems: 1, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&repair).Error; err != nil {
		t.Fatal(err)
	}
	item := models.MediaLibraryStructureRepairItem{RepairID: repair.ID, Ordinal: 0, Action: "move", Kind: "video", SourceRelative: "old/video.mkv", TargetRelative: "new/video.mkv", Status: "failed", ErrorCode: "file_locked", ErrorMessage: "文件正在被占用", CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&item).Error; err != nil {
		t.Fatal(err)
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	if err := db.Delete(&repair).Error; err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := db.Model(&models.MediaLibraryStructureRepairItem{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("cascade count=%d err=%v", count, err)
	}
}
