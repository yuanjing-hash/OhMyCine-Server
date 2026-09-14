package database

import (
	"fmt"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestStructureConflictCountersV95UpgradeScopeAndRepeat(t *testing.T) {
	db := structureMigrationDB(t, 94)
	library := seedStructureMigrationLibrary(t, db, 94, "conflicts", "issues", 9, 9)
	other := seedStructureMigrationLibrary(t, db, 94, "stale-only", "issues", 8, 8)
	now := time.Now().UTC()
	user := models.User{Username: "review-v95", UsernameNormalized: "review-v95", DisplayName: "Review", PasswordHash: "unused", Status: models.UserStatusActive, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	session := models.MediaLibraryStructureReviewSession{ID: "review-v95", OwnerID: user.ID, LibraryID: library.ID, DiagnosisJobID: "conflicts-job", Revision: 7, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&session).Error; err != nil {
		t.Fatal(err)
	}
	choice := models.MediaLibraryStructureReviewChoice{SessionID: session.ID, SubjectKey: "issue:current-1", SubjectKind: "issue", IssueToken: "current-1", Action: "skip", State: "draft", CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&choice).Error; err != nil {
		t.Fatal(err)
	}
	for index, code := range []string{"duplicate_target", "recognition_suspect_conflict", "recognition_suspect_conflict", "catalog_duplicate_conflict", "catalog_duplicate_conflict", "catalog_duplicate_conflict", "invalid_path"} {
		row := models.MediaLibraryStructureIssue{LibraryID: library.ID, DiagnosisJobID: "conflicts-job", Generation: 2, Token: fmt.Sprintf("current-%d", index), Code: code, Kind: "conflict", State: "pending", CreatedAt: now, UpdatedAt: now}
		if err := db.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
	}
	for index, row := range []models.MediaLibraryStructureIssue{
		{LibraryID: library.ID, DiagnosisJobID: "old-job", Generation: 2},
		{LibraryID: library.ID, DiagnosisJobID: "conflicts-job", Generation: 1},
		{LibraryID: other.ID, DiagnosisJobID: "conflicts-job", Generation: 2},
	} {
		row.Token, row.Code, row.Kind, row.State = fmt.Sprintf("stale-%d", index), "duplicate_target", "conflict", "pending"
		row.CreatedAt, row.UpdatedAt = now, now
		if err := db.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	var current, stale models.MediaLibraryStructureDiagnosis
	if err := db.First(&current, "library_id = ?", library.ID).Error; err != nil {
		t.Fatal(err)
	}
	if current.DuplicateTargetCount != 1 || current.RecognitionSuspectConflictCount != 2 || current.CatalogDuplicateConflictCount != 3 {
		t.Fatalf("wrong exact counters: %+v", current)
	}
	if current.IssueCount != 9 || current.RepairableCount != 9 || current.Status != "issues" || current.JobID != "conflicts-job" || current.Generation != 2 || current.IssuesJSON != `[{"code":"duplicate_target"}]` {
		t.Fatalf("unrelated diagnosis data changed: %+v", current)
	}
	if err := db.First(&stale, "library_id = ?", other.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stale.DuplicateTargetCount != 0 || stale.RecognitionSuspectConflictCount != 0 || stale.CatalogDuplicateConflictCount != 0 || stale.IssueCount != 8 {
		t.Fatalf("stale issues counted: %+v", stale)
	}
	var count int64
	if err := db.Model(&models.MediaLibraryStructureIssue{}).Count(&count).Error; err != nil || count != 10 {
		t.Fatalf("issues altered: %d %v", count, err)
	}
	// A normal restart skips the ledgered migration; it must not overwrite later runtime publication.
	if err := db.Model(&current).Update("duplicate_target_count", 17).Error; err != nil {
		t.Fatal(err)
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&current, "library_id = ?", library.ID).Error; err != nil || current.DuplicateTargetCount != 17 {
		t.Fatalf("migration replayed: %+v %v", current, err)
	}
	if err := db.Table("schema_migrations").Where("version = 95").Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("ledger: %d %v", count, err)
	}
	var keptSession models.MediaLibraryStructureReviewSession
	var keptChoice models.MediaLibraryStructureReviewChoice
	if err := db.First(&keptSession, "id = ?", session.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&keptChoice, choice.ID).Error; err != nil {
		t.Fatal(err)
	}
	if keptSession.OwnerID != user.ID || keptSession.LibraryID != library.ID || keptSession.DiagnosisJobID != session.DiagnosisJobID || keptSession.Revision != 7 || !keptSession.UpdatedAt.Equal(now) {
		t.Fatalf("review session changed: %+v", keptSession)
	}
	if keptChoice.SessionID != session.ID || keptChoice.SubjectKey != choice.SubjectKey || keptChoice.SubjectKind != "issue" || keptChoice.IssueToken != "current-1" || keptChoice.Action != "skip" || keptChoice.State != "draft" || !keptChoice.UpdatedAt.Equal(now) {
		t.Fatalf("user choice changed: %+v", keptChoice)
	}
}
