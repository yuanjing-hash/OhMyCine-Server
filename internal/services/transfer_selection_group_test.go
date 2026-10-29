package services

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
)

func TestTransferSharedSelectionKeepsSourceAndOtherReferenceData(t *testing.T) {
	queue, _, download, source, destination := transferFixture(t, models.MediaLibraryTransferMove, models.MediaLibraryConflictOverwrite, false)
	download.BTSelectionGroupID = strings.Repeat("a", 64)
	if err := queue.db.Create(&models.BTSelectionGroup{ID: download.BTSelectionGroupID, ScopeDigest: "fixture-scope", OwnerID: download.OwnerID, Workspace: download.StagingAbsolutePath, KnownDigestsJSON: "[]"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := queue.db.Model(&models.DownloadTask{}).Where("id = ?", download.ID).Update("bt_selection_group_id", download.BTSelectionGroupID).Error; err != nil {
		t.Fatal(err)
	}
	sibling := filepath.Join(filepath.Dir(source), "Other.Episode.mkv")
	if err := os.WriteFile(sibling, []byte("another-reference"), 0600); err != nil {
		t.Fatal(err)
	}
	service := NewTransferService(queue.db, queue.audit, queue, zerolog.Nop())
	manifest := downloadpkg.Manifest{Name: "Movie.2024", Complete: true, Files: []downloadpkg.File{{RelativePath: "Movie.2024.mkv", Size: minimumAutomaticTransferVideoBytes}}}
	if err := service.Enqueue(download, manifest); err != nil {
		t.Fatal(err)
	}
	claimed, err := queue.Claim([]string{"transfer"})
	if err != nil || claimed == nil {
		t.Fatal(err)
	}
	result := NewTransferWorker(service).Run(context.Background(), workerRuntime{queue: queue, job: *claimed}, *claimed)
	if result.ErrorCode != "" || result.Wait != nil {
		t.Fatal(result)
	}
	for _, file := range []string{source, sibling, destination} {
		if _, err = os.Stat(file); err != nil {
			t.Fatalf("shared source consumer removed %s: %v", file, err)
		}
	}
	var transfer models.TransferTask
	if err = queue.db.Where("download_task_id = ?", download.ID).First(&transfer).Error; err != nil {
		t.Fatal(err)
	}
	if transfer.CleanupStatus != models.TransferCleanupDeferred || transfer.CleanupErrorCode != "shared_source_retained" {
		t.Fatal(transfer.CleanupStatus, transfer.CleanupErrorCode)
	}
	var stored models.DownloadTask
	if err = queue.db.First(&stored, "id = ?", download.ID).Error; err != nil || stored.TransferMode != models.MediaLibraryTransferMove {
		t.Fatal("frozen policy overwritten", err)
	}
}
