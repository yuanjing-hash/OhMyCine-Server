package services

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func scheduleFixture(t *testing.T) (*UnifiedScheduleService, *QueueService, Actor, time.Time) {
	t.Helper()
	queue, actor, _ := queueFixture(t)
	now := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	service := NewUnifiedScheduleService(queue.db, queue, NewAuthorizationService(queue.db), nil, nil, nil, nil, nil, zerolog.Nop())
	service.now = func() time.Time { return now }
	return service, queue, actor, now
}

func createDueSchedule(t *testing.T, service *UnifiedScheduleService, actor Actor, now time.Time, misfire, overlap string) models.ScheduleDefinition {
	t.Helper()
	due := now.Add(-2 * time.Hour)
	record := models.ScheduleDefinition{
		ID: uuid.NewString(), OwnerID: actor.User.ID, Name: uuid.NewString(), ActionType: "cookiecloud_sync", TargetType: "system", TargetID: "system",
		CronExpression: "0 * * * *", Timezone: "UTC", Enabled: true, MisfirePolicy: misfire, OverlapPolicy: overlap,
		MaxRetries: 1, RetryDelaySeconds: 60, MaxRuntimeSeconds: 300, NextRunAt: &due, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := service.db.Create(&record).Error; err != nil {
		t.Fatal(err)
	}
	return record
}

func TestUnifiedSchedulePollSkipsMisfireAndDoesNotDuplicateAfterRestart(t *testing.T) {
	service, _, actor, now := scheduleFixture(t)
	record := createDueSchedule(t, service, actor, now, "skip", "skip")
	if err := service.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := service.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	var runs []models.ScheduleRun
	if err := service.db.Where("schedule_id = ?", record.ID).Find(&runs).Error; err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].Status != "skipped_misfire" {
		t.Fatalf("runs=%+v", runs)
	}
	var refreshed models.ScheduleDefinition
	if err := service.db.First(&refreshed, "id = ?", record.ID).Error; err != nil {
		t.Fatal(err)
	}
	if refreshed.NextRunAt == nil || !refreshed.NextRunAt.After(now) {
		t.Fatalf("next_run_at=%v", refreshed.NextRunAt)
	}
}

func TestUnifiedSchedulePollAppliesOverlapPolicy(t *testing.T) {
	service, _, actor, now := scheduleFixture(t)
	record := createDueSchedule(t, service, actor, now, "run_once", "skip")
	active := models.ScheduleRun{ID: uuid.NewString(), ScheduleID: record.ID, ScheduledAt: now.Add(-3 * time.Hour), Status: "running", Attempt: 1, CreatedAt: now, UpdatedAt: now}
	if err := service.db.Create(&active).Error; err != nil {
		t.Fatal(err)
	}
	if err := service.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	var skipped int64
	if err := service.db.Model(&models.ScheduleRun{}).Where("schedule_id = ? AND status = ?", record.ID, "skipped_overlap").Count(&skipped).Error; err != nil || skipped != 1 {
		t.Fatalf("skipped=%d err=%v", skipped, err)
	}
}

func TestUnifiedScheduleWorkerRetriesActionFailuresOnlyAsConfigured(t *testing.T) {
	service, _, actor, now := scheduleFixture(t)
	definition := createDueSchedule(t, service, actor, now, "run_once", "queue")
	definition.ActionType = "unsupported_test_action"
	definition.MaxRetries = 1
	if err := service.db.Save(&definition).Error; err != nil {
		t.Fatal(err)
	}
	run := models.ScheduleRun{ID: uuid.NewString(), ScheduleID: definition.ID, ScheduledAt: now, Status: "queued", Attempt: 1, CreatedAt: now, UpdatedAt: now}
	if err := service.db.Create(&run).Error; err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(scheduleJobPayload{RunID: run.ID, DefinitionID: definition.ID, Revision: definition.Revision})
	result := service.run(context.Background(), nil, ClaimedJob{Job: models.Job{PayloadJSON: string(payload), AttemptCount: 1}})
	if result.RetryAt == nil || result.ErrorCode != CodeInvalidRequest {
		t.Fatalf("first result=%+v", result)
	}
	result = service.run(context.Background(), nil, ClaimedJob{Job: models.Job{PayloadJSON: string(payload), AttemptCount: 2}})
	if result.RetryAt != nil || result.ErrorCode != CodeInvalidRequest {
		t.Fatalf("terminal result=%+v", result)
	}
	var refreshed models.ScheduleRun
	if err := service.db.First(&refreshed, "id = ?", run.ID).Error; err != nil {
		t.Fatal(err)
	}
	if refreshed.Status != "failed" || refreshed.FinishedAt == nil {
		t.Fatalf("run=%+v", refreshed)
	}
}

func TestManagedScheduleDoesNotAdoptOrOverwriteManualDefinition(t *testing.T) {
	service, _, actor, now := scheduleFixture(t)
	manual := models.ScheduleDefinition{
		ID: uuid.NewString(), OwnerID: actor.User.ID, Name: "我的每周同步", ActionType: "cookiecloud_sync", TargetType: "system", TargetID: "system",
		CronExpression: "0 6 * * 1", Timezone: "UTC", Enabled: true, MisfirePolicy: "skip", OverlapPolicy: "queue",
		MaxRetries: 3, RetryDelaySeconds: 120, MaxRuntimeSeconds: 600, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := service.db.Create(&manual).Error; err != nil {
		t.Fatal(err)
	}
	if err := syncManagedSchedule(service.db, actor.User.ID, "CookieCloud 自动同步", "cookiecloud_sync", "system", "system", "0 */2 * * *", "Asia/Shanghai", true, false, now); err != nil {
		t.Fatal(err)
	}
	var rows []models.ScheduleDefinition
	if err := service.db.Where("action_type = ? AND target_id = ?", "cookiecloud_sync", "system").Order("name").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("definitions=%+v", rows)
	}
	var refreshed models.ScheduleDefinition
	if err := service.db.First(&refreshed, "id = ?", manual.ID).Error; err != nil {
		t.Fatal(err)
	}
	if refreshed.CronExpression != manual.CronExpression || refreshed.ManagedKey != "" || refreshed.OverlapPolicy != manual.OverlapPolicy {
		t.Fatalf("manual definition was changed: %+v", refreshed)
	}
}

func TestLegacySyncPreservesEditedManagedCronUntilBusinessSettingChanges(t *testing.T) {
	service, _, actor, now := scheduleFixture(t)
	if err := syncManagedSchedule(service.db, actor.User.ID, "媒体库全量扫描 · A · 7", "media_library_scan", "media_library", "7", "0 3 * * *", "Asia/Shanghai", true, true, now); err != nil {
		t.Fatal(err)
	}
	key := managedScheduleKey("media_library_scan", "media_library", "7")
	if err := service.db.Model(&models.ScheduleDefinition{}).Where("managed_key = ?", key).Updates(map[string]any{"cron_expression": "15 4 * * 2", "timezone": "UTC", "overlap_policy": "queue"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := syncManagedSchedule(service.db, actor.User.ID, "媒体库全量扫描 · A · 7", "media_library_scan", "media_library", "7", "0 */6 * * *", "Asia/Shanghai", true, false, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var preserved models.ScheduleDefinition
	if err := service.db.First(&preserved, "managed_key = ?", key).Error; err != nil {
		t.Fatal(err)
	}
	if preserved.CronExpression != "15 4 * * 2" || preserved.Timezone != "UTC" || preserved.OverlapPolicy != "queue" {
		t.Fatalf("startup sync overwrote user schedule: %+v", preserved)
	}
	if err := syncManagedSchedule(service.db, actor.User.ID, "媒体库全量扫描 · A · 7", "media_library_scan", "media_library", "7", "0 */6 * * *", "Asia/Shanghai", false, true, now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	var updated models.ScheduleDefinition
	if err := service.db.First(&updated, "managed_key = ?", key).Error; err != nil {
		t.Fatal(err)
	}
	if updated.CronExpression != "0 */6 * * *" || updated.Timezone != "Asia/Shanghai" || updated.Enabled {
		t.Fatalf("business setting did not update managed schedule: %+v", updated)
	}
}

func TestLegacyScheduleSyncAllowsInstallWithoutOwner(t *testing.T) {
	service, _, _, _ := scheduleFixture(t)
	if err := service.syncLegacyDefinitions(); err != nil {
		t.Fatal(err)
	}
}
