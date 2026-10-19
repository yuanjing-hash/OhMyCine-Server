package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/logging"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

type ExecutionLogFilter struct {
	Attempt    int
	Level      string
	Keyword    string
	Limit      int
	Cursor     string
	SnapshotTo time.Time
}

type ExecutionLogEntry struct {
	EventID        string         `json:"event_id"`
	Timestamp      time.Time      `json:"timestamp"`
	Level          string         `json:"level"`
	Message        string         `json:"message"`
	Operation      string         `json:"operation,omitempty"`
	OperationLabel string         `json:"operation_label,omitempty"`
	Stage          string         `json:"stage,omitempty"`
	Outcome        string         `json:"outcome,omitempty"`
	Fields         map[string]any `json:"fields"`
}

type ExecutionSummary struct {
	Title               string         `json:"title"`
	Status              string         `json:"status"`
	Trigger             string         `json:"trigger,omitempty"`
	SearchedNamesCount  *int           `json:"searched_names_count,omitempty"`
	Candidates          *int           `json:"candidates,omitempty"`
	AcceptedSubmissions *int           `json:"accepted_submissions,omitempty"`
	FilterSummary       map[string]int `json:"filter_summary,omitempty"`
	ErrorCode           string         `json:"error_code,omitempty"`
	ErrorMessage        string         `json:"error_message,omitempty"`
	StartedAt           *time.Time     `json:"started_at,omitempty"`
	FinishedAt          *time.Time     `json:"finished_at,omitempty"`
}

type ExecutionLogPage struct {
	List          []ExecutionLogEntry `json:"list"`
	NextCursor    string              `json:"next_cursor,omitempty"`
	SnapshotTo    time.Time           `json:"snapshot_to"`
	AttemptNumber *int                `json:"attempt_number,omitempty"`
	Partial       bool                `json:"partial"`
	Availability  string              `json:"availability"`
	Summary       *ExecutionSummary   `json:"summary,omitempty"`
}

// Execution logs share the one runtime sink. Domain services remain the
// authority for access, ownership, run relationships and durable summaries.
func (s *RuntimeLogService) SetExecutionServices(queue *QueueService, follows *FollowService) {
	s.executionQueue, s.executionFollows = queue, follows
}

func (s *RuntimeLogService) JobExecutionLogs(ctx context.Context, actor Actor, id string, filter ExecutionLogFilter) (ExecutionLogPage, error) {
	if s.executionQueue == nil {
		return ExecutionLogPage{}, appError(CodeRuntimeLogUnavailable, "执行日志服务暂不可用", nil)
	}
	var job models.Job
	if err := s.db.WithContext(ctx).First(&job, "id = ? AND history_cleared_at IS NULL", strings.TrimSpace(id)).Error; err != nil {
		return ExecutionLogPage{}, queueNotFound(err)
	}
	if !s.executionQueue.canRead(actor, job.OwnerID) {
		return ExecutionLogPage{}, appError(CodePermissionDenied, "没有查看任务的权限", nil)
	}
	if job.JobType == JobTypeFollowSearch {
		var run models.FollowRun
		if err := s.db.WithContext(ctx).First(&run, "job_id = ? AND history_cleared_at IS NULL", job.ID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return s.executionPage(ctx, actor, job, nil, nil, filter, false)
			}
			return ExecutionLogPage{}, err
		}
		if s.executionFollows == nil {
			return s.executionPage(ctx, actor, job, nil, nil, filter, false)
		}
		subscription, err := s.executionFollows.load(actor, run.SubscriptionID, "read")
		if err != nil {
			return s.executionPage(ctx, actor, job, nil, nil, filter, false)
		}
		if run.OwnerID != subscription.OwnerID || job.OwnerID == nil || *job.OwnerID != run.OwnerID {
			return s.executionPage(ctx, actor, job, nil, nil, filter, false)
		}
		return s.executionPage(ctx, actor, job, &run, &subscription, filter, true)
	}
	return s.executionPage(ctx, actor, job, nil, nil, filter, true)
}

func (s *RuntimeLogService) FollowExecutionLogs(ctx context.Context, actor Actor, subscriptionID, runID string, filter ExecutionLogFilter) (ExecutionLogPage, error) {
	if s.executionFollows == nil {
		return ExecutionLogPage{}, appError(CodeRuntimeLogUnavailable, "执行日志服务暂不可用", nil)
	}
	subscription, err := s.executionFollows.load(actor, strings.TrimSpace(subscriptionID), "read")
	if err != nil {
		return ExecutionLogPage{}, err
	}
	var run models.FollowRun
	if err := s.db.WithContext(ctx).First(&run, "id = ? AND subscription_id = ? AND owner_id = ? AND history_cleared_at IS NULL", strings.TrimSpace(runID), subscription.ID, subscription.OwnerID).Error; err != nil {
		return ExecutionLogPage{}, queueNotFound(err)
	}
	var job models.Job
	if err := s.db.WithContext(ctx).First(&job, "id = ? AND job_type = ? AND owner_id = ? AND history_cleared_at IS NULL", run.JobID, JobTypeFollowSearch, run.OwnerID).Error; err != nil {
		return ExecutionLogPage{}, queueNotFound(err)
	}
	// No jobs.read or global logs.read is required for this domain-owned route.
	return s.executionPage(ctx, actor, job, &run, &subscription, filter, true)
}

func (s *RuntimeLogService) executionPage(ctx context.Context, actor Actor, job models.Job, run *models.FollowRun, subscription *models.FollowSubscription, input ExecutionLogFilter, domainAllowed bool) (ExecutionLogPage, error) {
	now := time.Now().UTC()
	to := input.SnapshotTo
	if to.IsZero() {
		to = now
	}
	if to.After(now.Add(time.Second)) || to.Before(job.CreatedAt.Add(-time.Second)) || (input.Cursor != "" && input.SnapshotTo.IsZero()) || input.Attempt < 0 || input.Limit < 0 {
		return ExecutionLogPage{}, executionFilterError(nil)
	}
	page := ExecutionLogPage{List: []ExecutionLogEntry{}, SnapshotTo: to, Availability: "unavailable"}
	rows, err := jobResourceAccessRows(s.db.WithContext(ctx), []models.Job{job})
	if err != nil {
		return page, err
	}
	if !domainAllowed || !rows[job.ID].visible(actor) {
		page.Availability = "restricted"
		if !job.UpdatedAt.After(to) {
			page.Summary = &ExecutionSummary{Title: "资源权限受限的任务", Status: job.Status}
		}
		return page, nil
	}
	infrastructure, err := s.executionInfrastructure(ctx, actor, job, rows[job.ID])
	if err != nil {
		return page, err
	}
	if infrastructure.restricted {
		page.Availability = "restricted"
		if !job.UpdatedAt.After(to) {
			page.Summary = &ExecutionSummary{Title: "资源权限受限的任务", Status: job.Status}
		}
		return page, nil
	}
	page.Summary = &ExecutionSummary{Title: logging.BusinessDisplayName(job.DisplayName), Status: job.Status, ErrorCode: safeExecutionCode(job.LastErrorCode), StartedAt: job.StartedAt, FinishedAt: job.FinishedAt}
	if run != nil && subscription != nil {
		counts := map[string]int{}
		if json.Unmarshal([]byte(run.FilterSummaryJSON), &counts) != nil {
			counts = nil
		}
		clean := logging.BusinessFields(map[string]any{"filter_summary": counts})
		if truncated, _ := clean["event_truncated"].(bool); truncated {
			page.Partial = true
		}
		projected := map[string]int{}
		if values, ok := clean["filter_summary"].(map[string]any); ok {
			for key, value := range values {
				switch n := value.(type) {
				case int:
					projected[key] = n
				case float64:
					projected[key] = int(n)
				}
			}
		}
		page.Summary = &ExecutionSummary{Title: logging.BusinessDisplayName(subscription.Title), Status: run.Status, Trigger: safeExecutionCode(run.Trigger), SearchedNamesCount: &run.SearchedNamesCount, Candidates: &run.Candidates, AcceptedSubmissions: &run.Selected, FilterSummary: projected, ErrorCode: safeExecutionCode(run.ErrorCode), ErrorMessage: safeExecutionMessage(run.ErrorMessage), StartedAt: run.StartedAt, FinishedAt: run.FinishedAt}
	}
	// Domain rows retain current state, not a historical snapshot of their
	// counters/status. Do not attach later facts to an earlier log snapshot.
	if job.UpdatedAt.After(to) || (run != nil && run.UpdatedAt.After(to)) {
		page.Summary = nil
	}
	var attempt models.JobAttempt
	query := s.db.WithContext(ctx).Where("job_id = ? AND started_at <= ?", job.ID, to)
	if input.Attempt > 0 {
		query = query.Where("attempt_number = ?", input.Attempt)
	}
	err = query.Order("attempt_number DESC").First(&attempt).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return page, err
	}
	if errors.Is(err, gorm.ErrRecordNotFound) && input.Attempt > 0 {
		return page, appError(CodeNotFound, "执行尝试不存在", err)
	}
	from := job.CreatedAt.Add(-time.Second)
	filter := logging.Filter{JobID: job.ID, BusinessOnly: true, Direction: "asc", Limit: input.Limit, Cursor: input.Cursor, Keyword: strings.TrimSpace(input.Keyword), To: to}
	if attempt.ID != 0 {
		page.AttemptNumber = &attempt.AttemptNumber
		filter.Attempt = strconv.Itoa(attempt.AttemptNumber)
		from = attempt.StartedAt.Add(-time.Second)
		if attempt.FinishedAt != nil && attempt.FinishedAt.Before(filter.To) {
			filter.To = *attempt.FinishedAt
		}
		// FollowRun keeps the latest durable run result, not a per-attempt
		// snapshot. Historical attempts must not inherit later-run counts.
		if job.AttemptCount > 0 && attempt.AttemptNumber != job.AttemptCount {
			page.Summary = nil
		}
	}
	if from.Before(filter.To.Add(-31 * 24 * time.Hour)) {
		from = filter.To.Add(-31 * 24 * time.Hour)
		page.Partial = true
	}
	if !from.Before(filter.To) {
		return page, executionFilterError(nil)
	}
	filter.From = from
	if input.Level != "" {
		filter.Levels = []string{input.Level}
	}
	if run != nil {
		filter.FollowID, filter.FollowRunID = run.SubscriptionID, run.ID
	}
	if err := filter.Normalize(now); err != nil {
		return page, executionFilterError(err)
	}
	if s.manager == nil {
		page.Availability = "degraded"
		page.Partial = true
		return page, nil
	}
	queryContext, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	result, err := s.manager.Query(queryContext, filter)
	if err != nil {
		return page, executionFilterError(err)
	}
	page.NextCursor = result.NextCursor
	page.Partial = page.Partial || result.Partial || result.Malformed > 0
	linked := map[string][2]bool{}
	for _, entry := range result.List {
		if truncated, _ := entry.Fields["event_truncated"].(bool); truncated {
			page.Partial = true
		}
		if (job.OwnerID != nil && entry.Fields["user_id"] != nil && executionID(entry.Fields["user_id"]) != uintID(*job.OwnerID)) || !s.executionEntryVisible(actor, entry, run) {
			page.Partial = true
			continue
		}
		fields := logging.BusinessFields(entry.Fields)
		if !infrastructure.visible(fields) {
			page.Partial = true
			continue
		}
		// Linked tasks must themselves be readable. Do not preserve a link just
		// because the parent run is readable under follows.read_own.
		if related, ok := fields["related_job_id"].(string); ok {
			access, seen := linked[related]
			if !seen {
				readable, resources := s.executionRelatedJobAccess(ctx, actor, related)
				access = [2]bool{readable, resources}
				linked[related] = access
			}
			if !access[1] {
				page.Partial = true
				continue
			}
			if !access[0] {
				delete(fields, "related_job_id")
				delete(fields, "related_task_id")
			}
		} else {
			delete(fields, "related_task_id")
		}
		if run == nil {
			delete(fields, "follow_id")
			delete(fields, "follow_run_id")
		}
		for key := range fields {
			if strings.Contains(key, "connection_id") && !actor.Can(authz.PermissionConnectionsRead) {
				delete(fields, key)
			}
			if strings.Contains(key, "storage_id") && !actor.Can(authz.PermissionStoragesRead) {
				delete(fields, key)
			}
		}
		fields["event_code"] = safeExecutionCode(fmt.Sprint(entry.Fields["event_code"]))
		delete(fields, "user_id")
		delete(fields, "resource_scope_incomplete")
		id, _ := entry.Fields["event_id"].(string)
		stage, _ := entry.Fields["stage"].(string)
		outcome, _ := entry.Fields["outcome"].(string)
		page.List = append(page.List, ExecutionLogEntry{EventID: id, Timestamp: entry.Timestamp, Level: entry.Level, Message: entry.Message, Operation: entry.Operation, OperationLabel: entry.OperationLabel, Stage: stage, Outcome: outcome, Fields: fields})
	}
	if len(page.List) > 0 {
		page.Availability = "available"
	}
	if s.manager.Health().Degraded {
		page.Availability = "degraded"
		page.Partial = true
	}
	return page, nil
}

// Drop the whole observation before projecting message/sample/link fields if
// any referenced resource is no longer visible. Removing IDs alone leaks names.
func (s *RuntimeLogService) executionEntryVisible(actor Actor, entry logging.Entry, run *models.FollowRun) bool {
	fields := logging.BusinessFields(entry.Fields)
	if incomplete, _ := fields["resource_scope_incomplete"].(bool); incomplete {
		return false
	}
	if entry.Operation == logging.OperationFollowSearch.Code && run == nil {
		return false
	}
	if entry.Operation == logging.OperationUnifiedSchedule.Code {
		if followID, _ := fields["follow_id"].(string); followID != "" {
			if s.executionFollows == nil {
				return false
			}
			if _, err := s.executionFollows.load(actor, followID, "read"); err != nil {
				return false
			}
		}
	}
	return executionResourceReferencesVisible(actor, fields)
}

func executionResourceReferencesVisible(actor Actor, fields map[string]any) bool {
	if incomplete, _ := fields["resource_scope_incomplete"].(bool); incomplete {
		return false
	}
	resourceID := func(value any, resourceType string) string {
		if number, ok := value.(float64); ok && number != float64(int64(number)) {
			return ""
		}
		return canonicalResourceID(resourceType, executionID(value))
	}
	for _, key := range []string{"library_id", "source_library_id", "target_library_id"} {
		if value := fields[key]; value != nil {
			if id := resourceID(value, models.AuthorizationResourceMediaLibrary); id == "" || !actor.CanResource(authz.PermissionMediaLibrariesRead, models.AuthorizationResourceMediaLibrary, id) {
				return false
			}
		}
	}
	if value := fields["site_id"]; value != nil {
		if id := resourceID(value, models.AuthorizationResourceSite); id == "" || !actor.CanResource(authz.PermissionDiscoveryRead, models.AuthorizationResourceSite, id) {
			return false
		}
	}
	if id := executionID(fields["downloader_id"]); id != "" && !taskDownloaderVisible(actor, &id) {
		return false
	}
	for _, key := range []string{"site_ids", "library_ids"} {
		value, exists := fields[key]
		if !exists {
			continue
		}
		ids, ok := value.([]any)
		if !ok {
			return false
		}
		resourceType, permission := models.AuthorizationResourceMediaLibrary, authz.PermissionMediaLibrariesRead
		if key == "site_ids" {
			resourceType, permission = models.AuthorizationResourceSite, authz.PermissionDiscoveryRead
		}
		for _, value := range ids {
			if id := resourceID(value, resourceType); id == "" || !actor.CanResource(permission, resourceType, id) {
				return false
			}
		}
	}
	return true
}

type executionInfrastructure struct {
	connections map[string]bool
	storages    map[string]bool
	restricted  bool
}

func (scope executionInfrastructure) visible(fields map[string]any) bool {
	if scope.restricted {
		return false
	}
	for key, value := range fields {
		id := executionID(value)
		if strings.Contains(key, "connection_id") && id != "" && !scope.connections[id] {
			return false
		}
		if strings.Contains(key, "storage_id") && id != "" && !scope.storages[id] {
			return false
		}
	}
	return true
}

// Infrastructure display references must belong to the already-authorized
// job/library route. Configuration-read permissions are not prerequisites for
// ordinary users to see their own safe file movement narrative.
func (s *RuntimeLogService) executionInfrastructure(ctx context.Context, actor Actor, job models.Job, access jobResourceAccess) (executionInfrastructure, error) {
	return s.executionInfrastructureForRead(ctx, actor, job, access, false)
}

func (s *RuntimeLogService) executionInfrastructureForRead(ctx context.Context, actor Actor, job models.Job, access jobResourceAccess, global bool) (executionInfrastructure, error) {
	scope := executionInfrastructure{connections: map[string]bool{}, storages: map[string]bool{}}
	switch job.JobType {
	case "download", "transfer", "seeding":
		scope.restricted = !access.Bound
	case JobTypeFollowSearch:
		scope.restricted = access.LibraryID == 0
	case JobTypePan115RecycleCleanup, JobTypeMediaServerRefresh:
		scope.restricted = true
	}
	addStorage := func(id *uint) {
		if id != nil && *id > 0 {
			scope.storages[uintID(*id)] = true
		}
	}
	addConnection := func(id *uint) {
		if id != nil && *id > 0 {
			scope.connections[uintID(*id)] = true
		}
	}
	// System connection work is not a user download route. Its payload and
	// resource key must agree, and the caller must be allowed to read connections.
	if job.JobType == JobTypePan115RecycleCleanup && (global || actor.Can(authz.PermissionConnectionsRead)) {
		var payload pan115RecycleCleanupJobPayload
		if json.Unmarshal([]byte(job.PayloadJSON), &payload) == nil && payload.ConnectionID > 0 && job.ResourceKey == "connection:"+uintID(payload.ConnectionID) {
			var connection models.Connection
			if err := s.db.WithContext(ctx).Select("id").First(&connection, payload.ConnectionID).Error; err == nil {
				addConnection(&payload.ConnectionID)
				scope.restricted = false
			} else if !errors.Is(err, gorm.ErrRecordNotFound) {
				return scope, err
			}
		}
	}
	// A refresh target is a real library binding, not the library's own Storage
	// connection. Ordinary authorized task readers may see its safe name without
	// obtaining media-server/connection configuration management privileges.
	if job.JobType == JobTypeMediaServerRefresh {
		var payload mediaServerRefreshJobPayload
		if json.Unmarshal([]byte(job.PayloadJSON), &payload) == nil && payload.TargetID > 0 && job.ResourceKey == "media-server-target:"+uintID(payload.TargetID) {
			var target models.MediaServerRefreshTarget
			if err := s.db.WithContext(ctx).Select("id", "library_id", "connection_id").First(&target, payload.TargetID).Error; err == nil {
				if target.LibraryID > 0 && target.ConnectionID > 0 && actor.CanResource(authz.PermissionMediaLibrariesRead, models.AuthorizationResourceMediaLibrary, uintID(target.LibraryID)) {
					addConnection(&target.ConnectionID)
					scope.restricted = false
				}
			} else if !errors.Is(err, gorm.ErrRecordNotFound) {
				return scope, err
			}
		}
	}
	if access.LibraryID != 0 {
		var library models.MediaLibrary
		if err := s.db.WithContext(ctx).Select("id", "storage_id").First(&library, access.LibraryID).Error; err == nil {
			addStorage(&library.StorageID)
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return scope, err
		}
	}
	if job.JobType == "download" || job.JobType == "transfer" || job.JobType == "seeding" {
		query := s.db.WithContext(ctx).Table("download_tasks AS download")
		if job.JobType == "download" {
			query = query.Where("download.job_id = ?", job.ID)
		} else {
			table := "transfer_tasks"
			if job.JobType == "seeding" {
				table = "seeding_tasks"
			}
			query = query.Joins("JOIN "+table+" AS phase ON phase.download_task_id = download.id").Where("phase.job_id = ?", job.ID)
		}
		var download models.DownloadTask
		if err := query.Select("download.staging_storage_id", "download.target_storage_id", "download.target_connection_id", "download.source_data_source_json", "download.target_data_source_json").Take(&download).Error; err == nil {
			addStorage(download.StagingStorageID)
			addStorage(download.TargetStorageID)
			addConnection(download.TargetConnectionID)
			for _, raw := range []string{download.SourceDataSourceJSON, download.TargetDataSourceJSON} {
				if identity, err := decodeDataSourceIdentity(raw); err == nil && identity.Kind == models.DataSourceKindProvider {
					if id, err := strconv.ParseUint(identity.ConnectionIdentity, 10, 64); err == nil && id > 0 {
						scope.connections[strconv.FormatUint(id, 10)] = true
					}
					if id, err := strconv.ParseUint(identity.StorageScope, 10, 64); err == nil && id > 0 {
						scope.storages[strconv.FormatUint(id, 10)] = true
					}
				}
			}
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return scope, err
		}
	}
	if len(scope.storages) > 0 {
		ids := make([]string, 0, len(scope.storages))
		for id := range scope.storages {
			ids = append(ids, id)
		}
		var storages []models.Storage
		if err := s.db.WithContext(ctx).Select("id", "connection_id").Where("id IN ?", ids).Find(&storages).Error; err != nil {
			return scope, err
		}
		for _, storage := range storages {
			addConnection(storage.ConnectionID)
		}
	}
	return scope, nil
}

func (s *RuntimeLogService) executionRelatedJobAccess(ctx context.Context, actor Actor, id string) (bool, bool) {
	if s.executionQueue == nil {
		return false, false
	}
	var job models.Job
	if err := s.db.WithContext(ctx).First(&job, "id = ? AND history_cleared_at IS NULL", id).Error; err != nil {
		return false, false
	}
	readable := s.executionQueue.canRead(actor, job.OwnerID)
	// A domain-only reader may see the fact that its own download was queued
	// without jobs.read. A foreign owner's unreadable task is not such a fact.
	if !readable && job.OwnerID != nil && *job.OwnerID != actor.User.ID {
		return false, false
	}
	if job.JobType == JobTypeFollowSearch {
		var run models.FollowRun
		if s.executionFollows == nil || s.db.WithContext(ctx).First(&run, "job_id = ? AND history_cleared_at IS NULL", job.ID).Error != nil {
			return false, false
		}
		subscription, err := s.executionFollows.load(actor, run.SubscriptionID, "read")
		if err != nil || run.OwnerID != subscription.OwnerID || job.OwnerID == nil || *job.OwnerID != run.OwnerID {
			return false, false
		}
	}
	rows, err := jobResourceAccessRows(s.db.WithContext(ctx), []models.Job{job})
	resources := err == nil && rows[job.ID].visible(actor)
	if resources {
		scope, err := s.executionInfrastructure(ctx, actor, job, rows[job.ID])
		resources = err == nil && !scope.restricted
	}
	return resources && readable, resources
}

func executionID(value any) string {
	if value == nil {
		return ""
	}
	if n, ok := value.(float64); ok {
		return strconv.FormatInt(int64(n), 10)
	}
	return fmt.Sprint(value)
}

func safeExecutionCode(value string) string {
	fields := logging.BusinessFields(map[string]any{"error_code": value})
	result, _ := fields["error_code"].(string)
	return result
}

func safeExecutionMessage(value string) string {
	return logging.BusinessMessage(value)
}

func executionFilterError(err error) error {
	return appError(CodeRuntimeLogFilterInvalid, "执行日志筛选条件无效，请刷新后重试", err)
}
