package services

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

// Production-shaped persisted findings/skip choices, not a UI counter fixture.
// All mutations target an isolated test DB and use bounded batch inserts.
func TestStructureReviewRegressionAllSkippedLargeSnapshot(t *testing.T) {
	s, actor, library := structureConfirmationFixture(t)
	now := time.Now().UTC()
	job := enqueueFake(t, s.queue, actor, "review-summary", "review-summary")
	diagnosis := models.MediaLibraryStructureDiagnosis{LibraryID: library.ID, JobID: job.ID, Generation: library.BaselineGeneration, SourceRevision: 1, Status: models.MediaLibraryStructureIssues, CreatedAt: now, UpdatedAt: now}
	if err := s.db.Create(&diagnosis).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.db.Create(&models.MediaLibraryStructureAutoState{LibraryID: library.ID, SourceRevision: 1, Status: "completed", UpdatedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	const naming, locations = 16000, 457
	issues := make([]StructureIssue, 0, naming+locations)
	for index := 0; index < naming+locations; index++ {
		code := "naming_mismatch"
		if index >= naming {
			code = "location_mismatch"
		}
		issues = append(issues, StructureIssue{Code: code, Kind: "video", CurrentPath: fmt.Sprintf("source/%d.mkv", index), ExpectedPath: fmt.Sprintf("target/%d.mkv", index), Repairable: true})
	}
	if err := s.db.Transaction(func(tx *gorm.DB) error {
		return persistStructureIssuesTx(tx, library.ID, job.ID, library.BaselineGeneration, StructurePlan{AllIssues: issues}, now)
	}); err != nil {
		t.Fatal(err)
	}
	diagnostics, err := s.Diagnostics(context.Background(), actor, library.ID)
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.SaveStructureReviewBulk(context.Background(), actor, library.ID, MediaLibraryStructureReviewBulkInput{DiagnosisRevision: diagnostics.Revision, Codes: []string{"naming_mismatch"}, Action: StructureSelectionSkip}, RequestContext{})
	if err != nil || first.Updated != naming {
		t.Fatalf("skip names=%+v %v", first, err)
	}
	for _, filter := range []string{"all", "naming_mismatch", "location_mismatch"} {
		started := time.Now()
		page, err := s.StructureIssues(context.Background(), actor, library.ID, MediaLibraryStructureIssueQuery{Page: 1, PageSize: 1, Actionable: true, Code: filter, ReviewState: "handled"})
		t.Logf("filtered summary %s: %s", filter, time.Since(started))
		if err != nil {
			t.Fatal(err)
		}
		if page.PendingTotal != locations || page.HandledTotal != naming {
			t.Fatalf("filter %s polluted global counts: pending=%d handled=%d", filter, page.PendingTotal, page.HandledTotal)
		}
		if page.PendingRepairableCount != locations || page.PendingClassifications.LocationMismatch != locations || page.HandledClassifications.NamingMismatch != naming || page.DiagnosisRevision != diagnostics.Revision {
			t.Fatalf("filtered summary lost classification/revision: %+v", page)
		}
	}
	second, err := s.SaveStructureReviewBulk(context.Background(), actor, library.ID, MediaLibraryStructureReviewBulkInput{DiagnosisRevision: diagnostics.Revision, ReviewRevision: first.ReviewRevision, Codes: []string{"location_mismatch"}, Action: StructureSelectionSkip}, RequestContext{})
	if err != nil || second.Updated != locations {
		t.Fatalf("skip locations=%+v %v", second, err)
	}
	var jobsBefore int64
	var choicesBefore, choicesAfter []models.MediaLibraryStructureReviewChoice
	if err := s.db.Order("id").Find(&choicesBefore).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.db.Model(&models.Job{}).Count(&jobsBefore).Error; err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"pending", "handled", ""} {
		counter := &traceCountingGORMLogger{}
		s.db = s.db.Session(&gorm.Session{Logger: counter})
		started := time.Now()
		page, err := s.StructureIssues(context.Background(), actor, library.ID, MediaLibraryStructureIssueQuery{Page: 2, PageSize: 200, Actionable: true, Code: "all", ReviewState: state})
		t.Logf("all-skipped state=%s: %s / %d SQL statements", state, time.Since(started), counter.count.Load())
		if counter.count.Load() > 30 {
			t.Fatalf("unbounded issue query count: %d", counter.count.Load())
		}
		if err != nil {
			t.Fatal(err)
		}
		if page.PendingTotal != 0 || page.HandledTotal != naming+locations || page.ReviewRevision != second.ReviewRevision {
			t.Fatalf("all skipped state=%s pending=%d handled=%d rev=%d", state, page.PendingTotal, page.HandledTotal, page.ReviewRevision)
		}
		if page.PendingRepairableCount != 0 || page.PendingClassifications != (StructureIssueClassifications{}) || page.HandledClassifications.NamingMismatch != naming || page.HandledClassifications.LocationMismatch != locations {
			t.Fatalf("all-skipped classification mismatch: %+v", page)
		}
		if state == "pending" {
			if page.Total != 0 || len(page.List) != 0 {
				t.Fatal("skips reappeared pending")
			}
		} else {
			if page.Total != naming+locations || len(page.List) != 200 {
				t.Fatalf("handled pagination total=%d len=%d", page.Total, len(page.List))
			}
			for _, row := range page.List {
				if row.ReviewAction != StructureSelectionSkip || row.ReviewState != "draft" {
					t.Fatalf("persisted choice absent: %+v", row)
				}
			}
		}
	}
	if err := s.db.Order("id").Find(&choicesAfter).Error; err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(choicesBefore, choicesAfter) {
		t.Fatal("reading review pages changed persisted user decisions")
	}
	other := actor
	other.User.ID++
	page, err := s.StructureIssues(context.Background(), other, library.ID, MediaLibraryStructureIssueQuery{Page: 1, PageSize: 1, Actionable: true, ReviewState: "pending"})
	if err != nil || page.PendingTotal != naming+locations || page.HandledTotal != 0 {
		t.Fatalf("another actor inherited skips: %+v %v", page, err)
	}
	handled, err := s.StructureIssues(context.Background(), actor, library.ID, MediaLibraryStructureIssueQuery{Page: 1, PageSize: 1, Actionable: true, ReviewState: "handled"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteStructureReviewChoice(context.Background(), actor, library.ID, handled.List[0].Token, MediaLibraryStructureReviewChoiceInput{DiagnosisRevision: diagnostics.Revision, ReviewRevision: second.ReviewRevision}, RequestContext{}); err != nil {
		t.Fatal(err)
	}
	page, err = s.StructureIssues(context.Background(), actor, library.ID, MediaLibraryStructureIssueQuery{Page: 1, PageSize: 50, Actionable: true, ReviewState: "pending"})
	if err != nil || page.PendingTotal != 1 || page.HandledTotal != naming+locations-1 || len(page.List) != 1 {
		t.Fatalf("undo not reflected: %+v %v", page, err)
	}
	if _, err := s.SaveStructureReviewChoice(context.Background(), actor, library.ID, page.List[0].Token, MediaLibraryStructureReviewChoiceInput{DiagnosisRevision: diagnostics.Revision, ReviewRevision: page.ReviewRevision, Action: StructureSelectionSkip}, RequestContext{}); err != nil {
		t.Fatalf("re-skip after undo in large workspace failed: %v", err)
	}
	page, err = s.StructureIssues(context.Background(), actor, library.ID, MediaLibraryStructureIssueQuery{Page: 1, PageSize: 1, Actionable: true, ReviewState: "handled"})
	if err != nil || page.PendingTotal != 0 || page.HandledTotal != naming+locations {
		t.Fatalf("re-skip did not persist: %+v %v", page, err)
	}
	var jobsAfter int64
	if err := s.db.Model(&models.Job{}).Count(&jobsAfter).Error; err != nil || jobsAfter != jobsBefore {
		t.Fatalf("review enqueued business work: before=%d after=%d err=%v", jobsBefore, jobsAfter, err)
	}
}

func TestStructureReviewRegressionUnavailableSessionReadIsNotEmpty(t *testing.T) {
	s, actor, library, _ := prepareStructureSelectionConflicts(t, 1)
	sentinel := errors.New("isolated session read failure")
	key := "review-summary-read-failure"
	if err := s.db.Callback().Query().Before("gorm:query").Register(key, func(tx *gorm.DB) {
		if tx.Statement.Table == "media_library_structure_review_sessions" {
			_ = tx.AddError(sentinel)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.db.Callback().Query().Remove(key) })
	_, err := s.StructureIssues(context.Background(), actor, library.ID, MediaLibraryStructureIssueQuery{Page: 1, PageSize: 50, Actionable: true, ReviewState: "pending"})
	if !errors.Is(err, sentinel) {
		t.Fatalf("session error became healthy zero totals: %v", err)
	}
}

func TestStructureReviewRegressionIndexedChoiceProbe(t *testing.T) {
	s, _, _ := structureConfirmationFixture(t)
	old := "EXISTS (SELECT 1 FROM media_library_structure_review_choices rc WHERE rc.session_id = ? AND (rc.subject_key = ('issue:' || media_library_structure_issues.token) OR (media_library_structure_issues.recognition_id IS NOT NULL AND rc.subject_key = ('recognition:' || CAST(media_library_structure_issues.recognition_id AS TEXT)))))"
	exact := structureReviewChoiceExistsSQL
	for _, probe := range []struct {
		name, sql string
		args      []any
	}{{"old", old, []any{"session"}}, {"exact", exact, []any{"session", "session"}}} {
		var rows []struct{ Detail string }
		if err := s.db.Raw("EXPLAIN QUERY PLAN SELECT COUNT(*) FROM media_library_structure_issues WHERE "+probe.sql, probe.args...).Scan(&rows).Error; err != nil {
			t.Fatal(err)
		}
		indexed := 0
		for _, row := range rows {
			t.Log(probe.name, row.Detail)
			if strings.Contains(row.Detail, "subject_key=?") {
				indexed++
			}
		}
		if probe.name == "exact" && indexed != 2 {
			t.Fatalf("exact probes lost composite-key index: %+v", rows)
		}
	}
}
