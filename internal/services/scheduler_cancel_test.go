package services

import (
	"context"
	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"sync/atomic"
	"testing"
	"time"
)

func TestSchedulerCancellationKeepsLeaseDuringSlowCleanup(t *testing.T) {
	service, actor, _ := queueFixture(t)
	service.SetClock(realClock{})
	var policy models.QueuePolicy
	if err := service.db.First(&policy, "job_type=?", "fake").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := service.UpdatePolicy(actor, "fake", policy.Revision, 1, 1, 3, 5, RequestContext{}); err != nil {
		t.Fatal(err)
	}
	job := enqueueFake(t, service, actor, "slow cancellation cleanup", "cancel-test")
	started := make(chan struct{}, 2)
	cleanup := make(chan struct{}, 2)
	release := make(chan struct{})
	var runs atomic.Int32
	registry := NewWorkerRegistry()
	if err := registry.Register("fake", WorkerFunc(func(ctx context.Context, _ JobRuntime, _ ClaimedJob) WorkerResult {
		runs.Add(1)
		started <- struct{}{}
		<-ctx.Done()
		cleanup <- struct{}{}
		<-release
		return WorkerResult{}
	})); err != nil {
		t.Fatal(err)
	}
	scheduler := NewScheduler(service, registry, zerolog.Nop())
	scheduler.tick = 25 * time.Millisecond
	if err := scheduler.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer scheduler.Close()
	defer close(release)
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("not started")
	}
	if _, err := service.Control(actor, job.ID, "cancel", RequestContext{}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cleanup:
	case <-time.After(3 * time.Second):
		t.Fatal("worker was not cancelled")
	}
	// Cleanup lasts longer than the five-second lease while recovery is active.
	time.Sleep(6 * time.Second)
	detail, err := service.Get(actor, job.ID)
	if err != nil || detail.Status != models.JobStatusRunning || detail.InterruptPending != models.JobStatusCancelled || detail.AttemptCount != 1 || runs.Load() != 1 {
		t.Fatalf("cleanup lost ownership: %+v runs=%d err=%v", detail, runs.Load(), err)
	}
	release <- struct{}{}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		detail, err = service.Get(actor, job.ID)
		if err == nil && detail.Status == models.JobStatusCancelled {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil || detail.Status != models.JobStatusCancelled || detail.InterruptPending != "" || detail.AttemptCount != 1 {
		t.Fatalf("cancel failed: %+v %v", detail, err)
	}
}

func TestSchedulerRecoveredLocalInterruptNeverRunsWorker(t *testing.T) {
	for _, action := range []string{"pause", "cancel"} {
		t.Run(action, func(t *testing.T) {
			service, actor, clock := queueFixture(t)
			job := enqueueFake(t, service, actor, "recovered local interrupt", "recovered-test")
			claimed, err := service.Claim([]string{"fake"})
			if err != nil || claimed == nil {
				t.Fatalf("claim %v", err)
			}
			if _, err := service.Control(actor, job.ID, action, RequestContext{}); err != nil {
				t.Fatal(err)
			}
			clock.now = clock.now.Add(time.Minute)
			if err := service.RecoverExpiredLeases(); err != nil {
				t.Fatal(err)
			}
			var runs atomic.Int32
			registry := NewWorkerRegistry()
			if err := registry.Register("fake", WorkerFunc(func(context.Context, JobRuntime, ClaimedJob) WorkerResult { runs.Add(1); return WorkerResult{} })); err != nil {
				t.Fatal(err)
			}
			scheduler := NewScheduler(service, registry, zerolog.Nop())
			scheduler.dispatch(context.Background())
			scheduler.wg.Wait()
			detail, err := service.Get(actor, job.ID)
			want := models.JobStatusCancelled
			if action == "pause" {
				want = models.JobStatusPaused
			}
			if err != nil || detail.Status != want || detail.InterruptPending != "" || detail.LastErrorCode != "" || runs.Load() != 0 {
				t.Fatalf("recovered interrupt resumed work: %+v runs=%d err=%v", detail, runs.Load(), err)
			}
		})
	}
}
