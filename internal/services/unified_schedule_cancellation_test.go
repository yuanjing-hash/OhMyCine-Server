package services

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestUnifiedScheduleRunOnceCancellationDoesNotReplayMissedDays(t *testing.T) {
	s, q, actor, now := scheduleFixture(t)
	d := createDueSchedule(t, s, actor, now, "run_once", "skip")
	due := now.Add(-72 * time.Hour)
	if err := s.db.Model(&d).Updates(map[string]any{"cron_expression": "0 3 * * *", "next_run_at": due}).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	var first models.ScheduleRun
	if err := s.db.Where("schedule_id = ?", d.ID).First(&first).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := q.Control(actor, first.JobID, "cancel", RequestContext{}); err != nil {
		t.Fatal(err)
	}
	if err := q.RecoverExpiredLeases(); err != nil {
		t.Fatal(err)
	}
	if err := s.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := s.db.Model(&models.ScheduleRun{}).Where("schedule_id = ?", d.ID).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("same occurrence replayed: %d %v", count, err)
	}
	var updated models.ScheduleDefinition
	if err := s.db.First(&updated, "id = ?", d.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !updated.Enabled || updated.NextRunAt == nil || !updated.NextRunAt.After(now) {
		t.Fatalf("definition changed incorrectly: %+v", updated)
	}
	// The next scheduled day is a new occurrence, even if the historical run
	// projection still says queued. Cancellation never revives its original Job.
	s.now = func() time.Time { return *updated.NextRunAt }
	if err := s.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	var rows []models.ScheduleRun
	if err := s.db.Where("schedule_id = ?", d.ID).Order("created_at,id").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("runs=%+v", rows)
	}
	for _, r := range rows {
		if r.ID != first.ID && (r.Status != "queued" || r.JobID == first.JobID) {
			t.Fatalf("next day did not create distinct occurrence: %+v", r)
		}
	}
	var original models.Job
	if err := s.db.First(&original, "id = ?", first.JobID).Error; err != nil {
		t.Fatal(err)
	}
	if original.Status != models.JobStatusCancelled || original.AttemptCount != 0 {
		t.Fatalf("cancelled job revived: %+v", original)
	}
}

func TestUnifiedScheduleStatePersistenceFailureIsNotSuccess(t *testing.T) {
	s, _, actor, now := scheduleFixture(t)
	d := createDueSchedule(t, s, actor, now, "run_once", "skip")
	if err := s.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	var r models.ScheduleRun
	if err := s.db.Where("schedule_id = ?", d.ID).First(&r).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.db.Exec("CREATE TRIGGER reject_schedule_state BEFORE UPDATE ON schedule_runs BEGIN SELECT RAISE(ABORT, 'fixture write refused'); END").Error; err != nil {
		t.Fatal(err)
	}
	if result := s.finishRun(r.ID, "completed", ""); result.ErrorCode != "schedule_state_failed" {
		t.Fatalf("save failure reported success: %+v", result)
	}
	payload, _ := json.Marshal(scheduleJobPayload{RunID: r.ID, DefinitionID: d.ID, Revision: d.Revision})
	// cookieCloud is deliberately nil; failed state persistence must stop before
	// reaching the external action, not run it and pretend its tracking succeeded.
	result := s.run(context.Background(), nil, ClaimedJob{Job: models.Job{PayloadJSON: string(payload), AttemptCount: 1}})
	if result.ErrorCode != "schedule_state_failed" || result.RetryAt != nil {
		t.Fatalf("state failure was swallowed: %+v", result)
	}
}

func TestDownloadTrackingPersistenceWaitDoesNotConsumeFailureBudget(t *testing.T) {
	if retryWaitConsumesFailureBudget("download_tracking_persist_wait") {
		t.Fatal("provider tracking wait consumes submission failure budget")
	}
	for _, code := range []string{"download_state_persist_failed", "worker_lease_expired", "unknown_failure"} {
		if !retryWaitConsumesFailureBudget(code) {
			t.Fatalf("real failure exempted: %s", code)
		}
	}
}

func TestUnifiedScheduleOverlapHonorsRetryingQueueOwner(t *testing.T) {
	s, _, actor, now := scheduleFixture(t)
	d := createDueSchedule(t, s, actor, now, "run_once", "skip")
	if err := s.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	var r models.ScheduleRun
	if err := s.db.Where("schedule_id = ?", d.ID).First(&r).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.db.Model(&models.Job{}).Where("id = ?", r.JobID).Update("status", models.JobStatusRetryWait).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.db.Model(&r).Update("status", "retry_wait").Error; err != nil {
		t.Fatal(err)
	}
	later := now.Add(24 * time.Hour)
	s.now = func() time.Time { return later }
	if err := s.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	var skipped int64
	if err := s.db.Model(&models.ScheduleRun{}).Where("schedule_id = ? AND status = 'skipped_overlap'", d.ID).Count(&skipped).Error; err != nil || skipped != 1 {
		t.Fatalf("retry owner overlap ignored: %d %v", skipped, err)
	}
}
