package services

import (
	"context"
	"strings"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	cloudpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader/pan115offline"
	"gorm.io/gorm"
)

type expiredShareLifecycleDriver struct {
	*shareIngestDriver
	inspections int
	receives    int
}

func (d *expiredShareLifecycleDriver) CreateDirectory(ctx context.Context, parent, name string) (cloudpkg.Item, error) {
	return d.fakeCloudDriver.CreateDirectory(ctx, parent, name)
}

func (d *expiredShareLifecycleDriver) InspectShare(context.Context, string) (cloudpkg.ShareSnapshot, error) {
	d.inspections++
	return cloudpkg.ShareSnapshot{}, cloudpkg.Error(cloudpkg.CodeShareExpired, false, nil)
}

func (d *expiredShareLifecycleDriver) InspectShareDirectory(ctx context.Context, raw, _ string) (cloudpkg.ShareSnapshot, error) {
	return d.InspectShare(ctx, raw)
}

func (d *expiredShareLifecycleDriver) ReceiveShare(context.Context, cloudpkg.ShareSnapshot, string) error {
	d.receives++
	return cloudpkg.Error(cloudpkg.CodeShareExpired, false, nil)
}

func TestExpiredShareFinishesFirstAttemptAndImmediatelyUnblocksNextDownload(t *testing.T) {
	for _, selected := range []bool{false, true} {
		name := "whole_share"
		if selected {
			name = "selected_files"
		}
		t.Run(name, func(t *testing.T) {
			fixture := newShareIngestFixture(t)
			fixture.driver.createDirectory = true
			driver := &expiredShareLifecycleDriver{shareIngestDriver: fixture.driver}
			registry := downloadpkg.NewRegistry()
			capabilities := downloadpkg.Capabilities{NativeOffline: true, ShareReceive: true, Cancel: true, OutputConstraint: downloadpkg.OutputConstraintProviderStorage}
			if err := registry.Register(models.DownloaderTypePan115Offline, capabilities, func(config downloadpkg.Config) (downloadpkg.Client, error) {
				config.CloudDriver = driver
				return pan115offline.New(config)
			}); err != nil {
				t.Fatal(err)
			}
			fixture.downloads.downloader.registry = registry
			if err := fixture.db.Model(&models.Downloader{}).Where("id = ?", fixture.downloader.ID).Update("max_concurrent_downloads", 1).Error; err != nil {
				t.Fatal(err)
			}
			library := fixture.createLibrary(t, "电影", "library", "intake", "/中转")
			if err := fixture.db.Model(&models.MediaLibrary{}).Where("id = ?", library.ID).Updates(map[string]any{"enabled": true, "baseline_generation": 1, "status": models.MediaLibraryStatusListening}).Error; err != nil {
				t.Fatal(err)
			}
			if err := fixture.db.Model(&models.MediaLibraryStructureAutoState{}).Where("library_id = ?", library.ID).Updates(map[string]any{"diagnosed_revision": 1, "source_revision": 1}).Error; err != nil {
				t.Fatal(err)
			}
			source := DownloadSourceInput{Kind: downloadpkg.SourcePan115Share, URL: "https://115.com/s/expired?password=abcd"}
			if selected {
				source.ShareSelection = &cloudpkg.ShareSelection{Version: 1, Files: []cloudpkg.ShareTreeItem{{ID: "selected-video", RelativePath: "Feature.mkv", Size: 1024}}}
			}
			ctx := context.Background()
			first, err := fixture.downloads.Submit(ctx, fixture.actor, SubmitDownloadInput{DownloaderID: fixture.downloader.ID, MediaLibraryID: &library.ID, DisplayName: "Expired share", Source: source}, RequestContext{})
			if err != nil {
				t.Fatal(err)
			}
			queue := fixture.downloads.queue
			claim, err := queue.Claim([]string{"download"})
			if err != nil || claim == nil || claim.Job.ID != first.JobID {
				t.Fatalf("first claim=%+v err=%v", claim, err)
			}
			second, err := fixture.downloads.Submit(ctx, fixture.actor, SubmitDownloadInput{DownloaderID: fixture.downloader.ID, MediaLibraryID: &library.ID, DisplayName: "Different share", Source: DownloadSourceInput{Kind: downloadpkg.SourcePan115Share, URL: "https://115.com/s/different?password=abcd"}}, RequestContext{})
			if err != nil {
				t.Fatal(err)
			}
			worker := NewDownloadWorker(fixture.downloads)
			result := worker.Run(ctx, workerRuntime{queue: queue, job: *claim}, *claim)
			if result.ErrorCode != "downloader_share_expired" || result.RetryAt != nil || !strings.Contains(result.ErrorMessage, "更换有效分享") || strings.Contains(result.ErrorMessage, "自动重试") {
				t.Fatalf("expired share result=%+v", result)
			}
			if driver.inspections != 1 || driver.receives != 0 || driver.offlineDirectory != "" {
				t.Fatalf("expired share performed writes: inspect=%d receive=%d native=%q", driver.inspections, driver.receives, driver.offlineDirectory)
			}
			if err := queue.Fail(claim.Job.ID, claim.LeaseToken, result.ErrorCode, result.ErrorMessage); err != nil {
				t.Fatal(err)
			}
			var task models.DownloadTask
			if err := fixture.db.First(&task, "id = ?", first.ID).Error; err != nil || task.Phase != models.DownloadTaskStatusFailed || task.FinishedAt == nil || task.ProviderTaskID != "" || task.OfflineStartedAt != nil || task.OfflineSettledAt != nil {
				t.Fatalf("expired task=%+v err=%v", task, err)
			}
			var job models.Job
			if err := fixture.db.First(&job, "id = ?", first.JobID).Error; err != nil || job.Status != models.JobStatusFailed || job.AttemptCount != 1 || job.FailureRetryCount != 0 || job.NextAttemptAt != nil || job.LastErrorMessage != task.LastErrorMessage {
				t.Fatalf("expired job=%+v err=%v", job, err)
			}
			var admission models.DownloadAdmission
			if err := fixture.db.First(&admission, "acquisition_key = ?", downloadAcquisitionKey(task)).Error; err != nil || admission.State != "released" || admission.ReleasedAt == nil {
				t.Fatalf("expired admission=%+v err=%v", admission, err)
			}
			// No maintenance scan, manual retry, cancellation or capacity change.
			next, err := queue.Claim([]string{"download"})
			if err != nil || next == nil || next.Job.ID != second.JobID {
				t.Fatalf("next share still blocked: claim=%+v err=%v", next, err)
			}
		})
	}
}

func TestExpiredShareFailureKeepsEstablishedAcquisitionAdmission(t *testing.T) {
	_, _, queue, actor, _ := downloadFixture(t)
	now := queue.clock.Now()
	provider := models.Downloader{ID: "share-expiry-boundary", OwnerID: actor.User.ID, Name: "115", NameNormalized: "share-expiry-boundary", Type: models.DownloaderTypePan115Offline, Enabled: true}
	if err := queue.db.Create(&provider).Error; err != nil {
		t.Fatal(err)
	}
	for name, modify := range map[string]func(*models.DownloadTask){
		"native_dispatch": func(task *models.DownloadTask) {
			task.OfflineKind = string(downloadpkg.OfflineOperationCloud)
			task.OfflineStartedAt = &now
		},
		"provider_output": func(task *models.DownloadTask) { task.ProviderOutputID = "existing-output" },
		"provider_task":   func(task *models.DownloadTask) { task.ProviderTaskID = "existing-task" },
		"shared_bt":       func(task *models.DownloadTask) { task.BTSelectionGroupID = "existing-group" },
		"completed":       func(task *models.DownloadTask) { task.Phase = models.DownloadTaskStatusCompleted },
		"other_source":    func(task *models.DownloadTask) { task.SourceOrigin = models.DownloadSourceOriginUser },
		"remote_node":     func(task *models.DownloadTask) { task.ExecutionLocation = models.NodeLocationRemote },
	} {
		t.Run(name, func(t *testing.T) {
			task := models.DownloadTask{ID: "expiry-boundary-" + name, OwnerID: actor.User.ID, DownloaderID: &provider.ID, ProviderType: provider.Type, SourceOrigin: models.DownloadSourceOriginShare, Phase: models.DownloadTaskStatusSubmitting}
			modify(&task)
			_, err := queue.EnqueueWith(EnqueueJobInput{OwnerID: actor.User.ID, JobType: "download", DisplayName: name, Payload: downloadJobPayload{DownloadTaskID: task.ID}}, func(tx *gorm.DB, job models.Job) error {
				task.JobID = job.ID
				return tx.Create(&task).Error
			})
			if err != nil {
				t.Fatal(err)
			}
			admission := models.DownloadAdmission{AcquisitionKey: downloadAcquisitionKey(task), DownloaderID: provider.ID, TaskID: task.ID, State: "held", CreatedAt: now, UpdatedAt: now}
			if err := queue.db.Create(&admission).Error; err != nil {
				t.Fatal(err)
			}
			var job models.Job
			if err := queue.db.First(&job, "id = ?", task.JobID).Error; err != nil {
				t.Fatal(err)
			}
			if err := queue.db.Transaction(func(tx *gorm.DB) error {
				return finalizeFailedDownloadTx(tx, job, "downloader_share_expired", downloadFailureMessage("downloader_share_expired", false), now)
			}); err != nil {
				t.Fatal(err)
			}
			if err := queue.db.First(&admission, "acquisition_key = ?", admission.AcquisitionKey).Error; err != nil || admission.State != "held" {
				t.Fatalf("established acquisition released: %+v err=%v", admission, err)
			}
		})
	}
}
