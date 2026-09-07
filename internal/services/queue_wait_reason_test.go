package services

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestJobWaitReasonLibraryGateAndSafeScope(t *testing.T) {
	s, db, actor, storage, profile := mediaLibraryTestService(t)
	l, err := s.Create(context.Background(), actor, testLibraryInput("wait reason", storage, profile, false), RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	queue := NewQueueService(db, NewAuditService(db))
	job, err := queue.Enqueue(EnqueueJobInput{System: true, JobType: "fake", DisplayName: "waiting", ResourceKey: "library:" + uintID(l.ID), Payload: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	reader := Actor{Permissions: map[string]struct{}{authz.PermissionJobsReadAll: {}, authz.PermissionMediaLibrariesRead: {}}}
	got, err := queue.Get(reader, job.ID)
	if err != nil || got.WaitReason == nil || got.WaitReason.Code != "library_disabled" {
		t.Fatalf("dto=%+v err=%v", got, err)
	}
	page, err := queue.List(reader, JobListFilter{})
	if err != nil || len(page.List) != 1 || page.List[0].WaitReason.Code != got.WaitReason.Code {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	reader.ResourceRules = []AuthorizationRule{{PermissionCode: authz.PermissionMediaLibrariesRead, Effect: models.AuthorizationEffectDeny, ResourceType: models.AuthorizationResourceMediaLibrary, ResourceID: uintID(l.ID)}}
	got, err = queue.Get(reader, job.ID)
	if err != nil || got.WaitReason == nil || got.WaitReason.Code != "dependency_not_ready" {
		t.Fatalf("resource denied dto=%+v err=%v", got, err)
	}
	page, err = queue.List(reader, JobListFilter{})
	if err != nil || page.List[0].WaitReason.Code != "dependency_not_ready" {
		t.Fatalf("resource denied page=%+v err=%v", page, err)
	}
	reader.ResourceRules = nil
	delete(reader.Permissions, authz.PermissionMediaLibrariesRead)
	got, err = queue.Get(reader, job.ID)
	if err != nil || got.WaitReason == nil || got.WaitReason.Code != "dependency_not_ready" {
		t.Fatalf("scope dto=%+v err=%v", got, err)
	}
	b, _ := json.Marshal(got.WaitReason)
	if strings.Contains(string(b), "library_disabled") || strings.Contains(string(b), job.ID) {
		t.Fatalf("scope leak %s", b)
	}
	for _, status := range []string{models.JobStatusCancelled, models.JobStatusCompleted, models.JobStatusRunning} {
		if err := db.Model(&models.Job{}).Where("id=?", job.ID).Update("status", status).Error; err != nil {
			t.Fatal(err)
		}
		got, err = queue.Get(reader, job.ID)
		if err != nil || got.WaitReason != nil {
			t.Fatalf("terminal/running=%+v err=%v", got, err)
		}
	}
}

func TestJobWaitReasonRunnableAndRetryDoesNotReplaceError(t *testing.T) {
	queue, actor, clock := queueFixture(t)
	job, err := queue.Enqueue(EnqueueJobInput{OwnerID: actor.User.ID, JobType: "fake", DisplayName: "runnable", Payload: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := queue.Get(actor, job.ID)
	if err != nil || got.WaitReason != nil {
		t.Fatalf("runnable=%+v err=%v", got, err)
	}
	next := clock.Now().Add(time.Hour)
	if err := queue.db.Model(&models.Job{}).Where("id=?", job.ID).Updates(map[string]any{"next_attempt_at": next, "last_error_message": "previous execution failed"}).Error; err != nil {
		t.Fatal(err)
	}
	got, err = queue.Get(actor, job.ID)
	if err != nil || got.WaitReason == nil || got.WaitReason.Code != "retry_at" || got.LastErrorMessage != "previous execution failed" {
		t.Fatalf("retry=%+v err=%v", got, err)
	}
}

func TestJobWaitReasonCancelledPhysicalReceiptNeedsVerification(t *testing.T) {
	s, db, actor, storage, profile := mediaLibraryTestService(t)
	l, err := s.Create(context.Background(), actor, testLibraryInput("pending receipt", storage, profile, false), RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	queue := NewQueueService(db, NewAuditService(db))
	old, err := queue.Enqueue(EnqueueJobInput{System: true, JobType: "fake", DisplayName: "private old owner", Payload: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Model(&models.Job{}).Where("id=?", old.ID).Update("status", models.JobStatusCancelled).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	proof := models.CatalogPhysicalWrite{LibraryID: l.ID, OwnerKind: CatalogPhysicalArtifact, OwnerID: "private-owner", JobID: old.ID, State: "quiescent", Revision: 1, OwnerDigest: "private-digest", EnteredAt: now, UpdatedAt: now}
	if err = db.Create(&proof).Error; err != nil {
		t.Fatal(err)
	}
	job, err := queue.Enqueue(EnqueueJobInput{OwnerID: actor.User.ID, JobType: "fake", DisplayName: "waiting", ResourceKey: "library:" + uintID(l.ID), Payload: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	reader := Actor{User: actor.User, Permissions: map[string]struct{}{authz.PermissionJobsReadOwn: {}, authz.PermissionMediaLibrariesRead: {}}}
	got, err := queue.Get(reader, job.ID)
	if err != nil || got.WaitReason == nil || got.WaitReason.Code != "library_busy" {
		t.Fatalf("waiting=%+v err=%v", got, err)
	}
	b, _ := json.Marshal(got.WaitReason)
	for _, secret := range []string{old.ID, "private-owner", "private-digest", "private old owner"} {
		if strings.Contains(string(b), secret) {
			t.Fatalf("private owner leak %s", b)
		}
	}
	if _, err := queue.Get(reader, old.ID); err == nil {
		t.Fatal("hidden owner became readable")
	}
}
