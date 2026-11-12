package services

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"gorm.io/gorm"
)

func seedingFixture(t *testing.T, mode string, cleanup bool) (*SeedingService, *QueueService, Actor, *stubDownloadClient, models.DownloadTask) {
	t.Helper()
	_, downloaders, queue, actor, client := downloadFixture(t)
	actor.Permissions[authz.PermissionJobsControlOwn] = struct{}{}
	actor.Permissions[authz.PermissionDiscoveryRead] = struct{}{}
	actor.Permissions[authz.PermissionDownloadsCreate] = struct{}{}
	persistFixtureAuthority(t, queue.db, actor)
	provider, err := downloaders.Create(actor, DownloaderInput{Name: "qBit seeding", Type: models.DownloaderTypeQBittorrent, BaseURL: "http://127.0.0.1:8080", Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	download := models.DownloadTask{ID: "download-" + mode, OwnerID: actor.User.ID, DownloaderID: &provider.ID, DownloaderName: provider.Name, ProviderType: provider.Type, ProviderTaskID: "provider-hash", SourceCiphertext: "encrypted", ProfileRulesJSON: "{}", TransferMode: mode, SeedingCleanupEnabled: cleanup, SeedingMinimumMinutes: 60, SeedingMinimumRatio: 1, SeedingCompletionMode: models.SeedingCompletionAll, DisplayName: "Example", Phase: models.DownloadTaskStatusCompleted, CreatedAt: now, UpdatedAt: now}
	legacySource, _ := json.Marshal(downloadSourceEnvelope{Kind: downloadpkg.SourceURL, URL: "magnet:?xt=urn:btih:" + strings.Repeat("a", 40)})
	download.SourceCiphertext, err = downloaders.credentials.Encrypt(downloadSourcePurpose(download.ID), string(legacySource))
	if err != nil {
		t.Fatal(err)
	}
	_, err = queue.EnqueueWith(EnqueueJobInput{OwnerID: actor.User.ID, JobType: "download", DisplayName: "original", Payload: map[string]any{"download_task_id": download.ID}}, func(tx *gorm.DB, job models.Job) error { download.JobID = job.ID; return tx.Create(&download).Error })
	if err != nil {
		t.Fatal(err)
	}
	manifest := downloadpkg.Manifest{Complete: true, Files: []downloadpkg.File{{RelativePath: "Movie.mkv", Size: minimumAutomaticTransferVideoBytes}}}
	rawManifest, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var profile models.MediaClassificationProfile
	if err := queue.db.Where("code = ?", "default-v1").First(&profile).Error; err != nil {
		t.Fatal(err)
	}
	storage := models.Storage{Name: "Seeding target " + mode, NameNormalized: "seeding-target-" + mode, Type: models.StorageTypeLocal, RootPath: t.TempDir(), RootPathNormalized: "seeding-root-" + mode, Enabled: true, Capabilities: `{}`, CreatedAt: now, UpdatedAt: now}
	if err := queue.db.Create(&storage).Error; err != nil {
		t.Fatal(err)
	}
	library := models.MediaLibrary{Name: "Seeding library " + mode, NameNormalized: "seeding-library-" + mode, StorageID: storage.ID, ProfileID: profile.ID, ProfileRevision: profile.Revision, RelativeRoot: "/", Enabled: false, VideoExtensionsJSON: `[]`, IgnorePatternsJSON: `[]`, Status: models.MediaLibraryStatusDisabled, CreatedAt: now, UpdatedAt: now}
	if err := queue.db.Create(&library).Error; err != nil {
		t.Fatal(err)
	}
	transfer := models.TransferTask{ID: "transfer-" + mode, OwnerID: actor.User.ID, JobID: download.JobID, DownloadTaskID: download.ID, LibraryID: library.ID, LibraryName: library.Name, ManifestJSON: string(rawManifest), SourceManifestJSON: string(rawManifest), Phase: models.TransferTaskStatusCompleted, CatalogPublishedAt: &now, CleanupStatus: models.TransferCleanupDeferred, CreatedAt: now, UpdatedAt: now}
	if err := queue.db.Create(&transfer).Error; err != nil {
		t.Fatal(err)
	}
	return NewSeedingService(queue.db, queue.audit, queue, downloaders, zerolog.Nop()), queue, actor, client, download
}

func TestSeedingWorkerUsesModeSpecificCleanup(t *testing.T) {
	for _, test := range []struct {
		name       string
		mode       string
		deleteData bool
		cloud      bool
		cleanup    bool
	}{
		{name: "local-copy", mode: models.MediaLibraryTransferCopy, deleteData: true, cleanup: true},
		{name: "symlink", mode: models.MediaLibraryTransferSymlink, cleanup: true},
		{name: "move-disabled", mode: models.MediaLibraryTransferMove},
		{name: "move-enabled", mode: models.MediaLibraryTransferMove, cleanup: true},
		{name: "cloud-move", mode: models.MediaLibraryTransferMove, cloud: true, cleanup: true, deleteData: true},
		{name: "cloud-copy", mode: models.MediaLibraryTransferCopy, cloud: true, cleanup: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, queue, actor, client, download := seedingFixture(t, test.mode, test.cleanup)
			if test.cloud {
				download.SourceDataSourceJSON, _ = marshalDataSourceIdentity(models.DataSourceIdentity{Kind: models.DataSourceKindLocal, ProviderType: models.StorageTypeLocal, ConnectionIdentity: models.DataSourceLocalConnectionIdentity})
				download.TargetDataSourceJSON, _ = marshalDataSourceIdentity(models.DataSourceIdentity{Kind: models.DataSourceKindProvider, ProviderType: "fixture-cloud", ConnectionIdentity: "1", StorageScope: "2"})
				download.TransferRouteKind, download.TransferRouteVersion = models.TransferRouteCrossSource, models.TransferRouteVersionCurrent
				download.TargetStorageType = "fixture-cloud"
				if err := queue.db.Save(&download).Error; err != nil {
					t.Fatal(err)
				}
				if err := queue.db.Model(&models.TransferTask{}).Where("download_task_id = ?", download.ID).Updates(map[string]any{"source_data_source_json": download.SourceDataSourceJSON, "target_data_source_json": download.TargetDataSourceJSON, "route_kind": download.TransferRouteKind, "route_version": download.TransferRouteVersion}).Error; err != nil {
					t.Fatal(err)
				}
			}
			logManager, logger := captureRemainingBusiness(t, "seeding")
			service.log = logger
			cleanupCalls := 0
			service.SetStagingCleanup(func(_ context.Context, downloadTaskID string, deleteData bool) error {
				cleanupCalls++
				if downloadTaskID != download.ID || deleteData != test.deleteData {
					t.Fatalf("staging cleanup download=%s deleteData=%v", downloadTaskID, deleteData)
				}
				return nil
			})
			ratio, seeded, uploaded := 1.5, int64(7200), int64(1024)
			immediate := test.mode == models.MediaLibraryTransferMove || test.cloud
			remote := download
			remote.ExecutionLocation = models.NodeLocationRemote
			disposition, err := downloadSourceDisposition(remote)
			if err != nil || disposition.immediate() != immediate || disposition.allowsWholePackageDelete(remote) != (test.mode == models.MediaLibraryTransferMove || test.mode == models.MediaLibraryTransferCopy && !test.cloud) {
				t.Fatalf("Node placement changed logical policy: %+v err=%v", disposition, err)
			}
			if immediate {
				ratio, seeded = 0, 0 // Even explicit global thresholds do not apply.
			}
			client.seedTask = &downloadpkg.Task{ID: "provider-hash", Status: "uploading", Ratio: &ratio, SeededSeconds: &seeded, UploadedBytes: &uploaded, Seeding: true}
			if err := service.Enqueue(download); err != nil {
				t.Fatal(err)
			}
			if test.cloud && test.mode == models.MediaLibraryTransferCopy {
				// A legacy remote Copy row used to permit whole-package deletion.
				if err := queue.db.Model(&models.SeedingTask{}).Where("download_task_id = ?", download.ID).Update("delete_data", true).Error; err != nil {
					t.Fatal(err)
				}
			}
			claimed, err := queue.Claim([]string{"seeding"})
			if err != nil || claimed == nil {
				t.Fatalf("claim=%v err=%v", claimed, err)
			}
			result := NewSeedingWorker(service).Run(context.Background(), workerRuntime{queue: queue, job: *claimed}, *claimed)
			if result.ErrorCode != "" || result.RetryAt != nil {
				t.Fatalf("result=%+v", result)
			}
			if !client.cancelled || client.deleteData != test.deleteData || client.getCalls == 0 {
				t.Fatalf("cancelled=%v deleteData=%v", client.cancelled, client.deleteData)
			}
			if cleanupCalls != 1 {
				t.Fatalf("staging cleanup calls=%d", cleanupCalls)
			}
			var task models.SeedingTask
			if err := queue.db.Where("download_task_id = ?", download.ID).First(&task).Error; err != nil {
				t.Fatal(err)
			}
			if task.Phase != models.SeedingTaskStatusCompleted || task.Ratio == nil || *task.Ratio != ratio {
				t.Fatalf("task=%+v", task)
			}
			items, err := service.List(actor, 10)
			if err != nil || len(items) != 1 || (items[0].Purpose == sourcePurposeCleanup) != immediate {
				t.Fatalf("purpose projection=%+v err=%v", items, err)
			}
			job, err := queue.Get(actor, task.JobID)
			if err != nil || (job.Purpose == sourcePurposeCleanup) != immediate || immediate && !strings.HasPrefix(job.DisplayName, "源文件清理：") {
				t.Fatalf("job projection=%+v err=%v", job, err)
			}
			entries := remainingBusinessEntries(t, logManager)
			planned := requireRemainingBusinessEvent(t, entries, "seeding.cleanup.start", "started")
			accepted := requireRemainingBusinessEvent(t, entries, "seeding.provider.accepted", "accepted")
			if !strings.HasPrefix(planned.Fields["action"].(string), "prepare_") || strings.HasPrefix(accepted.Fields["action"].(string), "prepare_") {
				t.Fatal("planned deletion was presented as an accepted action", planned, accepted)
			}
			finished := requireRemainingBusinessEvent(t, entries, "seeding.finish", "finished")
			if finished.Fields["mode"] != test.mode || finished.Fields["downloader_name"] != task.DownloaderName {
				t.Fatalf("cleanup narrative lost actual mode/downloader: %+v", finished)
			}
		})
	}
}

func TestSeedingCopyAvoidsWholePackageDeletionWhenProtectedFilesRemain(t *testing.T) {
	service, queue, _, client, download := seedingFixture(t, models.MediaLibraryTransferCopy, true)
	var transfer models.TransferTask
	if err := queue.db.Where("download_task_id = ?", download.ID).First(&transfer).Error; err != nil {
		t.Fatal(err)
	}
	selected := downloadpkg.Manifest{Complete: true, Files: []downloadpkg.File{{RelativePath: "Movie.mkv", Size: minimumAutomaticTransferVideoBytes}}}
	source := selected
	source.Files = append(append([]downloadpkg.File{}, selected.Files...),
		downloadpkg.File{RelativePath: "Possible.Real.Movie.mp4", Size: minimumAutomaticTransferVideoBytes},
		downloadpkg.File{RelativePath: "unmatched.zh-CN.srt", Size: 128},
		downloadpkg.File{RelativePath: "tracker.txt", Size: 32},
	)
	selectedJSON, _ := json.Marshal(selected)
	sourceJSON, _ := json.Marshal(source)
	if err := queue.db.Model(&transfer).Updates(map[string]any{"manifest_json": string(selectedJSON), "source_manifest_json": string(sourceJSON)}).Error; err != nil {
		t.Fatal(err)
	}
	cleanupCalls := 0
	service.SetStagingCleanup(func(_ context.Context, downloadTaskID string, deleteData bool) error {
		cleanupCalls++
		if downloadTaskID != download.ID || deleteData {
			t.Fatalf("unsafe staging cleanup download=%s deleteData=%v", downloadTaskID, deleteData)
		}
		return nil
	})
	ratio, seeded := 2.0, int64(7200)
	client.seedTask = &downloadpkg.Task{ID: "provider-hash", Ratio: &ratio, SeededSeconds: &seeded, Seeding: true}
	if err := service.Enqueue(download); err != nil {
		t.Fatal(err)
	}
	var task models.SeedingTask
	if err := queue.db.Where("download_task_id = ?", download.ID).First(&task).Error; err != nil {
		t.Fatal(err)
	}
	if task.DeleteData {
		t.Fatal("copy seeding task allowed whole-package deletion with protected leftovers")
	}
	// Simulate a task persisted by an older Server version. The destructive
	// boundary must still downgrade it before contacting qBittorrent.
	if err := queue.db.Model(&task).Update("delete_data", true).Error; err != nil {
		t.Fatal(err)
	}
	claimed, err := queue.Claim([]string{"seeding"})
	if err != nil || claimed == nil {
		t.Fatalf("claim=%v err=%v", claimed, err)
	}
	result := NewSeedingWorker(service).Run(context.Background(), workerRuntime{queue: queue, job: *claimed}, *claimed)
	if result.ErrorCode != "" || result.RetryAt != nil {
		t.Fatalf("result=%+v", result)
	}
	if !client.cancelled || client.deleteData || cleanupCalls != 1 {
		t.Fatalf("cancelled=%v deleteData=%v cleanupCalls=%d", client.cancelled, client.deleteData, cleanupCalls)
	}
}

func TestSeedingDisabledRetainsProviderAndMoveRemovesOnlyTask(t *testing.T) {
	service, queue, actor, client, download := seedingFixture(t, models.MediaLibraryTransferCopy, false)
	ratio, seeded := .5, int64(300)
	client.seedTask = &downloadpkg.Task{ID: "provider-hash", Ratio: &ratio, SeededSeconds: &seeded, Seeding: true}
	if err := service.Enqueue(download); err != nil {
		t.Fatal(err)
	}
	claimed, err := queue.Claim([]string{"seeding"})
	if err != nil || claimed == nil {
		t.Fatalf("claim=%v err=%v", claimed, err)
	}
	result := NewSeedingWorker(service).Run(context.Background(), workerRuntime{queue: queue, job: *claimed}, *claimed)
	if result.ErrorCode != "" || result.RetryAt == nil || client.cancelled {
		t.Fatalf("result=%+v cancelled=%v", result, client.cancelled)
	}
	var retained models.SeedingTask
	if err := queue.db.Where("download_task_id = ?", download.ID).First(&retained).Error; err != nil {
		t.Fatal(err)
	}
	if retained.Phase != models.SeedingTaskStatusRetained {
		t.Fatalf("phase=%s", retained.Phase)
	}
	if err := queue.db.Model(&models.Job{}).Where("id = ?", download.JobID).Updates(map[string]any{"status": models.JobStatusCompleted, "finished_at": time.Now().UTC()}).Error; err != nil {
		t.Fatal(err)
	}
	if err := service.downloaders.Delete(actor, *download.DownloaderID, RequestContext{}); ErrorCode(err) != CodeDownloaderInUse {
		t.Fatalf("delete active seeding downloader err=%v", err)
	}

	moveService, moveQueue, moveActor, moveClient, moved := seedingFixture(t, models.MediaLibraryTransferMove, true)
	moveClient.getErr = downloadpkg.Error("downloader_auth_failed", false, nil)
	logManager, logger := captureRemainingBusiness(t, "source-cleanup-retry")
	moveService.log = logger
	if err := moveService.AfterTransfer(context.Background(), moved); err != nil {
		t.Fatal(err)
	}
	var moveTask models.SeedingTask
	if err := moveQueue.db.First(&moveTask, "download_task_id = ?", moved.ID).Error; err != nil {
		t.Fatal(err)
	}
	// Exercise an old failed Job through normal retry. It remains the original
	// cleanup identity, and its original acquisition is never replayed.
	if err := moveQueue.db.Model(&models.Job{}).Where("id = ?", moveTask.JobID).Updates(map[string]any{"display_name": "做种：" + moved.DisplayName, "status": models.JobStatusFailed}).Error; err != nil {
		t.Fatal(err)
	}
	projected, err := moveQueue.Get(moveActor, moveTask.JobID)
	if err != nil || projected.Purpose != sourcePurposeCleanup || projected.DisplayName != "源文件清理："+moved.DisplayName {
		t.Fatalf("legacy job=%+v err=%v", projected, err)
	}
	if _, err := moveQueue.Control(moveActor, moveTask.JobID, "retry", RequestContext{}); err != nil {
		t.Fatal(err)
	}
	moveJob, err := moveQueue.Claim([]string{"seeding"})
	if err != nil || moveJob == nil {
		t.Fatalf("claim=%+v err=%v", moveJob, err)
	}
	worker := NewSeedingWorker(moveService)
	if err := moveQueue.db.Model(&models.TransferTask{}).Where("download_task_id = ?", moved.ID).Update("catalog_published_at", nil).Error; err != nil {
		t.Fatal(err)
	}
	blocked := worker.Run(context.Background(), workerRuntime{queue: moveQueue, job: *moveJob}, *moveJob)
	if blocked.ErrorCode != "source_cleanup_import_pending" || blocked.RetryAt == nil || moveClient.getCalls != 0 || moveClient.cancelled {
		t.Fatalf("unpublished import authorized native cleanup: %+v", blocked)
	}
	if err := moveQueue.db.Model(&models.TransferTask{}).Where("download_task_id = ?", moved.ID).Update("catalog_published_at", time.Now().UTC()).Error; err != nil {
		t.Fatal(err)
	}
	failed := worker.Run(context.Background(), workerRuntime{queue: moveQueue, job: *moveJob}, *moveJob)
	if failed.ErrorCode != "downloader_auth_failed" || failed.RetryAt != nil || moveClient.cancelled || moveClient.submits != 0 {
		t.Fatalf("auth failure sent cleanup or replayed acquisition: %+v client=%+v", failed, moveClient)
	}
	failureLog := requireRemainingBusinessEvent(t, remainingBusinessEntries(t, logManager), "seeding.failed", "failed")
	if failureLog.Fields["action"] != "inspect_task" || failureLog.Fields["stage"] != "cleanup" {
		t.Fatalf("failed read claimed source deletion: %+v", failureLog)
	}
	if err := moveQueue.Fail(moveJob.Job.ID, moveJob.LeaseToken, failed.ErrorCode, failed.ErrorMessage); err != nil {
		t.Fatal(err)
	}
	moveClient.getErr = nil
	if _, err := moveQueue.Control(moveActor, moveTask.JobID, "retry", RequestContext{}); err != nil {
		t.Fatal(err)
	}
	moveJob, err = moveQueue.Claim([]string{"seeding"})
	if err != nil || moveJob == nil {
		t.Fatal(err)
	}
	result = worker.Run(context.Background(), workerRuntime{queue: moveQueue, job: *moveJob}, *moveJob)
	if result.ErrorCode != "" || result.RetryAt != nil || !moveClient.cancelled || moveClient.deleteData || moveClient.submits != 0 {
		t.Fatalf("move retry=%+v cancelled=%v deleteData=%v submits=%d", result, moveClient.cancelled, moveClient.deleteData, moveClient.submits)
	}
	var count int64
	if err := moveQueue.db.Model(&models.SeedingTask{}).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("count=%d err=%v", count, err)
	}
	missingDownloader := "removed-downloader"
	moved.DownloaderID = &missingDownloader
	if err := moveService.AfterTransfer(context.Background(), moved); err != nil {
		t.Fatalf("completed move was blocked by removed downloader config: %v", err)
	}
}

func TestSeedingProviderTaskNotFoundCompletesIdempotently(t *testing.T) {
	service, queue, _, client, download := seedingFixture(t, models.MediaLibraryTransferCopy, true)
	client.getErr = downloadpkg.Error("downloader_task_not_found", false, errors.New("gone"))
	if err := service.Enqueue(download); err != nil {
		t.Fatal(err)
	}
	claimed, err := queue.Claim([]string{"seeding"})
	if err != nil || claimed == nil {
		t.Fatalf("claim=%v err=%v", claimed, err)
	}
	result := NewSeedingWorker(service).Run(context.Background(), workerRuntime{queue: queue, job: *claimed}, *claimed)
	if result.ErrorCode != "" {
		t.Fatalf("result=%+v", result)
	}
	var task models.SeedingTask
	if err := queue.db.Where("download_task_id = ?", download.ID).First(&task).Error; err != nil {
		t.Fatal(err)
	}
	if task.Phase != models.SeedingTaskStatusCompleted {
		t.Fatalf("phase=%s", task.Phase)
	}
	for _, action := range []string{"stop", "interrupt"} {
		t.Run(action, func(t *testing.T) {
			service, queue, actor, client, download := seedingFixture(t, models.MediaLibraryTransferMove, true)
			client.getErr = downloadpkg.Error("downloader_task_not_found", false, nil)
			cleanupCalls := 0
			service.SetStagingCleanup(func(ctx context.Context, _ string, _ bool) error {
				cleanupCalls++
				if missing, _ := ctx.Value(sourceNativeTaskMissingKey{}).(bool); !missing {
					t.Fatal("native task absence was not passed to source cleanup")
				}
				return nil
			})
			if err := service.Enqueue(download); err != nil {
				t.Fatal(err)
			}
			var task models.SeedingTask
			if err := queue.db.First(&task, "download_task_id = ?", download.ID).Error; err != nil {
				t.Fatal(err)
			}
			if action == "stop" {
				if _, err := service.Stop(context.Background(), actor, task.ID, RequestContext{}); err != nil {
					t.Fatal(err)
				}
			} else {
				claimed, err := queue.Claim([]string{"seeding"})
				if err != nil || claimed == nil {
					t.Fatal(err)
				}
				if err := NewSeedingWorker(service).Interrupt(context.Background(), *claimed, "cancel"); err != nil {
					t.Fatal(err)
				}
			}
			if client.cancelled || cleanupCalls != 1 {
				t.Fatalf("missing native task sent a delete request: cancelled=%v cleanup=%d", client.cancelled, cleanupCalls)
			}
		})
	}
}

func TestQueuedSeedingCancellationReconcilesProviderBeforeAcknowledgement(t *testing.T) {
	service, queue, actor, client, download := seedingFixture(t, models.MediaLibraryTransferCopy, true)
	cleanupCalls := 0
	service.SetStagingCleanup(func(_ context.Context, downloadTaskID string, deleteData bool) error {
		cleanupCalls++
		if downloadTaskID != download.ID || !deleteData {
			t.Fatalf("staging cleanup download=%s deleteData=%v", downloadTaskID, deleteData)
		}
		return nil
	})
	if err := service.Enqueue(download); err != nil {
		t.Fatal(err)
	}
	var task models.SeedingTask
	if err := queue.db.Where("download_task_id = ?", download.ID).First(&task).Error; err != nil {
		t.Fatal(err)
	}
	controlled, err := queue.Control(actor, task.JobID, "cancel", RequestContext{})
	if err != nil || controlled.Status != models.JobStatusQueued || controlled.InterruptPending != models.JobStatusCancelled {
		t.Fatalf("controlled=%+v err=%v", controlled, err)
	}
	claimed, err := queue.Claim([]string{"seeding"})
	if err != nil || claimed == nil {
		t.Fatalf("claim=%v err=%v", claimed, err)
	}
	worker := NewSeedingWorker(service)
	if err := worker.Interrupt(context.Background(), *claimed, "cancel"); err != nil {
		t.Fatal(err)
	}
	if !client.cancelled || !client.deleteData {
		t.Fatalf("provider cleanup cancelled=%v deleteData=%v", client.cancelled, client.deleteData)
	}
	if cleanupCalls != 1 {
		t.Fatalf("staging cleanup calls=%d", cleanupCalls)
	}
	if err := queue.AcknowledgeInterrupt(claimed.Job.ID, claimed.LeaseToken); err != nil {
		t.Fatal(err)
	}
	var job models.Job
	if err := queue.db.First(&job, "id = ?", task.JobID).Error; err != nil {
		t.Fatal(err)
	}
	if err := queue.db.First(&task, "id = ?", task.ID).Error; err != nil {
		t.Fatal(err)
	}
	if job.Status != models.JobStatusCancelled || task.Phase != models.SeedingTaskStatusCompleted {
		t.Fatalf("job=%s task=%s", job.Status, task.Phase)
	}
}
