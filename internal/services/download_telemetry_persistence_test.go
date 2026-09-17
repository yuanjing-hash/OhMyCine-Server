package services

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/database"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"gorm.io/gorm"
)

// Obtain an actual typed driver error instead of assuming its message format.
func downloadTestBusyError(t *testing.T) error {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "busy.db")+"?_pragma=busy_timeout(1)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err = db.Exec("CREATE TABLE sample (id INTEGER)"); err != nil {
		t.Fatal(err)
	}
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err = conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = conn.ExecContext(context.Background(), "ROLLBACK") }()
	_, err = db.Exec("INSERT INTO sample VALUES (1)")
	if !database.IsTransientWriteError(err) {
		t.Fatalf("expected typed busy, got %T", err)
	}
	return err
}

func TestDownloadTelemetryPersistenceRecovery(t *testing.T) {
	for _, mode := range []string{"temporary", "persistent", "unconfirmed_identity", "permanent", "cancelled", "lost_lease"} {
		t.Run(mode, func(t *testing.T) {
			downloads, downloaders, queue, actor, client := downloadFixture(t)
			var logs bytes.Buffer
			downloads.log = zerolog.New(&logs)
			check := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			staging := models.Storage{Name: "telemetry", NameNormalized: "telemetry", Type: models.StorageTypeLocal, RootPath: t.TempDir(), Enabled: true, Capabilities: "{}"}
			check(queue.db.Create(&staging).Error)
			configureDownloadStaging(t, queue, staging.ID)
			d, err := downloaders.Create(actor, DownloaderInput{Name: "telemetry", Type: models.DownloaderTypeQBittorrent, BaseURL: "http://qbit.example.test", Enabled: true}, RequestContext{})
			check(err)
			created, err := downloads.Submit(context.Background(), actor, SubmitDownloadInput{DownloaderID: d.ID, DisplayName: "telemetry", Source: DownloadSourceInput{Kind: downloadpkg.SourceURL, URL: "magnet:?xt=urn:btih:telemetry"}}, RequestContext{})
			check(err)
			check(queue.db.Model(&models.DownloadTask{}).Where("id=?", created.ID).Updates(map[string]any{"provider_task_id": "provider-hash", "phase": models.DownloadTaskStatusDownloading}).Error)
			if mode == "unconfirmed_identity" {
				check(queue.db.Model(&models.DownloadTask{}).Where("id=?", created.ID).Update("provider_task_id", "tag:unconfirmed").Error)
			}
			claim, err := queue.Claim([]string{"download"})
			check(err)
			if claim == nil {
				t.Fatal("no claim")
			}
			busy := downloadTestBusyError(t)
			writes := 0
			check(queue.db.Callback().Update().Before("gorm:update").Register("test:telemetry-persist", func(tx *gorm.DB) {
				values, ok := tx.Statement.Dest.(map[string]any)
				if !ok || tx.Statement.Table != "download_tasks" {
					return
				}
				if _, ok = values["last_sampled_at"]; !ok {
					return
				}
				writes++
				if mode == "temporary" && writes <= 2 || mode == "persistent" || mode == "unconfirmed_identity" {
					_ = tx.AddError(busy)
				}
				if mode == "permanent" {
					_ = tx.AddError(errors.New("private database details must not be logged"))
				}
			}))
			t.Cleanup(func() { _ = queue.db.Callback().Update().Remove("test:telemetry-persist") })
			worker := NewDownloadWorker(downloads)
			worker.pollInterval = time.Millisecond
			if mode == "cancelled" || mode == "lost_lease" {
				var task models.DownloadTask
				check(queue.db.First(&task, "id=?", created.ID).Error)
				if mode == "cancelled" {
					check(queue.db.Model(&task).Update("phase", models.DownloadTaskStatusCancelled).Error)
				} else {
					check(queue.db.Model(&models.Job{}).Where("id=?", created.JobID).Update("lease_token_hash", "new-owner").Error)
				}
				err = worker.persistTrackedTelemetry(context.Background(), *claim, &task, downloadpkg.Task{Status: "downloading"})
				if mode == "cancelled" && !errors.Is(err, context.Canceled) || mode == "lost_lease" && ErrorCode(err) != CodeQueueLeaseInvalid {
					t.Fatalf("fence err=%v", err)
				}
				return
			}
			result := worker.Run(context.Background(), workerRuntime{queue: queue, job: *claim}, *claim)
			if mode == "temporary" {
				if result.ErrorCode != "" || result.RetryAt != nil || writes < 3 {
					t.Fatalf("recovery result=%+v writes=%d", result, writes)
				}
			} else {
				wantCode := "download_state_persist_failed"
				if mode == "persistent" {
					wantCode = "download_tracking_persist_wait"
				}
				if result.ErrorCode != wantCode || (result.RetryAt != nil) != (mode == "persistent") {
					t.Fatalf("result=%+v", result)
				}
				if mode == "persistent" {
					if writes != 3 {
						t.Fatalf("unbounded retries=%d", writes)
					}
					var persistedTask models.DownloadTask
					check(queue.db.First(&persistedTask, "id=?", created.ID).Error)
					if persistedTask.Phase != models.DownloadTaskStatusDownloading || persistedTask.ProviderTaskID != "provider-hash" {
						t.Fatal("tracking failure changed download identity/phase")
					}
					check(queue.RetryLater(created.JobID, claim.LeaseToken, result.ErrorCode, result.ErrorMessage, queue.clock.Now().Add(-time.Second)))
					var waitingJob models.Job
					check(queue.db.First(&waitingJob, "id=?", created.JobID).Error)
					if waitingJob.Status != models.JobStatusRetryWait || waitingJob.FailureRetryCount != 0 {
						t.Fatal("tracking wait consumed failure budget")
					}
					check(queue.db.Callback().Update().Remove("test:telemetry-persist"))
					check(queue.PromoteDueRetries())
					retry, err := queue.Claim([]string{"download"})
					check(err)
					if retry == nil {
						t.Fatal("no retry")
					}
					if got := worker.Run(context.Background(), workerRuntime{queue: queue, job: *retry}, *retry); got.ErrorCode != "" {
						t.Fatalf("retry=%+v", got)
					}
				}
			}
			client.mu.Lock()
			submits := client.submits
			client.mu.Unlock()
			if submits != 0 {
				t.Fatalf("tracking resubmitted %d downloads", submits)
			}
			if strings.Contains(logs.String(), "private database details") {
				t.Fatal("raw DB diagnostics leaked")
			}
			if mode == "persistent" && !strings.Contains(logs.String(), `"database_error_class":"busy"`) {
				t.Fatal("safe busy diagnostic missing")
			}
		})
	}
}
