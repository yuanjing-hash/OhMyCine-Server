package services

import (
	"errors"
	"strconv"
	"strings"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

const (
	HistoryScopeTasks        = "tasks"
	HistoryScopeSTRMRuns     = "strm_runs"
	HistoryScopeLibraryScans = "library_scans"
	HistoryScopeDownloads    = "downloads"
	HistoryScopeOrganization = "organization"
	HistoryScopeSeeding      = "seeding"
	HistoryScopeFollowRuns   = "follow_runs"
	HistoryScopeScheduleRuns = "schedule_runs"
	historyPurgeBatchSize    = 100
)

type HistoryPurgeInput struct {
	Scope      string `json:"scope"`
	ResourceID string `json:"resource_id,omitempty"`
}

type HistoryPurgeResult struct {
	Scope    string `json:"scope"`
	Eligible int64  `json:"eligible"`
	Deleted  int64  `json:"deleted"`
	Skipped  int64  `json:"skipped"`
}

func historyTerminalStatuses() []string {
	return []string{models.JobStatusCompleted, models.JobStatusFailed, models.JobStatusCancelled}
}

func (s *QueueService) PreviewHistoryPurge(actor Actor, input HistoryPurgeInput) (HistoryPurgeResult, error) {
	return s.historyPurge(actor, input, RequestContext{}, true)
}

func (s *QueueService) PurgeHistory(actor Actor, input HistoryPurgeInput, request RequestContext) (HistoryPurgeResult, error) {
	return s.historyPurge(actor, input, request, false)
}

// historyPurge retires presentation history in bounded pages. Durable domain
// facts stay in place so foreign keys, retry provenance and media ownership are
// never destroyed merely because an administrator clears a list.
func (s *QueueService) historyPurge(actor Actor, input HistoryPurgeInput, request RequestContext, preview bool) (HistoryPurgeResult, error) {
	input.Scope = strings.TrimSpace(input.Scope)
	input.ResourceID = strings.TrimSpace(input.ResourceID)
	result := HistoryPurgeResult{Scope: input.Scope}
	after := ""
	for {
		ids, err := s.historyPurgeCandidatePage(actor, input, after, historyPurgeBatchSize)
		if err != nil {
			return result, err
		}
		if len(ids) == 0 {
			break
		}
		result.Eligible += int64(len(ids))
		var cleared, skipped int64
		err = s.db.Transaction(func(tx *gorm.DB) error {
			for _, id := range ids {
				var itemErr error
				if preview {
					itemErr = s.assertHistoryRecordClearableTx(tx, input.Scope, id)
				} else {
					itemErr = s.clearHistoryRecordTx(tx, input.Scope, id)
				}
				if errors.Is(itemErr, errHistoryRecoveryBearing) || errors.Is(itemErr, gorm.ErrRecordNotFound) {
					skipped++
					continue
				}
				if itemErr != nil {
					return itemErr
				}
				cleared++
			}
			if !preview && s.audit != nil && (cleared > 0 || skipped > 0) {
				if err := s.audit.Record(tx, &actor.User.ID, "management_history.purge", "history_scope", input.Scope, "success", map[string]any{
					"cleared":         cleared,
					"skipped":         skipped,
					"resource_scoped": input.ResourceID != "",
				}, request); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return result, err
		}
		result.Deleted += cleared
		result.Skipped += skipped
		after = ids[len(ids)-1]
		if len(ids) < historyPurgeBatchSize {
			break
		}
	}
	return result, nil
}

func (s *QueueService) historyPurgeCandidatePage(actor Actor, input HistoryPurgeInput, after string, limit int) ([]string, error) {
	ids := make([]string, 0, limit)
	keyset := func(query *gorm.DB, column string) *gorm.DB {
		if after != "" {
			query = query.Where(column+" > ?", after)
		}
		return query.Order(column).Limit(limit)
	}
	switch input.Scope {
	case HistoryScopeTasks:
		if !actor.Can(authz.PermissionJobsControlAll) && !actor.Can(authz.PermissionJobsControlOwn) {
			return nil, appError(CodePermissionDenied, "无权清除任务历史", nil)
		}
		query := s.db.Model(&models.Job{}).Where("history_cleared_at IS NULL")
		if !actor.Can(authz.PermissionJobsControlAll) {
			query = query.Where("owner_id = ?", actor.User.ID)
		}
		return ids, keyset(query, "id").Pluck("id", &ids).Error

	case HistoryScopeSTRMRuns:
		if !actor.HasPermission(authz.PermissionSTRMCleanup) {
			return nil, appError(CodePermissionDenied, "无权清除 STRM 运行历史", nil)
		}
		if input.ResourceID == "" {
			return nil, appError(CodeInvalidRequest, "请选择要清除运行历史的媒体库", nil)
		}
		query := s.db.Model(&models.MediaArtifactRun{}).Where("history_cleared_at IS NULL")
		libraryID, err := historyLibraryID(input.ResourceID)
		if err != nil {
			return nil, err
		}
		if !actor.CanResource(authz.PermissionSTRMCleanup, models.AuthorizationResourceMediaLibrary, uintID(libraryID)) {
			return nil, appError(CodePermissionDenied, "无权清除这个媒体库的 STRM 历史", nil)
		}
		query = query.Where("library_id = ?", libraryID)
		return ids, keyset(query, "id").Pluck("id", &ids).Error

	case HistoryScopeLibraryScans:
		libraryID, err := historyLibraryID(input.ResourceID)
		if err != nil {
			return nil, err
		}
		if !actor.CanResource(authz.PermissionMediaLibrariesScan, models.AuthorizationResourceMediaLibrary, uintID(libraryID)) {
			return nil, appError(CodePermissionDenied, "无权清除这个媒体库的扫描记录", nil)
		}
		query := s.db.Model(&models.MediaLibraryScanRun{}).
			Where("history_cleared_at IS NULL AND library_id = ?", libraryID)
		if after != "" {
			numericAfter, parseErr := strconv.ParseUint(after, 10, 64)
			if parseErr != nil {
				return nil, parseErr
			}
			query = query.Where("id > ?", numericAfter)
		}
		var numeric []uint
		if err := query.Order("id").Limit(limit).Pluck("id", &numeric).Error; err != nil {
			return nil, err
		}
		for _, id := range numeric {
			ids = append(ids, strconv.FormatUint(uint64(id), 10))
		}
		return ids, nil

	case HistoryScopeFollowRuns:
		if !actor.Can(authz.PermissionFollowsExecuteAll) && !actor.Can(authz.PermissionFollowsExecuteOwn) {
			return nil, appError(CodePermissionDenied, "无权清除追更运行记录", nil)
		}
		query := s.db.Model(&models.FollowRun{}).Where("history_cleared_at IS NULL")
		if !actor.Can(authz.PermissionFollowsExecuteAll) {
			query = query.Where("owner_id = ?", actor.User.ID)
		}
		if input.ResourceID != "" {
			var ownerID uint
			if err := s.db.Model(&models.FollowSubscription{}).Select("owner_id").Where("id = ?", input.ResourceID).Scan(&ownerID).Error; err != nil {
				return nil, err
			}
			if ownerID == 0 || (!actor.Can(authz.PermissionFollowsExecuteAll) && ownerID != actor.User.ID) {
				return nil, appError(CodePermissionDenied, "无权清除这个订阅的运行记录", nil)
			}
			query = query.Where("subscription_id = ?", input.ResourceID)
		}
		return ids, keyset(query, "id").Pluck("id", &ids).Error

	case HistoryScopeScheduleRuns:
		if !actor.Can(authz.PermissionSettingsUpdate) {
			return nil, appError(CodePermissionDenied, "无权清除计划运行记录", nil)
		}
		if input.ResourceID == "" && !actor.IsSystemAdmin() {
			return nil, appError(CodePermissionDenied, "请选择自己的计划任务后清除运行记录", nil)
		}
		query := s.db.Model(&models.ScheduleRun{}).Where("history_cleared_at IS NULL")
		if input.ResourceID != "" {
			var schedule models.ScheduleDefinition
			if err := s.db.Select("id", "owner_id").First(&schedule, "id = ?", input.ResourceID).Error; err != nil {
				return nil, notFound(err, "计划任务不存在")
			}
			if !actor.IsSystemAdmin() && schedule.OwnerID != actor.User.ID {
				return nil, appError(CodePermissionDenied, "无权清除这个计划任务的运行记录", nil)
			}
			query = query.Where("schedule_id = ?", input.ResourceID)
		}
		return ids, keyset(query, "id").Pluck("id", &ids).Error

	case HistoryScopeDownloads:
		if !actor.Can(authz.PermissionJobsControlAll) && !actor.Can(authz.PermissionJobsControlOwn) {
			return nil, appError(CodePermissionDenied, "无权清除下载历史", nil)
		}
		query := s.db.Model(&models.DownloadTask{}).
			Where("download_tasks.history_cleared_at IS NULL")
		if !actor.Can(authz.PermissionJobsControlAll) {
			query = query.Where("download_tasks.owner_id = ?", actor.User.ID)
		}
		return ids, keyset(query.Distinct(), "download_tasks.id").Pluck("download_tasks.id", &ids).Error

	case HistoryScopeOrganization:
		if !actor.Can(authz.PermissionJobsControlAll) && !actor.Can(authz.PermissionJobsControlOwn) {
			return nil, appError(CodePermissionDenied, "无权清除整理历史", nil)
		}
		query := s.db.Model(&models.TransferTask{}).
			Where("transfer_tasks.history_cleared_at IS NULL")
		if !actor.Can(authz.PermissionJobsControlAll) {
			query = query.Where("transfer_tasks.owner_id = ?", actor.User.ID)
		}
		return ids, keyset(query, "transfer_tasks.id").Pluck("transfer_tasks.id", &ids).Error

	case HistoryScopeSeeding:
		if !actor.Can(authz.PermissionJobsControlAll) && !actor.Can(authz.PermissionJobsControlOwn) && !actor.Can(authz.PermissionDownloadsManageAll) {
			return nil, appError(CodePermissionDenied, "无权清除做种历史", nil)
		}
		query := s.db.Model(&models.SeedingTask{}).
			Where("seeding_tasks.history_cleared_at IS NULL")
		if !actor.Can(authz.PermissionJobsControlAll) && !actor.Can(authz.PermissionDownloadsManageAll) {
			query = query.Where("seeding_tasks.owner_id = ?", actor.User.ID)
		}
		return ids, keyset(query, "seeding_tasks.id").Pluck("seeding_tasks.id", &ids).Error
	default:
		return nil, appError(CodeInvalidRequest, "历史记录范围无效", nil)
	}
}

var errHistoryRecoveryBearing = errors.New("history record retains recovery evidence")

func historyLibraryID(raw string) (uint, error) {
	value, err := strconv.ParseUint(strings.TrimSpace(raw), 10, 32)
	if err != nil || value == 0 {
		return 0, appError(CodeInvalidRequest, "媒体库 ID 无效", err)
	}
	return uint(value), nil
}

func countTable(tx *gorm.DB, table, where string, args ...any) (int64, error) {
	var count int64
	err := tx.Table(table).Where(where, args...).Count(&count).Error
	return count, err
}

func assertNoUnsettledPhysicalJobTx(tx *gorm.DB, jobID string) error {
	if jobID == "" {
		return nil
	}
	count, err := countTable(tx, "catalog_physical_writes", "job_id = ? AND state IN ('entered','quiescent')", jobID)
	if err != nil {
		return err
	}
	if count > 0 {
		return errHistoryRecoveryBearing
	}
	return nil
}

func (s *QueueService) assertHistoryRecordClearableTx(tx *gorm.DB, scope, id string) error {
	switch scope {
	case HistoryScopeTasks:
		if err := assertTerminalJobStateTx(tx, id); err != nil {
			return err
		}
		if err := assertNoUnsettledPhysicalJobTx(tx, id); err != nil {
			return err
		}
		return assertCatalogJobDomainSettledTx(tx, id)

	case HistoryScopeSTRMRuns:
		var run models.MediaArtifactRun
		if err := tx.First(&run, "id = ? AND history_cleared_at IS NULL", id).Error; err != nil {
			return err
		}
		if !containsString([]string{models.MediaArtifactStatusCompleted, models.MediaArtifactStatusFailed, models.MediaArtifactStatusSuperseded}, run.Status) {
			return errHistoryRecoveryBearing
		}
		for _, ref := range []struct {
			table string
			where string
			args  []any
		}{
			{"catalog_artifact_write_receipts", "run_id = ? AND phase IN ('prepared','conflict')", []any{id}},
			{"catalog_artifact_cleanup_claims", "owner_run_id = ?", []any{id}},
			{"catalog_physical_writes", "owner_kind = 'artifact' AND owner_id = ? AND state <> 'settled'", []any{id}},
		} {
			if count, err := countTable(tx, ref.table, ref.where, ref.args...); err != nil {
				return err
			} else if count > 0 {
				return errHistoryRecoveryBearing
			}
		}
		if run.CatalogBindingID != "" {
			var binding models.CatalogArtifactBinding
			if err := tx.Select("id", "state").First(&binding, "id = ?", run.CatalogBindingID).Error; err != nil {
				if !errors.Is(err, gorm.ErrRecordNotFound) {
					return err
				}
			} else if binding.State != "completed" && binding.State != "superseded" {
				return errHistoryRecoveryBearing
			}
		}
		if run.JobID != nil {
			if err := assertTerminalJobStateTx(tx, *run.JobID); err != nil {
				return err
			}
			if err := assertNoUnsettledPhysicalJobTx(tx, *run.JobID); err != nil {
				return err
			}
		}
		return assertCatalogArtifactRunDomainSettledTx(tx, run.ID)

	case HistoryScopeLibraryScans:
		var run models.MediaLibraryScanRun
		if err := tx.First(&run, "id = ? AND history_cleared_at IS NULL", id).Error; err != nil {
			return err
		}
		if !containsString([]string{"success", "superseded", "failed"}, run.Status) {
			return errHistoryRecoveryBearing
		}
		var activeDiagnosis uint
		if err := tx.Model(&models.MediaLibraryStructureDiagnosis{}).Select("library_id").Where("scan_run_id = ? AND status IN ?", run.ID, []string{"queued", "running"}).Limit(1).Scan(&activeDiagnosis).Error; err != nil {
			return err
		}
		if activeDiagnosis != 0 {
			return errHistoryRecoveryBearing
		}
		return nil

	case HistoryScopeFollowRuns:
		var row models.FollowRun
		if err := tx.First(&row, "id = ? AND history_cleared_at IS NULL", id).Error; err != nil {
			return err
		}
		if !containsString([]string{models.FollowRunNoMatch, models.FollowRunSubmitted, models.FollowRunCompleted, models.FollowRunFailed, models.FollowRunCancelled, models.FollowRunStale}, row.Status) {
			return errHistoryRecoveryBearing
		}
		if err := assertTerminalJobStateTx(tx, row.JobID); err != nil {
			return err
		}
		if err := assertNoUnsettledPhysicalJobTx(tx, row.JobID); err != nil {
			return err
		}
		var pendingClaims int64
		if err := tx.Model(&models.FollowEpisodeClaim{}).Where("run_id = ? AND state IN ?", row.ID, []string{"queued", "downloading"}).Count(&pendingClaims).Error; err != nil {
			return err
		}
		if pendingClaims != 0 {
			return errHistoryRecoveryBearing
		}
		return nil

	case HistoryScopeScheduleRuns:
		var row models.ScheduleRun
		if err := tx.First(&row, "id = ? AND history_cleared_at IS NULL", id).Error; err != nil {
			return err
		}
		if containsString([]string{models.JobStatusQueued, models.JobStatusRunning, models.JobStatusRetryWait, models.JobStatusWaitingUserAction, models.JobStatusPaused}, row.Status) {
			return errHistoryRecoveryBearing
		}
		if row.JobID == "" {
			return nil
		}
		if err := assertTerminalJobStateTx(tx, row.JobID); err != nil {
			return err
		}
		return assertNoUnsettledPhysicalJobTx(tx, row.JobID)

	case HistoryScopeDownloads:
		var row models.DownloadTask
		if err := tx.First(&row, "id = ? AND history_cleared_at IS NULL", id).Error; err != nil {
			return err
		}
		jobIDs, err := downloadHistoryJobIDsTx(tx, row)
		if err != nil {
			return err
		}
		for _, jobID := range jobIDs {
			if err := assertTerminalJobStateTx(tx, jobID); err != nil {
				return err
			}
			if err := assertNoUnsettledPhysicalJobTx(tx, jobID); err != nil {
				return err
			}
			if err := assertCatalogJobDomainSettledTx(tx, jobID); err != nil {
				return err
			}
		}
		return nil

	case HistoryScopeOrganization:
		var row models.TransferTask
		if err := tx.First(&row, "id = ? AND history_cleared_at IS NULL", id).Error; err != nil {
			return err
		}
		if err := assertTerminalJobStateTx(tx, row.JobID); err != nil {
			return err
		}
		if err := assertNoUnsettledPhysicalJobTx(tx, row.JobID); err != nil {
			return err
		}
		if err := assertCatalogJobDomainSettledTx(tx, row.JobID); err != nil {
			return err
		}
		if count, err := countTable(tx, "media_reorganization_tasks", "transfer_task_id = ? AND phase IN ?", id, []string{models.MediaReorganizationPhaseQueued, models.MediaReorganizationPhaseExecuting, models.MediaReorganizationPhaseReconciling}); err != nil {
			return err
		} else if count > 0 {
			return errHistoryRecoveryBearing
		}
		if count, err := countTable(tx, "transfer_deletion_previews", "transfer_task_id = ? AND completed_at IS NULL", id); err != nil {
			return err
		} else if count > 0 {
			return errHistoryRecoveryBearing
		}
		return nil

	case HistoryScopeSeeding:
		var row models.SeedingTask
		if err := tx.First(&row, "id = ? AND history_cleared_at IS NULL", id).Error; err != nil {
			return err
		}
		if !containsString([]string{models.SeedingTaskStatusCompleted, models.SeedingTaskStatusFailed}, row.Phase) {
			return errHistoryRecoveryBearing
		}
		if err := assertTerminalJobStateTx(tx, row.JobID); err != nil {
			return err
		}
		return assertNoUnsettledPhysicalJobTx(tx, row.JobID)
	}
	return errHistoryRecoveryBearing
}

func downloadHistoryJobIDsTx(tx *gorm.DB, row models.DownloadTask) ([]string, error) {
	jobIDs := []string{row.JobID}
	var related []string
	if err := tx.Table("transfer_tasks").Where("download_task_id = ?", row.ID).Pluck("job_id", &related).Error; err != nil {
		return nil, err
	}
	jobIDs = append(jobIDs, related...)
	related = nil
	if err := tx.Table("seeding_tasks").Where("download_task_id = ?", row.ID).Pluck("job_id", &related).Error; err != nil {
		return nil, err
	}
	return uniqueStrings(append(jobIDs, related...)), nil
}

func assertTerminalJobStateTx(tx *gorm.DB, id string) error {
	var job models.Job
	if err := tx.First(&job, "id = ?", id).Error; err != nil {
		return err
	}
	if !containsString(historyTerminalStatuses(), job.Status) || job.LeaseTokenHash != "" || job.LeaseExpiresAt != nil || job.InterruptStatus != "" {
		return errHistoryRecoveryBearing
	}
	return nil
}

func containsString(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func (s *QueueService) clearHistoryRecordTx(tx *gorm.DB, scope, id string) error {
	if err := s.assertHistoryRecordClearableTx(tx, scope, id); err != nil {
		return err
	}
	now := s.clock.Now()
	var result *gorm.DB
	jobIDs := make([]string, 0, 3)
	switch scope {
	case HistoryScopeTasks:
		result = tx.Model(&models.Job{}).Where("id = ? AND history_cleared_at IS NULL", id).Update("history_cleared_at", now)
		jobIDs = append(jobIDs, id)
	case HistoryScopeSTRMRuns:
		var run models.MediaArtifactRun
		if err := tx.Select("job_id").First(&run, "id = ?", id).Error; err != nil {
			return err
		}
		result = tx.Model(&models.MediaArtifactRun{}).Where("id = ? AND history_cleared_at IS NULL", id).Update("history_cleared_at", now)
		if run.JobID != nil {
			jobIDs = append(jobIDs, *run.JobID)
		}
	case HistoryScopeLibraryScans:
		result = tx.Model(&models.MediaLibraryScanRun{}).Where("id = ? AND history_cleared_at IS NULL", id).Update("history_cleared_at", now)
	case HistoryScopeFollowRuns:
		var row models.FollowRun
		if err := tx.Select("job_id").First(&row, "id = ?", id).Error; err != nil {
			return err
		}
		result = tx.Model(&models.FollowRun{}).Where("id = ? AND history_cleared_at IS NULL", id).Update("history_cleared_at", now)
		jobIDs = append(jobIDs, row.JobID)
	case HistoryScopeScheduleRuns:
		var row models.ScheduleRun
		if err := tx.Select("job_id").First(&row, "id = ?", id).Error; err != nil {
			return err
		}
		result = tx.Model(&models.ScheduleRun{}).Where("id = ? AND history_cleared_at IS NULL", id).Update("history_cleared_at", now)
		if row.JobID != "" {
			jobIDs = append(jobIDs, row.JobID)
		}
	case HistoryScopeDownloads:
		var row models.DownloadTask
		if err := tx.Select("job_id").First(&row, "id = ?", id).Error; err != nil {
			return err
		}
		result = tx.Model(&models.DownloadTask{}).Where("id = ? AND history_cleared_at IS NULL", id).Update("history_cleared_at", now)
		var err error
		jobIDs, err = downloadHistoryJobIDsTx(tx, row)
		if err != nil {
			return err
		}
	case HistoryScopeOrganization:
		var row models.TransferTask
		if err := tx.Select("job_id").First(&row, "id = ?", id).Error; err != nil {
			return err
		}
		result = tx.Model(&models.TransferTask{}).Where("id = ? AND history_cleared_at IS NULL", id).Update("history_cleared_at", now)
		jobIDs = append(jobIDs, row.JobID)
	case HistoryScopeSeeding:
		var row models.SeedingTask
		if err := tx.Select("job_id").First(&row, "id = ?", id).Error; err != nil {
			return err
		}
		result = tx.Model(&models.SeedingTask{}).Where("id = ? AND history_cleared_at IS NULL", id).Update("history_cleared_at", now)
		jobIDs = append(jobIDs, row.JobID)
	default:
		return errHistoryRecoveryBearing
	}
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return gorm.ErrRecordNotFound
	}
	for _, jobID := range uniqueStrings(jobIDs) {
		if err := tx.Model(&models.Job{}).Where("id = ? AND history_cleared_at IS NULL", jobID).Update("history_cleared_at", now).Error; err != nil {
			return err
		}
		inUse, err := jobHasVisibleManagementHistoryReferenceTx(tx, jobID)
		if err != nil {
			return err
		}
		if !inUse {
			if err := deleteJobPresentationDetailsTx(tx, jobID); err != nil {
				return err
			}
		}
	}
	return nil
}

// Queue history and domain history are separate views over some of the same
// durable jobs. Keep the shared timeline while any visible domain record still
// references it; the last domain clear may retire those display-only details.
func jobHasVisibleManagementHistoryReferenceTx(tx *gorm.DB, jobID string) (bool, error) {
	for _, table := range []string{"media_artifact_runs", "follow_runs", "schedule_runs", "download_tasks", "transfer_tasks", "seeding_tasks"} {
		count, err := countTable(tx, table, "job_id = ? AND history_cleared_at IS NULL", jobID)
		if err != nil {
			return false, err
		}
		if count > 0 {
			return true, nil
		}
	}
	return false, nil
}

func deleteJobPresentationDetailsTx(tx *gorm.DB, id string) error {
	for _, target := range []any{&models.JobAttempt{}, &models.JobStatusEvent{}, &models.JobActionRequest{}, &models.NotificationReceipt{}} {
		if err := tx.Where("job_id = ?", id).Delete(target).Error; err != nil {
			return err
		}
	}
	return nil
}

// A hidden failed record becomes visible again when its same durable Job is
// explicitly retried. This keeps every domain page coherent with task center
// state without inventing per-module retry exceptions.
func restoreManagementHistoryForJobTx(tx *gorm.DB, jobID string) error {
	for _, target := range []any{
		&models.DownloadTask{},
		&models.TransferTask{},
		&models.MediaArtifactRun{},
		&models.FollowRun{},
		&models.ScheduleRun{},
		&models.SeedingTask{},
	} {
		if err := tx.Model(target).Where("job_id = ?", jobID).Update("history_cleared_at", nil).Error; err != nil {
			return err
		}
	}
	return nil
}
