package database

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

func TestMigrationV72RecoversLegacyIssueProjection(t *testing.T) {
	for _, version := range []int{70, 71} {
		t.Run(fmt.Sprintf("from_v%d", version), func(t *testing.T) {
			db := structureMigrationDB(t, version)
			broken := seedStructureMigrationLibrary(t, db, version, "legacy", "issues", 3, 3)
			healthy := seedStructureMigrationLibrary(t, db, version, "healthy", "healthy", 0, 0)
			if version == 71 {
				state := models.MediaLibraryStructureAutoState{LibraryID: broken.ID, SourceRevision: 4, DiagnosedRevision: 4, Status: "completed", UpdatedAt: time.Now().UTC()}
				if err := db.Create(&state).Error; err != nil {
					t.Fatal(err)
				}
			}
			// Manual identity is user authority, not part of the replaced projection.
			if err := db.Exec(`INSERT INTO media_library_recognitions (library_id, source_key, input_fingerprint, last_generation, profile_id, profile_revision, status, media_type, title, tmdb_id, manual_override, created_at, updated_at) VALUES (?, 'manual-source', 'fingerprint', 2, ?, 1, 'matched', 'tv', 'User choice', 123, 1, ?, ?)`, broken.ID, broken.ProfileID, time.Now().UTC(), time.Now().UTC()).Error; err != nil {
				t.Fatal(err)
			}
			if err := Migrate(db); err != nil {
				t.Fatal(err)
			}
			var state models.MediaLibraryStructureAutoState
			if err := db.First(&state, "library_id = ?", broken.ID).Error; err != nil {
				t.Fatal(err)
			}
			wantRevision := uint64(1)
			if version == 71 {
				wantRevision = 4
			}
			if state.Status != "projection_pending" || state.SourceRevision != wantRevision || state.DiagnosedRevision != wantRevision {
				t.Fatalf("recovery state = %+v", state)
			}
			var migrated models.MediaLibrary
			if err := db.First(&migrated, broken.ID).Error; err != nil {
				t.Fatal(err)
			}
			if migrated.StructureStatus != "pending" || migrated.StructureIssueCount != 0 || migrated.StructureCheckedAt != nil {
				t.Fatalf("obsolete summary remains: %+v", migrated)
			}
			var count int64
			if err := db.Model(&models.MediaLibraryStructureDiagnosis{}).Where("library_id = ?", broken.ID).Count(&count).Error; err != nil || count != 0 {
				t.Fatalf("obsolete diagnosis count=%d err=%v", count, err)
			}
			if err := db.Model(&models.MediaLibraryRecognition{}).Where("library_id = ? AND manual_override = 1 AND tmdb_id = 123", broken.ID).Count(&count).Error; err != nil || count != 1 {
				t.Fatalf("manual identity count=%d err=%v", count, err)
			}
			if err := db.First(&healthy, healthy.ID).Error; err != nil {
				t.Fatal(err)
			}
			if healthy.StructureStatus != "healthy" || healthy.StructureCheckedAt == nil {
				t.Fatalf("healthy library invalidated: %+v", healthy)
			}
			// A restart after a completed compatibility rebuild must not re-arm it.
			if err := db.Model(&state).Update("status", "completed").Error; err != nil {
				t.Fatal(err)
			}
			if err := Migrate(db); err != nil {
				t.Fatal(err)
			}
			if err := db.First(&state, "library_id = ?", broken.ID).Error; err != nil {
				t.Fatal(err)
			}
			if state.Status != "completed" {
				t.Fatalf("repeat migration re-armed recovery: %+v", state)
			}
			if err := db.Table("schema_migrations").Where("version = 72").Count(&count).Error; err != nil || count != 1 {
				t.Fatalf("v72 count=%d err=%v", count, err)
			}
		})
	}
}

func TestMigrationV72PreservesCurrentIssuesAndDoesNotResurrectRepairedIssues(t *testing.T) {
	db := structureMigrationDB(t, 71)
	valid := seedStructureMigrationLibrary(t, db, 71, "valid", "issues", 1, 1)
	repaired := seedStructureMigrationLibrary(t, db, 71, "repaired", "healthy", 0, 3)
	now := time.Now().UTC()
	for _, library := range []models.MediaLibrary{valid, repaired} {
		if err := db.Create(&models.MediaLibraryStructureAutoState{LibraryID: library.ID, SourceRevision: 8, DiagnosedRevision: 8, Status: "completed", UpdatedAt: now}).Error; err != nil {
			t.Fatal(err)
		}
	}
	issue := models.MediaLibraryStructureIssue{Token: "kept-issue", LibraryID: valid.ID, DiagnosisJobID: "valid-job", Generation: 2, Code: "duplicate_target", Kind: "conflict", State: "pending", CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&issue).Error; err != nil {
		t.Fatal(err)
	}
	member := models.MediaLibraryStructureIssueMember{IssueID: issue.ID, Token: "kept-member", SourcePath: "Show/01.mkv", CreatedAt: now}
	if err := db.Create(&member).Error; err != nil {
		t.Fatal(err)
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	for _, library := range []models.MediaLibrary{valid, repaired} {
		var state models.MediaLibraryStructureAutoState
		if err := db.First(&state, "library_id = ?", library.ID).Error; err != nil {
			t.Fatal(err)
		}
		if state.Status != "completed" || state.SourceRevision != 8 || state.DiagnosedRevision != 8 {
			t.Fatalf("valid state modified: %+v", state)
		}
	}
	var diagnosis models.MediaLibraryStructureDiagnosis
	if err := db.First(&diagnosis, "library_id = ?", repaired.ID).Error; err != nil {
		t.Fatal(err)
	}
	if diagnosis.Status != "healthy" || diagnosis.IssueCount != 0 || diagnosis.RepairableCount != 0 || diagnosis.IssuesJSON != "[]" || diagnosis.DuplicateTargetCount != 0 {
		t.Fatalf("repaired summary still stale: %+v", diagnosis)
	}
	if err := db.First(&issue, issue.ID).Error; err != nil {
		t.Fatalf("valid issue lost: %v", err)
	}
	if err := db.First(&member, member.ID).Error; err != nil {
		t.Fatalf("valid member lost: %v", err)
	}
	var validDiagnosis models.MediaLibraryStructureDiagnosis
	if err := db.First(&validDiagnosis, "library_id = ?", valid.ID).Error; err != nil {
		t.Fatal(err)
	}
	if validDiagnosis.IssueCount != 1 || validDiagnosis.Status != "issues" {
		t.Fatalf("valid summary modified: %+v", validDiagnosis)
	}
}

func structureMigrationDB(t *testing.T, version int) *gorm.DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "upgrade.db"))
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	applyMigrationsThrough(t, db, version)
	if err := seedMediaClassificationProfiles(db); err != nil {
		t.Fatal(err)
	}
	return db
}

func seedStructureMigrationLibrary(t *testing.T, db *gorm.DB, version int, name, status string, issueCount, diagnosisCount int) models.MediaLibrary {
	t.Helper()
	now := time.Now().UTC()
	storage := models.Storage{Name: name, NameNormalized: name, Type: models.StorageTypeLocal, RootPath: "/media/" + name, RootPathNormalized: "/media/" + name, Enabled: true, Capabilities: "{}", CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&storage).Error; err != nil {
		t.Fatal(err)
	}
	var profile models.MediaClassificationProfile
	if err := db.Where("code = ?", "default-v1").First(&profile).Error; err != nil {
		t.Fatal(err)
	}
	library := models.MediaLibrary{Name: name, NameNormalized: name, StorageID: storage.ID, ProfileID: profile.ID, ProfileRevision: profile.Revision, RelativeRoot: "/", Status: models.MediaLibraryStatusListening, StructureStatus: status, StructureIssueCount: issueCount, StructureCheckedAt: &now, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&library).Error; err != nil {
		t.Fatal(err)
	}
	job := models.Job{ID: name + "-job", CreatedByKind: "system", JobType: "media_library_structure_diagnosis", Status: models.JobStatusCompleted, Revision: 1, Generation: 2, PayloadJSON: "{}", CheckpointJSON: "{}", DisplayName: name, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&job).Error; err != nil {
		t.Fatal(err)
	}
	diagnosisStatus := "healthy"
	if diagnosisCount > 0 {
		diagnosisStatus = "issues"
	}
	diagnosis := models.MediaLibraryStructureDiagnosis{LibraryID: library.ID, JobID: job.ID, Generation: 2, Status: diagnosisStatus, SourceRevision: 1, IssueCount: diagnosisCount, RepairableCount: diagnosisCount, DuplicateTargetCount: diagnosisCount, IssuesJSON: `[{"code":"duplicate_target"}]`, CreatedAt: now, UpdatedAt: now}
	query := db
	if version < 71 {
		query = query.Omit("Automatic", "SourceRevision")
	}
	if err := query.Create(&diagnosis).Error; err != nil {
		t.Fatal(err)
	}
	return library
}
