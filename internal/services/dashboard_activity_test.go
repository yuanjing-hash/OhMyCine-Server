package services

import (
	"context"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestDashboardActivityNamesOnlyCurrentWorkAndSeparatesAttention(t *testing.T) {
	queue, actor, _ := queueFixture(t)
	running := enqueueFake(t, queue, actor, "入库：测试电视剧 S01E08", "r")
	done := enqueueFake(t, queue, actor, "已经完成的旧入库", "done")
	failed := enqueueFake(t, queue, actor, "等待修复的下载", "failed")
	for id, status := range map[string]string{running.ID: models.JobStatusRunning, done.ID: models.JobStatusCompleted, failed.ID: models.JobStatusFailed} {
		if err := queue.db.Model(&models.Job{}).Where("id=?", id).Update("status", status).Error; err != nil {
			t.Fatal(err)
		}
	}
	foreign := actor
	foreign.User = models.User{Username: "dashboard-other", UsernameNormalized: "dashboard-other", DisplayName: "Other", PasswordHash: "x", Status: models.UserStatusActive}
	if err := queue.db.Create(&foreign.User).Error; err != nil {
		t.Fatal(err)
	}
	foreignJob := enqueueFake(t, queue, foreign, "其他账号私有下载", "foreign")
	if err := queue.db.Model(&models.Job{}).Where("id=?", foreignJob.ID).Update("status", models.JobStatusRunning).Error; err != nil {
		t.Fatal(err)
	}
	actor.Permissions = map[string]struct{}{authz.PermissionDashboardRead: {}, authz.PermissionJobsReadOwn: {}}
	admin := NewAdminService(queue.db, nil, nil, nil)
	view, err := admin.DashboardActivity(context.Background(), actor)
	if err != nil || view.Active.Status != "ok" || len(view.Active.List) != 1 || view.Active.List[0].DisplayName != running.DisplayName {
		t.Fatalf("active=%+v err=%v", view.Active, err)
	}
	if len(view.Attention.List) != 1 || view.Attention.List[0].ID != failed.ID {
		t.Fatalf("attention=%+v", view.Attention)
	}
	if view.Recent.Status != "unavailable" || view.Capacities.Status != "unavailable" {
		t.Fatal("domain permissions widened")
	}
	if err := queue.db.Model(&models.Job{}).Where("id=?", running.ID).Update("status", models.JobStatusCompleted).Error; err != nil {
		t.Fatal(err)
	}
	view, err = admin.DashboardActivity(context.Background(), actor)
	if err != nil || view.Active.Status != "ok" || len(view.Active.List) != 0 {
		t.Fatalf("empty=%+v err=%v", view.Active, err)
	}
	delete(actor.Permissions, authz.PermissionDashboardRead)
	if _, err := admin.DashboardActivity(context.Background(), actor); ErrorCode(err) != CodePermissionDenied {
		t.Fatalf("unauthorized=%v", err)
	}
}

func TestDashboardManagementCountersRequireTheirOwnPermissions(t *testing.T) {
	queue, actor, _ := queueFixture(t)
	admin := NewAdminService(queue.db, nil, nil, nil)
	actor.Permissions = map[string]struct{}{authz.PermissionDashboardRead: {}}
	result, err := admin.DashboardForActor(context.Background(), actor)
	if err != nil || result.Users != nil || result.ActiveUsers != nil || result.Roles != nil || result.AuditEvents != nil {
		t.Fatalf("private counters=%+v %v", result, err)
	}
	actor.Permissions[authz.PermissionUsersRead] = struct{}{}
	result, err = admin.DashboardForActor(context.Background(), actor)
	if err != nil || result.Users == nil || *result.Users != 1 || result.ActiveUsers == nil || result.Roles != nil || result.AuditEvents != nil {
		t.Fatalf("granted counters=%+v %v", result, err)
	}
}

func TestDashboardRecentUsesEffectiveCatalogAndPreservesPartitionFailures(t *testing.T) {
	_, store, library, _, _, actor := playerSnapshotFixture(t)
	actor.Permissions[authz.PermissionDashboardRead] = struct{}{}
	actor.Permissions[authz.PermissionJobsReadOwn] = struct{}{}
	actor.Permissions[authz.PermissionStoragesRead] = struct{}{}
	admin := NewAdminService(store.writeDB, nil, nil, nil)
	admin.SetCatalogSnapshotStore(store)
	view, err := admin.DashboardActivity(context.Background(), actor)
	if err != nil || view.Recent.Status != "ok" || len(view.Recent.List) != 1 || view.Recent.List[0].LibraryID != library.ID {
		t.Fatalf("recent=%+v %v", view.Recent, err)
	}
	actor.ResourceRules = []AuthorizationRule{{PermissionCode: authz.PermissionMediaLibrariesRead, Effect: models.AuthorizationEffectDeny, ResourceType: models.AuthorizationResourceMediaLibrary, ResourceID: uintID(library.ID)}}
	view, err = admin.DashboardActivity(context.Background(), actor)
	if err != nil || len(view.Recent.List) != 0 {
		t.Fatalf("denied recent=%+v %v", view.Recent, err)
	}
	if err := store.writeDB.Exec("ALTER TABLE storages RENAME TO unavailable_storages").Error; err != nil {
		t.Fatal(err)
	}
	view, err = admin.DashboardActivity(context.Background(), actor)
	if err != nil || view.Capacities.Status != "unavailable" || view.Active.Status != "ok" {
		t.Fatalf("partition failed=%+v %v", view.Active, err)
	}
}
