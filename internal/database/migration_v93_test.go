package database

import (
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestStructureDraftPreviewRowsV93MigrateAndCascade(t *testing.T) {
	db := structureMigrationDB(t, 92)
	library := seedStructureMigrationLibrary(t, db, 92, "preview-v93", "issues", 1, 1)
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	if !db.Migrator().HasTable(&models.MediaLibraryStructureRepairDraftPreviewItem{}) {
		t.Fatal("missing relational draft preview table")
	}
	now := time.Now().UTC()
	user := models.User{Username: "preview-v93", UsernameNormalized: "preview-v93", DisplayName: "Preview", PasswordHash: "unused", Status: models.UserStatusActive, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	draft := models.MediaLibraryStructureRepairDraft{ID: "preview-v93", OwnerID: user.ID, LibraryID: library.ID, DiagnosisJobID: "diagnosis-v93", SourceRevision: 1, Generation: 1, RuleFingerprint: "rule", PlanHash: "hash", SelectionsJSON: `{}`, PreviewItemsJSON: "sql:v1", ExpiresAt: now.Add(time.Hour), CreatedAt: now}
	if err := db.Create(&draft).Error; err != nil {
		t.Fatal(err)
	}
	row := models.MediaLibraryStructureRepairDraftPreviewItem{DraftID: draft.ID, Ordinal: 0, Action: "move", Kind: "video", CurrentPath: "old/video.mkv", ExpectedPath: "new/video.mkv", CreatedAt: now}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	if err := db.Delete(&draft).Error; err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := db.Model(&models.MediaLibraryStructureRepairDraftPreviewItem{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("cascade count=%d err=%v", count, err)
	}
}
