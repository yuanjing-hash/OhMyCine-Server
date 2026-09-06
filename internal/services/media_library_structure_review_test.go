package services

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

func TestStructureReviewGroupsRecognitionAndPersistsPerDiagnosis(t *testing.T) {
	s, actor, library := structureConfirmationFixture(t)
	now := time.Now().UTC()
	if err := s.db.Create(&models.MediaLibraryStructureAutoState{LibraryID: library.ID, SourceRevision: 1, Status: "completed", UpdatedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	job := enqueueFake(t, s.queue, actor, "review diagnosis", "review-diagnosis")
	diagnosis := models.MediaLibraryStructureDiagnosis{LibraryID: library.ID, JobID: job.ID, Generation: library.BaselineGeneration, ScanKind: "manual", SourceRevision: 1, Status: models.MediaLibraryStructureIssues, CreatedAt: now, UpdatedAt: now}
	if err := s.db.Create(&diagnosis).Error; err != nil {
		t.Fatal(err)
	}
	var profile models.MediaClassificationProfile
	if err := s.db.First(&profile, library.ProfileID).Error; err != nil {
		t.Fatal(err)
	}
	recognition := models.MediaLibraryRecognition{LibraryID: library.ID, SourceKey: "review-source", InputFingerprint: "fingerprint", ProfileID: profile.ID, ProfileRevision: profile.Revision, Status: mediaRecognitionStatusUnrecognized, ErrorCode: "tmdb_no_match", MetadataJSON: "{}", LastGeneration: library.BaselineGeneration, CreatedAt: now, UpdatedAt: now}
	if err := s.db.Create(&recognition).Error; err != nil {
		t.Fatal(err)
	}
	issues := make([]StructureIssue, 0, 36)
	for index := 1; index <= 36; index++ {
		issues = append(issues, StructureIssue{Code: "media_unrecognized", Kind: "video", Title: "同一部剧", CurrentPath: fmt.Sprintf("同一部剧/Season 01/%02d.mkv", index), RecognitionID: recognition.ID})
	}
	if err := s.db.Transaction(func(tx *gorm.DB) error {
		return persistStructureIssuesTx(tx, library.ID, job.ID, library.BaselineGeneration, StructurePlan{AllIssues: issues}, now)
	}); err != nil {
		t.Fatal(err)
	}
	pending, err := s.StructureIssues(context.Background(), actor, library.ID, MediaLibraryStructureIssueQuery{Page: 1, PageSize: 50, Actionable: true, ReviewState: "pending"})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Total != 1 || len(pending.List) != 1 || pending.List[0].AffectedFileCount != 36 || len(pending.List[0].Members) != 8 {
		t.Fatalf("grouped page=%+v", pending)
	}
	diagnostics, err := s.Diagnostics(context.Background(), actor, library.ID)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := s.SaveStructureReviewChoice(context.Background(), actor, library.ID, pending.List[0].Token, MediaLibraryStructureReviewChoiceInput{DiagnosisRevision: diagnostics.Revision, ReviewRevision: 0, Action: StructureSelectionSkip}, RequestContext{})
	if err != nil || saved.ReviewRevision != 1 {
		t.Fatalf("save=%+v err=%v", saved, err)
	}
	handled, err := s.StructureIssues(context.Background(), actor, library.ID, MediaLibraryStructureIssueQuery{Page: 1, PageSize: 50, Actionable: true, ReviewState: "handled"})
	if err != nil || handled.Total != 1 || handled.List[0].ReviewAction != StructureSelectionSkip {
		t.Fatalf("handled=%+v err=%v", handled, err)
	}
	otherActor := actor
	otherActor.User.ID++
	otherPending, err := s.StructureIssues(context.Background(), otherActor, library.ID, MediaLibraryStructureIssueQuery{Page: 1, PageSize: 50, Actionable: true, ReviewState: "pending"})
	if err != nil || otherPending.Total != 1 || otherPending.HandledTotal != 0 {
		t.Fatalf("other actor observed private review state: page=%+v err=%v", otherPending, err)
	}
	if _, err := s.SaveStructureReviewChoice(context.Background(), actor, library.ID, pending.List[0].Token, MediaLibraryStructureReviewChoiceInput{DiagnosisRevision: diagnostics.Revision, ReviewRevision: 0, Action: StructureSelectionSkip}, RequestContext{}); ErrorCode(err) != CodeConflict {
		t.Fatalf("stale review revision accepted: %v", err)
	}
	if _, err := s.DeleteStructureReviewChoice(context.Background(), actor, library.ID, handled.List[0].Token, MediaLibraryStructureReviewChoiceInput{DiagnosisRevision: diagnostics.Revision, ReviewRevision: 1}, RequestContext{}); err != nil {
		t.Fatal(err)
	}
	pending, err = s.StructureIssues(context.Background(), actor, library.ID, MediaLibraryStructureIssueQuery{Page: 1, PageSize: 50, Actionable: true, ReviewState: "pending"})
	if err != nil || pending.Total != 1 || pending.HandledTotal != 0 {
		t.Fatalf("after undo=%+v err=%v", pending, err)
	}
	resaved, err := s.SaveStructureReviewChoice(context.Background(), actor, library.ID, pending.List[0].Token, MediaLibraryStructureReviewChoiceInput{DiagnosisRevision: diagnostics.Revision, ReviewRevision: 2, Action: StructureSelectionSkip}, RequestContext{})
	if err != nil || resaved.ReviewRevision != 3 {
		t.Fatalf("resave=%+v err=%v", resaved, err)
	}
	var session models.MediaLibraryStructureReviewSession
	if err := s.db.Where("owner_id = ? AND library_id = ? AND diagnosis_job_id = ?", actor.User.ID, library.ID, job.ID).First(&session).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.db.Model(&models.MediaLibraryStructureReviewChoice{}).Where("session_id = ? AND issue_token = ?", session.ID, pending.List[0].Token).Update("state", "submitted").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteStructureReviewChoice(context.Background(), actor, library.ID, pending.List[0].Token, MediaLibraryStructureReviewChoiceInput{DiagnosisRevision: diagnostics.Revision, ReviewRevision: 3}, RequestContext{}); ErrorCode(err) != CodeConflict {
		t.Fatalf("submitted choice was undoable: %v", err)
	}
	if _, err := s.SaveStructureReviewChoice(context.Background(), actor, library.ID, pending.List[0].Token, MediaLibraryStructureReviewChoiceInput{DiagnosisRevision: diagnostics.Revision, ReviewRevision: 3, Action: StructureSelectionSkip}, RequestContext{}); ErrorCode(err) != CodeConflict {
		t.Fatalf("submitted choice was editable: %v", err)
	}
	if _, err := s.SaveStructureReviewBulk(context.Background(), actor, library.ID, MediaLibraryStructureReviewBulkInput{DiagnosisRevision: diagnostics.Revision, ReviewRevision: 3, Codes: []string{"media_unrecognized"}, Action: StructureSelectionSkip}, RequestContext{}); ErrorCode(err) != CodeConflict {
		t.Fatalf("bulk action rewrote a submitted choice: %v", err)
	}
	marked, err := s.SaveStructureRecognitionReview(context.Background(), actor, library.ID, encodeRecognitionToken(recognition.ID), MediaLibraryStructureReviewChoiceInput{DiagnosisRevision: diagnostics.Revision, ReviewRevision: 3}, RequestContext{})
	if err != nil || marked.ReviewRevision != 4 {
		t.Fatalf("save recognition marker=%+v err=%v", marked, err)
	}
	if err := s.db.Where("library_id = ? AND diagnosis_job_id = ? AND recognition_id = ?", library.ID, job.ID, recognition.ID).Delete(&models.MediaLibraryStructureIssue{}).Error; err != nil {
		t.Fatal(err)
	}
	unmarked, err := s.DeleteStructureRecognitionReview(context.Background(), actor, library.ID, encodeRecognitionToken(recognition.ID), MediaLibraryStructureReviewChoiceInput{DiagnosisRevision: diagnostics.Revision, ReviewRevision: 4}, RequestContext{})
	if err != nil || unmarked.ReviewRevision != 5 {
		t.Fatalf("delete marker after healthy projection=%+v err=%v", unmarked, err)
	}
	var recognitionMarkers int64
	if err := s.db.Model(&models.MediaLibraryStructureReviewChoice{}).Where("session_id = ? AND subject_key = ?", session.ID, fmt.Sprintf("recognition:%d", recognition.ID)).Count(&recognitionMarkers).Error; err != nil {
		t.Fatal(err)
	}
	if recognitionMarkers != 0 {
		t.Fatalf("healthy automatic projection retained %d recognition marker(s)", recognitionMarkers)
	}

	newJob := enqueueFake(t, s.queue, actor, "new review diagnosis", "new-review-diagnosis")
	if err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&models.MediaLibraryStructureDiagnosis{}).Where("library_id = ?", library.ID).Updates(map[string]any{"job_id": newJob.ID, "status": models.MediaLibraryStructureIssues, "updated_at": time.Now().UTC()}).Error; err != nil {
			return err
		}
		return persistStructureIssuesTx(tx, library.ID, newJob.ID, library.BaselineGeneration, StructurePlan{AllIssues: issues}, time.Now().UTC())
	}); err != nil {
		t.Fatal(err)
	}
	var sessions int64
	if err := s.db.Model(&models.MediaLibraryStructureReviewSession{}).Where("library_id = ?", library.ID).Count(&sessions).Error; err != nil {
		t.Fatal(err)
	}
	if sessions != 0 {
		t.Fatalf("old diagnosis review session retained: %d", sessions)
	}
}

func TestStructureReviewFirstMutationInvalidatesPreviewCreatedWithoutSession(t *testing.T) {
	s, actor, library, diagnostics := prepareStructureSelectionConflicts(t, 1)
	page, err := s.StructureIssues(context.Background(), actor, library.ID, MediaLibraryStructureIssueQuery{Page: 1, PageSize: 50, Actionable: true, ReviewState: "pending"})
	if err != nil || len(page.List) != 1 {
		t.Fatalf("issues=%+v err=%v", page, err)
	}
	issue := page.List[0]
	preview, err := s.PreviewSelectionRepair(context.Background(), actor, library.ID, MediaLibraryStructureSelectionInput{Revision: diagnostics.Revision, ReviewRevision: 0, Selections: []MediaLibraryStructureSelection{{IssueToken: issue.Token, Action: StructureSelectionSkip}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveStructureReviewChoice(context.Background(), actor, library.ID, issue.Token, MediaLibraryStructureReviewChoiceInput{DiagnosisRevision: diagnostics.Revision, ReviewRevision: 0, Action: StructureSelectionSkip}, RequestContext{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnqueueSelectionRepair(context.Background(), actor, library.ID, preview.ConfirmationToken, RequestContext{}); ErrorCode(err) != CodeConflict {
		t.Fatalf("preview created before the review session survived its first mutation: %v", err)
	}
}

func TestStructureReviewSubmissionAdvancesRevisionAndFreezesChoice(t *testing.T) {
	s, actor, library, diagnostics := prepareStructureSelectionConflicts(t, 1)
	page, err := s.StructureIssues(context.Background(), actor, library.ID, MediaLibraryStructureIssueQuery{Page: 1, PageSize: 50, Actionable: true, ReviewState: "pending"})
	if err != nil || len(page.List) != 1 {
		t.Fatalf("issues=%+v err=%v", page, err)
	}
	issue := page.List[0]
	saved, err := s.SaveStructureReviewChoice(context.Background(), actor, library.ID, issue.Token, MediaLibraryStructureReviewChoiceInput{DiagnosisRevision: diagnostics.Revision, ReviewRevision: 0, Action: StructureSelectionSkip}, RequestContext{})
	if err != nil || saved.ReviewRevision != 1 {
		t.Fatalf("save=%+v err=%v", saved, err)
	}
	preview, err := s.PreviewSelectionRepair(context.Background(), actor, library.ID, MediaLibraryStructureSelectionInput{Revision: diagnostics.Revision, ReviewRevision: saved.ReviewRevision})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnqueueSelectionRepair(context.Background(), actor, library.ID, preview.ConfirmationToken, RequestContext{}); err != nil {
		t.Fatal(err)
	}
	var session models.MediaLibraryStructureReviewSession
	if err := s.db.Where("owner_id = ? AND library_id = ?", actor.User.ID, library.ID).First(&session).Error; err != nil {
		t.Fatal(err)
	}
	if session.Revision != 2 {
		t.Fatalf("review revision=%d want=2 after submission", session.Revision)
	}
	var choice models.MediaLibraryStructureReviewChoice
	if err := s.db.Where("session_id = ? AND issue_token = ?", session.ID, issue.Token).First(&choice).Error; err != nil {
		t.Fatal(err)
	}
	if choice.State != "submitted" {
		t.Fatalf("choice state=%q want=submitted", choice.State)
	}
	if _, err := s.DeleteStructureReviewChoice(context.Background(), actor, library.ID, issue.Token, MediaLibraryStructureReviewChoiceInput{DiagnosisRevision: diagnostics.Revision, ReviewRevision: session.Revision}, RequestContext{}); ErrorCode(err) != CodeConflict {
		t.Fatalf("submitted choice was undoable: %v", err)
	}
}

func TestStructureReviewRecognitionGroupingPrecedesConflictShape(t *testing.T) {
	s, actor, library := structureConfirmationFixture(t)
	now := time.Now().UTC()
	job := enqueueFake(t, s.queue, actor, "recognition grouping precedence", "recognition-grouping-precedence")
	diagnosis := models.MediaLibraryStructureDiagnosis{LibraryID: library.ID, JobID: job.ID, Generation: library.BaselineGeneration, ScanKind: "manual", Status: models.MediaLibraryStructureIssues, CreatedAt: now, UpdatedAt: now}
	if err := s.db.Create(&diagnosis).Error; err != nil {
		t.Fatal(err)
	}
	var profile models.MediaClassificationProfile
	if err := s.db.First(&profile, library.ProfileID).Error; err != nil {
		t.Fatal(err)
	}
	recognition := models.MediaLibraryRecognition{LibraryID: library.ID, SourceKey: "recognition-grouping-precedence", InputFingerprint: "recognition-grouping-precedence", ProfileID: profile.ID, ProfileRevision: profile.Revision, Status: mediaRecognitionStatusUnrecognized, MetadataJSON: "{}", LastGeneration: library.BaselineGeneration, CreatedAt: now, UpdatedAt: now}
	if err := s.db.Create(&recognition).Error; err != nil {
		t.Fatal(err)
	}
	issues := []StructureIssue{
		{Code: "media_unrecognized", Kind: "video", Title: "同一部剧", CurrentPath: "同一部剧/Season 01/01.mkv", ExpectedPath: "target-a", ConflictSourceCount: 2, RecognitionID: recognition.ID},
		{Code: "media_unrecognized", Kind: "video", Title: "同一部剧", CurrentPath: "同一部剧/Season 01/02.mkv", ExpectedPath: "target-b", ConflictSourceCount: 2, RecognitionID: recognition.ID},
	}
	if err := s.db.Transaction(func(tx *gorm.DB) error {
		return persistStructureIssuesTx(tx, library.ID, job.ID, library.BaselineGeneration, StructurePlan{AllIssues: issues}, now)
	}); err != nil {
		t.Fatal(err)
	}
	page, err := s.StructureIssues(context.Background(), actor, library.ID, MediaLibraryStructureIssueQuery{Page: 1, PageSize: 50, Actionable: true})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || len(page.List) != 1 || page.List[0].AffectedFileCount != 2 {
		t.Fatalf("recognition grouping was split by conflict shape: %+v", page)
	}
}

func TestStructureReviewBulkUsesBoundedStatements(t *testing.T) {
	s, actor, library := structureConfirmationFixture(t)
	now := time.Now().UTC()
	job := enqueueFake(t, s.queue, actor, "bounded review bulk", "bounded-review-bulk")
	diagnosis := models.MediaLibraryStructureDiagnosis{LibraryID: library.ID, JobID: job.ID, Generation: library.BaselineGeneration, ScanKind: "manual", Status: models.MediaLibraryStructureIssues, CreatedAt: now, UpdatedAt: now}
	if err := s.db.Create(&diagnosis).Error; err != nil {
		t.Fatal(err)
	}
	const issueTotal = 1500
	issues := make([]StructureIssue, 0, issueTotal)
	for index := 0; index < issueTotal; index++ {
		issues = append(issues, StructureIssue{
			Code:                "duplicate_target",
			Kind:                "video",
			CurrentPath:         fmt.Sprintf("source/%04d.mkv", index),
			ExpectedPath:        fmt.Sprintf("target/%04d.mkv", index),
			ConflictSourceCount: 2,
			AllConflictSources: []string{
				fmt.Sprintf("source/%04d.mkv", index),
				fmt.Sprintf("source/%04d (1).mkv", index),
			},
		})
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
	counter := &traceCountingGORMLogger{}
	s.db = s.db.Session(&gorm.Session{Logger: counter})
	result, err := s.SaveStructureReviewBulk(context.Background(), actor, library.ID, MediaLibraryStructureReviewBulkInput{DiagnosisRevision: diagnostics.Revision, ReviewRevision: 0, Codes: []string{"duplicate_target"}, Action: StructureSelectionKeepRecommended}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Updated != issueTotal {
		t.Fatalf("updated=%d want=%d", result.Updated, issueTotal)
	}
	if statements := counter.count.Load(); statements > 20 {
		t.Fatalf("bulk review used %d SQL statements, want bounded batches", statements)
	}
}
