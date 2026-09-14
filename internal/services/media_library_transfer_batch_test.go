package services

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	cloudpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"gorm.io/gorm"
)

func TestTransferBatchPublishesOnlySuccessfulTargetsWithoutProviderEnumeration(t *testing.T) {
	f := newCloudTransferFixture(t, models.MediaLibraryTransferMove, models.MediaLibraryConflictOverwrite, false)
	if result := f.run(t); result.ErrorCode != "" {
		t.Fatalf("transfer: %+v", result)
	}
	s := NewMediaLibraryService(f.queue.db, f.queue.audit, zerolog.Nop())
	s.connections = f.service.connections
	var task models.TransferTask
	if err := f.queue.db.First(&task, "download_task_id = ?", f.download.ID).Error; err != nil {
		t.Fatal(err)
	}
	// An unrelated catalog row and provider file must remain untouched.
	now := time.Now().UTC()
	unrelated := models.MediaLibraryEntry{LibraryID: f.library.ID, RelativePath: "/Other/Other.mkv", ProviderID: "unrelated", Size: 88, ModifiedAt: now, LastGeneration: 777, CreatedAt: now, UpdatedAt: now}
	if err := f.queue.db.Create(&unrelated).Error; err != nil {
		t.Fatal(err)
	}
	f.driver.items["unrelated"] = cloudpkg.Item{ID: "unrelated", ParentID: "library-root", Name: "Other.mkv", Size: 88}
	before := f.driver.listCalls
	var unboundedQueries []string
	callback := "test_transfer_batch_no_library_sweep"
	if err := f.queue.db.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		sql := strings.ToLower(tx.Statement.SQL.String())
		if (strings.Contains(sql, "media_library_entries") || strings.Contains(sql, "media_library_source_assets")) && !strings.Contains(sql, "provider_id in") && !strings.Contains(sql, "relative_path in") && !strings.Contains(sql, "recognition_id =") {
			unboundedQueries = append(unboundedQueries, sql)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileTransferBatch(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}
	_ = f.queue.db.Callback().Query().Remove(callback)
	if len(unboundedQueries) > 0 {
		t.Fatalf("batch queried unrelated catalog baseline: %v", unboundedQueries)
	}
	if f.driver.listCalls != before {
		t.Fatalf("batch enumerated provider: %d listings", f.driver.listCalls-before)
	}
	var entries []models.MediaLibraryEntry
	if err := f.queue.db.Order("id").Find(&entries, "library_id = ?", f.library.ID).Error; err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries=%+v", entries)
	}
	if entries[0].LastGeneration != 777 || !entries[0].UpdatedAt.Equal(now) {
		t.Fatalf("unrelated entry touched: %+v", entries[0])
	}
	if entries[1].TMDBID == nil || *entries[1].TMDBID != *f.download.ScrapeTMDBID || entries[1].MatchStatus != mediaRecognitionStatusMatched {
		t.Fatalf("verified identity not reused: %+v", entries[1])
	}
	var runs int64
	f.queue.db.Model(&models.MediaLibraryScanRun{}).Where("library_id = ?", f.library.ID).Count(&runs)
	stats := f.driver.statCalls
	if err := s.ReconcileTransferBatch(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}
	var after int64
	f.queue.db.Model(&models.MediaLibraryScanRun{}).Where("library_id = ?", f.library.ID).Count(&after)
	if after != runs || f.driver.statCalls != stats {
		t.Fatal("completed batch was reprocessed")
	}
	// Lost receipt after durable publication reuses the same scan/batch.
	if err := f.queue.db.Model(&models.TransferTask{}).Where("id = ?", task.ID).Update("catalog_published_at", nil).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileTransferBatch(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}
	f.queue.db.Model(&models.MediaLibraryScanRun{}).Where("library_id = ?", f.library.ID).Count(&after)
	if after != runs || f.driver.statCalls != stats {
		t.Fatal("lost receipt restarted catalog work")
	}
}

func TestTransferBatchMissingManagedEvidenceRejectsBeforePublication(t *testing.T) {
	f := newCloudTransferFixture(t, models.MediaLibraryTransferMove, models.MediaLibraryConflictOverwrite, false)
	if result := f.run(t); result.ErrorCode != "" {
		t.Fatalf("transfer: %+v", result)
	}
	s := NewMediaLibraryService(f.queue.db, f.queue.audit, zerolog.Nop())
	s.connections = f.service.connections
	var task models.TransferTask
	if err := f.queue.db.First(&task, "download_task_id = ?", f.download.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.queue.db.Where("transfer_task_id = ?", task.ID).Delete(&models.MediaManagedItem{}).Error; err != nil {
		t.Fatal(err)
	}
	before := f.driver.statCalls
	if err := s.ReconcileTransferBatch(context.Background(), task.ID); err == nil {
		t.Fatal("incomplete successful target manifest accepted")
	}
	if f.driver.statCalls != before {
		t.Fatal("incomplete batch reached provider")
	}
	var count int64
	f.queue.db.Model(&models.MediaLibraryScanRun{}).Where("library_id = ?", f.library.ID).Count(&count)
	if count != 0 {
		t.Fatal("incomplete batch was published")
	}
}

func TestTransferBatchMissingTargetDoesNotFallBackToScan(t *testing.T) {
	f := newCloudTransferFixture(t, models.MediaLibraryTransferMove, models.MediaLibraryConflictOverwrite, false)
	if result := f.run(t); result.ErrorCode != "" {
		t.Fatalf("transfer: %+v", result)
	}
	s := NewMediaLibraryService(f.queue.db, f.queue.audit, zerolog.Nop())
	s.connections = f.service.connections
	var task models.TransferTask
	if err := f.queue.db.First(&task, "download_task_id = ?", f.download.ID).Error; err != nil {
		t.Fatal(err)
	}
	delete(f.driver.items, f.sourceID)
	before := f.driver.listCalls
	if err := s.ReconcileTransferBatch(context.Background(), task.ID); err == nil {
		t.Fatal("missing target accepted")
	}
	if f.driver.listCalls != before {
		t.Fatal("failed batch enumerated provider")
	}
	if err := f.queue.db.First(&task, "id = ?", task.ID).Error; err != nil {
		t.Fatal(err)
	}
	if task.CatalogPublishedAt != nil {
		t.Fatal("failed batch acknowledged")
	}
}
