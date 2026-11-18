package services

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"gorm.io/gorm"
)

func downloadAcquisitionKey(task models.DownloadTask) string {
	if task.BTSelectionGroupID != "" {
		return "bt:" + task.BTSelectionGroupID
	}
	return "task:" + task.ID
}

// Migration adopts possibly live work conservatively, including failed tracking
// whose Job will never be claimed again. This maintenance only observes the
// original operation and never resumes a paused Job or submits/reapplies files.
func (w *DownloadWorker) reconcileDownloadAdmissions(ctx context.Context) error {
	if err := w.releaseDormantDownloadAdmissions(ctx); err != nil {
		return err
	}
	var tasks []models.DownloadTask
	if err := w.service.db.WithContext(ctx).Table("download_tasks AS t").Select("t.*").Joins("JOIN download_admissions a ON a.task_id = t.id AND a.state = 'held'").Joins("JOIN jobs j ON j.id = t.job_id").Where("j.status IN ? AND t.provider_task_id <> ''", []string{models.JobStatusFailed, models.JobStatusPaused, models.JobStatusCancelled, models.JobStatusCompleted}).Order("a.updated_at,a.acquisition_key").Limit(4).Find(&tasks).Error; err != nil {
		return err
	}
	for _, task := range tasks {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		call, cancel := context.WithTimeout(ctx, 2*time.Second)
		call = context.WithValue(call, readOnlyDownloadObservationKey{}, true)
		client, err := w.service.downloader.clientForDownloadTask(call, task)
		savePath := task.StagingAbsolutePath
		var source downloadSourceEnvelope
		if err == nil {
			var plain string
			plain, err = w.service.credentials.Decrypt(downloadSourcePurpose(task.ID), task.SourceCiphertext)
			if err == nil {
				err = json.Unmarshal([]byte(plain), &source)
			}
		}
		var provider downloadpkg.Task
		if err == nil {
			if inspector, ok := client.(interface {
				inspectForCleanup(context.Context, string) (downloadpkg.Task, error)
			}); ok {
				provider, err = inspector.inspectForCleanup(call, task.ProviderTaskID)
			} else if normalizeExecutionLocation(task.ExecutionLocation) == models.NodeLocationRemote {
				if resolver, ok := client.(downloadpkg.SubmissionReconciler); ok {
					provider, err = resolver.ReconcileSubmission(call, downloadpkg.SubmitRequest{Source: offlineSource(source, task), SavePath: savePath, Tag: task.ProviderTag, ProviderDirectoryID: task.StagingProviderDirectoryID})
				} else {
					err = downloadpkg.Error("downloader_observation_unsupported", true, nil)
				}
			} else if downloadpkg.ResolveOfflineOperation(client, offlineSource(source, task)).Kind == downloadpkg.OfflineOperationCloud {
				provider, err = downloadpkg.ObserveOffline(call, client, task.ProviderTaskID)
			} else {
				provider, err = client.Get(call, task.ProviderTaskID)
			}
		}
		settled := false
		if err == nil {
			settled = provider.Completed || provider.Failed
			if downloadpkg.ResolveOfflineOperation(client, offlineSource(source, task)).Kind == downloadpkg.OfflineOperationCloud {
				settled = provider.Completed && task.OfflineKind == "" // legacy complete transport includes native completion
				if provider.Offline != nil {
					settled = provider.Offline.TerminalFailure || provider.Offline.UpstreamStopped && !provider.Offline.StopPending
				}
			}
		} else {
			settled = downloadpkg.DefiniteSubmissionNonAcceptance(err) || downloadpkg.OfflineTerminalFailure(err)
			// A missing remote Operation is not proof that its native task exited.
			if normalizeExecutionLocation(task.ExecutionLocation) != models.NodeLocationRemote && isEstablishedProviderTask(task.ProviderTaskID) && providerTaskMissing(err) {
				settled = true
			}
		}
		cancel()
		if err := w.service.db.Transaction(func(tx *gorm.DB) error {
			var current models.DownloadTask
			if err := tx.First(&current, "id = ? AND job_id = ?", task.ID, task.JobID).Error; err != nil {
				return err
			}
			if current.ProviderTaskID != task.ProviderTaskID || current.SourceCiphertext != task.SourceCiphertext || current.OfflineAccountNamespace != task.OfflineAccountNamespace {
				return errDownloadIdentityChanged
			}
			now := w.service.queue.clock.Now()
			if settled {
				// An active timeout-control still owns settlement of its stop intent.
				if current.OfflineStopPending {
					return nil
				}
				return releaseDownloadAdmissionTx(tx, current, now)
			}
			return tx.Model(&models.DownloadAdmission{}).Where("acquisition_key = ? AND state = 'held'", downloadAcquisitionKey(current)).Update("updated_at", now).Error
		}); err != nil {
			return err
		}
	}
	return nil
}

// A cancelled control or terminal legacy job can retain its unconfirmed receipt
// without counting as an active acquisition. Recheck these conditions under
// the writer transaction after checking the original source or stop intent.
func dormantDownloadAdmissions(db *gorm.DB) *gorm.DB {
	return db.Table("download_tasks AS t").
		Joins("JOIN download_admissions a ON a.acquisition_key = 'task:' || t.id AND a.task_id = t.id AND a.downloader_id = t.downloader_id AND a.state = 'held'").
		Joins("JOIN jobs j ON j.id = t.job_id AND j.owner_id = t.owner_id AND j.job_type = 'download'").
		Where("j.status <> ? AND j.lease_token_hash = '' AND j.lease_expires_at IS NULL AND j.interrupt_status = ''", models.JobStatusRunning).
		Where("t.bt_selection_group_id = '' AND t.provider_task_id = '' AND t.provider_output_id = '' AND t.last_sampled_at IS NULL").
		Where("t.completed_manifest_json IN ('','{}') AND t.offline_ready_at IS NULL AND t.offline_settled_at IS NULL").
		Where(`((t.offline_kind = ? AND t.phase = ? AND t.last_error_code = ? AND t.offline_stop_pending = 1 AND t.offline_start_provenance NOT IN ?) OR
 (t.offline_kind IN ? AND t.provider_type = ? AND t.execution_location IN ? AND t.offline_started_at IS NULL AND t.offline_deadline_at IS NULL AND t.phase IN ? AND j.status IN ?))`,
			string(downloadpkg.OfflineOperationCloud), models.DownloadTaskStatusCancelled, CodeOfflineDeadlineExceeded, []string{downloadpkg.OfflineStartAccepted, downloadpkg.OfflineStartProviderAdded},
			[]string{"", string(downloadpkg.OfflineOperationCloud)}, models.DownloaderTypePan115Offline, []string{"", models.NodeLocationServer}, []string{models.DownloadTaskStatusSubmitting, models.DownloadTaskStatusFailed, models.DownloadTaskStatusCancelled}, []string{models.JobStatusFailed, models.JobStatusCancelled})
}

func (w *DownloadWorker) releaseDormantDownloadAdmissions(ctx context.Context) error {
	var candidates []struct {
		models.DownloadTask
		JobPayload string
	}
	db := w.service.db.WithContext(ctx)
	if err := dormantDownloadAdmissions(db).Select("t.*, j.payload_json AS job_payload").Order("a.updated_at,a.acquisition_key").Limit(16).Find(&candidates).Error; err != nil {
		return err
	}
	for _, candidate := range candidates {
		task := candidate.DownloadTask
		var payload downloadJobPayload
		eligible := json.Unmarshal([]byte(candidate.JobPayload), &payload) == nil && payload.DownloadTaskID == task.ID
		if eligible {
			if task.OfflineStopPending && task.LastErrorCode == CodeOfflineDeadlineExceeded {
				eligible = payload.ControlIntent == "cancel" || payload.ControlIntent == "remove"
			} else {
				eligible = w.isStoredNative115Source(task)
			}
		}
		released := false
		if err := db.Transaction(func(tx *gorm.DB) error {
			var matches int64
			if err := dormantDownloadAdmissions(tx).Where("t.id = ? AND t.job_id = ? AND t.source_ciphertext = ? AND j.payload_json = ?", task.ID, task.JobID, task.SourceCiphertext, candidate.JobPayload).Count(&matches).Error; err != nil || matches != 1 {
				return err
			}
			now := w.service.queue.clock.Now()
			if eligible {
				released = true
				return releaseDownloadCapacityTx(tx, task, now)
			}
			// Keep unsupported or unreadable historical records from starving
			// later candidates in the bounded maintenance scan.
			return tx.Model(&models.DownloadAdmission{}).Where("acquisition_key = ?", downloadAcquisitionKey(task)).Update("updated_at", now).Error
		}); err != nil {
			return err
		}
		if released {
			w.service.queue.wake()
		}
	}
	return nil
}

func (w *DownloadWorker) isStoredNative115Source(task models.DownloadTask) bool {
	plain, err := w.service.credentials.Decrypt(downloadSourcePurpose(task.ID), task.SourceCiphertext)
	if err != nil {
		return false
	}
	var source downloadSourceEnvelope
	if json.Unmarshal([]byte(plain), &source) != nil || validateStoredContentSelection(source) != nil {
		return false
	}
	if source.Kind == downloadpkg.SourceURL {
		_, _, err := normalizeDownloadSource(DownloadSourceInput{Kind: source.Kind, URL: source.URL}, task.DisplayName)
		return err == nil
	}
	return source.Kind == downloadpkg.SourceTorrent && source.ContentSelection != nil && len(source.Torrent) > 0
}

// Called inside Claim's writer transaction; external work and lease lifetimes
// cannot race the physical acquisition count. Controls never need a new slot.
type downloadAdmissionCapacity struct {
	Limit   int
	Held    map[string]bool
	Missing bool
}
type downloadClaimAdmissionCache map[string]downloadAdmissionCapacity

func admitDownloadJobTx(tx *gorm.DB, job models.Job, now time.Time, cache downloadClaimAdmissionCache) (bool, error) {
	if job.JobType != "download" {
		return true, nil
	}
	var payload downloadJobPayload
	if json.Unmarshal([]byte(job.PayloadJSON), &payload) != nil || payload.DownloadTaskID == "" {
		return true, nil
	}
	var candidate struct {
		models.DownloadTask
		HasCompletedManifest bool
	}
	// Selection/admission decisions need no source ciphertext, original torrent
	// bytes, identity JSON or full completed manifests inside this writer scan.
	columns := "id,job_id,owner_id,downloader_id,provider_type,bt_selection_group_id,provider_task_id,phase,offline_stop_pending,offline_settled_at,offline_ready_at,scrape_status,target_library_id,last_error_code,CASE WHEN completed_manifest_json <> '' AND completed_manifest_json <> '{}' THEN 1 ELSE 0 END AS has_completed_manifest"
	if err := tx.Model(&models.DownloadTask{}).Select(columns).First(&candidate, "id = ? AND job_id = ?", payload.DownloadTaskID, job.ID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return true, nil
		}
		return false, err
	}
	task := candidate.DownloadTask
	completedRecovery := task.TargetLibraryID != nil && (task.ProviderTaskID != "" || candidate.HasCompletedManifest) && (task.ScrapeStatus == "completed_unverified" || task.ScrapeStatus == "completed_unrecognized" || task.ScrapeStatus == "completed_verified")
	if task.DownloaderID == nil || payload.ControlIntent != "" || task.OfflineStopPending || task.OfflineSettledAt != nil || task.Phase == models.DownloadTaskStatusCancelled || task.Phase == models.DownloadTaskStatusCompleted || task.OfflineReadyAt != nil || completedRecovery {
		return true, nil
	}
	snapshot, loaded := cache[*task.DownloaderID]
	if !loaded {
		var downloader models.Downloader
		if err := tx.Select("id", "max_concurrent_downloads").First(&downloader, "id = ?", *task.DownloaderID).Error; err != nil {
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				return false, err
			}
			snapshot.Missing = true
		} else {
			snapshot.Limit = downloader.MaxConcurrentDownloads
			if snapshot.Limit < 1 {
				snapshot.Limit = 2
			}
			var keys []string
			if err := tx.Model(&models.DownloadAdmission{}).Where("downloader_id = ? AND state = 'held'", downloader.ID).Pluck("acquisition_key", &keys).Error; err != nil {
				return false, err
			}
			snapshot.Held = make(map[string]bool, len(keys))
			for _, key := range keys {
				snapshot.Held[key] = true
			}
		}
		cache[*task.DownloaderID] = snapshot
	}
	if snapshot.Missing {
		return true, nil
	}
	key := downloadAcquisitionKey(task)
	if snapshot.Held[key] {
		return true, nil
	}
	existingWork := task.ProviderTaskID != "" || task.Phase == models.DownloadTaskStatusSubmitting || unconfirmedDownloadCode(task.LastErrorCode)
	if !existingWork && len(snapshot.Held) >= snapshot.Limit {
		return false, nil
	}
	var admission models.DownloadAdmission
	err := tx.First(&admission, "acquisition_key = ?", key).Error
	if err == nil {
		if admission.DownloaderID != *task.DownloaderID {
			return false, errDownloadIdentityChanged
		}
		// An explicit retry of a reclaimed legacy reservation must wait for
		// room unless it already has a confirmed provider task.
		if admission.State == "released" && task.ProviderType == models.DownloaderTypePan115Offline && task.BTSelectionGroupID == "" && task.ProviderTaskID == "" && len(snapshot.Held) >= snapshot.Limit {
			return false, nil
		}
		return true, tx.Model(&admission).Updates(map[string]any{"state": "held", "released_at": nil, "updated_at": now}).Error
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return false, err
	}
	admission = models.DownloadAdmission{AcquisitionKey: key, DownloaderID: *task.DownloaderID, TaskID: task.ID, State: "held", CreatedAt: now, UpdatedAt: now}
	return true, tx.Create(&admission).Error
}

// A shared BT group holds one unit until every live reference has established
// acquisition completion or has been authoritatively released/stopped.
func releaseDownloadAdmissionTx(tx *gorm.DB, task models.DownloadTask, now time.Time) error {
	if task.OfflineKind != "" && !task.OfflineStopPending {
		if err := tx.Model(&models.DownloadTask{}).Where("id = ? AND offline_settled_at IS NULL", task.ID).Update("offline_settled_at", now).Error; err != nil {
			return err
		}
	}
	if task.BTSelectionGroupID != "" {
		var group models.BTSelectionGroup
		if err := tx.First(&group, "id = ?", task.BTSelectionGroupID).Error; err != nil {
			return err
		}
		if group.PendingDigest != "" {
			return nil
		}
		var refs int64
		if err := tx.Table("bt_selection_refs AS r").Joins("JOIN download_tasks AS t ON t.id = r.task_id").Where("r.group_id = ? AND r.task_id <> ? AND t.phase <> ? AND (r.state NOT IN ('released','retired') OR t.offline_stop_pending = 1 OR t.last_error_code IN ?) AND (t.phase <> ? OR t.offline_stop_pending = 1 OR t.last_error_code IN ?) AND t.offline_ready_at IS NULL", task.BTSelectionGroupID, task.ID, models.DownloadTaskStatusCompleted, []string{downloadCancellationPending, "downloader_submission_unconfirmed", "downloader_submit_unknown", "downloader_selection_result_unknown", "downloader_share_result_unknown"}, models.DownloadTaskStatusCancelled, []string{downloadCancellationPending, "downloader_submission_unconfirmed", "downloader_submit_unknown", "downloader_selection_result_unknown", "downloader_share_result_unknown"}).Count(&refs).Error; err != nil {
			return err
		}
		if refs > 0 {
			return nil
		}
	}
	return releaseDownloadCapacityTx(tx, task, now)
}

// Quota release alone must not claim that an ambiguous operation settled.
func releaseDownloadCapacityTx(tx *gorm.DB, task models.DownloadTask, now time.Time) error {
	return tx.Model(&models.DownloadAdmission{}).Where("acquisition_key = ? AND state = 'held'", downloadAcquisitionKey(task)).Updates(map[string]any{"state": "released", "released_at": now, "updated_at": now}).Error
}

func restoreDownloadCapacityTx(tx *gorm.DB, task models.DownloadTask, now time.Time) error {
	if task.DownloaderID == nil || task.BTSelectionGroupID != "" || task.OfflineKind != string(downloadpkg.OfflineOperationCloud) && task.ProviderType != models.DownloaderTypePan115Offline {
		return nil
	}
	return tx.Model(&models.DownloadAdmission{}).
		Where("acquisition_key = ? AND task_id = ? AND downloader_id = ? AND state = 'released'", downloadAcquisitionKey(task), task.ID, *task.DownloaderID).
		Updates(map[string]any{"state": "held", "released_at": nil, "updated_at": now}).Error
}

func (s *DownloadService) releaseDownloadAdmission(task models.DownloadTask) error {
	return s.db.Transaction(func(tx *gorm.DB) error { return releaseDownloadAdmissionTx(tx, task, s.queue.clock.Now()) })
}

func downloadCapacityBlocked(tx *gorm.DB, task models.DownloadTask) (bool, error) {
	if task.DownloaderID == nil || task.ProviderTaskID != "" || task.Phase != models.DownloadTaskStatusQueued {
		return false, nil
	}
	var admission models.DownloadAdmission
	err := tx.First(&admission, "acquisition_key = ? AND state = 'held'", downloadAcquisitionKey(task)).Error
	if err == nil {
		return false, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return false, err
	}
	var downloader models.Downloader
	if err := tx.Select("id", "max_concurrent_downloads").First(&downloader, "id = ?", *task.DownloaderID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, nil
		}
		return false, err
	}
	limit := downloader.MaxConcurrentDownloads
	if limit < 1 {
		limit = 2
	}
	var held int64
	err = tx.Model(&models.DownloadAdmission{}).Where("downloader_id = ? AND state = 'held'", downloader.ID).Count(&held).Error
	return held >= int64(limit), err
}
