package services

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/robfig/cron/v3"
	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

const JobTypeUnifiedSchedule = "unified_schedule"

var fiveFieldCronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

var scheduleActions = map[string]string{
	"media_library_scan":               "媒体库全量扫描",
	"media_library_structure_diagnose": "媒体库目录诊断",
	"media_library_structure_repair":   "媒体库目录修复",
	"strm_reconcile":                   "STRM/元数据一致性检查",
	"follow_search":                    "订阅缺集搜索",
	"cookiecloud_sync":                 "CookieCloud 同步",
	"pan115_recycle_cleanup":           "清空 115 回收站",
}

type ScheduleInput struct {
	Name              string `json:"name"`
	ActionType        string `json:"action_type"`
	TargetType        string `json:"target_type"`
	TargetID          string `json:"target_id"`
	CronExpression    string `json:"cron_expression"`
	Timezone          string `json:"timezone"`
	Enabled           bool   `json:"enabled"`
	MisfirePolicy     string `json:"misfire_policy"`
	OverlapPolicy     string `json:"overlap_policy"`
	MaxRetries        int    `json:"max_retries"`
	RetryDelaySeconds int    `json:"retry_delay_seconds"`
	MaxRuntimeSeconds int    `json:"max_runtime_seconds"`
	Revision          uint64 `json:"revision,omitempty"`
}

type ScheduleAction struct {
	Code       string `json:"code"`
	Name       string `json:"name"`
	TargetType string `json:"target_type"`
}
type scheduleJobPayload struct {
	RunID        string `json:"run_id"`
	DefinitionID string `json:"definition_id"`
	Revision     uint64 `json:"revision"`
}

type UnifiedScheduleService struct {
	db             *gorm.DB
	queue          *QueueService
	authorization  *AuthorizationService
	libraries      *MediaLibraryService
	structure      *MediaLibraryStructureService
	strm           *STRMManagementService
	follows        *FollowService
	cookieCloud    *CookieCloudService
	recycleCleanup *Pan115RecycleCleanupService
	log            zerolog.Logger
	now            func() time.Time
	tick           time.Duration
	pollMu         sync.Mutex
	cancel         context.CancelFunc
	wg             sync.WaitGroup
}

func (s *UnifiedScheduleService) SetRecycleCleanup(service *Pan115RecycleCleanupService) {
	s.recycleCleanup = service
}

func NewUnifiedScheduleService(db *gorm.DB, queue *QueueService, authorization *AuthorizationService, libraries *MediaLibraryService, structure *MediaLibraryStructureService, strm *STRMManagementService, follows *FollowService, cookieCloud *CookieCloudService, log zerolog.Logger) *UnifiedScheduleService {
	return &UnifiedScheduleService{db: db, queue: queue, authorization: authorization, libraries: libraries, structure: structure, strm: strm, follows: follows, cookieCloud: cookieCloud, log: log, now: func() time.Time { return time.Now().UTC() }, tick: 30 * time.Second}
}

func (s *UnifiedScheduleService) Actions() []ScheduleAction {
	return []ScheduleAction{{"media_library_scan", scheduleActions["media_library_scan"], "media_library"}, {"media_library_structure_diagnose", scheduleActions["media_library_structure_diagnose"], "media_library"}, {"media_library_structure_repair", scheduleActions["media_library_structure_repair"], "media_library"}, {"strm_reconcile", scheduleActions["strm_reconcile"], "media_library"}, {"follow_search", scheduleActions["follow_search"], "follow"}, {"cookiecloud_sync", scheduleActions["cookiecloud_sync"], "system"}, {"pan115_recycle_cleanup", scheduleActions["pan115_recycle_cleanup"], "connection"}}
}

func PreviewSchedule(expression, timezone string, count int, after time.Time) ([]time.Time, error) {
	expression = strings.TrimSpace(expression)
	timezone = strings.TrimSpace(timezone)
	if len(strings.Fields(expression)) != 5 {
		return nil, appError(CodeInvalidRequest, "Cron 必须是标准五段表达式", nil)
	}
	schedule, err := fiveFieldCronParser.Parse(expression)
	if err != nil {
		return nil, appError(CodeInvalidRequest, "Cron 表达式无效", err)
	}
	location, err := time.LoadLocation(timezone)
	if err != nil {
		return nil, appError(CodeInvalidRequest, "时区无效", err)
	}
	if count < 1 {
		count = 5
	}
	if count > 20 {
		count = 20
	}
	cursor := after.In(location)
	result := make([]time.Time, 0, count)
	for range count {
		cursor = schedule.Next(cursor)
		result = append(result, cursor.UTC())
	}
	return result, nil
}

func (s *UnifiedScheduleService) List(actor Actor) ([]models.ScheduleDefinition, error) {
	if !actor.Can(authz.PermissionSettingsRead) {
		return nil, appError(CodePermissionDenied, "无权查看计划任务", nil)
	}
	var rows []models.ScheduleDefinition
	query := s.db.Order("name,id")
	if !actor.IsSystemAdmin() {
		query = query.Where("owner_id = ?", actor.User.ID)
	}
	if err := query.Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

func (s *UnifiedScheduleService) Runs(actor Actor, id string, limit int) ([]models.ScheduleRun, error) {
	if _, err := s.load(actor, id, false); err != nil {
		return nil, err
	}
	if limit < 1 || limit > 200 {
		limit = 50
	}
	var rows []models.ScheduleRun
	if err := s.db.Where("schedule_id = ?", id).Order("scheduled_at DESC,id").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

func (s *UnifiedScheduleService) Create(actor Actor, input ScheduleInput, request RequestContext) (models.ScheduleDefinition, error) {
	if !actor.Can(authz.PermissionSettingsUpdate) {
		return models.ScheduleDefinition{}, appError(CodePermissionDenied, "无权创建计划任务", nil)
	}
	record, err := s.normalize(actor.User.ID, input)
	if err != nil {
		return models.ScheduleDefinition{}, err
	}
	if err := s.validateTarget(actor, record); err != nil {
		return models.ScheduleDefinition{}, err
	}
	record.ID, record.Revision = uuid.NewString(), 1
	now := s.now()
	record.CreatedAt, record.UpdatedAt = now, now
	if err := s.db.Create(&record).Error; err != nil {
		return models.ScheduleDefinition{}, appError(CodeConflict, "计划任务名称已存在", err)
	}
	_ = request
	return record, nil
}

func (s *UnifiedScheduleService) Update(actor Actor, id string, input ScheduleInput, request RequestContext) (models.ScheduleDefinition, error) {
	if !actor.Can(authz.PermissionSettingsUpdate) {
		return models.ScheduleDefinition{}, appError(CodePermissionDenied, "无权修改计划任务", nil)
	}
	current, err := s.load(actor, id, true)
	if err != nil {
		return models.ScheduleDefinition{}, err
	}
	if input.Revision != current.Revision {
		return models.ScheduleDefinition{}, appError(CodeConflict, "计划任务已被其他操作修改", nil)
	}
	record, err := s.normalize(current.OwnerID, input)
	if err != nil {
		return models.ScheduleDefinition{}, err
	}
	if err := s.validateTarget(actor, record); err != nil {
		return models.ScheduleDefinition{}, err
	}
	updates := map[string]any{"name": record.Name, "action_type": record.ActionType, "target_type": record.TargetType, "target_id": record.TargetID, "cron_expression": record.CronExpression, "timezone": record.Timezone, "enabled": record.Enabled, "misfire_policy": record.MisfirePolicy, "overlap_policy": record.OverlapPolicy, "max_retries": record.MaxRetries, "retry_delay_seconds": record.RetryDelaySeconds, "max_runtime_seconds": record.MaxRuntimeSeconds, "next_run_at": record.NextRunAt, "revision": gorm.Expr("revision + 1"), "updated_at": s.now()}
	result := s.db.Model(&models.ScheduleDefinition{}).Where("id = ? AND revision = ?", id, input.Revision).Updates(updates)
	if result.Error != nil {
		return models.ScheduleDefinition{}, result.Error
	}
	if result.RowsAffected != 1 {
		return models.ScheduleDefinition{}, appError(CodeConflict, "计划任务已被其他操作修改", nil)
	}
	_ = request
	return s.load(actor, id, false)
}

func (s *UnifiedScheduleService) Delete(actor Actor, id string) error {
	if !actor.Can(authz.PermissionSettingsUpdate) {
		return appError(CodePermissionDenied, "无权删除计划任务", nil)
	}
	if _, err := s.load(actor, id, true); err != nil {
		return err
	}
	return s.db.Delete(&models.ScheduleDefinition{}, "id = ?", id).Error
}

func (s *UnifiedScheduleService) normalize(ownerID uint, input ScheduleInput) (models.ScheduleDefinition, error) {
	name := strings.Join(strings.Fields(input.Name), " ")
	action := strings.TrimSpace(input.ActionType)
	targetType := strings.TrimSpace(input.TargetType)
	targetID := strings.TrimSpace(input.TargetID)
	timezone := strings.TrimSpace(input.Timezone)
	if name == "" || len([]rune(name)) > 128 {
		return models.ScheduleDefinition{}, appError(CodeInvalidRequest, "计划任务名称无效", nil)
	}
	expected := ""
	for _, item := range s.Actions() {
		if item.Code == action {
			expected = item.TargetType
		}
	}
	if expected == "" || targetType != expected {
		return models.ScheduleDefinition{}, appError(CodeInvalidRequest, "计划任务动作或目标类型无效", nil)
	}
	if expected == "system" {
		targetID = "system"
	} else if targetID == "" || len(targetID) > 128 {
		return models.ScheduleDefinition{}, appError(CodeInvalidRequest, "计划任务目标无效", nil)
	}
	misfire := input.MisfirePolicy
	if misfire == "" {
		misfire = "run_once"
	}
	overlap := input.OverlapPolicy
	if overlap == "" {
		overlap = "skip"
	}
	if misfire != "skip" && misfire != "run_once" || overlap != "skip" && overlap != "queue" || input.MaxRetries < 0 || input.MaxRetries > 10 || input.RetryDelaySeconds < 10 || input.RetryDelaySeconds > 86400 || input.MaxRuntimeSeconds < 30 || input.MaxRuntimeSeconds > 86400 {
		return models.ScheduleDefinition{}, appError(CodeInvalidRequest, "计划任务执行策略无效", nil)
	}
	next, err := PreviewSchedule(input.CronExpression, timezone, 1, s.now())
	if err != nil {
		return models.ScheduleDefinition{}, err
	}
	record := models.ScheduleDefinition{OwnerID: ownerID, Name: name, ActionType: action, TargetType: targetType, TargetID: targetID, CronExpression: strings.TrimSpace(input.CronExpression), Timezone: timezone, Enabled: input.Enabled, MisfirePolicy: misfire, OverlapPolicy: overlap, MaxRetries: input.MaxRetries, RetryDelaySeconds: input.RetryDelaySeconds, MaxRuntimeSeconds: input.MaxRuntimeSeconds}
	if input.Enabled {
		record.NextRunAt = &next[0]
	}
	return record, nil
}

func (s *UnifiedScheduleService) validateTarget(actor Actor, record models.ScheduleDefinition) error {
	switch record.TargetType {
	case "media_library":
		id, err := strconv.ParseUint(record.TargetID, 10, 64)
		if err != nil || id == 0 {
			return appError(CodeInvalidRequest, "媒体库目标无效", err)
		}
		var count int64
		if err := s.db.Model(&models.MediaLibrary{}).Where("id = ?", id).Count(&count).Error; err != nil {
			return err
		}
		if count == 0 {
			return appError(CodeNotFound, "媒体库不存在", nil)
		}
		if !actor.CanResource(authz.PermissionMediaLibrariesScan, models.AuthorizationResourceMediaLibrary, record.TargetID) {
			return appError(CodePermissionDenied, "无权为这个媒体库创建计划", nil)
		}
	case "follow":
		var follow models.FollowSubscription
		if err := s.db.First(&follow, "id = ?", record.TargetID).Error; err != nil {
			return notFound(err, "订阅不存在")
		}
		if follow.OwnerID != actor.User.ID && !actor.Can(authz.PermissionFollowsExecuteAll) {
			return appError(CodePermissionDenied, "无权为这个订阅创建计划", nil)
		}
	case "connection":
		id, err := strconv.ParseUint(record.TargetID, 10, 64)
		if err != nil || id == 0 {
			return appError(CodeInvalidRequest, "连接目标无效", err)
		}
		var count int64
		if err := s.db.Model(&models.Connection{}).Where("id = ? AND provider = ?", id, models.ConnectionProviderPan115).Count(&count).Error; err != nil {
			return err
		}
		if count == 0 {
			return appError(CodeNotFound, "115 连接不存在", nil)
		}
		if !actor.Can(authz.PermissionConnectionsUpdate) {
			return appError(CodePermissionDenied, "无权为这个连接创建计划", nil)
		}
	case "system":
		if record.ActionType == "cookiecloud_sync" && !actor.Can(authz.PermissionSitesUpdate) {
			return appError(CodePermissionDenied, "无权创建 CookieCloud 同步计划", nil)
		}
	}
	return nil
}

func (s *UnifiedScheduleService) load(actor Actor, id string, mutate bool) (models.ScheduleDefinition, error) {
	var row models.ScheduleDefinition
	if err := s.db.First(&row, "id = ?", strings.TrimSpace(id)).Error; err != nil {
		return row, notFound(err, "计划任务不存在")
	}
	if !actor.IsSystemAdmin() && row.OwnerID != actor.User.ID {
		return row, appError(CodePermissionDenied, "无权访问这个计划任务", nil)
	}
	if mutate && row.OwnerID != actor.User.ID && !actor.IsSystemAdmin() {
		return row, appError(CodePermissionDenied, "无权修改这个计划任务", nil)
	}
	return row, nil
}

func (s *UnifiedScheduleService) Start(parent context.Context) error {
	if s.cancel != nil {
		return errors.New("unified schedule service already started")
	}
	ctx, cancel := context.WithCancel(parent)
	s.cancel = cancel
	if err := s.syncLegacyDefinitions(); err != nil {
		cancel()
		s.cancel = nil
		return err
	}
	if err := s.Poll(ctx); err != nil {
		cancel()
		s.cancel = nil
		return err
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(s.tick)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := s.Poll(ctx); err != nil {
					s.log.Warn().Str("error_code", ErrorCode(err)).Msg("统一计划任务扫描失败")
				}
			}
		}
	}()
	return nil
}

func cronFromIntervalMinutes(minutes int) (string, error) {
	switch {
	case minutes >= 1 && minutes < 60:
		return "*/" + strconv.Itoa(minutes) + " * * * *", nil
	case minutes%60 == 0 && minutes/60 >= 1 && minutes/60 <= 23:
		return "0 */" + strconv.Itoa(minutes/60) + " * * *", nil
	case minutes%1440 == 0 && minutes/1440 >= 1 && minutes/1440 <= 31:
		return "0 3 */" + strconv.Itoa(minutes/1440) + " * *", nil
	default:
		return "", appError(CodeInvalidRequest, "原有间隔无法无损转换为标准五段 Cron，请在计划任务中重新设置", nil)
	}
}

func managedScheduleKey(actionType, targetType, targetID string) string {
	return actionType + ":" + targetType + ":" + targetID
}

func syncManagedSchedule(tx *gorm.DB, ownerID uint, name, actionType, targetType, targetID, expression, timezone string, enabled, overwriteExisting bool, now time.Time) error {
	next, err := PreviewSchedule(expression, timezone, 1, now)
	if err != nil {
		return err
	}
	var existing models.ScheduleDefinition
	managedKey := managedScheduleKey(actionType, targetType, targetID)
	err = tx.Where("managed_key = ?", managedKey).First(&existing).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		record := models.ScheduleDefinition{ID: uuid.NewString(), ManagedKey: managedKey, OwnerID: ownerID, Name: name, ActionType: actionType, TargetType: targetType, TargetID: targetID, CronExpression: expression, Timezone: timezone, Enabled: enabled, MisfirePolicy: "run_once", OverlapPolicy: "skip", MaxRetries: 1, RetryDelaySeconds: 60, MaxRuntimeSeconds: 3600, Revision: 1, CreatedAt: now, UpdatedAt: now}
		if enabled {
			record.NextRunAt = &next[0]
		}
		return tx.Create(&record).Error
	}
	if err != nil {
		return err
	}
	if !overwriteExisting {
		return nil
	}
	updates := map[string]any{"owner_id": ownerID, "name": name, "cron_expression": expression, "timezone": timezone, "enabled": enabled, "revision": gorm.Expr("revision + 1"), "updated_at": now}
	if enabled {
		updates["next_run_at"] = next[0]
	} else {
		updates["next_run_at"] = nil
	}
	return tx.Model(&existing).Updates(updates).Error
}

func syncFollowUnifiedSchedule(tx *gorm.DB, follow models.FollowSubscription, overwriteExisting bool, now time.Time) error {
	var snapshot FollowExecutionSnapshot
	if err := json.Unmarshal([]byte(follow.ExecutionSnapshotJSON), &snapshot); err != nil {
		return err
	}
	expression, err := cronFromIntervalMinutes(snapshot.Schedule.Minutes)
	if err != nil {
		return err
	}
	shortID := follow.ID
	if len(shortID) > 8 {
		shortID = shortID[:8]
	}
	if err := syncManagedSchedule(tx, follow.OwnerID, "自动追更 · "+follow.Title+" · "+shortID, "follow_search", "follow", follow.ID, expression, "Asia/Shanghai", follow.Status == models.FollowStatusActive, overwriteExisting, now); err != nil {
		return err
	}
	if follow.Status == models.FollowStatusActive && follow.NextRunAt != nil && follow.NextRunAt.Before(now) {
		return tx.Model(&models.ScheduleDefinition{}).
			Where("managed_key = ?", managedScheduleKey("follow_search", "follow", follow.ID)).
			Update("next_run_at", follow.NextRunAt).Error
	}
	return nil
}

func syncCookieCloudUnifiedSchedule(tx *gorm.DB, ownerID uint, settings models.CookieCloudSettings, overwriteExisting bool, now time.Time) error {
	enabled := settings.Mode != "disabled" && settings.AutoSyncMinutes > 0
	minutes := settings.AutoSyncMinutes
	if minutes == 0 {
		minutes = 60
	}
	expression, err := cronFromIntervalMinutes(minutes)
	if err != nil {
		return err
	}
	return syncManagedSchedule(tx, ownerID, "CookieCloud 自动同步", "cookiecloud_sync", "system", "system", expression, "Asia/Shanghai", enabled, overwriteExisting, now)
}

func syncMediaLibraryUnifiedSchedule(tx *gorm.DB, ownerID uint, library models.MediaLibrary, overwriteExisting bool, now time.Time) error {
	expression, err := cronFromIntervalMinutes(library.FullScanIntervalHours * 60)
	if err != nil {
		return err
	}
	return syncManagedSchedule(tx, ownerID, "媒体库全量扫描 · "+library.Name+" · "+strconv.FormatUint(uint64(library.ID), 10), "media_library_scan", "media_library", strconv.FormatUint(uint64(library.ID), 10), expression, "Asia/Shanghai", library.Enabled, overwriteExisting, now)
}

func (s *UnifiedScheduleService) syncLegacyDefinitions() error {
	now := s.now()
	return s.db.Transaction(func(tx *gorm.DB) error {
		var follows []models.FollowSubscription
		if err := tx.Find(&follows).Error; err != nil {
			return err
		}
		for _, follow := range follows {
			if err := syncFollowUnifiedSchedule(tx, follow, false, now); err != nil {
				shortID := follow.ID
				if len(shortID) > 8 {
					shortID = shortID[:8]
				}
				if syncErr := syncManagedSchedule(tx, follow.OwnerID, "自动追更 · "+follow.Title+" · "+shortID, "follow_search", "follow", follow.ID, "0 3 * * *", "Asia/Shanghai", false, false, now); syncErr != nil {
					return syncErr
				}
				if updateErr := tx.Model(&models.ScheduleDefinition{}).
					Where("managed_key = ?", managedScheduleKey("follow_search", "follow", follow.ID)).
					Updates(map[string]any{"last_status": "migration_required", "last_error_code": CodeInvalidRequest}).Error; updateErr != nil {
					return updateErr
				}
			}
		}
		var owner models.User
		if err := tx.Where("is_owner = ?", true).First(&owner).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		} else if err != nil {
			return err
		}
		var libraries []models.MediaLibrary
		if err := tx.Find(&libraries).Error; err != nil {
			return err
		}
		for _, library := range libraries {
			if err := syncMediaLibraryUnifiedSchedule(tx, owner.ID, library, false, now); err != nil {
				return err
			}
		}
		var settings models.CookieCloudSettings
		if err := tx.First(&settings, 1).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		} else if err == nil {
			if err := syncCookieCloudUnifiedSchedule(tx, owner.ID, settings, false, now); err != nil {
				if syncErr := syncManagedSchedule(tx, owner.ID, "CookieCloud 自动同步", "cookiecloud_sync", "system", "system", "0 3 * * *", "Asia/Shanghai", false, false, now); syncErr != nil {
					return syncErr
				}
				if updateErr := tx.Model(&models.ScheduleDefinition{}).
					Where("managed_key = ?", managedScheduleKey("cookiecloud_sync", "system", "system")).
					Updates(map[string]any{"last_status": "migration_required", "last_error_code": CodeInvalidRequest}).Error; updateErr != nil {
					return updateErr
				}
			}
		}
		return nil
	})
}

func (s *UnifiedScheduleService) Close() {
	if s.cancel != nil {
		s.cancel()
		s.wg.Wait()
		s.cancel = nil
	}
}

func (s *UnifiedScheduleService) Poll(ctx context.Context) error {
	s.pollMu.Lock()
	defer s.pollMu.Unlock()
	now := s.now()
	var rows []models.ScheduleDefinition
	if err := s.db.WithContext(ctx).Where("enabled = ? AND next_run_at IS NOT NULL AND next_run_at <= ?", true, now).Order("next_run_at,id").Limit(100).Find(&rows).Error; err != nil {
		return err
	}
	for _, row := range rows {
		scheduledAt := *row.NextRunAt
		next, err := PreviewSchedule(row.CronExpression, row.Timezone, 1, scheduledAt)
		if err != nil {
			s.db.Model(&row).Updates(map[string]any{"enabled": false, "last_status": "failed", "last_error_code": CodeInvalidRequest, "next_run_at": nil})
			continue
		}
		if row.MisfirePolicy == "skip" && now.Sub(scheduledAt) > time.Minute {
			cursor := next[0]
			for !cursor.After(now) {
				following, previewErr := PreviewSchedule(row.CronExpression, row.Timezone, 1, cursor)
				if previewErr != nil {
					break
				}
				cursor = following[0]
			}
			finished := now
			run := models.ScheduleRun{ID: uuid.NewString(), ScheduleID: row.ID, ScheduledAt: scheduledAt, Status: "skipped_misfire", ErrorCode: "schedule_misfire_skipped", FinishedAt: &finished, CreatedAt: now, UpdatedAt: now}
			if err := s.db.Transaction(func(tx *gorm.DB) error {
				if err := tx.Create(&run).Error; err != nil {
					return err
				}
				return tx.Model(&row).Updates(map[string]any{"next_run_at": cursor, "last_run_at": scheduledAt, "last_status": run.Status, "updated_at": now}).Error
			}); err != nil {
				return err
			}
			continue
		}
		var active int64
		if err := s.db.Model(&models.ScheduleRun{}).Where("schedule_id = ? AND status IN ?", row.ID, []string{"queued", "running"}).Count(&active).Error; err != nil {
			return err
		}
		if active > 0 && row.OverlapPolicy == "skip" {
			finished := now
			run := models.ScheduleRun{ID: uuid.NewString(), ScheduleID: row.ID, ScheduledAt: scheduledAt, Status: "skipped_overlap", ErrorCode: "schedule_overlap_skipped", FinishedAt: &finished, CreatedAt: now, UpdatedAt: now}
			if err := s.db.Transaction(func(tx *gorm.DB) error {
				if err := tx.Create(&run).Error; err != nil {
					return err
				}
				return tx.Model(&row).Updates(map[string]any{"next_run_at": next[0], "last_run_at": scheduledAt, "last_status": run.Status, "updated_at": now}).Error
			}); err != nil {
				return err
			}
			continue
		}
		runID := uuid.NewString()
		_, err = s.queue.EnqueueWith(EnqueueJobInput{OwnerID: row.OwnerID, JobType: JobTypeUnifiedSchedule, DisplayName: "计划任务 · " + row.Name, Provider: "scheduler", ResourceKey: "schedule:" + row.ID, Payload: scheduleJobPayload{RunID: runID, DefinitionID: row.ID, Revision: row.Revision}}, func(tx *gorm.DB, job models.Job) error {
			run := models.ScheduleRun{ID: runID, ScheduleID: row.ID, JobID: job.ID, ScheduledAt: scheduledAt, Status: "queued", Attempt: 1, CreatedAt: now, UpdatedAt: now}
			if err := tx.Create(&run).Error; err != nil {
				return err
			}
			return tx.Model(&models.ScheduleDefinition{}).Where("id = ? AND revision = ?", row.ID, row.Revision).Updates(map[string]any{"next_run_at": next[0], "last_run_at": scheduledAt, "last_status": "queued", "updated_at": now}).Error
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func NewUnifiedScheduleWorker(service *UnifiedScheduleService) Worker { return WorkerFunc(service.run) }

func (s *UnifiedScheduleService) run(ctx context.Context, _ JobRuntime, claimed ClaimedJob) WorkerResult {
	var payload scheduleJobPayload
	if json.Unmarshal([]byte(claimed.Job.PayloadJSON), &payload) != nil || payload.RunID == "" {
		return WorkerResult{ErrorCode: CodeInvalidRequest, ErrorMessage: "计划任务参数无效"}
	}
	var definition models.ScheduleDefinition
	if err := s.db.First(&definition, "id = ?", payload.DefinitionID).Error; err != nil {
		return s.finishRun(payload.RunID, "failed", CodeNotFound)
	}
	if definition.Revision != payload.Revision || !definition.Enabled {
		return s.finishRun(payload.RunID, "cancelled", "schedule_definition_changed")
	}
	actor, err := s.authorization.Resolve(definition.OwnerID)
	if err != nil {
		return s.failOrRetry(payload.RunID, definition, claimed.Job.AttemptCount, err)
	}
	started := s.now()
	_ = s.db.Model(&models.ScheduleRun{}).Where("id = ?", payload.RunID).Updates(map[string]any{"status": "running", "started_at": started, "updated_at": started}).Error
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(definition.MaxRuntimeSeconds)*time.Second)
	defer cancel()
	targetID, _ := strconv.ParseUint(definition.TargetID, 10, 64)
	if (strings.HasPrefix(definition.ActionType, "media_library_") && !actor.CanResource(authz.PermissionMediaLibrariesScan, models.AuthorizationResourceMediaLibrary, definition.TargetID)) || (definition.ActionType == "strm_reconcile" && !actor.CanResource(authz.PermissionSTRMRunsCreate, models.AuthorizationResourceMediaLibrary, definition.TargetID)) || (definition.ActionType == "follow_search" && !actor.HasPermission(authz.PermissionFollowsExecuteOwn) && !actor.Can(authz.PermissionFollowsExecuteAll)) || (definition.ActionType == "cookiecloud_sync" && !actor.Can(authz.PermissionSitesUpdate)) {
		return s.finishRun(payload.RunID, "failed", CodePermissionDenied)
	}
	switch definition.ActionType {
	case "media_library_scan":
		_, err = s.libraries.ScanNow(runCtx, actor, uint(targetID))
	case "media_library_structure_diagnose":
		_, err = s.structure.Diagnose(runCtx, uint(targetID), "")
	case "media_library_structure_repair":
		_, err = s.structure.EnqueueRepair(runCtx, actor, uint(targetID), "", RequestContext{})
	case "strm_reconcile":
		_, err = s.strm.RequestReconcile(actor, uint(targetID), "full")
	case "follow_search":
		_, err = s.follows.Enqueue(runCtx, actor, definition.TargetID, "schedule", RequestContext{})
	case "cookiecloud_sync":
		_, err = s.cookieCloud.Sync(runCtx, actor, RequestContext{})
	case "pan115_recycle_cleanup":
		if !actor.Can(authz.PermissionConnectionsUpdate) || s.recycleCleanup == nil {
			err = appError(CodePermissionDenied, "无权执行 115 回收站清理", nil)
		} else {
			_, err = s.recycleCleanup.EnqueueConnection(uint(targetID))
		}
	default:
		err = appError(CodeInvalidRequest, "计划任务动作无效", nil)
	}
	if err != nil {
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			err = appError("schedule_runtime_exceeded", "计划任务超过最大运行时间", runCtx.Err())
		}
		return s.failOrRetry(payload.RunID, definition, claimed.Job.AttemptCount, err)
	}
	return s.finishRun(payload.RunID, "completed", "")
}

func (s *UnifiedScheduleService) failOrRetry(runID string, definition models.ScheduleDefinition, attempt int, cause error) WorkerResult {
	code := ErrorCode(cause)
	if code == "" {
		code = "schedule_action_failed"
	}
	if attempt <= definition.MaxRetries {
		now := s.now()
		next := now.Add(time.Duration(definition.RetryDelaySeconds) * time.Second)
		if err := s.db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Model(&models.ScheduleRun{}).Where("id = ?", runID).Updates(map[string]any{"status": "retry_wait", "attempt": attempt + 1, "error_code": code, "finished_at": nil, "updated_at": now}).Error; err != nil {
				return err
			}
			return tx.Model(&models.ScheduleDefinition{}).Where("id = ?", definition.ID).Updates(map[string]any{"last_status": "retry_wait", "last_error_code": code, "updated_at": now}).Error
		}); err != nil {
			return s.finishRun(runID, "failed", "schedule_state_failed")
		}
		return WorkerResult{RetryAt: &next, ErrorCode: code, ErrorMessage: "计划任务执行失败，将按策略重试"}
	}
	return s.finishRun(runID, "failed", code)
}

func (s *UnifiedScheduleService) finishRun(runID, status, code string) WorkerResult {
	now := s.now()
	var run models.ScheduleRun
	_ = s.db.First(&run, "id = ?", runID).Error
	_ = s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&models.ScheduleRun{}).Where("id = ?", runID).Updates(map[string]any{"status": status, "error_code": code, "finished_at": now, "updated_at": now}).Error; err != nil {
			return err
		}
		return tx.Model(&models.ScheduleDefinition{}).Where("id = ?", run.ScheduleID).Updates(map[string]any{"last_status": status, "last_error_code": code, "updated_at": now}).Error
	})
	if status == "failed" {
		if code == "" {
			code = "schedule_action_failed"
		}
		return WorkerResult{ErrorCode: code, ErrorMessage: "计划任务执行失败"}
	}
	return WorkerResult{}
}
