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
	service, _, library, _, _, _ := playerSnapshotFixture(t)
	var role models.Role
	if err := service.db.Where("code = ?", authz.RoleUser).First(&role).Error; err != nil {
		t.Fatal(err)
	}
	user := models.User{Username: "normal-library-user", UsernameNormalized: "normal-library-user", DisplayName: "Normal User", PasswordHash: "unused", Status: models.UserStatusActive, AuthzVersion: 1}
	if err := service.db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	if err := service.db.Create(&models.UserRole{UserID: user.ID, RoleID: role.ID}).Error; err != nil {
		t.Fatal(err)
	}
	authorization := NewAuthorizationService(service.db)
	resolve := func(t *testing.T) Actor {
		t.Helper()
		actor, err := authorization.Resolve(user.ID)
		if err != nil {
			t.Fatal(err)
		}
		return actor
	}
	assertLibraryAccess := func(t *testing.T, actor Actor, allowed bool) {
		t.Helper()
		wantLibraries := 0
		if allowed {
			wantLibraries = 1
		}
		libraries, err := service.PlayerLibraries(actor)
		if err != nil || len(libraries) != wantLibraries {
			t.Fatalf("normal-user libraries=%+v err=%v allowed=%t", libraries, err, allowed)
		}
		if allowed {
			if libraries[0].ID != library.ID || !actor.CanIngestLibrary(uintID(library.ID)) {
				t.Fatal("normal-user cannot read or acquire into the permitted library")
			}
			if page, err := service.PlayerCatalog(actor, library.ID, MediaPageQuery{}); err != nil || len(page.List) != 1 {
				t.Fatalf("permitted normal-user catalog=%+v err=%v", page, err)
			}
			if err := ensurePlayerMediaLibraryReadableTx(service.db, actor, library.ID); err != nil {
				t.Fatalf("permitted playback authority=%v", err)
			}
			return
		}
		if actor.CanIngestLibrary(uintID(library.ID)) {
			t.Fatal("unreadable library remained a normal-user ingest target")
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
	actor := resolve(t)
	assertLibraryAccess(t, actor, true)
	for _, code := range []string{
		authz.PermissionUsersRead, authz.PermissionUsersCreate, authz.PermissionUsersUpdate,
		authz.PermissionRolesRead, authz.PermissionRolesAssign, authz.PermissionSettingsRead,
		authz.PermissionSettingsUpdate, authz.PermissionSystemAdmin,
		authz.PermissionMediaLibrariesCreate, authz.PermissionMediaLibrariesUpdate,
		authz.PermissionMediaLibrariesDelete, authz.PermissionMediaLibrariesScan,
	} {
		if actor.HasPermission(code) {
			t.Fatalf("normal-user template acquired management permission %s", code)
		}
	}
	if _, err := service.Create(context.Background(), actor, MediaLibraryInput{}, RequestContext{}); ErrorCode(err) != CodePermissionDenied {
		t.Fatalf("normal-user library creation=%v", err)
	}
	if _, err := service.Update(context.Background(), actor, library.ID, MediaLibraryInput{}, RequestContext{}); ErrorCode(err) != CodePermissionDenied {
		t.Fatalf("normal-user library modification=%v", err)
	}
	policy := models.UserResourceAccessPolicy{UserID: user.ID, Scope: models.ResourceAccessScopeLibraryRead}
	for _, test := range []struct {
		name, mode string
		ids        []string
		allowed    bool
	}{
		{"included allowlist", models.ResourceAccessModeAllowlist, []string{uintID(library.ID)}, true},
		{"empty allowlist", models.ResourceAccessModeAllowlist, []string{}, false},
		{"excluded allowlist", models.ResourceAccessModeAllowlist, []string{"999"}, false},
		{"excluded denylist", models.ResourceAccessModeDenylist, []string{uintID(library.ID)}, false},
		{"empty denylist", models.ResourceAccessModeDenylist, []string{}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ids, err := json.Marshal(test.ids)
			if err != nil {
				t.Fatal(err)
			}
			policy.Mode, policy.ResourceIDsJSON = test.mode, string(ids)
			if err := service.db.Save(&policy).Error; err != nil {
				t.Fatal(err)
			}
			assertLibraryAccess(t, resolve(t), test.allowed)
		})
	}
	rule := models.UserAuthorizationRule{UserID: user.ID, PermissionCode: authz.PermissionMediaLibrariesRead, Effect: models.AuthorizationEffectDeny, ResourceType: models.AuthorizationResourceMediaLibrary, ResourceID: uintID(library.ID), CreatedBy: user.ID}
	if err := service.db.Create(&rule).Error; err != nil {
		t.Fatal(err)
	}
	assertLibraryAccess(t, resolve(t), false)
	rule.ID, rule.ResourceType, rule.ResourceID = 0, "", ""
	if err := service.db.Create(&rule).Error; err != nil {
		t.Fatal(err)
	}
	actor = resolve(t)
	if actor.HasPermission(authz.PermissionMediaLibrariesRead) {
		t.Fatal("normal-user role bypassed a global library-read deny")
	}
	if _, err := service.PlayerLibraries(actor); ErrorCode(err) != CodePermissionDenied {
		t.Fatalf("globally denied normal-user libraries=%v", err)
	}
}
