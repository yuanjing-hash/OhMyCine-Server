package services

import (
	"fmt"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func historyAdmin(actor Actor) Actor {
	for _, code := range []string{
		authz.PermissionJobsControlAll,
		authz.PermissionDownloadsManageAll,
		authz.PermissionSTRMCleanup,
		authz.PermissionMediaLibrariesScan,
		authz.PermissionFollowsExecuteAll,
		authz.PermissionSettingsUpdate,
		authz.PermissionSystemAdmin,
	} {
		actor.Permissions[code] = struct{}{}
	}
	return actor
}

func historyJob(t *testing.T, service *QueueService, owner uint, id, status string) models.Job {
	t.Helper()
	now := service.clock.Now()
	job := models.Job{
		ID: id, OwnerID: &owner, CreatedByKind: "user", JobType: "history-test",
		Status: status, Revision: 1, PayloadJSON: `{}`, CheckpointJSON: `{}`,
		DisplayName: id, CreatedAt: now, UpdatedAt: now,
	}
	if containsString(historyTerminalStatuses(), status) {
		job.FinishedAt = &now
	}
	if err := service.db.Create(&job).Error; err != nil {
		t.Fatal(err)
	}
	return job
}

func historyLibrary(t *testing.T, service *QueueService, suffix string) models.MediaLibrary {
	t.Helper()
	now := service.clock.Now()
	storage := models.Storage{
		Name: suffix, NameNormalized: suffix, Type: models.StorageTypeLocal,
		RootPath: "/" + suffix, RootPathNormalized: "/" + suffix,
		Capabilities: `{}`, Enabled: true, CreatedAt: now, UpdatedAt: now,
	}
	if err := service.db.Create(&storage).Error; err != nil {
		t.Fatal(err)
	}
	var profile models.MediaClassificationProfile
	if err := service.db.First(&profile).Error; err != nil {
		t.Fatal(err)
	}
	library := models.MediaLibrary{
		Name: suffix, NameNormalized: suffix, StorageID: storage.ID, ProfileID: profile.ID,
		ProfileRevision: profile.Revision, RelativeRoot: "/", VideoExtensionsJSON: `[]`,
		STRMAssetExtraExtensionsJSON: `[]`, IgnorePatternsJSON: `[]`,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := service.db.Create(&library).Error; err != nil {
		t.Fatal(err)
	}
	return library
}

func TestManagementHistoryPurgePagesAllTerminalTasksAndKeepsActiveStates(t *testing.T) {
	service, actor, _ := queueFixture(t)
	actor = historyAdmin(actor)
	for index := 0; index < historyPurgeBatchSize+5; index++ {
		historyJob(t, service, actor.User.ID, fmt.Sprintf("history-%03d", index), models.JobStatusCompleted)
	}
	for index, status := range []string{
		models.JobStatusQueued, models.JobStatusRunning, models.JobStatusRetryWait,
		models.JobStatusWaitingUserAction, models.JobStatusPaused,
	} {
		historyJob(t, service, actor.User.ID, fmt.Sprintf("active-%d", index), status)
	}

	preview, err := service.PreviewHistoryPurge(actor, HistoryPurgeInput{Scope: HistoryScopeTasks})
	if err != nil {
		t.Fatal(err)
	}
	if preview.Deleted != historyPurgeBatchSize+5 || preview.Skipped != 5 {
		t.Fatalf("preview=%+v", preview)
	}
	result, err := service.PurgeHistory(actor, HistoryPurgeInput{Scope: HistoryScopeTasks}, RequestContext{RequestID: "purge"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Deleted != historyPurgeBatchSize+5 || result.Skipped != 5 {
		t.Fatalf("result=%+v", result)
	}
	var visible, active, audits int64
	if err := service.db.Model(&models.Job{}).Where("id LIKE 'history-%' AND history_cleared_at IS NULL").Count(&visible).Error; err != nil {
		t.Fatal(err)
	}
	if err := service.db.Model(&models.Job{}).Where("id LIKE 'active-%' AND history_cleared_at IS NULL").Count(&active).Error; err != nil {
		t.Fatal(err)
	}
	if err := service.db.Model(&models.AuditLog{}).Where("action = ? AND request_id = ?", "management_history.purge", "purge").Count(&audits).Error; err != nil {
		t.Fatal(err)
	}
	if visible != 0 || active != 5 || audits != 2 {
		t.Fatalf("visible=%d active=%d audits=%d", visible, active, audits)
	}
}

func TestManagementHistoryPurgeProtectsFollowS0E0Claim(t *testing.T) {
	service, actor, _ := queueFixture(t)
	actor = historyAdmin(actor)
	now := service.clock.Now()
	job := historyJob(t, service, actor.User.ID, "follow-s0e0-job", models.JobStatusCompleted)
	subscription := models.FollowSubscription{
		ID: "follow-s0e0", OwnerID: actor.User.ID, MediaType: "tv", TMDBID: 1,
		Title: "Special", Status: models.FollowStatusActive, Revision: 1, LifecycleRevision: 1,
		ExecutionSnapshotJSON: `{}`, CreatedAt: now, UpdatedAt: now,
	}
	if err := service.db.Create(&subscription).Error; err != nil {
		t.Fatal(err)
	}
	run := models.FollowRun{
		ID: "follow-s0e0-run", SubscriptionID: subscription.ID, OwnerID: actor.User.ID,
		SubscriptionRevision: 1, LifecycleRevision: 1, ExecutionSnapshotJSON: `{}`,
		JobID: job.ID, Trigger: "manual", Status: models.FollowRunCompleted,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := service.db.Create(&run).Error; err != nil {
		t.Fatal(err)
	}
	runID := run.ID
	if err := service.db.Exec("PRAGMA ignore_check_constraints = ON").Error; err != nil {
		t.Fatal(err)
	}
	if err := service.db.Create(&models.FollowEpisodeClaim{SubscriptionID: subscription.ID, SeasonNumber: 0, EpisodeNumber: 0, State: "queued", RunID: &runID, UpdatedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	if err := service.db.Exec("PRAGMA ignore_check_constraints = OFF").Error; err != nil {
		t.Fatal(err)
	}

	preview, err := service.PreviewHistoryPurge(actor, HistoryPurgeInput{Scope: HistoryScopeFollowRuns, ResourceID: subscription.ID})
	if err != nil {
		t.Fatal(err)
	}
	if preview.Deleted != 0 || preview.Skipped != 1 {
		t.Fatalf("preview=%+v", preview)
	}
}

func TestManagementHistoryPurgeKeepsSharedJobTimelineUntilLastVisibleDomainRecord(t *testing.T) {
	service, actor, _ := queueFixture(t)
	actor = historyAdmin(actor)
	now := service.clock.Now()
	library := historyLibrary(t, service, "history-shared")
	downloadJob := historyJob(t, service, actor.User.ID, "download-history-job", models.JobStatusCompleted)
	transferJob := historyJob(t, service, actor.User.ID, "transfer-history-job", models.JobStatusCompleted)
	download := models.DownloadTask{
		ID: "download-history", OwnerID: actor.User.ID, JobID: downloadJob.ID,
		DownloaderName: "test", ProviderType: "test", SourceCiphertext: "encrypted",
		DisplayName: "download", Phase: models.DownloadTaskStatusCompleted,
		CreatedAt: now, UpdatedAt: now, FinishedAt: &now,
	}
	if err := service.db.Create(&download).Error; err != nil {
		t.Fatal(err)
	}
	transfer := models.TransferTask{
		ID: "transfer-history", OwnerID: actor.User.ID, JobID: transferJob.ID,
		DownloadTaskID: download.ID, LibraryID: library.ID, LibraryName: library.Name,
		ManifestJSON: `{}`, Phase: models.TransferTaskStatusCompleted,
		CreatedAt: now, UpdatedAt: now, FinishedAt: &now,
	}
	if err := service.db.Create(&transfer).Error; err != nil {
		t.Fatal(err)
	}
	for _, jobID := range []string{downloadJob.ID, transferJob.ID} {
		if err := service.db.Create(&models.JobStatusEvent{JobID: jobID, EventType: "completed", ToStatus: models.JobStatusCompleted, CreatedAt: now}).Error; err != nil {
			t.Fatal(err)
		}
	}

	if _, err := service.PurgeHistory(actor, HistoryPurgeInput{Scope: HistoryScopeDownloads}, RequestContext{}); err != nil {
		t.Fatal(err)
	}
	var events int64
	if err := service.db.Model(&models.JobStatusEvent{}).Where("job_id = ?", transferJob.ID).Count(&events).Error; err != nil || events != 1 {
		t.Fatalf("transfer timeline removed while its history was visible: count=%d err=%v", events, err)
	}
	if _, err := service.PurgeHistory(actor, HistoryPurgeInput{Scope: HistoryScopeOrganization}, RequestContext{}); err != nil {
		t.Fatal(err)
	}
	if err := service.db.Model(&models.JobStatusEvent{}).Where("job_id = ?", transferJob.ID).Count(&events).Error; err != nil || events != 0 {
		t.Fatalf("retired transfer timeline remains: count=%d err=%v", events, err)
	}
	var catalogs int64
	if err := service.db.Model(&models.MediaLibrary{}).Where("id = ?", library.ID).Count(&catalogs).Error; err != nil || catalogs != 1 {
		t.Fatalf("library fact changed: count=%d err=%v", catalogs, err)
	}
}

func TestManagementHistoryPurgeSupportsEveryIndependentHistoryScope(t *testing.T) {
	scopes := []string{
		HistoryScopeTasks, HistoryScopeSTRMRuns, HistoryScopeLibraryScans,
		HistoryScopeDownloads, HistoryScopeOrganization, HistoryScopeFollowRuns,
		HistoryScopeScheduleRuns, HistoryScopeSeeding,
	}
	for _, scope := range scopes {
		t.Run(scope, func(t *testing.T) {
			service, actor, _ := queueFixture(t)
			actor = historyAdmin(actor)
			now := service.clock.Now()
			library := historyLibrary(t, service, "scope-"+scope)
			input := HistoryPurgeInput{Scope: scope}
			job := historyJob(t, service, actor.User.ID, "job-"+scope, models.JobStatusCompleted)

			switch scope {
			case HistoryScopeTasks:
			case HistoryScopeSTRMRuns:
				input.ResourceID = fmt.Sprint(library.ID)
				run := models.MediaArtifactRun{ID: "run-" + scope, LibraryID: library.ID, Generation: 1, PolicyJSON: `{}`, Status: models.MediaArtifactStatusCompleted, CleanupStatus: models.MediaArtifactCleanupSkipped, FinishedAt: &now, CreatedAt: now, UpdatedAt: now}
				if err := service.db.Create(&run).Error; err != nil {
					t.Fatal(err)
				}
			case HistoryScopeLibraryScans:
				input.ResourceID = fmt.Sprint(library.ID)
				if err := service.db.Create(&models.MediaLibraryScanRun{LibraryID: library.ID, Kind: "full", Status: "success", Generation: 1, StartedAt: now, FinishedAt: &now}).Error; err != nil {
					t.Fatal(err)
				}
			case HistoryScopeDownloads:
				row := models.DownloadTask{ID: "download-" + scope, OwnerID: actor.User.ID, JobID: job.ID, DownloaderName: "test", ProviderType: "test", SourceCiphertext: "encrypted", DisplayName: "download", Phase: models.DownloadTaskStatusCompleted, CreatedAt: now, UpdatedAt: now, FinishedAt: &now}
				if err := service.db.Create(&row).Error; err != nil {
					t.Fatal(err)
				}
			case HistoryScopeOrganization:
				downloadJob := historyJob(t, service, actor.User.ID, "download-for-organization", models.JobStatusCompleted)
				download := models.DownloadTask{ID: "download-for-organization", OwnerID: actor.User.ID, JobID: downloadJob.ID, DownloaderName: "test", ProviderType: "test", SourceCiphertext: "encrypted", DisplayName: "download", Phase: models.DownloadTaskStatusCompleted, CreatedAt: now, UpdatedAt: now, FinishedAt: &now}
				if err := service.db.Create(&download).Error; err != nil {
					t.Fatal(err)
				}
				row := models.TransferTask{ID: "transfer-" + scope, OwnerID: actor.User.ID, JobID: job.ID, DownloadTaskID: download.ID, LibraryID: library.ID, LibraryName: library.Name, ManifestJSON: `{}`, Phase: models.TransferTaskStatusCompleted, CreatedAt: now, UpdatedAt: now, FinishedAt: &now}
				if err := service.db.Create(&row).Error; err != nil {
					t.Fatal(err)
				}
			case HistoryScopeSeeding:
				downloadJob := historyJob(t, service, actor.User.ID, "download-for-seeding", models.JobStatusCompleted)
				download := models.DownloadTask{ID: "download-for-seeding", OwnerID: actor.User.ID, JobID: downloadJob.ID, DownloaderName: "test", ProviderType: "test", SourceCiphertext: "encrypted", DisplayName: "download", Phase: models.DownloadTaskStatusCompleted, CreatedAt: now, UpdatedAt: now, FinishedAt: &now}
				if err := service.db.Create(&download).Error; err != nil {
					t.Fatal(err)
				}
				row := models.SeedingTask{ID: "seeding-" + scope, OwnerID: actor.User.ID, JobID: job.ID, DownloadTaskID: download.ID, DownloaderName: "test", ProviderType: "test", ProviderTaskID: "provider", TransferMode: models.MediaLibraryTransferCopy, Phase: models.SeedingTaskStatusCompleted, CreatedAt: now, UpdatedAt: now, FinishedAt: &now}
				if err := service.db.Create(&row).Error; err != nil {
					t.Fatal(err)
				}
			case HistoryScopeFollowRuns:
				subscription := models.FollowSubscription{ID: "follow-" + scope, OwnerID: actor.User.ID, MediaType: "tv", TMDBID: 1, Title: "show", Status: models.FollowStatusActive, Revision: 1, LifecycleRevision: 1, ExecutionSnapshotJSON: `{}`, CreatedAt: now, UpdatedAt: now}
				if err := service.db.Create(&subscription).Error; err != nil {
					t.Fatal(err)
				}
				input.ResourceID = subscription.ID
				run := models.FollowRun{ID: "run-" + scope, SubscriptionID: subscription.ID, OwnerID: actor.User.ID, SubscriptionRevision: 1, LifecycleRevision: 1, ExecutionSnapshotJSON: `{}`, JobID: job.ID, Trigger: "manual", Status: models.FollowRunCompleted, CreatedAt: now, UpdatedAt: now}
				if err := service.db.Create(&run).Error; err != nil {
					t.Fatal(err)
				}
			case HistoryScopeScheduleRuns:
				schedule := models.ScheduleDefinition{ID: "schedule-" + scope, OwnerID: actor.User.ID, Name: "schedule", ActionType: "test", TargetType: "test", TargetID: "test", CronExpression: "0 0 * * *", Timezone: "UTC", MisfirePolicy: "skip", OverlapPolicy: "skip", Revision: 1, CreatedAt: now, UpdatedAt: now}
				if err := service.db.Create(&schedule).Error; err != nil {
					t.Fatal(err)
				}
				input.ResourceID = schedule.ID
				run := models.ScheduleRun{ID: "run-" + scope, ScheduleID: schedule.ID, JobID: job.ID, ScheduledAt: now, Status: models.JobStatusCompleted, CreatedAt: now, UpdatedAt: now, FinishedAt: &now}
				if err := service.db.Create(&run).Error; err != nil {
					t.Fatal(err)
				}
			}

			preview, err := service.PreviewHistoryPurge(actor, input)
			if err != nil {
				t.Fatal(err)
			}
			if preview.Deleted == 0 {
				t.Fatalf("scope %s has no clearable history: %+v", scope, preview)
			}
			result, err := service.PurgeHistory(actor, input, RequestContext{})
			if err != nil {
				t.Fatal(err)
			}
			if result.Deleted == 0 {
				t.Fatalf("scope %s did not clear history: %+v", scope, result)
			}
		})
	}
}

func TestManagementHistoryPurgeAppliesOwnerAndLibraryResourceIsolation(t *testing.T) {
	service, actor, _ := queueFixture(t)
	now := service.clock.Now()
	other := models.User{Username: "history-other", UsernameNormalized: "history-other", DisplayName: "Other", PasswordHash: "x", Status: models.UserStatusActive, CreatedAt: now, UpdatedAt: now}
	if err := service.db.Create(&other).Error; err != nil {
		t.Fatal(err)
	}
	historyJob(t, service, actor.User.ID, "own-history", models.JobStatusCompleted)
	historyJob(t, service, other.ID, "other-history", models.JobStatusCompleted)
	actor.Permissions = map[string]struct{}{authz.PermissionJobsControlOwn: {}}
	preview, err := service.PreviewHistoryPurge(actor, HistoryPurgeInput{Scope: HistoryScopeTasks})
	if err != nil || preview.Deleted != 1 {
		t.Fatalf("own preview=%+v err=%v", preview, err)
	}

	library := historyLibrary(t, service, "resource-isolation")
	actor.Permissions = map[string]struct{}{authz.PermissionSTRMCleanup: {}}
	actor.ResourceRules = []AuthorizationRule{{PermissionCode: authz.PermissionSTRMCleanup, Effect: models.AuthorizationEffectDeny, ResourceType: models.AuthorizationResourceMediaLibrary, ResourceID: fmt.Sprint(library.ID)}}
	if _, err := service.PreviewHistoryPurge(actor, HistoryPurgeInput{Scope: HistoryScopeSTRMRuns, ResourceID: fmt.Sprint(library.ID)}); ErrorCode(err) != CodePermissionDenied {
		t.Fatalf("denied resource error=%v", err)
	}
	if _, err := service.PreviewHistoryPurge(actor, HistoryPurgeInput{Scope: HistoryScopeSTRMRuns, ResourceID: "not-an-id"}); ErrorCode(err) != CodeInvalidRequest {
		t.Fatalf("invalid resource error=%v", err)
	}
}

func TestManagementHistoryPurgeSkipsUnsettledPhysicalEvidenceAndRetryRestoresHiddenHistory(t *testing.T) {
	service, actor, _ := queueFixture(t)
	actor = historyAdmin(actor)
	now := service.clock.Now()
	library := historyLibrary(t, service, "recovery-history")
	job := historyJob(t, service, actor.User.ID, "recovery-history-job", models.JobStatusFailed)
	run := models.MediaArtifactRun{ID: "recovery-history-run", LibraryID: library.ID, Generation: 1, JobID: &job.ID, PolicyJSON: `{}`, Status: models.MediaArtifactStatusFailed, CreatedAt: now, UpdatedAt: now, FinishedAt: &now}
	if err := service.db.Create(&run).Error; err != nil {
		t.Fatal(err)
	}
	proof := models.CatalogPhysicalWrite{LibraryID: library.ID, OwnerKind: "artifact", OwnerID: run.ID, Revision: 1, State: "entered", JobID: job.ID, OwnerDigest: "owner", SourceFingerprint: "source", ConfigFingerprint: "config", EnteredAt: now, UpdatedAt: now}
	if err := service.db.Create(&proof).Error; err != nil {
		t.Fatal(err)
	}
	preview, err := service.PreviewHistoryPurge(actor, HistoryPurgeInput{Scope: HistoryScopeSTRMRuns, ResourceID: fmt.Sprint(library.ID)})
	if err != nil || preview.Deleted != 0 || preview.Skipped != 1 {
		t.Fatalf("unsettled preview=%+v err=%v", preview, err)
	}
	if err := service.db.Model(&proof).Updates(map[string]any{"state": "settled", "settled_at": now}).Error; err != nil {
		t.Fatal(err)
	}
	result, err := service.PurgeHistory(actor, HistoryPurgeInput{Scope: HistoryScopeSTRMRuns, ResourceID: fmt.Sprint(library.ID)}, RequestContext{})
	if err != nil || result.Deleted != 1 {
		t.Fatalf("settled purge=%+v err=%v", result, err)
	}
	if _, err := service.Control(actor, job.ID, "retry", RequestContext{}); err != nil {
		t.Fatal(err)
	}
	if err := service.db.First(&run, "id = ? AND history_cleared_at IS NULL", run.ID).Error; err != nil {
		t.Fatalf("domain history was not restored by retry: %v", err)
	}
}

func TestManagementHistoryCanClearExactFailedZeroItemArtifactWithoutIO(t *testing.T) {
	service, actor, _ := queueFixture(t)
	actor = historyAdmin(actor)
	library := historyLibrary(t, service, "failed-zero-item-history")
	now := service.clock.Now()
	job := historyJob(t, service, actor.User.ID, "failed-zero-item-history-job", models.JobStatusFailed)
	if err := service.db.Model(&job).Updates(map[string]any{
		"job_type":     JobTypeMediaArtifact,
		"payload_json": `{"artifact_run_id":"failed-zero-item-history-run"}`,
	}).Error; err != nil {
		t.Fatal(err)
	}
	run := models.MediaArtifactRun{
		ID: "failed-zero-item-history-run", LibraryID: library.ID, Generation: 1, JobID: &job.ID,
		PolicyJSON: `{}`, Status: models.MediaArtifactStatusFailed, ErrorCode: "any_terminal_error",
		CleanupStatus: models.MediaArtifactCleanupSkipped, CleanupAt: &now, FinishedAt: &now,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := service.db.Create(&run).Error; err != nil {
		t.Fatal(err)
	}
	for _, input := range []HistoryPurgeInput{
		{Scope: HistoryScopeTasks},
		{Scope: HistoryScopeSTRMRuns, ResourceID: fmt.Sprint(library.ID)},
	} {
		preview, err := service.PreviewHistoryPurge(actor, input)
		if err != nil || preview.Deleted != 1 || preview.Skipped != 0 {
			t.Fatalf("scope=%s preview=%+v err=%v", input.Scope, preview, err)
		}
	}
}
