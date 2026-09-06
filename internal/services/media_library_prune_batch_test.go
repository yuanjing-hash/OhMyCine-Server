package services

import (
	"context"
	"fmt"
	"testing"
	"time"

	serverlog "github.com/yuanjing-hash/OhMyCine-Server/internal/logging"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/medialibrary"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm/logger"
)

func TestFastScanPrunesCatalogInBoundedBatches(t *testing.T) {
	service, db, actor, storage, profile := mediaLibraryTestService(t)
	created, err := service.Create(context.Background(), actor, testLibraryInput("Prune batch", storage, profile, false), RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	var library models.MediaLibrary
	if err := db.First(&library, created.ID).Error; err != nil {
		t.Fatal(err)
	}
	entries := make([]models.MediaLibraryEntry, 1001)
	assets := make([]models.MediaLibrarySourceAsset, 1001)
	for i := range entries {
		entries[i] = models.MediaLibraryEntry{LibraryID: library.ID, RelativePath: fmt.Sprintf("/Movie.%d.mkv", i), WorkKey: fmt.Sprint(i), Title: "Synthetic", LastGeneration: 1}
		assets[i] = models.MediaLibrarySourceAsset{LibraryID: library.ID, RelativePath: fmt.Sprintf("/Movie.%d.srt", i), Generation: 1, Active: true}
	}
	for _, rows := range []any{&entries, &assets} {
		if err := db.CreateInBatches(rows, 100).Error; err != nil {
			t.Fatal(err)
		}
	}
	run := models.MediaLibraryScanRun{LibraryID: library.ID, Kind: "full", Status: "running", Phase: "enumerating", Generation: library.DirtyGeneration + 1, SourceFingerprint: mediaLibraryScanSourceFingerprint(library, storage, profile), CheckpointJSON: "{}", StartedAt: time.Now().UTC()}
	if err := db.Create(&run).Error; err != nil {
		t.Fatal(err)
	}
	log := &performanceLogger{Interface: logger.Default.LogMode(logger.Silent)}
	db.Logger = log
	published, err := service.publishFastPan115Scan(context.Background(), library, storage, profile, run, medialibrary.Result{}, time.Now(), serverlog.OperationLibraryFullScan)
	if err != nil || published.Removed != len(entries) {
		t.Fatalf("removed=%d error=%v", published.Removed, err)
	}
	for _, model := range []any{&models.MediaLibraryEntry{}, &models.MediaLibrarySourceAsset{}} {
		var count int64
		if err := db.Model(model).Where("library_id = ?", library.ID).Count(&count).Error; err != nil || count != 0 {
			t.Fatalf("remaining=%d error=%v", count, err)
		}
	}
	if count := log.queries.Load(); count > 50 {
		t.Fatalf("prune made %d queries; expected bounded batches, not one DELETE per row", count)
	}
}
