package services

import (
	"context"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestCancelledUnenteredRepairAllowsNewDiagnosisAndSubmission(t *testing.T) {
	for _, mode := range []string{"full", "work", "selection"} {
		t.Run(mode, func(t *testing.T) {
			s, actor, library, diagnostics := prepareLegacyStructureRepair(t)
			actor.Permissions[authz.PermissionJobsControlAll] = struct{}{}
			ctx := context.Background()
			enqueue := func(d MediaLibraryStructureDiagnostics) models.MediaLibraryStructureRepair {
				t.Helper()
				if mode != "selection" {
					return enqueueLegacyStructureRepair(t, s, actor, library, d, mode)
				}
				page, err := s.StructureIssues(ctx, actor, library.ID, MediaLibraryStructureIssueQuery{Actionable: true})
				if err != nil {
					t.Fatal(err)
				}
				var token string
				for _, issue := range page.List {
					if issue.RecommendedMemberToken != "" {
						token = issue.Token
						break
					}
				}
				if token == "" {
					t.Fatal("fixture has no selectable physical conflict")
				}
				preview, err := s.PreviewSelectionRepair(ctx, actor, library.ID, MediaLibraryStructureSelectionInput{Revision: d.Revision, Selections: []MediaLibraryStructureSelection{{IssueToken: token, Action: StructureSelectionKeepRecommended}}})
				if err != nil {
					t.Fatal(err)
				}
				repair, err := s.EnqueueSelectionRepair(ctx, actor, library.ID, preview.ConfirmationToken, RequestContext{})
				if err != nil {
					t.Fatal(err)
				}
				return repair
			}
			old := enqueue(diagnostics)
			if old.JobID == nil {
				t.Fatal("original submission has no Job")
			}
			if _, err := s.queue.Control(actor, *old.JobID, "cancel", RequestContext{}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Diagnose(ctx, library.ID, ""); err != nil {
				t.Fatal(err)
			}
			diagnosis, err := s.queue.Claim([]string{JobTypeMediaLibraryStructureDiagnosis})
			if err != nil || diagnosis == nil {
				t.Fatalf("new diagnosis blocked: %+v %v", diagnosis, err)
			}
			if result := NewMediaLibraryStructureDiagnosisWorker(s).Run(ctx, fastScanTestRuntime{}, *diagnosis); result.ErrorCode != "" {
				t.Fatalf("new diagnosis failed: %+v", result)
			}
			if err := s.queue.Complete(diagnosis.Job.ID, diagnosis.LeaseToken); err != nil {
				t.Fatal(err)
			}
			diagnostics, err = s.Diagnostics(ctx, actor, library.ID)
			if err != nil {
				t.Fatal(err)
			}
			next := enqueue(diagnostics)
			if next.ID == old.ID || next.JobID == nil || *next.JobID == *old.JobID {
				t.Fatalf("submission reused cancelled repair: old=%+v new=%+v", old, next)
			}
			var oldJob models.Job
			if err := s.db.First(&oldJob, "id=?", *old.JobID).Error; err != nil || oldJob.Status != models.JobStatusCancelled || oldJob.AttemptCount != 0 {
				t.Fatalf("cancelled history changed: %+v %v", oldJob, err)
			}
			for _, repair := range []models.MediaLibraryStructureRepair{old, next} {
				var proof models.CatalogPhysicalWrite
				if err := s.db.Where("owner_kind=? AND owner_id=?", CatalogPhysicalRepair, repair.ID).First(&proof).Error; err != nil || proof.State != "admitted" || proof.JobID != *repair.JobID {
					t.Fatalf("admission erased or reassigned: %+v %v", proof, err)
				}
			}
			claim, err := s.queue.Claim([]string{JobTypeMediaLibraryRepair})
			if err != nil || claim == nil || claim.Job.ID != *next.JobID {
				t.Fatalf("new repair not executable: %+v %v", claim, err)
			}
			// Do not execute either physical plan: this tests admission and Job identity.
		})
	}
}
