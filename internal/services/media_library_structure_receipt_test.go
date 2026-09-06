package services

import (
	"context"
	"errors"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

func TestDiagnosisSubmissionDoesNotReadAfterAcceptance(t *testing.T) {
	s, actor, library := structureConfirmationFixture(t)
	// The queue transaction writes this projection. A separate read outage must
	// not turn the already durable enqueue into an apparent submission failure.
	const callback = "test:diagnosis_read_outage"
	if err := s.db.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "media_library_structure_diagnoses" {
			_ = tx.AddError(errors.New("injected diagnosis read outage"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.db.Callback().Query().Remove(callback) })
	accepted, err := s.Diagnose(context.Background(), library.ID, "")
	if err != nil {
		t.Fatalf("durable submission reported failure: %v", err)
	}
	if accepted.JobID == "" || accepted.Status != models.MediaLibraryStructureQueued || accepted.LibraryID != library.ID {
		t.Fatalf("invalid acceptance receipt: %+v", accepted)
	}
	_ = s.db.Callback().Query().Remove(callback)
	current, err := s.Diagnostics(context.Background(), actor, library.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.JobID != accepted.JobID || current.Revision != accepted.Revision || current.Generation != accepted.Generation {
		t.Fatalf("receipt differs from committed projection: accepted=%+v current=%+v", accepted, current)
	}
}

func TestDiagnosisFailedEnqueuePreservesPreviousResult(t *testing.T) {
	s, _, library := structureConfirmationFixture(t)
	if err := s.db.Model(&library).Updates(map[string]any{"structure_status": models.MediaLibraryStructureIssues, "structure_issue_count": 9}).Error; err != nil {
		t.Fatal(err)
	}
	const callback = "test:diagnosis_write_outage"
	if err := s.db.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "media_library_structure_diagnoses" {
			_ = tx.AddError(errors.New("injected enqueue failure"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.db.Callback().Create().Remove(callback) })
	if _, err := s.Diagnose(context.Background(), library.ID, ""); err == nil {
		t.Fatal("enqueue failure was hidden")
	}
	var current models.MediaLibrary
	if err := s.db.First(&current, library.ID).Error; err != nil {
		t.Fatal(err)
	}
	if current.StructureStatus != models.MediaLibraryStructureIssues || current.StructureIssueCount != 9 {
		t.Fatalf("failed new attempt overwrote previous diagnosis: %+v", current)
	}
}
