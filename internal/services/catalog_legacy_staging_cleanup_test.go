package services

import (
	"context"
	"fmt"

	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestCatalogLegacyStagingCleanupIsBoundedAndPreservesActiveAndRecent(t *testing.T) {
	s, library, _, _ := catalogFixture(t)
	now := time.Now().UTC()
	runs := []models.MediaLibraryScanRun{
		{LibraryID: library.ID, Status: "success", StartedAt: now.Add(-8 * 24 * time.Hour)},
		{LibraryID: library.ID, Status: "running", StartedAt: now.Add(-8 * 24 * time.Hour)},
		{LibraryID: library.ID, Status: "failed", StartedAt: now},
	}
	if err := s.writeDB.Create(&runs).Error; err != nil {
		t.Fatal(err)
	}
	for _, run := range runs {
		rows := make([]models.MediaLibraryScanStaging, 600)
		for index := range rows {
			rows[index] = models.MediaLibraryScanStaging{RunID: run.ID, LibraryID: library.ID, ItemKind: "video", RelativePath: fmt.Sprintf("%d.mkv", index), CreatedAt: now, UpdatedAt: now}
		}
		if err := s.writeDB.CreateInBatches(rows, 100).Error; err != nil {
			t.Fatal(err)
		}
	}
	cursor, total := uint(0), int64(0)
	for pass := 0; pass < 5; pass++ {
		next, deleted, err := s.CleanupLegacyScanStaging(context.Background(), cursor, now)
		if err != nil || deleted > CatalogBatchRows {
			t.Fatalf("unbounded cleanup: deleted=%d err=%v", deleted, err)
		}
		cursor, total = next, total+deleted
	}
	if total != 600 {
		t.Fatalf("eligible backlog not drained: %d", total)
	}
	for _, run := range runs[1:] {
		var count int64
		if err := s.writeDB.Model(&models.MediaLibraryScanStaging{}).Where("run_id=?", run.ID).Count(&count).Error; err != nil || count != 600 {
			t.Fatalf("active/recent recovery data removed: count=%d err=%v", count, err)
		}
	}
}
