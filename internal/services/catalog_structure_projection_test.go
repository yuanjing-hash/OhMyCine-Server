package services

import (
	"context"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

func TestStructureProjectionIgnoresRetainedOldSourceIssues(t *testing.T) {
	s, repair, _, _, _ := catalogStructureRepairFixture(t)
	now := time.Now().UTC()
	queue := NewQueueService(s.db, NewAuditService(s.db))
	job, err := queue.Enqueue(EnqueueJobInput{OwnerID: repair.OwnerID, JobType: JobTypeMediaLibraryStructureDiagnosis, DisplayName: "current diagnosis", Payload: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	diagnosis := models.MediaLibraryStructureDiagnosis{LibraryID: repair.LibraryID, JobID: job.ID, Generation: 2, SourceRevision: 2, Status: "issues", CreatedAt: now, UpdatedAt: now}
	source := models.MediaLibraryStructureAutoState{LibraryID: repair.LibraryID, SourceRevision: 2, UpdatedAt: now}
	issues := []models.MediaLibraryStructureIssue{
		{LibraryID: repair.LibraryID, DiagnosisJobID: "old", Generation: 1, Token: "old", Code: "invalid_path", Kind: "video", CreatedAt: now, UpdatedAt: now},
		{LibraryID: repair.LibraryID, DiagnosisJobID: job.ID, Generation: 2, Token: "current", Code: "path_mismatch", Kind: "video", CreatedAt: now, UpdatedAt: now},
	}
	if err := s.db.Save(&source).Error; err != nil {
		t.Fatal(err)
	}
	for _, row := range []any{&diagnosis, &issues} {
		if err := s.db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := s.db.Transaction(func(tx *gorm.DB) error { return refreshStructureSummaryTx(tx, repair.LibraryID, now) }); err != nil {
		t.Fatal(err)
	}
	var current models.MediaLibrary
	if err := s.db.First(&current, repair.LibraryID).Error; err != nil || current.StructureIssueCount != 1 {
		t.Fatalf("mixed generations=%+v %v", current, err)
	}
	if err := s.withCatalogRead(context.Background(), repair.LibraryID, func(tx *gorm.DB, r *CatalogReader) error {
		page, err := s.structureIssuesTx(tx, r, repair.LibraryID, MediaLibraryStructureIssueQuery{Page: 1, PageSize: 50})
		if err == nil && (page.Total != 1 || page.List[0].Token != "current") {
			t.Fatalf("mixed page=%+v", page)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.db.Model(&source).Update("source_revision", 3).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.db.Model(&diagnosis).Updates(map[string]any{"status": "failed", "last_error_code": "source_changed"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.db.Model(&current).Updates(map[string]any{"structure_status": "unchecked", "structure_issue_count": 0}).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.db.Transaction(func(tx *gorm.DB) error { return refreshStructureSummaryTx(tx, repair.LibraryID, now) }); err != nil {
		t.Fatal(err)
	}
	if err := s.db.First(&current, repair.LibraryID).Error; err != nil || current.StructureIssueCount != 0 || current.StructureStatus != "unchecked" {
		t.Fatalf("stale projection resurrected=%+v %v", current, err)
	}
	if err := s.withCatalogRead(context.Background(), repair.LibraryID, func(tx *gorm.DB, r *CatalogReader) error {
		page, err := s.structureIssuesTx(tx, r, repair.LibraryID, MediaLibraryStructureIssueQuery{Page: 1, PageSize: 50})
		if err == nil && page.Total != 0 {
			t.Fatalf("old source visible=%+v", page)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
