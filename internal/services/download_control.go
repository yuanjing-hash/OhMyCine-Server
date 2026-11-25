package services

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const downloadCancellationPending = "downloader_cancellation_pending"
const downloadCancellationMessage = "已取消后续下载和入库，正在后台核对并收尾原云端操作；源文件保留"

func downloadControlDeletesData(payloadJSON string) bool {
	var payload downloadJobPayload
	return json.Unmarshal([]byte(payloadJSON), &payload) == nil && payload.ControlDeleteData
}

func downloadControlMessage(payloadJSON string) string {
	if downloadControlDeletesData(payloadJSON) {
		return "已取消后续下载和入库，正在清理本任务的源／临时文件"
	}
	return downloadCancellationMessage
}

func unconfirmedDownloadCode(code string) bool {
	switch code {
	case "downloader_submission_unconfirmed", "downloader_submit_unknown", "downloader_selection_result_unknown", "downloader_share_result_unknown":
		return true
	}
	return false
}

func downloadPreparationNeedsControl(task models.DownloadTask) bool {
	return task.LastErrorCode == "downloader_preparation_failed" && downloadPreparationCandidate(task)
}

// Only a backend-created intent paired with its original owner can bypass the
// library intake gate. It grants original-operation cancellation or explicitly
// requested cleanup of its proven preparation workspace.
func pendingDownloadControlTask(db *gorm.DB, job models.Job) (models.DownloadTask, bool, error) {
	var payload downloadJobPayload
	if job.JobType != "download" || json.Unmarshal([]byte(job.PayloadJSON), &payload) != nil || (payload.ControlIntent != "cancel" && payload.ControlIntent != "remove") {
		return models.DownloadTask{}, false, nil
	}
	var task models.DownloadTask
	if err := db.First(&task, "id = ? AND job_id = ?", payload.DownloadTaskID, job.ID).Error; err != nil {
		return task, false, err
	}
	if job.OwnerID == nil || *job.OwnerID != task.OwnerID || task.Phase != models.DownloadTaskStatusCancelled {
		return task, false, errDownloadIdentityChanged
	}
	return task, true, nil
}

func savePendingDownloadControlTx(tx *gorm.DB, task *models.DownloadTask, job *models.Job, remove bool, actorID uint, now time.Time, deleteData ...bool) error {
	var payload downloadJobPayload
	if json.Unmarshal([]byte(job.PayloadJSON), &payload) != nil || payload.DownloadTaskID != task.ID || task.JobID != job.ID || job.OwnerID == nil || *job.OwnerID != task.OwnerID {
		return errDownloadIdentityChanged
	}
	if len(deleteData) > 0 && deleteData[0] {
		payload.ControlDeleteData = true
	}
	payload.ControlIntent = "cancel"
	if remove || task.HistoryClearedAt != nil {
		payload.ControlIntent = "remove"
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	message := downloadControlMessage(string(encoded))
	taskUpdates := map[string]any{"phase": models.DownloadTaskStatusCancelled, "last_error_code": downloadCancellationPending, "last_error_message": message, "finished_at": now, "updated_at": now}
	if task.LastErrorCode == CodeOfflineDeadlineExceeded {
		taskUpdates["last_error_code"], taskUpdates["last_error_message"] = CodeOfflineDeadlineExceeded, offlineTimeoutMessage
	}
	jobUpdates := map[string]any{"payload_json": string(encoded), "last_error_code": downloadCancellationPending, "last_error_message": message, "revision": job.Revision + 1, "updated_at": now}
	// An entered Submit retains its lease until it returns. Its late identity
	// meets the cancelled phase barrier and is handed to the same cleanup.
	if job.Status != models.JobStatusRunning {
		jobUpdates["status"], jobUpdates["next_attempt_at"], jobUpdates["finished_at"] = models.JobStatusQueued, nil, nil
		jobUpdates["interrupt_status"], jobUpdates["cancellation_asked"] = "", false
		releaseLease(jobUpdates)
		job.Status = models.JobStatusQueued
	}
	if payload.ControlIntent == "remove" {
		taskUpdates["history_cleared_at"], jobUpdates["history_cleared_at"] = now, now
		task.HistoryClearedAt, job.HistoryClearedAt = &now, &now
	}
	if err := tx.Model(task).Updates(taskUpdates).Error; err != nil {
		return err
	}
	if err := tx.Model(job).Updates(jobUpdates).Error; err != nil {
		return err
	}
	if err := tx.Model(&models.JobActionRequest{}).Where("job_id = ? AND response = ''", job.ID).Updates(map[string]any{"response": "closed_by_control", "responded_by": actorID, "responded_at": now}).Error; err != nil {
		return err
	}
	job.PayloadJSON, job.LastErrorCode, job.LastErrorMessage = string(encoded), downloadCancellationPending, message
	job.Revision++
	return recordJobEvent(tx, job.ID, "control.cancel_reconcile", "", job.Status, &actorID, downloadCancellationPending, now)
}

func (s *DownloadService) requestPendingDownloadControl(actor Actor, taskID string, remove bool, request RequestContext, deleteData ...bool) error {
	var task models.DownloadTask
	if err := s.db.First(&task, "id = ?", taskID).Error; err != nil {
		return downloadTaskNotFound(err)
	}
	var job models.Job
	err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&job, "id = ?", task.JobID).Error; err != nil {
			return queueNotFound(err)
		}
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&task, "id = ? AND job_id = ?", taskID, job.ID).Error; err != nil {
			return downloadTaskNotFound(err)
		}
		if !actor.Can(authz.PermissionDownloadsManageAll) && !actor.Can(authz.PermissionJobsControlAll) && (task.OwnerID != actor.User.ID || !actor.Can(authz.PermissionJobsControlOwn)) {
			return appError(CodePermissionDenied, "无权控制该下载任务", nil)
		}
		terminalRemoval := remove && (job.Status == models.JobStatusFailed || job.Status == models.JobStatusCancelled || job.Status == models.JobStatusCompleted)
		if !terminalRemoval && !downloadSubmissionUnconfirmed(task) && !downloadPreparationNeedsControl(task) && task.LastErrorCode != downloadCancellationPending && !task.OfflineStopPending && !(len(deleteData) > 0 && deleteData[0] && downloadPreparationCandidate(task)) {
			return errDownloadProviderIdentityChanged
		}
		if err := savePendingDownloadControlTx(tx, &task, &job, remove, actor.User.ID, time.Now().UTC(), deleteData...); err != nil {
			return err
		}
		return s.audit.Record(tx, &actor.User.ID, "download.control_accepted", "download_task", task.ID, "success", map[string]any{"control_pending": true, "record_removed": remove, "provider_data_retained": !downloadControlDeletesData(job.PayloadJSON)}, request)
	})
	if err == nil && s.queue != nil {
		event := "job.status_changed"
		if remove {
			event = "job.deleted"
		}
		s.queue.publish(job, event)
		s.queue.wake()
	}
	return err
}

func (s *DownloadService) ControlPending(taskID string) bool {
	var task models.DownloadTask
	return s.db.Select("phase", "last_error_code", "offline_stop_pending").First(&task, "id = ?", taskID).Error == nil && (task.OfflineStopPending || task.Phase == models.DownloadTaskStatusCancelled && task.LastErrorCode == downloadCancellationPending)
}

func (w *DownloadWorker) controlPendingResult(payloadJSON string) WorkerResult {
	next := time.Now().UTC().Add(time.Minute)
	return WorkerResult{RetryAt: &next, ErrorCode: downloadCancellationPending, ErrorMessage: downloadControlMessage(payloadJSON)}
}

// Empty local evidence only makes the adapter's exact original preparation
// eligible for inspection; the adapter's durable receipt must still prove that
// native acquisition has never dispatched. It is not inferred from task age.
func downloadPreparationCandidate(task models.DownloadTask) bool {
	return task.BTSelectionGroupID == "" && task.ProviderTaskID == "" && task.ProviderOutputID == "" &&
		task.LastSampledAt == nil && task.OfflineStartedAt == nil && task.OfflineDeadlineAt == nil &&
		task.OfflineReadyAt == nil && task.OfflineSettledAt == nil &&
		(task.CompletedManifestJSON == "" || task.CompletedManifestJSON == "{}")
}

func (s *DownloadService) abandonDownloadPreparation(ctx context.Context, task models.DownloadTask, deleteData bool) (bool, error) {
	if !downloadPreparationCandidate(task) {
		return false, nil
	}
	plain, err := s.credentials.Decrypt(downloadSourcePurpose(task.ID), task.SourceCiphertext)
	if err != nil {
		return false, err
	}
	var source downloadSourceEnvelope
	if err := json.Unmarshal([]byte(plain), &source); err != nil {
		return false, err
	}
	if err := validateStoredContentSelection(source); err != nil {
		return false, err
	}
	// Local synchronous share receive cannot leave a long-lived native
	// downloader task. Its original Submit has returned before this is used
	// for settlement; an expired provider credential must not block that fact.
	if !deleteData && source.Kind == downloadpkg.SourcePan115Share && task.ProviderType == models.DownloaderTypePan115Offline && normalizeExecutionLocation(task.ExecutionLocation) == models.NodeLocationServer {
		if _, _, err := normalizeDownloadSource(DownloadSourceInput{Kind: source.Kind, URL: source.URL}, task.DisplayName); err != nil {
			return false, err
		}
		return true, nil
	}
	client, err := s.downloader.clientForDownloadTask(ctx, task)
	if err != nil {
		return false, err
	}
	controller, ok := client.(downloadpkg.PreparationController)
	if !ok {
		return false, nil
	}
	return controller.AbandonPreparation(ctx, downloadpkg.SubmitRequest{
		Source: offlineSource(source, task), SavePath: task.StagingAbsolutePath,
		Tag: task.ProviderTag, ProviderDirectoryID: task.StagingProviderDirectoryID,
	}, deleteData)
}

// Called only from the exclusive cancellation worker, never from admission
// inspection. Retiring the original plan prevents stale Submit replay after
// its resource owner becomes available to a different request.
func (s *DownloadService) releasePreparationOwnership(ctx context.Context, task models.DownloadTask) error {
	plain, err := s.credentials.Decrypt(downloadSourcePurpose(task.ID), task.SourceCiphertext)
	if err != nil {
		return err
	}
	var source downloadSourceEnvelope
	if err := json.Unmarshal([]byte(plain), &source); err != nil {
		return err
	}
	if source.ContentSelection == nil {
		return nil
	}
	client, err := s.downloader.clientForDownloadTask(ctx, task)
	if err != nil {
		return err
	}
	releaser, ok := client.(downloadpkg.PreparationReleaser)
	if !ok {
		return downloadpkg.Error("downloader_action_unsupported", false, nil)
	}
	return releaser.ReleasePreparation(ctx, downloadpkg.SubmitRequest{
		Source: offlineSource(source, task), SavePath: task.StagingAbsolutePath,
		Tag: task.ProviderTag, ProviderDirectoryID: task.StagingProviderDirectoryID,
	})
}

func (w *DownloadWorker) pendingControl(ctx context.Context, claimed ClaimedJob) (*WorkerResult, error) {
	var job models.Job
	if err := w.service.db.First(&job, "id = ?", claimed.Job.ID).Error; err != nil {
		return nil, err
	}
	task, pending, err := pendingDownloadControlTask(w.service.db, job)
	if err != nil || !pending {
		return nil, err
	}
	result := w.controlPendingResult(job.PayloadJSON)
	if job.LeaseTokenHash != leaseHash(claimed.LeaseToken) || job.Status != models.JobStatusRunning || claimed.Job.OwnerID == nil || *claimed.Job.OwnerID != task.OwnerID {
		return &result, nil
	}
	cleanupCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var control downloadJobPayload
	if json.Unmarshal([]byte(job.PayloadJSON), &control) != nil {
		return &result, nil
	}
	if downloadPreparationCandidate(task) {
		prepared, prepErr := w.service.abandonDownloadPreparation(cleanupCtx, task, control.ControlDeleteData)
		if prepErr == nil && prepared {
			if err := w.service.releasePreparationOwnership(cleanupCtx, task); err != nil {
				return &result, nil
			}
			if err := w.service.settlePreparationControl(claimed, task, job.PayloadJSON); err == nil {
				return &WorkerResult{}, nil
			}
			return &result, nil
		}
		if control.ControlDeleteData {
			// The explicit request applies only to the proven original preparation
			// workspace. Never broaden it to a later native task or library output.
			return &result, nil
		}
	}
	if control.ControlDeleteData {
		return &result, nil
	}
	original := claimed
	original.Job = job
	loaded, _, client, source, savePath, err := w.load(cleanupCtx, original)
	if err != nil {
		return &result, nil
	}
	if loaded.ID != task.ID || loaded.JobID != job.ID {
		return &result, nil
	}
	resolver, canResolve := client.(downloadpkg.SubmissionReconciler)
	if canResolve {
		found, err := resolver.ReconcileSubmission(cleanupCtx, downloadpkg.SubmitRequest{Source: source, SavePath: savePath, Tag: task.ProviderTag, ProviderDirectoryID: task.StagingProviderDirectoryID})
		code, _ := downloadpkg.ErrorInfo(err)
		// A native resolver has already checked the account and original
		// receipt before a task-not-found fact can be returned.
		if err != nil && !(task.ProviderTaskID != "" && (providerTaskMissing(err) || code == "downloader_submission_reconcile_unsupported")) {
			return &result, nil
		}
		if err == nil {
			if strings.TrimSpace(found.ID) == "" || task.ProviderTaskID != "" && task.ProviderTaskID != found.ID {
				return &result, nil
			}
			if err := w.recordPendingControlProvider(cleanupCtx, claimed, task, found); err != nil {
				return &result, nil
			}
			task.ProviderTaskID = found.ID
		}
	}
	if task.ProviderTaskID == "" {
		return &result, nil
	}
	if err := client.Cancel(cleanupCtx, task.ProviderTaskID, false); err != nil && !providerTaskMissing(err) {
		return &result, nil
	}
	if err := w.service.settlePendingDownloadControl(task); err != nil {
		return &result, nil
	}
	return &WorkerResult{}, nil
}

func (w *DownloadWorker) recordPendingControlProvider(ctx context.Context, claim ClaimedJob, original models.DownloadTask, found downloadpkg.Task) error {
	return w.service.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		job, err := w.service.queue.verifyLease(tx, claim.Job.ID, claim.LeaseToken)
		if err != nil {
			return err
		}
		task, pending, err := pendingDownloadControlTask(tx, job)
		if err != nil {
			return err
		}
		if !pending || task.ID != original.ID || task.SourceCiphertext != original.SourceCiphertext || task.OfflineAccountNamespace != original.OfflineAccountNamespace || task.OfflineResourceDigest != original.OfflineResourceDigest || task.ProviderTaskID != "" && task.ProviderTaskID != found.ID {
			return errDownloadProviderIdentityChanged
		}
		now := w.service.queue.clock.Now()
		if err := tx.Model(&task).Updates(map[string]any{"provider_task_id": found.ID, "provider_status": found.Status, "updated_at": now}).Error; err != nil {
			return err
		}
		stopped := found.Completed || found.Failed
		if offline := found.Offline; offline != nil && offline.Version == 1 && offline.Kind == downloadpkg.OfflineOperationCloud {
			stopped = !offline.StopPending && (offline.UpstreamStopped || offline.TerminalFailure)
		}
		if !stopped {
			return restoreDownloadCapacityTx(tx, task, now)
		}
		return nil
	})
}

func (s *DownloadService) settlePendingDownloadControl(original models.DownloadTask) error {
	return s.settleDownloadControl(original, nil, "")
}

func (s *DownloadService) settlePreparationControl(claim ClaimedJob, original models.DownloadTask, payload string) error {
	return s.settleDownloadControl(original, &claim, payload)
}

func (s *DownloadService) settleDownloadControl(original models.DownloadTask, preparationClaim *ClaimedJob, expectedPayload string) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		var job models.Job
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&job, "id = ?", original.JobID).Error; err != nil {
			return err
		}
		task, pending, err := pendingDownloadControlTask(tx, job)
		if err != nil || !pending {
			return err
		}
		if task.SourceCiphertext != original.SourceCiphertext || !sameOptional(task.DownloaderID, original.DownloaderID) {
			return errDownloadProviderIdentityChanged
		}
		if preparationClaim != nil {
			if _, err := s.queue.verifyLease(tx, preparationClaim.Job.ID, preparationClaim.LeaseToken); err != nil {
				return err
			}
			if job.PayloadJSON != expectedPayload || !downloadPreparationCandidate(task) {
				return errDownloadProviderIdentityChanged
			}
		} else if task.ProviderTaskID == "" || task.ProviderTaskID != original.ProviderTaskID {
			return errDownloadProviderIdentityChanged
		}
		var payload downloadJobPayload
		if err := json.Unmarshal([]byte(job.PayloadJSON), &payload); err != nil {
			return err
		}
		payload.ControlIntent = ""
		payload.ControlDeleteData = false
		encoded, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		code, message := "", ""
		if task.LastErrorCode == CodeOfflineDeadlineExceeded {
			code, message = CodeOfflineDeadlineExceeded, offlineTimeoutMessage
		}
		if err := tx.Model(&task).Updates(map[string]any{"last_error_code": code, "last_error_message": message, "offline_stop_pending": false, "updated_at": now}).Error; err != nil {
			return err
		}
		updates := map[string]any{"payload_json": string(encoded), "last_error_code": code, "last_error_message": message, "updated_at": now, "revision": job.Revision + 1}
		if job.Status != models.JobStatusRunning {
			updates["status"], updates["finished_at"], updates["next_attempt_at"] = models.JobStatusCancelled, now, nil
			releaseLease(updates)
		}
		if err := tx.Model(&job).Updates(updates).Error; err != nil {
			return err
		}
		task.OfflineStopPending = false
		if err := releaseDownloadAdmissionTx(tx, task, now); err != nil {
			return err
		}
		return syncFailedFollowClaims(tx, task.ID, now)
	})
}

// Cancellation may be accepted while a normal worker returns a stale result.
// Re-read the intent under its lease transaction before deciding terminality.
func downloadControlCompletion(tx *gorm.DB, job models.Job) (pending, cancelled bool, err error) {
	_, pending, err = pendingDownloadControlTask(tx, job)
	if err != nil || pending || job.JobType != "download" {
		return pending, false, err
	}
	var task models.DownloadTask
	err = tx.First(&task, "job_id = ?", job.ID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, false, nil
	}
	return false, task.Phase == models.DownloadTaskStatusCancelled, err
}
