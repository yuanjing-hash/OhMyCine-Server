package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	serverlog "github.com/yuanjing-hash/OhMyCine-Server/internal/logging"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"gorm.io/gorm"
)

const seedingSampleInterval = 5 * time.Minute

type SeedingPolicySnapshot struct {
	CleanupEnabled     bool
	MinimumSeedMinutes int
	MinimumRatio       float64
	CompletionMode     string
}

type SeedingSettingsSummary struct {
	Enabled            bool      `json:"enabled"`
	MinimumSeedMinutes int       `json:"minimum_seed_minutes"`
	MinimumRatio       float64   `json:"minimum_ratio"`
	CompletionMode     string    `json:"completion_mode"`
	Revision           uint64    `json:"revision"`
	UpdatedAt          time.Time `json:"updated_at"`
}

type UpdateSeedingSettingsInput struct {
	Enabled            bool
	MinimumSeedMinutes int
	MinimumRatio       float64
	CompletionMode     string
	Revision           uint64
}

type SeedingSettingsService struct {
	db    *gorm.DB
	audit *AuditService
}

func NewSeedingSettingsService(db *gorm.DB, audit *AuditService) *SeedingSettingsService {
	return &SeedingSettingsService{db: db, audit: audit}
}

func (s *SeedingSettingsService) Get(actor Actor) (SeedingSettingsSummary, error) {
	if !actor.Can(authz.PermissionSettingsRead) {
		return SeedingSettingsSummary{}, appError(CodePermissionDenied, "无权查看做种设置", nil)
	}
	var record models.SeedingSettings
	if err := s.db.First(&record, 1).Error; err != nil {
		return SeedingSettingsSummary{}, err
	}
	return seedingSettingsSummary(record), nil
}

func (s *SeedingSettingsService) Snapshot() (SeedingPolicySnapshot, error) {
	var record models.SeedingSettings
	if err := s.db.First(&record, 1).Error; err != nil {
		return SeedingPolicySnapshot{}, err
	}
	return SeedingPolicySnapshot{CleanupEnabled: record.Enabled, MinimumSeedMinutes: record.MinimumSeedMinutes, MinimumRatio: record.MinimumRatio, CompletionMode: record.CompletionMode}, nil
}

func (s *SeedingSettingsService) Update(actor Actor, input UpdateSeedingSettingsInput, request RequestContext) (SeedingSettingsSummary, error) {
	if !actor.Can(authz.PermissionSettingsUpdate) {
		return SeedingSettingsSummary{}, appError(CodePermissionDenied, "无权编辑做种设置", nil)
	}
	input.CompletionMode = strings.ToLower(strings.TrimSpace(input.CompletionMode))
	if input.Revision == 0 || input.Revision >= math.MaxInt64 || input.MinimumSeedMinutes < 0 || input.MinimumSeedMinutes > 525600 || math.IsNaN(input.MinimumRatio) || math.IsInf(input.MinimumRatio, 0) || input.MinimumRatio < 0 || input.MinimumRatio > 1000 || (input.CompletionMode != models.SeedingCompletionAll && input.CompletionMode != models.SeedingCompletionAny) || (input.Enabled && input.MinimumSeedMinutes == 0 && input.MinimumRatio == 0) {
		return SeedingSettingsSummary{}, appError(CodeInvalidRequest, "做种设置无效；启用自动清理时至少需要一个有效条件", nil)
	}
	now := time.Now().UTC()
	var updated models.SeedingSettings
	err := s.db.Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&models.SeedingSettings{}).Where("id = ? AND revision = ?", 1, input.Revision).Updates(map[string]any{"enabled": input.Enabled, "minimum_seed_minutes": input.MinimumSeedMinutes, "minimum_ratio": input.MinimumRatio, "completion_mode": input.CompletionMode, "revision": gorm.Expr("revision + 1"), "updated_at": now})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return appError(CodeConflict, "做种设置已被其他会话更新，请刷新后重试", nil)
		}
		if err := tx.First(&updated, 1).Error; err != nil {
			return err
		}
		return s.audit.Record(tx, &actor.User.ID, "seeding_settings.update", "seeding_settings", "1", "success", map[string]any{"enabled": input.Enabled, "minimum_seed_minutes": input.MinimumSeedMinutes, "minimum_ratio": input.MinimumRatio, "completion_mode": input.CompletionMode}, request)
	})
	if err != nil {
		return SeedingSettingsSummary{}, err
	}
	return seedingSettingsSummary(updated), nil
}

func seedingSettingsSummary(record models.SeedingSettings) SeedingSettingsSummary {
	return SeedingSettingsSummary{Enabled: record.Enabled, MinimumSeedMinutes: record.MinimumSeedMinutes, MinimumRatio: record.MinimumRatio, CompletionMode: record.CompletionMode, Revision: record.Revision, UpdatedAt: record.UpdatedAt}
}

type seedingJobPayload struct {
	SeedingTaskID string `json:"seeding_task_id"`
}

type SeedingTaskSummary struct {
	ID                 string     `json:"id"`
	JobID              string     `json:"job_id"`
	JobStatus          string     `json:"job_status"`
	DownloadTaskID     string     `json:"download_task_id"`
	OwnerID            uint       `json:"owner_id"`
	DownloaderName     string     `json:"downloader_name"`
	DisplayName        string     `json:"display_name"`
	ProviderType       string     `json:"provider_type"`
	TransferMode       string     `json:"transfer_mode"`
	DeleteData         bool       `json:"delete_data"`
	CleanupEnabled     bool       `json:"cleanup_enabled"`
	MinimumSeedMinutes int        `json:"minimum_seed_minutes"`
	MinimumRatio       float64    `json:"minimum_ratio"`
	CompletionMode     string     `json:"completion_mode"`
	Phase              string     `json:"phase"`
	Ratio              *float64   `json:"ratio"`
	SeededSeconds      *int64     `json:"seeded_seconds"`
	UploadedBytes      *int64     `json:"uploaded_bytes"`
	LastSampledAt      *time.Time `json:"last_sampled_at"`
	LastErrorCode      string     `json:"last_error_code"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
	FinishedAt         *time.Time `json:"finished_at"`
}

type SeedingService struct {
	db             *gorm.DB
	audit          *AuditService
	queue          *QueueService
	downloaders    *DownloaderService
	log            zerolog.Logger
	stagingCleanup func(context.Context, string, bool) error
}

func NewSeedingService(db *gorm.DB, audit *AuditService, queue *QueueService, downloaders *DownloaderService, log zerolog.Logger) *SeedingService {
	return &SeedingService{db: db, audit: audit, queue: queue, downloaders: downloaders, log: log}
}

func (s *SeedingService) SetStagingCleanup(cleanup func(context.Context, string, bool) error) {
	s.stagingCleanup = cleanup
}

// AfterTransfer applies the mode-specific provider lifecycle. Moving has
// already removed the data from qBittorrent's save path, so its stale task is
// removed without asking the provider to delete data. Copy/symlink continue
// through durable seeding management.
func (s *SeedingService) AfterTransfer(ctx context.Context, download models.DownloadTask) error {
	if normalizeExecutionLocation(download.ExecutionLocation) == models.NodeLocationRemote {
		// Node downloads are copied over the data plane even when the final local
		// organization mode is move. The original bytes still back qB seeding and
		// must remain on the Node until the ordinary seeding policy finishes.
		return s.Enqueue(download)
	}
	if download.TransferMode != models.MediaLibraryTransferMove {
		return s.Enqueue(download)
	}
	if download.DownloaderID == nil || download.ProviderTaskID == "" {
		return nil
	}
	_, supported, err := s.seedingDownloader(*download.DownloaderID)
	if err != nil {
		// Import has already completed and the provider configuration may have
		// been removed concurrently. There is no credential left with which to
		// clean the stale task, so do not turn a successful move into a stuck
		// transfer retry loop.
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		return err
	}
	if !supported {
		return nil
	}
	client, err := s.downloaders.clientForDownloadTask(ctx, download)
	if err != nil {
		return err
	}
	if err := client.Cancel(ctx, download.ProviderTaskID, false); err != nil && !providerTaskMissing(err) {
		return err
	}
	return nil
}

// Enqueue is restart-safe and deliberately stores no source path. Whole-package
// provider deletion is allowed only when the immutable transfer manifests prove
// that no unselected video or unmatched subtitle needs conservative retention.
func (s *SeedingService) Enqueue(download models.DownloadTask) error {
	remote := normalizeExecutionLocation(download.ExecutionLocation) == models.NodeLocationRemote
	if !remote && download.TransferMode != models.MediaLibraryTransferCopy && download.TransferMode != models.MediaLibraryTransferSymlink {
		return nil
	}
	if download.DownloaderID == nil || download.ProviderTaskID == "" {
		return nil
	}
	_, supported, err := s.seedingDownloader(*download.DownloaderID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		return err
	}
	if !supported {
		return nil
	}
	var existing models.SeedingTask
	if err := s.db.Where("download_task_id = ?", download.ID).First(&existing).Error; err == nil {
		return nil
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	now, id := time.Now().UTC(), uuid.NewString()
	deleteData := (remote || download.TransferMode == models.MediaLibraryTransferCopy) && s.transferAllowsWholePackageCleanup(download.ID)
	record := models.SeedingTask{ID: id, OwnerID: download.OwnerID, DownloadTaskID: download.ID, ExecutionLocation: normalizeExecutionLocation(download.ExecutionLocation), NodeID: download.NodeID, NodeName: download.NodeName, DownloaderID: download.DownloaderID, DownloaderName: download.DownloaderName, ProviderType: download.ProviderType, ProviderTaskID: download.ProviderTaskID, TransferMode: download.TransferMode, DeleteData: deleteData, CleanupEnabled: download.SeedingCleanupEnabled, MinimumSeedMinutes: download.SeedingMinimumMinutes, MinimumRatio: download.SeedingMinimumRatio, CompletionMode: download.SeedingCompletionMode, Phase: models.SeedingTaskStatusQueued, CreatedAt: now, UpdatedAt: now}
	_, err = s.queue.EnqueueWith(EnqueueJobInput{OwnerID: download.OwnerID, JobType: "seeding", DisplayName: "做种：" + download.DisplayName, Provider: download.ProviderType, ResourceKey: "downloader:" + *download.DownloaderID, Payload: seedingJobPayload{SeedingTaskID: id}}, func(tx *gorm.DB, job models.Job) error {
		record.JobID = job.ID
		return tx.Create(&record).Error
	})
	if err != nil {
		var raced models.SeedingTask
		if lookup := s.db.Where("download_task_id = ?", download.ID).First(&raced).Error; lookup == nil {
			return nil
		}
	}
	return err
}

func (s *SeedingService) transferAllowsWholePackageCleanup(downloadTaskID string) bool {
	var transfer models.TransferTask
	if err := s.db.Where("download_task_id = ?", downloadTaskID).First(&transfer).Error; err != nil {
		return false
	}
	plan, err := buildTransferCleanupPlan(transfer)
	return err == nil && plan.ProtectedCount == 0
}

func (s *SeedingService) seedingDownloader(id string) (models.Downloader, bool, error) {
	var record models.Downloader
	if err := s.db.First(&record, "id = ?", id).Error; err != nil {
		return record, false, err
	}
	var capabilities downloadpkg.Capabilities
	if json.Unmarshal([]byte(record.CapabilitiesJSON), &capabilities) != nil {
		return record, false, nil
	}
	return record, capabilities.Seeding, nil
}

func (s *SeedingService) List(actor Actor, limit int) ([]SeedingTaskSummary, error) {
	if !actor.Can(authz.PermissionDownloadsReadAll) && !actor.Can(authz.PermissionDownloadsReadOwn) {
		return nil, appError(CodePermissionDenied, "无权查看做种任务", nil)
	}
	if limit < 1 {
		limit = 100
	}
	if limit > 200 {
		limit = 200
	}
	query := s.db.Where("history_cleared_at IS NULL").Order("created_at DESC, id DESC").Limit(limit)
	if !actor.Can(authz.PermissionDownloadsReadAll) {
		query = query.Where("owner_id = ?", actor.User.ID)
	}
	var records []models.SeedingTask
	if err := query.Find(&records).Error; err != nil {
		return nil, err
	}
	jobIDs := make([]string, 0, len(records))
	for _, record := range records {
		jobIDs = append(jobIDs, record.JobID)
	}
	statuses := map[string]string{}
	displayNames := map[string]string{}
	resources := map[string]jobResourceAccess{}
	if len(jobIDs) > 0 {
		var jobs []models.Job
		if err := s.db.Select("id", "status", "job_type").Where("id IN ?", jobIDs).Find(&jobs).Error; err != nil {
			return nil, err
		}
		rows, err := jobResourceAccessRows(s.db, jobs)
		if err != nil {
			return nil, err
		}
		resources = rows
		for _, job := range jobs {
			statuses[job.ID] = job.Status
		}
	}
	if len(records) > 0 {
		downloadIDs := make([]string, 0, len(records))
		for _, record := range records {
			downloadIDs = append(downloadIDs, record.DownloadTaskID)
		}
		var downloads []models.DownloadTask
		if err := s.db.Select("id", "display_name").Where("id IN ?", downloadIDs).Find(&downloads).Error; err != nil {
			return nil, err
		}
		for _, download := range downloads {
			displayNames[download.ID] = download.DisplayName
		}
	}
	result := make([]SeedingTaskSummary, 0, len(records))
	for _, record := range records {
		item := seedingTaskSummary(record, statuses[record.JobID], displayNames[record.DownloadTaskID])
		if !resources[record.JobID].visible(actor) {
			item = safeSeedingSummary(item)
		}
		result = append(result, item)
	}
	return result, nil
}

// Stop is an explicit destructive operation. copy tasks delete provider data;
// symlink tasks never do so because the library link depends on that source.
func (s *SeedingService) Stop(ctx context.Context, actor Actor, id string, request RequestContext) (summary SeedingTaskSummary, resultErr error) {
	var task models.SeedingTask
	if err := s.db.First(&task, "id = ?", strings.TrimSpace(id)).Error; err != nil {
		return SeedingTaskSummary{}, downloadTaskNotFound(err)
	}
	if !actor.Can(authz.PermissionDownloadsManageAll) && (task.OwnerID != actor.User.ID || !actor.Can(authz.PermissionJobsControlOwn)) {
		return SeedingTaskSummary{}, appError(CodePermissionDenied, "无权停止该做种任务", nil)
	}
	started := time.Now()
	ctx = serverlog.WithBusinessFields(ctx, seedingBusinessFields(task))
	defer func() {
		fields := seedingBusinessFields(task)
		fields["user_id"], fields["request_id"] = actor.User.ID, request.RequestID
		remainingBusinessResult(ctx, s.log, serverlog.OperationSeedingManagement, "seeding.manual.finish", "cleanup", "手动结束做种流程已返回，管理记录已结束；任务移除和数据保留策略见本次处理记录", "手动结束做种流程未完成", started, fields, resultErr)
	}()
	if task.Phase != models.SeedingTaskStatusCompleted {
		if err := s.cleanupProvider(ctx, &task); err != nil && !providerTaskMissing(err) {
			return SeedingTaskSummary{}, appError(CodeDownloaderUnavailable, "下载器未能停止做种，请稍后重试", nil)
		}
		if s.stagingCleanup != nil {
			if err := s.stagingCleanup(ctx, task.DownloadTaskID, task.DeleteData); err != nil {
				return SeedingTaskSummary{}, appError(CodeDownloaderUnavailable, "下载器任务已停止，但暂存清理将继续重试", err)
			}
		}
		if err := s.finish(task, &actor.User.ID, "manual", request); err != nil {
			return SeedingTaskSummary{}, err
		}
	}
	var job models.Job
	_ = s.db.First(&job, "id = ?", task.JobID).Error
	var download models.DownloadTask
	_ = s.db.Select("display_name").First(&download, "id = ?", task.DownloadTaskID).Error
	if err := s.db.First(&task, "id = ?", task.ID).Error; err != nil {
		return SeedingTaskSummary{}, err
	}
	item := seedingTaskSummary(task, job.Status, download.DisplayName)
	resources, err := jobResourceAccessRows(s.db, []models.Job{job})
	if err != nil {
		return SeedingTaskSummary{}, err
	}
	if !resources[job.ID].visible(actor) {
		item = safeSeedingSummary(item)
	}
	return item, nil
}

func (s *SeedingService) cleanupProvider(ctx context.Context, task *models.SeedingTask) error {
	s.enforceSafeDeleteData(task)
	if task.DownloaderID == nil {
		return appError(CodeDownloaderUnavailable, "原下载器配置已不存在", nil)
	}
	var download models.DownloadTask
	if err := s.db.First(&download, "id = ?", task.DownloadTaskID).Error; err != nil {
		return err
	}
	client, err := s.downloaders.clientForDownloadTask(ctx, download)
	if err != nil {
		return err
	}
	policy := "仅移除下载器任务，保留下载数据"
	if task.DeleteData {
		policy = "移除下载器任务并请求删除下载数据"
	}
	fields := seedingBusinessFields(*task)
	fields["resource_name"] = download.DisplayName
	serverlog.RecordBusiness(ctx, s.log, serverlog.OperationSeedingManagement, serverlog.BusinessEvent{Code: "seeding.cleanup.start", Stage: "cleanup", Outcome: "started", Message: "开始处理“" + serverlog.BusinessDisplayName(download.DisplayName) + "”：" + policy, Fields: fields})
	err = client.Cancel(ctx, task.ProviderTaskID, task.DeleteData)
	if err == nil || providerTaskMissing(err) {
		serverlog.RecordBusiness(ctx, s.log, serverlog.OperationSeedingManagement, serverlog.BusinessEvent{Code: "seeding.provider.accepted", Stage: "cleanup", Outcome: "accepted", Message: "下载器“" + serverlog.BusinessDisplayName(task.DownloaderName) + "”已接受任务移除或确认任务已不存在；策略：" + policy + "，后续还需处理暂存清理和管理记录", Fields: fields})
	}
	return err
}

func seedingBusinessFields(task models.SeedingTask) map[string]any {
	fields := map[string]any{"job_id": task.JobID, "task_id": task.ID, "downloader_name": task.DownloaderName, "mode": task.TransferMode}
	if task.DownloaderID != nil {
		fields["downloader_id"] = *task.DownloaderID
	}
	if task.DeleteData {
		fields["action"] = "remove_task_and_data"
	} else {
		fields["action"] = "remove_task_keep_data"
	}
	return fields
}

func seedingProgressMessage(task models.SeedingTask) string {
	timeText, ratioText := "未知", "未知"
	if task.SeededSeconds != nil {
		timeText = fmt.Sprintf("%d 分钟", *task.SeededSeconds/60)
	}
	if task.Ratio != nil {
		ratioText = fmt.Sprintf("%.2f", *task.Ratio)
	}
	return "当前做种时长 " + timeText + "，分享率 " + ratioText
}

func (s *SeedingService) enforceSafeDeleteData(task *models.SeedingTask) {
	if task == nil || !task.DeleteData || s.transferAllowsWholePackageCleanup(task.DownloadTaskID) {
		return
	}
	// Re-evaluate persisted tasks at the destructive boundary so jobs created by
	// an older Server version cannot bypass the protected-leftover policy.
	task.DeleteData = false
	_ = s.db.Model(&models.SeedingTask{}).Where("id = ?", task.ID).Update("delete_data", false).Error
}

func (s *SeedingService) finish(task models.SeedingTask, actorID *uint, reason string, request RequestContext) error {
	now := time.Now().UTC()
	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&models.SeedingTask{}).Where("id = ?", task.ID).Updates(map[string]any{"phase": models.SeedingTaskStatusCompleted, "last_error_code": "", "finished_at": now, "updated_at": now}).Error; err != nil {
			return err
		}
		if err := tx.Model(&models.Job{}).Where("id = ? AND status IN ?", task.JobID, []string{models.JobStatusQueued, models.JobStatusRetryWait, models.JobStatusPaused}).Updates(map[string]any{"status": models.JobStatusCompleted, "next_attempt_at": nil, "last_error_code": "", "last_error_message": "", "finished_at": now, "revision": gorm.Expr("revision + 1"), "updated_at": now}).Error; err != nil {
			return err
		}
		return s.audit.Record(tx, actorID, "seeding.cleanup", "seeding_task", task.ID, "success", map[string]any{"download_task_id": task.DownloadTaskID, "delete_data": task.DeleteData, "reason": reason}, request)
	})
}

func seedingTaskSummary(record models.SeedingTask, jobStatus, displayName string) SeedingTaskSummary {
	return SeedingTaskSummary{ID: record.ID, JobID: record.JobID, JobStatus: jobStatus, DownloadTaskID: record.DownloadTaskID, OwnerID: record.OwnerID, DownloaderName: record.DownloaderName, DisplayName: displayName, ProviderType: record.ProviderType, TransferMode: record.TransferMode, DeleteData: record.DeleteData, CleanupEnabled: record.CleanupEnabled, MinimumSeedMinutes: record.MinimumSeedMinutes, MinimumRatio: record.MinimumRatio, CompletionMode: record.CompletionMode, Phase: record.Phase, Ratio: record.Ratio, SeededSeconds: record.SeededSeconds, UploadedBytes: record.UploadedBytes, LastSampledAt: record.LastSampledAt, LastErrorCode: record.LastErrorCode, CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt, FinishedAt: record.FinishedAt}
}

type SeedingWorker struct{ service *SeedingService }

func NewSeedingWorker(service *SeedingService) *SeedingWorker {
	return &SeedingWorker{service: service}
}

// Interrupt pauses monitoring without pausing the torrent. Cancelling is an
// explicit cleanup request and therefore uses the immutable mode-derived
// DeleteData flag before the queue acknowledges cancellation.
func (w *SeedingWorker) Interrupt(ctx context.Context, job ClaimedJob, action string) error {
	if action == "pause" {
		serverlog.RecordBusiness(ctx, w.service.log, serverlog.OperationSeedingManagement, serverlog.BusinessEvent{Code: "seeding.monitor.paused", Stage: "monitor", Outcome: "confirmed", Message: "本次做种监测已停止，下载器中的做种任务和数据保持原状，恢复监测后继续采样", Fields: map[string]any{"job_id": job.Job.ID, "resource_name": job.Job.DisplayName}})
		return nil
	}
	if action != "cancel" {
		return appError(CodeInvalidRequest, "不支持的做种任务控制操作", nil)
	}
	var payload seedingJobPayload
	if json.Unmarshal([]byte(job.Job.PayloadJSON), &payload) != nil || payload.SeedingTaskID == "" {
		return appError(CodeInvalidRequest, "做种任务参数无效", nil)
	}
	var task models.SeedingTask
	if err := w.service.db.First(&task, "id = ?", payload.SeedingTaskID).Error; err != nil {
		return err
	}
	w.service.enforceSafeDeleteData(&task)
	if task.Phase == models.SeedingTaskStatusCompleted {
		return nil
	}
	if err := w.service.cleanupProvider(ctx, &task); err != nil && !providerTaskMissing(err) {
		return err
	}
	if w.service.stagingCleanup != nil {
		if err := w.service.stagingCleanup(ctx, task.DownloadTaskID, task.DeleteData); err != nil {
			return err
		}
	}
	if err := w.service.finish(task, nil, "task_center", RequestContext{}); err != nil {
		return err
	}
	serverlog.RecordBusiness(ctx, w.service.log, serverlog.OperationSeedingManagement, serverlog.BusinessEvent{Code: "seeding.control.finish", Stage: "cleanup", Outcome: "finished", Message: "任务中心请求的做种清理流程已返回，做种管理记录已结束", Fields: seedingBusinessFields(task)})
	return nil
}

func (w *SeedingWorker) Run(ctx context.Context, _ JobRuntime, job ClaimedJob) WorkerResult {
	var payload seedingJobPayload
	if json.Unmarshal([]byte(job.Job.PayloadJSON), &payload) != nil || payload.SeedingTaskID == "" {
		return WorkerResult{ErrorCode: "seeding_payload_invalid", ErrorMessage: "做种任务参数无效"}
	}
	var task models.SeedingTask
	if err := w.service.db.First(&task, "id = ?", payload.SeedingTaskID).Error; err != nil {
		return WorkerResult{ErrorCode: "seeding_task_missing", ErrorMessage: "做种任务不存在"}
	}
	w.service.enforceSafeDeleteData(&task)
	ctx = serverlog.WithBusinessFields(ctx, seedingBusinessFields(task))
	serverlog.OperationSeedingManagement.Event(w.service.log.Debug()).Str("task_id", task.ID).Msg(serverlog.OperationSeedingManagement.Message("检查做种状态"))
	if task.Phase == models.SeedingTaskStatusCompleted {
		serverlog.RecordBusiness(ctx, w.service.log, serverlog.OperationSeedingManagement, serverlog.BusinessEvent{Code: "seeding.skip", Stage: "monitor", Outcome: "skipped", Message: "做种管理记录已经结束，本次不重复清理"})
		return WorkerResult{}
	}
	if task.DownloaderID == nil {
		return w.fail(task, "seeding_downloader_missing", "原下载器配置已不存在", ctx)
	}
	var download models.DownloadTask
	if err := w.service.db.First(&download, "id = ?", task.DownloadTaskID).Error; err != nil {
		return w.fail(task, "seeding_download_missing", "原下载任务不存在", ctx)
	}
	ctx = serverlog.WithBusinessFields(ctx, map[string]any{"resource_name": download.DisplayName})
	if task.LastSampledAt == nil {
		serverlog.RecordBusiness(ctx, w.service.log, serverlog.OperationSeedingManagement, serverlog.BusinessEvent{Code: "seeding.start", Stage: "monitor", Outcome: "started", Message: "开始检查“" + serverlog.BusinessDisplayName(download.DisplayName) + "”在下载器“" + serverlog.BusinessDisplayName(task.DownloaderName) + "”的做种状态"})
	}
	client, err := w.service.downloaders.clientForDownloadTask(ctx, download)
	if err != nil {
		return w.retry(task, "seeding_downloader_unavailable", "暂时无法连接下载器", ctx)
	}
	provider, err := client.Get(ctx, task.ProviderTaskID)
	if err != nil {
		if providerTaskMissing(err) {
			if w.service.stagingCleanup != nil {
				if cleanupErr := w.service.stagingCleanup(ctx, task.DownloadTaskID, task.DeleteData); cleanupErr != nil {
					return w.retry(task, "download_staging_cleanup_failed", "下载暂存清理失败", ctx)
				}
			}
			if err := w.service.finish(task, nil, "provider_missing", RequestContext{}); err != nil {
				return WorkerResult{ErrorCode: "seeding_state_persist_failed", ErrorMessage: "做种清理结果保存失败"}
			}
			serverlog.RecordBusiness(ctx, w.service.log, serverlog.OperationSeedingManagement, serverlog.BusinessEvent{Code: "seeding.finish", Stage: "cleanup", Outcome: "finished", Message: "下载器已无此任务，暂存清理已返回，做种管理记录已结束", Fields: map[string]any{"reason": "provider_missing"}})
			return WorkerResult{}
		}
		code, retryable := downloadpkg.ErrorInfo(err)
		if retryable {
			return w.retry(task, code, "暂时无法读取做种状态", ctx)
		}
		return w.fail(task, code, "下载器拒绝读取做种状态", ctx)
	}
	now := time.Now().UTC()
	previousPhase := task.Phase
	phase := models.SeedingTaskStatusSeeding
	if !task.CleanupEnabled {
		phase = models.SeedingTaskStatusRetained
	}
	updates := map[string]any{"phase": phase, "ratio": provider.Ratio, "seeded_seconds": provider.SeededSeconds, "uploaded_bytes": provider.UploadedBytes, "last_sampled_at": now, "last_error_code": "", "updated_at": now}
	if err := w.service.db.Model(&task).Updates(updates).Error; err != nil {
		return WorkerResult{ErrorCode: "seeding_state_persist_failed", ErrorMessage: "做种状态保存失败"}
	}
	task.Ratio, task.SeededSeconds, task.UploadedBytes = provider.Ratio, provider.SeededSeconds, provider.UploadedBytes
	if !task.CleanupEnabled {
		next := now.Add(seedingSampleInterval)
		if previousPhase != phase {
			serverlog.RecordBusiness(ctx, w.service.log, serverlog.OperationSeedingManagement, serverlog.BusinessEvent{Code: "seeding.retained", Stage: "monitor", Outcome: "skipped", Message: "“" + serverlog.BusinessDisplayName(download.DisplayName) + "”未启用自动做种清理，保留下载器任务和数据，后续仅定期采样", Fields: map[string]any{"reason": "cleanup_disabled"}})
		}
		serverlog.OperationSeedingManagement.Event(w.service.log.Debug()).Str("task_id", task.ID).Str("phase", phase).Time("next_sample_at", next).Msg(serverlog.OperationSeedingManagement.Message("保留任务，等待下次采样"))
		return WorkerResult{RetryAt: &next}
	}
	if !seedingThresholdReached(task) {
		next := now.Add(seedingSampleInterval)
		if previousPhase != phase {
			serverlog.RecordBusiness(ctx, w.service.log, serverlog.OperationSeedingManagement, serverlog.BusinessEvent{Code: "seeding.wait", Stage: "monitor", Outcome: "waiting", Message: fmt.Sprintf("“%s”尚未达到做种清理条件，将继续采样；%s；目标时间 %d 分钟、目标分享率 %.2f，组合规则 %s", serverlog.BusinessDisplayName(download.DisplayName), seedingProgressMessage(task), task.MinimumSeedMinutes, task.MinimumRatio, map[string]string{models.SeedingCompletionAll: "全部满足", models.SeedingCompletionAny: "满足任一"}[task.CompletionMode]), Fields: map[string]any{"reason": "threshold_not_reached"}})
		}
		serverlog.OperationSeedingManagement.Event(w.service.log.Debug()).Str("task_id", task.ID).Str("phase", phase).Time("next_sample_at", next).Msg(serverlog.OperationSeedingManagement.Message("尚未达到清理条件"))
		return WorkerResult{RetryAt: &next}
	}
	cleanupPolicy := "仅移除下载器任务，保留下载数据"
	if task.DeleteData {
		cleanupPolicy = "移除下载器任务并请求删除下载数据"
	}
	serverlog.RecordBusiness(ctx, w.service.log, serverlog.OperationSeedingManagement, serverlog.BusinessEvent{Code: "seeding.cleanup.start", Stage: "cleanup", Outcome: "started", Message: "“" + serverlog.BusinessDisplayName(download.DisplayName) + "”已达到做种条件，" + seedingProgressMessage(task) + "；本次策略：" + cleanupPolicy})
	_ = w.service.db.Model(&task).Updates(map[string]any{"phase": models.SeedingTaskStatusCleanup, "updated_at": now}).Error
	if err := client.Cancel(ctx, task.ProviderTaskID, task.DeleteData); err != nil && !providerTaskMissing(err) {
		code, _ := downloadpkg.ErrorInfo(err)
		return w.retry(task, code, "下载器暂时无法完成做种清理", ctx)
	}
	serverlog.RecordBusiness(ctx, w.service.log, serverlog.OperationSeedingManagement, serverlog.BusinessEvent{Code: "seeding.provider.accepted", Stage: "cleanup", Outcome: "accepted", Message: "下载器已接受任务移除或确认任务已不存在，继续处理受管暂存清理；此记录不证明所有下载数据已删除"})
	if w.service.stagingCleanup != nil {
		if err := w.service.stagingCleanup(ctx, task.DownloadTaskID, task.DeleteData); err != nil {
			return w.retry(task, "download_staging_cleanup_failed", "下载暂存清理失败", ctx)
		}
	}
	if err := w.service.finish(task, nil, "threshold", RequestContext{}); err != nil {
		return WorkerResult{ErrorCode: "seeding_state_persist_failed", ErrorMessage: "做种清理结果保存失败"}
	}
	serverlog.RecordBusiness(ctx, w.service.log, serverlog.OperationSeedingManagement, serverlog.BusinessEvent{Code: "seeding.finish", Stage: "cleanup", Outcome: "finished", Message: "“" + serverlog.BusinessDisplayName(download.DisplayName) + "”的任务移除请求与暂存清理流程已返回，做种管理记录已结束"})
	return WorkerResult{}
}

func seedingThresholdReached(task models.SeedingTask) bool {
	timeEnabled, ratioEnabled := task.MinimumSeedMinutes > 0, task.MinimumRatio > 0
	timeReached := timeEnabled && task.SeededSeconds != nil && *task.SeededSeconds >= int64(task.MinimumSeedMinutes)*60
	ratioReached := ratioEnabled && task.Ratio != nil && *task.Ratio >= task.MinimumRatio
	if task.CompletionMode == models.SeedingCompletionAny {
		return timeReached || ratioReached
	}
	return (!timeEnabled || timeReached) && (!ratioEnabled || ratioReached) && (timeEnabled || ratioEnabled)
}

func (w *SeedingWorker) retry(task models.SeedingTask, code, message string, contexts ...context.Context) WorkerResult {
	now := time.Now().UTC()
	next := now.Add(seedingSampleInterval)
	phase := models.SeedingTaskStatusSeeding
	if !task.CleanupEnabled {
		phase = models.SeedingTaskStatusRetained
	}
	_ = w.service.db.Model(&task).Updates(map[string]any{"phase": phase, "last_error_code": safeLabel(code, 96), "updated_at": now}).Error
	fields := seedingBusinessFields(task)
	fields["error_code"] = code
	ctx := context.Background()
	if len(contexts) > 0 {
		ctx = contexts[0]
	}
	serverlog.RecordBusiness(ctx, w.service.log, serverlog.OperationSeedingManagement, serverlog.BusinessEvent{Code: "seeding.retry", Stage: "monitor", Outcome: "retry", Message: "下载器“" + serverlog.BusinessDisplayName(task.DownloaderName) + "”的做种状态读取或清理未完成：" + message + "，将按原采样策略重试", Fields: fields})
	return WorkerResult{RetryAt: &next, ErrorCode: safeLabel(code, 96), ErrorMessage: message}
}

func (w *SeedingWorker) fail(task models.SeedingTask, code, message string, contexts ...context.Context) WorkerResult {
	_ = w.service.db.Model(&task).Updates(map[string]any{"phase": models.SeedingTaskStatusFailed, "last_error_code": safeLabel(code, 96), "updated_at": time.Now().UTC()}).Error
	fields := seedingBusinessFields(task)
	fields["error_code"] = code
	ctx := context.Background()
	if len(contexts) > 0 {
		ctx = contexts[0]
	}
	serverlog.RecordBusiness(ctx, w.service.log, serverlog.OperationSeedingManagement, serverlog.BusinessEvent{Code: "seeding.failed", Stage: "monitor", Outcome: "failed", Message: "下载器“" + serverlog.BusinessDisplayName(task.DownloaderName) + "”的做种管理未能完成：" + message, Fields: fields})
	return WorkerResult{ErrorCode: safeLabel(code, 96), ErrorMessage: message}
}
