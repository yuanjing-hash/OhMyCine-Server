package database

import (
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestStructureReviewWorkspaceV91MigratesAndCascades(t *testing.T) {
	db := structureMigrationDB(t, 90)
	library := seedStructureMigrationLibrary(t, db, 90, "review-v91", "issues", 1, 1)
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	for _, model := range []any{&models.MediaLibraryStructureReviewSession{}, &models.MediaLibraryStructureReviewChoice{}} {
		if !db.Migrator().HasTable(model) {
			t.Fatalf("missing table for %T", model)
		}
	}
	for _, column := range []string{"review_session_id", "review_revision"} {
		if !db.Migrator().HasColumn(&models.MediaLibraryStructureRepairDraft{}, column) {
			t.Fatalf("missing repair draft column %s", column)
		}
	}
	now := time.Now().UTC()
	user := models.User{Username: "review-v91", UsernameNormalized: "review-v91", DisplayName: "Review", PasswordHash: "unused", Status: models.UserStatusActive, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	session := models.MediaLibraryStructureReviewSession{ID: "review-session", OwnerID: user.ID, LibraryID: library.ID, DiagnosisJobID: "review-v91-job", Revision: 2, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&session).Error; err != nil {
		t.Fatal(err)
	}
	choice := models.MediaLibraryStructureReviewChoice{SessionID: session.ID, SubjectKey: "issue:opaque", SubjectKind: "issue", IssueToken: "opaque", Action: "skip", State: "draft", CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&choice).Error; err != nil {
		t.Fatal(err)
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	if err := db.Delete(&library).Error; err != nil {
		t.Fatal(err)
	}
	var sessions, choices int64
	if err := db.Model(&models.MediaLibraryStructureReviewSession{}).Count(&sessions).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.MediaLibraryStructureReviewChoice{}).Count(&choices).Error; err != nil {
		t.Fatal(err)
	}
	if sessions != 0 || choices != 0 {
		t.Fatalf("cascade retained sessions=%d choices=%d", sessions, choices)
	}
}
