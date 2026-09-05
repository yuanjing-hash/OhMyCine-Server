package services

import (
	"context"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

func TestStructureDiagnosisCommitFencesConcurrentCatalogChanges(t *testing.T) {
	for _, change := range []string{"generation", "rules", "source_revision"} {
		t.Run(change, func(t *testing.T) {
			s, _, library := structureConfirmationFixture(t)
			now := time.Now().UTC()
			state := models.MediaLibraryStructureAutoState{LibraryID: library.ID, SourceRevision: 1, DiagnosedRevision: 1, Status: "projection_pending", UpdatedAt: now}
			if err := s.db.Create(&state).Error; err != nil {
				t.Fatal(err)
			}
			oldIssue := models.MediaLibraryStructureIssue{Token: "preserve-current-issue", LibraryID: library.ID, DiagnosisJobID: "old", Generation: 1, Code: "invalid_path", Kind: "file", CreatedAt: now, UpdatedAt: now}
			if err := s.db.Create(&oldIssue).Error; err != nil {
				t.Fatal(err)
			}
			if err := s.RecoverLegacyProjections(context.Background()); err != nil {
				t.Fatal(err)
			}
			job, err := s.queue.Claim([]string{JobTypeMediaLibraryStructureDiagnosis})
			if err != nil || job == nil {
				t.Fatalf("claim: %v", err)
			}
			queries, changed := 0, false
			callback := "test:change-after-diagnosis-precheck"
			if err := s.db.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
				if tx.Statement.Table != "media_libraries" || changed {
					return
				}
				queries++
				// The second library read is the final pre-transaction check.
				// Its result is already materialized, exactly like another writer
				// publishing immediately after the read returned.
				if queries != 2 {
					return
				}
				changed = true
				updates := map[string]any{"structure_status": "issues", "structure_issue_count": 1}
				if change == "generation" {
					updates["baseline_generation"] = 2
				}
				if change == "rules" {
					updates["movie_filename_template"] = "{title}-changed"
				}
				if err := s.db.Model(&models.MediaLibrary{}).Where("id = ?", library.ID).Updates(updates).Error; err != nil {
					tx.AddError(err)
					return
				}
				if change == "source_revision" {
					if err := s.db.Model(&models.MediaLibraryStructureAutoState{}).Where("library_id = ?", library.ID).Update("source_revision", 2).Error; err != nil {
						tx.AddError(err)
					}
				}
			}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.db.Callback().Query().Remove(callback) })
			if result := NewMediaLibraryStructureDiagnosisWorker(s).Run(context.Background(), fastScanTestRuntime{}, *job); result.ErrorCode != "" {
				t.Fatalf("worker: %+v", result)
			}
			if !changed {
				t.Fatal("race hook did not execute")
			}
			var current models.MediaLibrary
			if err := s.db.First(&current, library.ID).Error; err != nil {
				t.Fatal(err)
			}
			if current.StructureStatus != "issues" || current.StructureIssueCount != 1 {
				t.Fatalf("new summary overwritten: %+v", current)
			}
			if err := s.db.First(&oldIssue, oldIssue.ID).Error; err != nil {
				t.Fatalf("current issue replaced: %v", err)
			}
			var currentState models.MediaLibraryStructureAutoState
			if err := s.db.First(&currentState, "library_id = ?", library.ID).Error; err != nil {
				t.Fatal(err)
			}
			if currentState.Status != "projection_pending" || currentState.DiagnosedRevision != 1 {
				t.Fatalf("stale diagnosis consumed recovery: %+v", currentState)
			}
			var diagnosis models.MediaLibraryStructureDiagnosis
			if err := s.db.First(&diagnosis, "library_id = ?", library.ID).Error; err != nil {
				t.Fatal(err)
			}
			if diagnosis.Status == "healthy" || diagnosis.Status == "issues" || diagnosis.FinishedAt != nil {
				t.Fatalf("stale diagnosis published: %+v", diagnosis)
			}
			if err := s.queue.Complete(job.Job.ID, job.LeaseToken); err != nil {
				t.Fatal(err)
			}
			if err := s.RecoverLegacyProjections(context.Background()); err != nil {
				t.Fatalf("stale worker recovery: %v", err)
			}
			next, err := s.queue.Claim([]string{JobTypeMediaLibraryStructureDiagnosis})
			if err != nil || next == nil {
				t.Fatalf("completed stale worker blocked recovery: %v", err)
			}
		})
	}
}

func TestStructureUpgradeRecoveryDefersStartupRaces(t *testing.T) {
	for _, change := range []string{"catch_up", "delete_after_load", "delete_before_load"} {
		t.Run(change, func(t *testing.T) {
			s, _, library := structureConfirmationFixture(t)
			state := models.MediaLibraryStructureAutoState{LibraryID: library.ID, SourceRevision: 1, DiagnosedRevision: 1, Status: "projection_pending", UpdatedAt: time.Now().UTC()}
			if err := s.db.Create(&state).Error; err != nil {
				t.Fatal(err)
			}
			changed := false
			callback := "test:recovery-startup-race"
			if err := s.db.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
				if changed {
					return
				}
				wantedTable := "media_libraries"
				if change == "delete_before_load" {
					wantedTable = "media_library_structure_auto_states"
				}
				if tx.Statement.Table != wantedTable {
					return
				}
				changed = true
				var err error
				if change == "catch_up" {
					err = s.db.Model(&models.MediaLibrary{}).Where("id = ?", library.ID).Update("baseline_generation", 2).Error
				} else {
					err = s.db.Delete(&models.MediaLibrary{}, library.ID).Error
				}
				if err != nil {
					tx.AddError(err)
				}
			}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.db.Callback().Query().Remove(callback) })
			if err := s.RecoverLegacyProjections(context.Background()); err != nil {
				t.Fatalf("normal startup race failed: %v", err)
			}
			if !changed {
				t.Fatal("race hook did not execute")
			}
			var count int64
			if err := s.db.Model(&models.Job{}).Where("job_type = ?", JobTypeMediaLibraryStructureDiagnosis).Count(&count).Error; err != nil || count != 0 {
				t.Fatalf("stale job count=%d error=%v", count, err)
			}
			if change == "catch_up" {
				var currentState models.MediaLibraryStructureAutoState
				if err := s.db.First(&currentState, "library_id = ?", library.ID).Error; err != nil {
					t.Fatal(err)
				}
				if currentState.Status != "projection_pending" {
					t.Fatalf("lost marker: %+v", currentState)
				}
				if err := s.RecoverLegacyProjections(context.Background()); err != nil {
					t.Fatalf("retry: %v", err)
				}
				if err := s.db.Model(&models.Job{}).Where("job_type = ?", JobTypeMediaLibraryStructureDiagnosis).Count(&count).Error; err != nil || count != 1 {
					t.Fatalf("retry job count=%d error=%v", count, err)
				}
			}
		})
	}
}
