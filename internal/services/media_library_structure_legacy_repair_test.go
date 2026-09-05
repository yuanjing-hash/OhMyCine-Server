package services

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func prepareLegacyStructureRepair(t *testing.T) (*MediaLibraryStructureService, Actor, models.MediaLibrary, MediaLibraryStructureDiagnostics) {
	t.Helper()
	s, actor, library, _ := prepareStructureSelectionConflicts(t, 3)
	// Two ordinary moves, one unresolved real-file conflict, and one unknown
	// work make it observable if a completed move clears unrelated problems.
	if err := s.db.Where("library_id = ? AND provider_id IN ?", library.ID, []string{"copy-冲突影片A", "copy-冲突影片B"}).Delete(&models.MediaLibraryEntry{}).Error; err != nil {
		t.Fatal(err)
	}
	unknown := models.MediaLibraryEntry{LibraryID: library.ID, RelativePath: "/unknown.mkv", ProviderID: "unknown", Title: "未知作品", WorkKey: "unknown", MatchStatus: "unrecognized"}
	if err := s.db.Create(&unknown).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.EnqueueDiagnosis(context.Background(), library.ID, 0, library.BaselineGeneration, "manual"); err != nil {
		t.Fatal(err)
	}
	job, err := s.queue.Claim([]string{JobTypeMediaLibraryStructureDiagnosis})
	if err != nil || job == nil {
		t.Fatalf("diagnosis claim=%+v err=%v", job, err)
	}
	if result := NewMediaLibraryStructureDiagnosisWorker(s).Run(context.Background(), fastScanTestRuntime{}, *job); result.ErrorCode != "" {
		t.Fatalf("diagnosis=%+v", result)
	}
	if err := s.queue.Complete(job.Job.ID, job.LeaseToken); err != nil {
		t.Fatal(err)
	}
	diagnostics, err := s.Diagnostics(context.Background(), actor, library.ID)
	if err != nil || diagnostics.IssueCount != 4 || diagnostics.RepairableCount != 2 {
		t.Fatalf("diagnostics=%+v err=%v", diagnostics, err)
	}
	return s, actor, library, diagnostics
}

func enqueueLegacyStructureRepair(t *testing.T, s *MediaLibraryStructureService, actor Actor, library models.MediaLibrary, diagnostics MediaLibraryStructureDiagnostics, mode string) models.MediaLibraryStructureRepair {
	t.Helper()
	var repair models.MediaLibraryStructureRepair
	var err error
	if mode == "full" {
		var preview MediaLibraryStructurePreview
		preview, err = s.PreviewRepair(context.Background(), actor, library.ID, "", diagnostics.Revision)
		if err == nil {
			repair, err = s.EnqueueConfirmedRepair(context.Background(), actor, library.ID, "", preview.ConfirmationToken, RequestContext{})
		}
	} else {
		repair, err = s.EnqueueWorkRepair(context.Background(), actor, library.ID, encodeCatalogToken("movie:tmdb:9000"), RequestContext{})
	}
	if err != nil {
		t.Fatal(err)
	}
	return repair
}

func TestLegacyStructureRepairClearsOnlyCompletedPathIssues(t *testing.T) {
	for _, mode := range []string{"full", "work", "ensure_work_layout"} {
		t.Run(mode, func(t *testing.T) {
			s, actor, library, diagnostics := prepareLegacyStructureRepair(t)
			if mode == "ensure_work_layout" {
				if err := s.EnsureWorkLayout(context.Background(), actor.User.ID, library.ID, 9000, "movie"); err != nil {
					t.Fatal(err)
				}
			} else {
				repair := enqueueLegacyStructureRepair(t, s, actor, library, diagnostics, mode)
				if result := s.runRepair(context.Background(), fastScanTestRuntime{}, repair.ID); result.ErrorCode != "" {
					t.Fatalf("repair=%+v", result)
				}
			}
			want := 3
			if mode == "full" {
				want = 2
			}
			page, err := s.StructureIssues(context.Background(), actor, library.ID, MediaLibraryStructureIssueQuery{Actionable: true})
			if err != nil || page.Total != int64(want) {
				t.Fatalf("remaining=%+v err=%v", page, err)
			}
			diagnostics, err = s.Diagnostics(context.Background(), actor, library.ID)
			if err != nil || diagnostics.IssueCount != want || diagnostics.RepairableCount != want-2 || diagnostics.Classifications.DuplicateTarget != 1 || diagnostics.Unrecognized != 1 {
				t.Fatalf("summary=%+v err=%v", diagnostics, err)
			}
			for _, issue := range page.List {
				if issue.Code == "path_mismatch" && (mode == "full" || issue.CurrentPath == "incoming/冲突影片A.mkv") {
					t.Fatalf("completed path issue retained: %+v", issue)
				}
			}
			var storage models.Storage
			if err := s.db.First(&storage, library.StorageID).Error; err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(storage.RootPath, "电影", "冲突影片A (2024)", "冲突影片A (2024).mkv"))
			if err != nil || string(data) != "primary" {
				t.Fatalf("real file not organized: %q err=%v", data, err)
			}
			for _, filename := range []string{"冲突影片C.mkv", "冲突影片C (1).mkv"} {
				if _, err := os.Stat(filepath.Join(storage.RootPath, "incoming", filename)); err != nil {
					t.Fatalf("unhandled conflict file changed: %v", err)
				}
			}
		})
	}
}

func TestLegacyStructureRepairDoesNotReplaceActiveDiagnosis(t *testing.T) {
	for _, mode := range []string{"full", "work", "ensure_work_layout"} {
		for _, status := range []string{models.MediaLibraryStructureQueued, models.MediaLibraryStructureRunning} {
			t.Run(mode+"/"+status, func(t *testing.T) {
				s, actor, library, diagnostics := prepareLegacyStructureRepair(t)
				var repair models.MediaLibraryStructureRepair
				if mode != "ensure_work_layout" {
					repair = enqueueLegacyStructureRepair(t, s, actor, library, diagnostics, mode)
				}
				if err := s.db.Model(&models.MediaLibraryStructureDiagnosis{}).Where("library_id = ?", library.ID).Update("status", status).Error; err != nil {
					t.Fatal(err)
				}
				if err := s.db.Model(&models.MediaLibrary{}).Where("id = ?", library.ID).Update("structure_status", status).Error; err != nil {
					t.Fatal(err)
				}
				if mode == "ensure_work_layout" {
					if err := s.EnsureWorkLayout(context.Background(), actor.User.ID, library.ID, 9000, "movie"); err != nil {
						t.Fatal(err)
					}
				} else if result := s.runRepair(context.Background(), fastScanTestRuntime{}, repair.ID); result.ErrorCode != "" {
					t.Fatalf("repair=%+v", result)
				}
				var diagnosis models.MediaLibraryStructureDiagnosis
				if err := s.db.Where("library_id = ?", library.ID).First(&diagnosis).Error; err != nil || diagnosis.Status != status || diagnosis.IssueCount != 4 {
					t.Fatalf("active diagnosis changed: %+v err=%v", diagnosis, err)
				}
				if err := s.db.First(&library, library.ID).Error; err != nil || library.StructureStatus != status {
					t.Fatalf("active library diagnosis changed: %+v err=%v", library, err)
				}
				var remaining int64
				if err := s.db.Model(&models.MediaLibraryStructureIssue{}).Where("library_id = ?", library.ID).Count(&remaining).Error; err != nil || remaining != 4 {
					t.Fatalf("new diagnosis projection changed: count=%d err=%v", remaining, err)
				}
			})
		}
	}
}

func TestLegacyStructureEnsureWorkLayoutDoesNotInventDiagnosis(t *testing.T) {
	s, actor, library, _ := prepareLegacyStructureRepair(t)
	if err := s.db.Where("library_id = ?", library.ID).Delete(&models.MediaLibraryStructureDiagnosis{}).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.db.Model(&models.MediaLibrary{}).Where("id = ?", library.ID).Update("structure_status", models.MediaLibraryStructurePending).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureWorkLayout(context.Background(), actor.User.ID, library.ID, 9000, "movie"); err != nil {
		t.Fatal(err)
	}
	if err := s.db.First(&library, library.ID).Error; err != nil || library.StructureStatus != models.MediaLibraryStructurePending {
		t.Fatalf("layout falsely completed an absent diagnosis: %+v err=%v", library, err)
	}
}
