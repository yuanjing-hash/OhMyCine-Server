package services

import (
	"context"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestNotificationsReceiptsScopeRecurrenceAndRecovery(t *testing.T) {
	queue, actor, _ := queueFixture(t)
	job := enqueueFake(t, queue, actor, "Needs attention", "resource")
	ctx := context.Background()
	if err := queue.db.Model(&models.Job{}).Where("id = ?", job.ID).Update("status", "failed").Error; err != nil {
		t.Fatal(err)
	}
	first := models.JobStatusEvent{JobID: job.ID, EventType: "failed", ToStatus: "failed", CreatedAt: time.Now().UTC()}
	if err := queue.db.Create(&first).Error; err != nil {
		t.Fatal(err)
	}
	page, err := queue.Notifications(ctx, actor, 1, 24)
	if err != nil || page.Total != 1 || page.List[0].Read || page.List[0].Resolved {
		t.Fatalf("notification=%+v %v", page, err)
	}
	if err := queue.AcknowledgeNotification(ctx, actor, job.ID, page.List[0].Occurrence); err != nil {
		t.Fatal(err)
	}
	// Restart uses durable receipt; read acknowledgement does not resolve failure.
	restarted := NewQueueService(queue.db, nil)
	page, err = restarted.Notifications(ctx, actor, 1, 24)
	if err != nil || !page.List[0].Read || page.List[0].Resolved {
		t.Fatalf("restart=%+v %v", page, err)
	}
	if err := queue.db.Model(&models.Job{}).Where("id = ?", job.ID).Update("status", "completed").Error; err != nil {
		t.Fatal(err)
	}
	page, err = queue.Notifications(ctx, actor, 1, 24)
	if err != nil || !page.List[0].Resolved || !page.List[0].Read {
		t.Fatalf("recovery=%+v %v", page, err)
	}
	second := models.JobStatusEvent{JobID: job.ID, EventType: "failed", ToStatus: "failed", CreatedAt: time.Now().UTC()}
	if err := queue.db.Create(&second).Error; err != nil {
		t.Fatal(err)
	}
	if err := queue.db.Model(&models.Job{}).Where("id = ?", job.ID).Update("status", "failed").Error; err != nil {
		t.Fatal(err)
	}
	page, err = queue.Notifications(ctx, actor, 1, 24)
	if err != nil || page.Total != 1 || page.List[0].Read {
		t.Fatalf("recurrence=%+v %v", page, err)
	}
	if err := queue.AcknowledgeNotification(ctx, actor, job.ID, uint64(first.ID)); ErrorCode(err) != CodeConflict {
		t.Fatal("stale read", err)
	}
	foreign := Actor{User: models.User{ID: actor.User.ID + 999}, Permissions: map[string]struct{}{authz.PermissionJobsReadOwn: {}}}
	page, err = queue.Notifications(ctx, foreign, 1, 24)
	if err != nil || page.Total != 0 {
		t.Fatalf("foreign=%+v %v", page, err)
	}
	if err := queue.AcknowledgeNotification(ctx, foreign, job.ID, uint64(second.ID)); ErrorCode(err) != CodeNotFound {
		t.Fatal("foreign receipt", err)
	}
	actor.Permissions = map[string]struct{}{}
	if _, err := queue.Notifications(ctx, actor, 1, 24); ErrorCode(err) != CodePermissionDenied {
		t.Fatal("revoked access", err)
	}
}

func TestDashboardOperationsSectionsAreScopedAndFailIndependently(t *testing.T) {
	queue, actor, _ := queueFixture(t)
	enqueueFake(t, queue, actor, "Owned", "r")
	actor.Permissions = map[string]struct{}{authz.PermissionJobsReadOwn: {}, authz.PermissionStoragesRead: {}}
	s := NewAdminService(queue.db, nil, nil, nil)
	result := s.DashboardOperations(context.Background(), actor)
	if len(result) != 2 || result["active-tasks"].Status != "ok" || *result["active-tasks"].List[0].Value != 1 {
		t.Fatalf("scoped=%+v", result)
	}
	if err := queue.db.Exec("DROP TABLE storages").Error; err != nil {
		t.Fatal(err)
	} // Isolated fixture only.
	result = s.DashboardOperations(context.Background(), actor)
	if result["storage-summary"].Status != "unavailable" || result["active-tasks"].Status != "ok" {
		t.Fatalf("failure isolation=%+v", result)
	}
}
