package services

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

func TestStructureConflictCountersMatchExactPagedCodes(t *testing.T) {
	s, repair, _, _, _ := catalogStructureRepairFixture(t)
	now := time.Now().UTC()
	job, err := NewQueueService(s.db, NewAuditService(s.db)).Enqueue(EnqueueJobInput{OwnerID: repair.OwnerID, JobType: JobTypeMediaLibraryStructureDiagnosis, DisplayName: "conflict counts", Payload: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	diagnosis := models.MediaLibraryStructureDiagnosis{LibraryID: repair.LibraryID, JobID: job.ID, Generation: 2, Status: "issues", CreatedAt: now, UpdatedAt: now}
	if err := s.db.Create(&diagnosis).Error; err != nil {
		t.Fatal(err)
	}
	plan := StructurePlan{}
	for index, code := range []string{"duplicate_target", "recognition_suspect_conflict", "recognition_suspect_conflict", "catalog_duplicate_conflict", "catalog_duplicate_conflict", "catalog_duplicate_conflict"} {
		plan.addIssue(StructureIssue{Code: code, Kind: "conflict"})
		row := models.MediaLibraryStructureIssue{LibraryID: repair.LibraryID, DiagnosisJobID: job.ID, Generation: 2, Token: fmt.Sprintf("conflict-%d", index), Code: code, Kind: "conflict", State: "pending", CreatedAt: now, UpdatedAt: now}
		if err := s.db.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
	}
	if plan.Classifications.DuplicateTarget != 1 || plan.Classifications.RecognitionSuspectConflict != 2 || plan.Classifications.CatalogDuplicateConflict != 3 {
		t.Fatalf("planner categories: %+v", plan.Classifications)
	}
	if err := s.db.Transaction(func(tx *gorm.DB) error { return refreshStructureSummaryTx(tx, repair.LibraryID, now) }); err != nil {
		t.Fatal(err)
	}
	var library models.MediaLibrary
	if err := s.db.First(&library, repair.LibraryID).Error; err != nil {
		t.Fatal(err)
	}
	dto, err := s.diagnosticsForLibrary(context.Background(), library)
	if err != nil {
		t.Fatal(err)
	}
	if dto.IssueCount != 6 || dto.Classifications != plan.Classifications {
		t.Fatalf("API counters differ: %+v", dto)
	}
	for code, want := range map[string]int{"duplicate_target": 1, "recognition_suspect_conflict": 2, "catalog_duplicate_conflict": 3} {
		if err := s.withCatalogRead(context.Background(), repair.LibraryID, func(tx *gorm.DB, reader *CatalogReader) error {
			page, err := s.structureIssuesTx(tx, reader, repair.LibraryID, MediaLibraryStructureIssueQuery{Code: code, Page: 1, PageSize: 50})
			if err == nil && int(page.Total) != want {
				t.Fatalf("%s page=%+v want=%d", code, page, want)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
}
