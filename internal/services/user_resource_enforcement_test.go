package services

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
)

func TestResourceRestrictedTaskViewsRetryAndFrozenExecution(t *testing.T) {
	queue, actor, download, _, destination := transferFixture(t, models.MediaLibraryTransferCopy, models.MediaLibraryConflictOverwrite, false)
	actor.Permissions[authz.PermissionTransfersReadAll] = struct{}{}
	actor.Permissions[authz.PermissionDownloadsReadAll] = struct{}{}
	service := NewTransferService(queue.db, queue.audit, queue, zerolog.Nop())
	manifest := downloadpkg.Manifest{Name: "Movie.2024", Complete: true, Files: []downloadpkg.File{{RelativePath: "Movie.2024.mkv", Size: minimumAutomaticTransferVideoBytes}}}
	if err := service.Enqueue(download, manifest); err != nil {
		t.Fatal(err)
	}
	var task models.TransferTask
	if err := queue.db.First(&task, "download_task_id = ?", download.ID).Error; err != nil {
		t.Fatal(err)
	}
	if job, err := queue.Get(actor, task.JobID); err != nil || !strings.Contains(job.DisplayName, "Movie") {
		t.Fatalf("proven no-site task was hidden: %+v err=%v", job, err)
	}
	legacyPayload, _ := json.Marshal(downloadJobPayload{DownloadTaskID: download.ID})
	if err := queue.db.Model(&models.Job{}).Where("id = ?", download.JobID).Update("payload_json", string(legacyPayload)).Error; err != nil {
		t.Fatal(err)
	}
	limitedSearch := actor
	limitedSearch.ResourceRules = append(append([]AuthorizationRule(nil), actor.ResourceRules...), AuthorizationRule{PermissionCode: authz.PermissionDiscoveryRead, ResourceType: models.AuthorizationResourceSite, ResourceID: "1", Effect: models.AuthorizationEffectAllow})
	if job, err := queue.Get(limitedSearch, task.JobID); err != nil || strings.Contains(job.DisplayName, "Movie") {
		t.Fatalf("unknown legacy source bypassed limited site authority: %+v err=%v", job, err)
	}
	if err := queue.db.Model(&models.Job{}).Where("id = ?", download.JobID).Update("payload_json", "{").Error; err != nil {
		t.Fatal(err)
	}
	if job, err := queue.Get(actor, task.JobID); err != nil || strings.Contains(job.DisplayName, "Movie") {
		t.Fatalf("corrupt task exposed resource details or blocked status: %+v err=%v", job, err)
	}
	currentPayload, _ := json.Marshal(downloadJobPayload{DownloadTaskID: download.ID, ResourceAccessVersion: 1})
	if err := queue.db.Model(&models.Job{}).Where("id = ?", download.JobID).Update("payload_json", string(currentPayload)).Error; err != nil {
		t.Fatal(err)
	}
	policy := models.UserResourceAccessPolicy{UserID: actor.User.ID, Scope: models.ResourceAccessScopeLibraryRead, Mode: models.ResourceAccessModeAllowlist, ResourceIDsJSON: "[]"}
	if err := queue.db.Create(&policy).Error; err != nil {
		t.Fatal(err)
	}
	denied := actor
	denied.ResourceAccessPolicies = map[string]ResourceAccessPolicy{models.ResourceAccessScopeLibraryRead: {Mode: models.ResourceAccessModeAllowlist}}
	page, err := service.List(denied, TransferListFilter{})
	if err != nil || len(page.List) != 1 || page.List[0].LibraryID != 0 || page.List[0].LibraryName != "" || len(page.FilterOptions.Libraries) != 0 || len(page.FilterOptions.Categories) != 0 {
		t.Fatalf("unsafe transfer page=%+v err=%v", page, err)
	}
	detail, err := service.Get(denied, task.ID)
	if err != nil || detail.LibraryID != 0 || detail.PlanSummary != nil || detail.MovieFilenameTemplate != "" || detail.Job.ResourceKey != "" || detail.Job.Action != nil {
		t.Fatalf("unsafe transfer detail=%+v err=%v", detail, err)
	}
	downloads, err := (&DownloadService{db: queue.db}).List(denied, 10)
	if err != nil || len(downloads) != 1 || downloads[0].TargetLibraryID != nil || downloads[0].ScrapeTitle != "" || downloads[0].WaitReason != nil {
		t.Fatalf("unsafe downloads=%+v err=%v", downloads, err)
	}
	job, err := queue.Get(denied, task.JobID)
	if err != nil || job.ResourceKey != "" || strings.Contains(job.DisplayName, "Movie") {
		t.Fatalf("unsafe queue job=%+v err=%v", job, err)
	}
	// An accepted worker uses its frozen plan even after access is revoked.
	claimed, err := queue.Claim([]string{"transfer"})
	if err != nil || claimed == nil {
		t.Fatalf("claim=%+v err=%v", claimed, err)
	}
	result := NewTransferWorker(service).Run(context.Background(), workerRuntime{queue: queue, job: *claimed}, *claimed)
	if result.ErrorCode != "" || result.Wait != nil {
		t.Fatalf("revocation interrupted frozen worker: %+v", result)
	}
	if _, err := os.Stat(destination); err != nil {
		t.Fatal(err)
	}
	if err := queue.Complete(claimed.Job.ID, claimed.LeaseToken); err != nil {
		t.Fatal(err)
	}
	if err := queue.db.Model(&models.Job{}).Where("id = ?", task.JobID).Update("status", models.JobStatusFailed).Error; err != nil {
		t.Fatal(err)
	}
	// Even a stale unrestricted Actor must consult the current DB before retry.
	if _, err := queue.Control(actor, task.JobID, "retry", RequestContext{}); ErrorCode(err) != CodePermissionDenied {
		t.Fatalf("stale actor retry=%v", err)
	}
	options, _ := json.Marshal([]string{"overwrite", "skip"})
	action := models.JobActionRequest{JobID: task.JobID, Version: 1, ActionType: "transfer_conflict", Prompt: "Movie target", OptionsJSON: string(options)}
	if err := queue.db.Create(&action).Error; err != nil {
		t.Fatal(err)
	}
	if err := queue.db.Model(&models.Job{}).Where("id = ?", task.JobID).Update("status", models.JobStatusWaitingUserAction).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := queue.Respond(actor, task.JobID, 1, "overwrite", RequestContext{}); ErrorCode(err) != CodePermissionDenied {
		t.Fatalf("old action bypassed authority: %v", err)
	}
	if err := queue.db.First(&action, action.ID).Error; err != nil || action.Response != "" {
		t.Fatalf("revoked action consumed=%+v err=%v", action, err)
	}
	if _, err := queue.Control(actor, task.JobID, "cancel", RequestContext{}); err != nil {
		t.Fatalf("revoked task cannot cancel: %v", err)
	}
}

func TestResourceRestrictedPlayerCatalogUsesCurrentLibraryAuthority(t *testing.T) {
	service, _, library, _, _, actor := playerSnapshotFixture(t)
	actor.ResourceAccessPolicies = map[string]ResourceAccessPolicy{models.ResourceAccessScopeLibraryRead: {Mode: models.ResourceAccessModeAllowlist}}
	if libraries, err := service.PlayerLibraries(actor); err != nil || len(libraries) != 0 {
		t.Fatalf("restricted Player libraries=%+v %v", libraries, err)
	}
	if _, err := service.PlayerCategories(actor, library.ID); ErrorCode(err) != CodePermissionDenied {
		t.Fatalf("categories=%v", err)
	}
	if _, err := service.PlayerCatalog(actor, library.ID, MediaPageQuery{}); ErrorCode(err) != CodePermissionDenied {
		t.Fatalf("catalog=%v", err)
	}
	if err := ensurePlayerMediaLibraryReadableTx(service.db, actor, library.ID); ErrorCode(err) != CodePermissionDenied {
		t.Fatalf("playback authority=%v", err)
	}
}
