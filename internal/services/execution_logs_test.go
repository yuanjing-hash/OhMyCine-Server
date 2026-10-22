package services

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/logging"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestExecutionLogsOwnDomainSummaryAndCurrentResourceProjection(t *testing.T) {
	queue, owner, _ := queueFixture(t)
	manager, err := logging.NewManager(t.TempDir(), "production", &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	follows := NewFollowService(queue.db, queue.audit, queue, nil, nil)
	service := &RuntimeLogService{db: queue.db, manager: manager, audit: queue.audit}
	service.SetExecutionServices(queue, follows)
	owner.Permissions = map[string]struct{}{authz.PermissionFollowsReadOwn: {}, authz.PermissionMediaLibrariesRead: {}, authz.PermissionDiscoveryRead: {}}
	owner.ResourceAccessPolicies = map[string]ResourceAccessPolicy{models.ResourceAccessScopeSiteSearch: {Mode: models.ResourceAccessModeAllowlist, ResourceIDs: []string{"10"}}}
	now := time.Now().UTC()
	created, started := now.Add(-time.Minute), now.Add(-time.Second)
	connection := models.Connection{ID: 42, Name: "授权业务来源", NameNormalized: "business-source", Provider: models.ConnectionProviderPan115, CredentialCiphertext: "unused", Enabled: true}
	storage := models.Storage{ID: 7, Name: "授权业务存储", NameNormalized: "business-storage", Type: models.StorageTypePan115, RootPath: "private-provider-root", RootPathNormalized: "business-storage", ConnectionID: &connection.ID, Enabled: true, Capabilities: `{}`}
	library := models.MediaLibrary{ID: 1, Name: "授权业务库", NameNormalized: "business-library", StorageID: storage.ID, ProfileID: 1, RelativeRoot: "/", Enabled: true, VideoExtensionsJSON: `[]`, STRMAssetExtraExtensionsJSON: `[]`, IgnorePatternsJSON: `[]`}
	snapshot := `{"version":2,"media_library_id":1,"site_ids":[10]}`
	subscription := models.FollowSubscription{ID: "follow-safe", OwnerID: owner.User.ID, MediaType: "tv", TMDBID: 1, Title: "正常作品", Status: models.FollowStatusActive, Revision: 1, LifecycleRevision: 1, ExecutionSnapshotJSON: snapshot, CreatedAt: created, UpdatedAt: created}
	job := models.Job{ID: "follow-job", OwnerID: &owner.User.ID, JobType: JobTypeFollowSearch, DisplayName: "自动追更 · 正常作品", Status: models.JobStatusCompleted, CreatedAt: created, UpdatedAt: now, AttemptCount: 2}
	run := models.FollowRun{ID: "follow-run", SubscriptionID: subscription.ID, OwnerID: owner.User.ID, SubscriptionRevision: 1, LifecycleRevision: 1, ExecutionSnapshotJSON: snapshot, JobID: job.ID, Trigger: "manual", Status: models.FollowRunSubmitted, SearchedNamesCount: 4, Candidates: 3, Selected: 2, FilterSummaryJSON: `{"already_in_library":7}`, CreatedAt: created, UpdatedAt: now, StartedAt: &started, FinishedAt: &now}
	run.ErrorMessage = "错误 file://private-summary-link/root /etc/private-summary-root password=private-summary-secret"
	for _, row := range []any{&connection, &storage, &library, &subscription, &job, &run, &models.JobAttempt{JobID: job.ID, AttemptNumber: 1, Status: models.JobStatusFailed, StartedAt: created}, &models.JobAttempt{JobID: job.ID, AttemptNumber: 2, Status: models.JobStatusCompleted, StartedAt: started}} {
		if err := queue.db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	ctx := logging.WithBusinessFields(context.Background(), map[string]any{"job_id": job.ID, "follow_id": subscription.ID, "follow_run_id": run.ID, "attempt_number": 2, "library_id": uint(1)})
	logger := manager.Logger("follow", "worker")
	logging.RecordBusiness(ctx, logger, logging.OperationFollowSearch, logging.BusinessEvent{Code: "follow.site.finish", Stage: "search", Outcome: "finished", Message: "正常站点返回 3 条资源", Fields: map[string]any{"site_id": uint(10), "site_name": "正常站点", "raw_results": 3, "cookie": "must-not-leak", "connection_id": connection.ID, "storage_id": storage.ID, "source_name": connection.Name, "target_relative_path": "动画/正常作品/第01集.mkv"}})
	logging.RecordBusiness(ctx, logger, logging.OperationFollowSearch, logging.BusinessEvent{Code: "follow.site.finish", Stage: "search", Outcome: "finished", Message: "已撤销站点的机密资源", Fields: map[string]any{"site_id": uint(20), "site_name": "已撤销站点", "samples": []string{"机密文件.mkv"}}})
	logging.RecordBusiness(ctx, logger, logging.OperationFollowSearch, logging.BusinessEvent{Code: "follow.site.finish", Stage: "search", Outcome: "finished", Message: "伪关联连接的机密来源", Fields: map[string]any{"site_id": uint(10), "connection_id": uint(99), "source_name": "其他任务的来源"}})
	older := logging.WithBusinessFields(context.Background(), map[string]any{"job_id": job.ID, "follow_id": subscription.ID, "follow_run_id": run.ID, "attempt_number": 1})
	logging.RecordBusiness(older, logger, logging.OperationFollowSearch, logging.BusinessEvent{Code: "follow.old", Stage: "search", Outcome: "failed", Message: "旧尝试不应混入"})
	page, err := service.FollowExecutionLogs(context.Background(), owner, subscription.ID, run.ID, ExecutionLogFilter{})
	if err != nil || len(page.List) != 1 || !page.Partial || page.AttemptNumber == nil || *page.AttemptNumber != 2 {
		t.Fatalf("follow-only read page=%+v err=%v", page, err)
	}
	if page.Summary == nil || page.Summary.AcceptedSubmissions == nil || *page.Summary.AcceptedSubmissions != 2 || page.Summary.SearchedNamesCount == nil || *page.Summary.SearchedNamesCount != 4 || page.Summary.FilterSummary["already_in_library"] != 7 || page.Summary.Status != models.FollowRunSubmitted {
		t.Fatalf("persisted summary changed units: %+v", page.Summary)
	}
	if page.List[0].Fields["connection_id"] != nil || page.List[0].Fields["storage_id"] != nil || page.List[0].Fields["source_name"] != connection.Name || page.List[0].Fields["target_relative_path"] != "动画/正常作品/第01集.mkv" {
		t.Fatalf("ordinary reader needs no infrastructure admin grant: %+v", page.List[0])
	}
	data, _ := json.Marshal(page)
	for _, secret := range []string{"机密", "已撤销", "must-not-leak", "旧尝试", "private-summary"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("scoped leak %q: %s", secret, data)
		}
	}
	if _, err := service.JobExecutionLogs(context.Background(), owner, job.ID, ExecutionLogFilter{}); ErrorCode(err) != CodePermissionDenied {
		t.Fatalf("follow authority bypassed jobs permission: %v", err)
	}
	jobsOnly := Actor{User: owner.User, Permissions: map[string]struct{}{authz.PermissionJobsReadOwn: {}, authz.PermissionMediaLibrariesRead: {}, authz.PermissionDiscoveryRead: {}}}
	page, err = service.JobExecutionLogs(context.Background(), jobsOnly, job.ID, ExecutionLogFilter{})
	if err != nil || page.Availability != "restricted" || len(page.List) != 0 || page.Summary.AcceptedSubmissions != nil {
		t.Fatalf("jobs-only revealed follow details: %+v err=%v", page, err)
	}
	foreign := owner
	foreign.User.ID++
	if _, err := service.FollowExecutionLogs(context.Background(), foreign, subscription.ID, run.ID, ExecutionLogFilter{}); ErrorCode(err) != CodePermissionDenied {
		t.Fatalf("cross owner: %v", err)
	}
	if _, err := service.FollowExecutionLogs(context.Background(), owner, subscription.ID, "foreign-run", ExecutionLogFilter{}); ErrorCode(err) != CodeNotFound {
		t.Fatalf("cross run: %v", err)
	}
	revoked := owner
	revoked.ResourceAccessPolicies = map[string]ResourceAccessPolicy{models.ResourceAccessScopeLibraryRead: {Mode: models.ResourceAccessModeDenylist, ResourceIDs: []string{"1"}}}
	page, err = service.FollowExecutionLogs(context.Background(), revoked, subscription.ID, run.ID, ExecutionLogFilter{})
	if err != nil || page.Availability != "restricted" || len(page.List) != 0 || strings.Contains(page.Summary.Title, "正常作品") {
		t.Fatalf("revoked library leaked: %+v err=%v", page, err)
	}
	page, err = service.FollowExecutionLogs(context.Background(), owner, subscription.ID, run.ID, ExecutionLogFilter{Attempt: 1})
	if err != nil || len(page.List) != 1 || !strings.Contains(page.List[0].Message, "旧尝试") || page.Summary != nil {
		t.Fatalf("attempt isolation: %+v err=%v", page, err)
	}
	t.Run("system connection and refresh bindings", func(t *testing.T) {
		reader := Actor{User: owner.User, Permissions: map[string]struct{}{authz.PermissionJobsReadAll: {}, authz.PermissionMediaLibrariesRead: {}}}
		cleanup := models.Job{ID: "cleanup-job", CreatedByKind: "system", JobType: JobTypePan115RecycleCleanup, ResourceKey: "connection:42", PayloadJSON: `{"connection_id":42,"revision":1}`, DisplayName: "清理回收站", Status: models.JobStatusCompleted, CreatedAt: created, UpdatedAt: now}
		target := models.MediaServerRefreshTarget{ID: 21, LibraryID: library.ID, ConnectionID: connection.ID, UpstreamLibraryID: "private-upstream-id", UpstreamLibraryName: "动画"}
		refresh := models.Job{ID: "refresh-job", CreatedByKind: "system", JobType: JobTypeMediaServerRefresh, ResourceKey: "media-server-target:21", PayloadJSON: `{"target_id":21}`, DisplayName: "刷新媒体服务器", Status: models.JobStatusCompleted, CreatedAt: created, UpdatedAt: now}
		for _, row := range []any{&cleanup, &target, &refresh} {
			if err := queue.db.Create(row).Error; err != nil {
				t.Fatal(err)
			}
		}
		for _, item := range []models.Job{cleanup, refresh} {
			executionCtx := logging.WithBusinessFields(context.Background(), map[string]any{"job_id": item.ID, "connection_id": connection.ID})
			fields := map[string]any{"source_name": "真实绑定的安全显示名"}
			if item.ID == refresh.ID {
				fields["library_id"] = library.ID
			}
			logging.RecordBusiness(executionCtx, logger, logging.OperationMediaServerRefresh, logging.BusinessEvent{Code: "system.confirmed", Stage: "finish", Outcome: "confirmed", Message: "真实绑定操作已确认", Fields: fields})
		}
		page, err := service.JobExecutionLogs(context.Background(), reader, cleanup.ID, ExecutionLogFilter{})
		if err != nil || len(page.List) != 0 {
			t.Fatalf("connection management authority bypassed: %+v %v", page, err)
		}
		logging.RecordBusiness(context.Background(), logger, logging.OperationPan115RecycleCleanup, logging.BusinessEvent{Code: "cleanup.private", Stage: "cleanup", Outcome: "confirmed", Message: "连接名称不应在缺少数值字段时绕过权限", Fields: map[string]any{"job_id": cleanup.ID}})
		page, err = service.JobExecutionLogs(context.Background(), reader, cleanup.ID, ExecutionLogFilter{})
		if err != nil || len(page.List) != 0 || page.Availability != "restricted" {
			t.Fatalf("unreferenced name bypassed job binding: %+v %v", page, err)
		}
		reader.Permissions[authz.PermissionConnectionsRead] = struct{}{}
		page, err = service.JobExecutionLogs(context.Background(), reader, cleanup.ID, ExecutionLogFilter{})
		if err != nil || len(page.List) != 2 {
			t.Fatalf("authorized cleanup logs hidden: %+v %v", page, err)
		}
		if err := queue.db.Model(&cleanup).Update("payload_json", `{"connection_id":99,"revision":1}`).Error; err != nil {
			t.Fatal(err)
		}
		page, err = service.JobExecutionLogs(context.Background(), reader, cleanup.ID, ExecutionLogFilter{})
		if err != nil || len(page.List) != 0 {
			t.Fatalf("mismatched system binding admitted: %+v %v", page, err)
		}
		delete(reader.Permissions, authz.PermissionConnectionsRead)
		page, err = service.JobExecutionLogs(context.Background(), reader, refresh.ID, ExecutionLogFilter{})
		if err != nil || len(page.List) != 1 || page.List[0].Fields["connection_id"] != nil {
			t.Fatalf("safe library refresh needs no connection management grant: %+v %v", page, err)
		}
		reader.ResourceAccessPolicies = map[string]ResourceAccessPolicy{models.ResourceAccessScopeLibraryRead: {Mode: models.ResourceAccessModeDenylist, ResourceIDs: []string{"1"}}}
		page, err = service.JobExecutionLogs(context.Background(), reader, refresh.ID, ExecutionLogFilter{})
		if err != nil || len(page.List) != 0 {
			t.Fatalf("revoked refresh library leaked: %+v %v", page, err)
		}
	})
}

func TestExecutionLogsMissingHistoryAndFilterBinding(t *testing.T) {
	queue, actor, _ := queueFixture(t)
	manager, err := logging.NewManager(t.TempDir(), "production", &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	service := &RuntimeLogService{db: queue.db, manager: manager}
	service.SetExecutionServices(queue, nil)
	created := time.Now().UTC().Add(-time.Minute)
	job := models.Job{ID: "ordinary-job", OwnerID: &actor.User.ID, JobType: "fake", DisplayName: "普通任务", Status: models.JobStatusCompleted, CreatedAt: created, UpdatedAt: created}
	if err := queue.db.Create(&job).Error; err != nil {
		t.Fatal(err)
	}
	page, err := service.JobExecutionLogs(context.Background(), actor, job.ID, ExecutionLogFilter{})
	if err != nil || page.Availability != "unavailable" || len(page.List) != 0 || page.Summary == nil {
		t.Fatalf("unknown history=%+v err=%v", page, err)
	}
	ctx := logging.WithBusinessFields(context.Background(), map[string]any{"job_id": job.ID})
	logger := manager.Logger("test", "worker")
	for _, text := range []string{"准备文件", "移动已确认"} {
		logging.RecordBusiness(ctx, logger, logging.OperationMediaTransfer, logging.BusinessEvent{Code: "transfer.step", Stage: "transfer", Outcome: "confirmed", Message: text})
	}
	page, err = service.JobExecutionLogs(context.Background(), actor, job.ID, ExecutionLogFilter{Limit: 1})
	if err != nil || page.NextCursor == "" || len(page.List) != 1 {
		t.Fatalf("first page=%+v err=%v", page, err)
	}
	next, err := service.JobExecutionLogs(context.Background(), actor, job.ID, ExecutionLogFilter{Limit: 1, Cursor: page.NextCursor, SnapshotTo: page.SnapshotTo})
	if err != nil || len(next.List) != 1 || next.List[0].EventID == page.List[0].EventID {
		t.Fatalf("next page=%+v err=%v", next, err)
	}
	if _, err := service.JobExecutionLogs(context.Background(), actor, job.ID, ExecutionLogFilter{Cursor: page.NextCursor}); ErrorCode(err) != CodeRuntimeLogFilterInvalid {
		t.Fatalf("missing snapshot accepted: %v", err)
	}
	if _, err := service.JobExecutionLogs(context.Background(), actor, job.ID, ExecutionLogFilter{Cursor: page.NextCursor, SnapshotTo: page.SnapshotTo, Keyword: "different"}); ErrorCode(err) != CodeRuntimeLogFilterInvalid {
		t.Fatalf("cursor changed filter: %v", err)
	}
}

func TestExecutionLogsScopeBoundsAndLinkedDomainAuthority(t *testing.T) {
	queue, actor, _ := queueFixture(t)
	manager, err := logging.NewManager(t.TempDir(), "production", &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	service := &RuntimeLogService{db: queue.db, manager: manager}
	service.SetExecutionServices(queue, NewFollowService(queue.db, queue.audit, queue, nil, nil))
	actor.Permissions = map[string]struct{}{authz.PermissionJobsReadOwn: {}, authz.PermissionMediaLibrariesRead: {}, authz.PermissionDiscoveryRead: {}}
	actor.ResourceAccessPolicies = map[string]ResourceAccessPolicy{models.ResourceAccessScopeLibraryRead: {Mode: models.ResourceAccessModeDenylist, ResourceIDs: []string{"99"}}, models.ResourceAccessScopeSiteSearch: {Mode: models.ResourceAccessModeDenylist, ResourceIDs: []string{"100"}}}
	now := time.Now().UTC()
	foreignID := actor.User.ID + 1
	foreignUser := models.User{ID: foreignID, Username: "foreign-log-owner", UsernameNormalized: "foreign-log-owner", DisplayName: "其他用户", PasswordHash: "unused", Status: models.UserStatusActive, CreatedAt: now, UpdatedAt: now}
	parent := models.Job{ID: "scope-parent", OwnerID: &actor.User.ID, JobType: "fake", DisplayName: "普通任务", Status: models.JobStatusCompleted, CreatedAt: now.Add(-time.Minute), UpdatedAt: now}
	foreign := models.Job{ID: "foreign-child", OwnerID: &foreignID, JobType: "fake", DisplayName: "其他用户任务", Status: models.JobStatusCompleted, CreatedAt: parent.CreatedAt, UpdatedAt: now}
	child := models.Job{ID: "follow-child", OwnerID: &actor.User.ID, JobType: JobTypeFollowSearch, DisplayName: "追更任务", Status: models.JobStatusCompleted, CreatedAt: parent.CreatedAt, UpdatedAt: now}
	subscription := models.FollowSubscription{ID: "linked-follow", OwnerID: actor.User.ID, MediaType: "tv", TMDBID: 1, Title: "私有追更作品", Status: models.FollowStatusActive, Revision: 1, LifecycleRevision: 1, ExecutionSnapshotJSON: `{}`, CreatedAt: parent.CreatedAt, UpdatedAt: now}
	run := models.FollowRun{ID: "linked-run", SubscriptionID: subscription.ID, OwnerID: actor.User.ID, SubscriptionRevision: 1, LifecycleRevision: 1, JobID: child.ID, ExecutionSnapshotJSON: `{"media_library_id":1}`, Trigger: "manual", Status: models.FollowRunCompleted, CreatedAt: parent.CreatedAt, UpdatedAt: now}
	for _, row := range []any{&foreignUser, &parent, &foreign, &child, &subscription, &run} {
		if err := queue.db.Create(row).Error; err != nil {
			t.Fatalf("create %T: %v", row, err)
		}
	}
	orphan := models.Job{ID: "orphan-download-job", OwnerID: &actor.User.ID, JobType: "download", DisplayName: "机密孤立下载名称", Status: models.JobStatusCompleted, CreatedAt: parent.CreatedAt, UpdatedAt: now}
	if err := queue.db.Create(&orphan).Error; err != nil {
		t.Fatal(err)
	}
	logging.RecordBusiness(context.Background(), manager.Logger("test", "scope"), logging.OperationDownloadTask, logging.BusinessEvent{Code: "scope.orphan", Stage: "enqueue", Outcome: "accepted", Message: "机密孤立下载细节", Fields: map[string]any{"job_id": orphan.ID}})
	orphanPage, err := service.JobExecutionLogs(context.Background(), actor, orphan.ID, ExecutionLogFilter{})
	if err != nil || orphanPage.Availability != "restricted" || len(orphanPage.List) != 0 || strings.Contains(orphanPage.Summary.Title, "机密") {
		t.Fatalf("orphan domain details were allowed: %+v %v", orphanPage, err)
	}
	ctx := logging.WithBusinessFields(context.Background(), map[string]any{"job_id": parent.ID, "user_id": actor.User.ID})
	logger := manager.Logger("test", "scope")
	logging.RecordBusiness(ctx, logger, logging.OperationMediaTransfer, logging.BusinessEvent{Code: "scope.safe", Stage: "move", Outcome: "confirmed", Message: "允许显示的普通任务事实"})
	diagnostics := map[string]any{}
	for i := 0; i < 80; i++ {
		diagnostics[fmt.Sprintf("extra_%d", i)] = strings.Repeat("z", 4096)
	}
	logging.RecordBusiness(ctx, logger.With().Fields(diagnostics).Logger(), logging.OperationMediaTransfer, logging.BusinessEvent{Code: "scope.compacted", Stage: "move", Outcome: "confirmed", Message: "机密压缩后的库名", Fields: map[string]any{"source_library_id": uint(99)}})
	ids := make([]uint, 100)
	for i := range ids {
		ids[i] = uint(i + 1)
	}
	logging.RecordBusiness(ctx, logger, logging.OperationMediaTransfer, logging.BusinessEvent{Code: "scope.sites", Stage: "move", Outcome: "confirmed", Message: "机密末尾站点样本", Fields: map[string]any{"site_ids": ids, "samples": []string{"不可见资源"}}})
	overflowIDs := make([]uint, 101)
	for i := range overflowIDs {
		overflowIDs[i] = 1
	}
	logging.RecordBusiness(ctx, logger, logging.OperationMediaTransfer, logging.BusinessEvent{Code: "scope.overflow", Stage: "move", Outcome: "confirmed", Message: "机密不完整资源范围", Fields: map[string]any{"library_ids": overflowIDs}})
	logging.RecordBusiness(context.Background(), logger, logging.OperationMediaTransfer, logging.BusinessEvent{Code: "scope.wrong.owner", Stage: "move", Outcome: "confirmed", Message: "机密错误归属记录", Fields: map[string]any{"job_id": parent.ID, "user_id": foreignID}})
	logging.RecordBusiness(ctx, logger, logging.OperationMediaTransfer, logging.BusinessEvent{Code: "scope.foreign", Stage: "enqueue", Outcome: "accepted", Message: "机密其他用户任务", Fields: map[string]any{"related_job_id": foreign.ID}})
	logging.RecordBusiness(ctx, logger, logging.OperationUnifiedSchedule, logging.BusinessEvent{Code: "scope.follow.linked", Stage: "enqueue", Outcome: "accepted", Message: "域内关联追更任务", Fields: map[string]any{"related_job_id": child.ID}})
	logging.RecordBusiness(ctx, logger, logging.OperationUnifiedSchedule, logging.BusinessEvent{Code: "scope.follow.target", Stage: "enqueue", Outcome: "accepted", Message: "域内追更计划目标", Fields: map[string]any{"follow_id": subscription.ID}})
	page, err := service.JobExecutionLogs(context.Background(), actor, parent.ID, ExecutionLogFilter{})
	if err != nil || len(page.List) != 1 || !page.Partial {
		t.Fatalf("jobs-only scope failed: %+v %v", page, err)
	}
	actor.Permissions[authz.PermissionFollowsReadOwn] = struct{}{}
	page, err = service.JobExecutionLogs(context.Background(), actor, parent.ID, ExecutionLogFilter{})
	if err != nil || len(page.List) != 3 {
		t.Fatalf("current follow authority did not open its domain: %+v %v", page, err)
	}
	encoded, _ := json.Marshal(page)
	if strings.Contains(string(encoded), "机密") || strings.Contains(string(encoded), foreign.ID) {
		t.Fatalf("revoked/foreign details leaked: %s", encoded)
	}
}

func TestExecutionLogsSnapshotChoosesAttemptAndDoesNotCopyFutureSummary(t *testing.T) {
	queue, actor, _ := queueFixture(t)
	manager, err := logging.NewManager(t.TempDir(), "production", &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	service := &RuntimeLogService{db: queue.db, manager: manager}
	service.SetExecutionServices(queue, nil)
	now := time.Now().UTC()
	firstStart, firstEnd, secondStart, secondEnd := now.Add(-3*time.Minute), now.Add(-2*time.Minute), now.Add(-time.Minute), now.Add(-10*time.Second)
	job := models.Job{ID: "snapshot-attempt", OwnerID: &actor.User.ID, JobType: "fake", DisplayName: "后来的摘要", Status: models.JobStatusCompleted, AttemptCount: 2, CreatedAt: firstStart.Add(-time.Second), UpdatedAt: secondEnd, FinishedAt: &secondEnd}
	for _, row := range []any{&job, &models.JobAttempt{JobID: job.ID, AttemptNumber: 1, StartedAt: firstStart, FinishedAt: &firstEnd, Status: models.JobStatusFailed}, &models.JobAttempt{JobID: job.ID, AttemptNumber: 2, StartedAt: secondStart, FinishedAt: &secondEnd, Status: models.JobStatusCompleted}} {
		if err := queue.db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, observation := range []struct {
		attempt int
		at      time.Time
		text    string
	}{{1, firstStart.Add(time.Second), "第一次执行事实"}, {1, secondStart.Add(time.Second), "旧尝试结束后不属于其时间窗"}, {2, secondStart.Add(time.Second), "第二次执行事实"}} {
		logger := zerolog.New(manager).With().Str(zerolog.TimestampFieldName, observation.at.Format(time.RFC3339Nano)).Str("module", "test").Str("component", "snapshot").Logger()
		ctx := logging.WithBusinessFields(context.Background(), map[string]any{"job_id": job.ID, "attempt_number": observation.attempt, "generation": observation.attempt, "user_id": actor.User.ID})
		logging.RecordBusiness(ctx, logger, logging.OperationMediaTransfer, logging.BusinessEvent{Code: "attempt.fact", Stage: "move", Outcome: "confirmed", Message: observation.text})
	}
	page, err := service.JobExecutionLogs(context.Background(), actor, job.ID, ExecutionLogFilter{SnapshotTo: firstEnd.Add(time.Second)})
	if err != nil || page.AttemptNumber == nil || *page.AttemptNumber != 1 || len(page.List) != 1 || page.Summary != nil || !strings.Contains(page.List[0].Message, "第一次") {
		t.Fatalf("snapshot mixed later attempt/summary: %+v %v", page, err)
	}
	page, err = service.JobExecutionLogs(context.Background(), actor, job.ID, ExecutionLogFilter{Attempt: 1})
	if err != nil || len(page.List) != 1 || page.Summary != nil {
		t.Fatalf("finished attempt admitted later observations: %+v %v", page, err)
	}
	page, err = service.JobExecutionLogs(context.Background(), actor, job.ID, ExecutionLogFilter{})
	if err != nil || page.AttemptNumber == nil || *page.AttemptNumber != 2 || len(page.List) != 1 || page.Summary == nil {
		t.Fatalf("latest attempt lost current facts: %+v %v", page, err)
	}
}

func TestRuntimeLogExportReportsBoundedCoverageAndSanitizes(t *testing.T) {
	queue, actor, _ := queueFixture(t)
	manager, err := logging.NewManager(t.TempDir(), "production", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	service := &RuntimeLogService{db: queue.db, manager: manager, audit: queue.audit}
	actor.Permissions[authz.PermissionLogsExport] = struct{}{}
	logger := manager.Logger("test", "export")
	logger.Info().Str("cookie", "private-export-cookie").Msg("安全导出记录")
	payload, count, partial, err := service.ExportWithCoverage(context.Background(), actor, logging.Filter{}, RequestContext{})
	if err != nil || count != 1 || partial {
		t.Fatalf("complete export count=%d partial=%v err=%v", count, partial, err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := io.ReadAll(gz)
	_ = gz.Close()
	if err != nil || bytes.Contains(plain, []byte("private-export-cookie")) || !bytes.Contains(plain, []byte("***redacted***")) {
		t.Fatalf("unsafe export: %s %v", plain, err)
	}
	for i := 0; i < 5000; i++ {
		logger.Info().Int("index", i).Msg("有界导出记录")
	}
	logging.OperationMediaTransfer.Event(logger.Info()).Uint("library_id", 99).Msg("没有权限的条目不能改变导出上限")
	_, count, partial, err = service.ExportWithCoverage(context.Background(), actor, logging.Filter{}, RequestContext{})
	if err != nil || count != 5000 || !partial {
		t.Fatalf("capped export count=%d partial=%v err=%v", count, partial, err)
	}
}

func TestRuntimeLogGlobalQueryFacetsAndExportRespectCurrentResources(t *testing.T) {
	queue, actor, _ := queueFixture(t)
	manager, err := logging.NewManager(t.TempDir(), "production", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	service := &RuntimeLogService{db: queue.db, manager: manager, audit: queue.audit}
	actor.Permissions = map[string]struct{}{authz.PermissionLogsRead: {}, authz.PermissionLogsExport: {}, authz.PermissionMediaLibrariesRead: {}, authz.PermissionDiscoveryRead: {}}
	actor.ResourceAccessPolicies = map[string]ResourceAccessPolicy{
		models.ResourceAccessScopeLibraryRead:   {Mode: models.ResourceAccessModeDenylist, ResourceIDs: []string{"99"}},
		models.ResourceAccessScopeSiteSearch:    {Mode: models.ResourceAccessModeDenylist, ResourceIDs: []string{"20"}},
		models.ResourceAccessScopeDownloaderUse: {Mode: models.ResourceAccessModeDenylist, ResourceIDs: []string{"hidden-downloader"}},
	}
	now := time.Now().UTC()
	job := models.Job{ID: "global-private-job", OwnerID: &actor.User.ID, JobType: "fake", ResourceKey: "library:99", DisplayName: "机密任务", Status: models.JobStatusCompleted, CreatedAt: now.Add(-time.Minute), UpdatedAt: now}
	if err := queue.db.Create(&job).Error; err != nil {
		t.Fatal(err)
	}
	for _, observation := range []struct {
		module string
		fields map[string]any
	}{
		{"visible", map[string]any{"library_id": uint(1), "site_id": uint(10), "downloader_id": "allowed-downloader"}},
		{"private-site", map[string]any{"site_id": uint(20), "samples": []string{"机密站点资源"}}},
		{"private-library", map[string]any{"library_id": uint(99), "library_name": "机密媒体库"}},
		{"private-downloader", map[string]any{"downloader_id": "hidden-downloader", "downloader_name": "机密下载器"}},
		{"private-job", map[string]any{"job_id": job.ID}},
		{"private-related", map[string]any{"related_job_id": job.ID}},
		{"private-unbound-infrastructure", map[string]any{"library_id": uint(1), "connection_id": uint(99), "source_name": "未知关联的机密来源"}},
	} {
		logging.RecordBusiness(context.Background(), manager.Logger(observation.module, "test"), logging.OperationMediaTransfer, logging.BusinessEvent{Code: "global.observe", Stage: "move", Outcome: "confirmed", Message: observation.module, Fields: observation.fields})
	}
	legacy := manager.Logger("private-legacy", "test")
	logging.OperationDownloadTask.Event(legacy.Info()).Str("task_id", job.ID).Str("resource_name", "机密旧任务资源").Msg("机密旧任务资源")
	logging.OperationFollowSearch.Event(legacy.Info()).Msg("机密无资源关联的旧追更日志")
	logging.OperationDiscoverySearch.Event(legacy.Info()).Str("site_id", "020").Msg("机密旧字符串站点引用")
	logging.RecordBusiness(context.Background(), manager.Logger("auth", "test"), logging.OperationAuthentication, logging.BusinessEvent{Code: "auth.login", Stage: "login", Outcome: "confirmed", Message: "登录成功"})
	page, err := service.Query(context.Background(), actor, logging.Filter{})
	if err != nil || len(page.List) != 2 || !page.Partial {
		t.Fatalf("global read widened resource scope: %+v %v", page, err)
	}
	encoded, _ := json.Marshal(page)
	if strings.Contains(string(encoded), "private-") || strings.Contains(string(encoded), "机密") {
		t.Fatalf("global query leaked names/text: %s", encoded)
	}
	facets, err := service.Facets(context.Background(), actor, logging.Filter{})
	encoded, _ = json.Marshal(facets)
	if err != nil || strings.Contains(string(encoded), "private-") || len(facets.Modules) != 2 {
		t.Fatalf("facets bypassed current resources: %s %v", encoded, err)
	}
	// Export remains its own global grant; it still uses the identical resource
	// projection even when this reader has no logs.read or jobs/follow grants.
	delete(actor.Permissions, authz.PermissionLogsRead)
	payload, count, partial, err := service.ExportWithCoverage(context.Background(), actor, logging.Filter{}, RequestContext{})
	if err != nil || count != 2 || !partial {
		t.Fatalf("global export widened resource scope: count=%d partial=%v err=%v", count, partial, err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := io.ReadAll(gz)
	_ = gz.Close()
	if err != nil || bytes.Contains(plain, []byte("private-")) || bytes.Contains(plain, []byte("机密")) {
		t.Fatalf("global export leaked names/text: %s %v", plain, err)
	}
}
