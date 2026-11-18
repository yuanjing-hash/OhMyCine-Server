package services

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/metadata/tmdb"
)

func TestSubscriptionCalendarMaintenanceReachesBeyondFirstTwoThousand(t *testing.T) {
	s, actor, _ := calendarFixture(t)
	generation, err := s.generation()
	if err != nil {
		t.Fatal(err)
	}
	now, fresh := s.now(), s.now().Add(time.Hour)
	raw, _ := json.Marshal(FollowExecutionSnapshot{Version: 2, Seasons: []int{1}, MediaLibraryID: 1, SiteIDs: []uint{}})
	subs := make([]models.FollowSubscription, 0, 2002)
	seasons := make([]models.FollowSubscriptionSeason, 0, 2002)
	heads := make([]models.CalendarSeason, 0, 2000)
	for n := 1; n <= 2001; n++ {
		id := fmt.Sprintf("sub-%04d", n)
		subs = append(subs, models.FollowSubscription{ID: id, OwnerID: actor.User.ID, MediaType: "tv", TMDBID: int64(n), Title: "订阅", Status: models.FollowStatusActive, Revision: 1, ExecutionSnapshotJSON: string(raw), CreatedAt: now, UpdatedAt: now})
		seasons = append(seasons, models.FollowSubscriptionSeason{SubscriptionID: id, OwnerID: actor.User.ID, TMDBID: int64(n), SeasonNumber: 1})
		if n <= 2000 {
			heads = append(heads, models.CalendarSeason{Key: calendarSeasonKey(generation, int64(n), 1), Generation: generation, TMDBID: int64(n), SeasonNumber: 1, Language: calendarLanguage, FetchedAt: &now, FreshUntil: &fresh, Complete: true, UpdatedAt: now})
		}
	}
	// A damaged older subscription must not prevent independent valid seasons.
	subs = append(subs, models.FollowSubscription{ID: "bad-first", OwnerID: actor.User.ID, MediaType: "tv", TMDBID: 999999, Title: "损坏配置", Status: models.FollowStatusActive, Revision: 1, ExecutionSnapshotJSON: "{", CreatedAt: now, UpdatedAt: now})
	seasons = append(seasons, models.FollowSubscriptionSeason{SubscriptionID: "bad-first", OwnerID: actor.User.ID, TMDBID: 999999, SeasonNumber: 1})
	for _, rows := range []any{&subs, &seasons, &heads} {
		if err := s.db.CreateInBatches(rows, 100).Error; err != nil {
			t.Fatal(err)
		}
	}
	for poll := 0; poll < 10; poll++ {
		if err := s.EnqueueDue(context.Background(), 1); err != nil {
			t.Fatalf("poll %d: %v", poll, err)
		}
	}
	var jobs []models.Job
	if err := s.db.Where("job_type = ?", JobTypeSubscriptionCalendar).Find(&jobs).Error; err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].ResourceKey != calendarSeasonKey(generation, 2001, 1) {
		t.Fatalf("later subscription starved: %+v", jobs)
	}
}

func TestSubscriptionCalendarMaintenanceResumesSeasonAfterRestartAndDeletedBookmark(t *testing.T) {
	s, actor, _ := calendarFixture(t)
	calendarSubscription(t, s, actor, "sub", 100, 1, 2, 3, 4)
	for season := 1; season <= 4; season++ {
		// Reconstructing the service proves the bookmark is durable, not a field.
		next := NewSubscriptionCalendarService(s.db, s.queue, nil, nil)
		next.now = s.now
		if err := next.EnqueueDue(context.Background(), 1); err != nil {
			t.Fatal(err)
		}
		var job models.Job
		generation, _ := s.generation()
		if err := s.db.First(&job, "job_type = ? AND resource_key = ?", JobTypeSubscriptionCalendar, calendarSeasonKey(generation, 100, season)).Error; err != nil {
			t.Fatalf("season %d was skipped: %v", season, err)
		}
	}
	if err := s.db.Delete(&models.FollowSubscription{}, "id = ?", "sub").Error; err != nil {
		t.Fatal(err)
	}
	calendarSubscription(t, s, actor, "z-next", 101, 1)
	if err := s.EnqueueDue(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	var bookmark models.CalendarRefreshCursor
	if err := s.db.First(&bookmark, "name = ?", calendarMaintenanceCursor).Error; err != nil {
		t.Fatal(err)
	}
	if bookmark.SubscriptionID != "z-next" || bookmark.SeasonNumber != 1 {
		t.Fatalf("deleted bookmark did not advance: %+v", bookmark)
	}
	// One bounded poll resets at the end; the following poll starts the next round.
	if err := s.EnqueueDue(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if err := s.db.First(&bookmark, "name = ?", calendarMaintenanceCursor).Error; err != nil {
		t.Fatal(err)
	}
	if bookmark.SubscriptionID != "" || bookmark.SeasonNumber != -1 {
		t.Fatalf("did not wrap: %+v", bookmark)
	}
	calendarSubscription(t, s, actor, "a-new", 102, 1)
	if err := s.db.Model(&models.MetadataSettings{}).Where("id = ?", 1).Update("revision", bookmark.Generation+1).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.EnqueueDue(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if err := s.db.First(&bookmark, "name = ?", calendarMaintenanceCursor).Error; err != nil {
		t.Fatal(err)
	}
	if bookmark.SubscriptionID != "a-new" {
		t.Fatalf("configuration change kept stale bookmark: %+v", bookmark)
	}
}

func TestSubscriptionCalendarMaintenanceFailureDoesNotBlockExistingSchedule(t *testing.T) {
	service, queue, actor, now := scheduleFixture(t)
	service.SetSubscriptionCalendar(NewSubscriptionCalendarService(queue.db, queue, nil, nil))
	record := createDueSchedule(t, service, actor, now, "skip", "skip")
	if err := queue.db.Migrator().DropTable(&models.CalendarRefreshCursor{}); err != nil {
		t.Fatal(err)
	}
	if err := service.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	var run models.ScheduleRun
	if err := queue.db.First(&run, "schedule_id = ?", record.ID).Error; err != nil || run.Status != "skipped_misfire" {
		t.Fatalf("normal schedule stopped by calendar error: %+v %v", run, err)
	}
}

func TestSubscriptionCalendarMaintenancePruneIsAtomicAndKeepsReferencedHeads(t *testing.T) {
	s, actor, _ := calendarFixture(t)
	calendarSubscription(t, s, actor, "sub", 100, 1)
	calendarSeed(t, s, 100, 1, tmdb.EpisodeSnapshot{EpisodeNumber: 1, AirDate: "2026-08-13"})
	if err := s.pruneUnused(context.Background()); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := s.db.Model(&models.CalendarEpisode{}).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("referenced cache lost: %d %v", count, err)
	}
	if err := s.db.Delete(&models.FollowSubscription{}, "id = ?", "sub").Error; err != nil {
		t.Fatal(err)
	}
	if err := s.db.Exec("CREATE TRIGGER stop_calendar_prune BEFORE DELETE ON calendar_seasons BEGIN SELECT RAISE(ABORT,'fixture failure'); END").Error; err != nil {
		t.Fatal(err)
	}
	if err := s.pruneUnused(context.Background()); err == nil {
		t.Fatal("expected fixture delete failure")
	}
	if err := s.db.Model(&models.CalendarEpisode{}).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("child delete escaped rollback: %d %v", count, err)
	}
	if err := s.db.Exec("DROP TRIGGER stop_calendar_prune").Error; err != nil {
		t.Fatal(err)
	}
	if err := s.pruneUnused(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.db.Model(&models.CalendarSeason{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("orphan cache retained: %d %v", count, err)
	}
}

func TestSubscriptionCalendarMaintenanceSeparatesCompletedSettledAndUpcoming(t *testing.T) {
	s, actor, clock := calendarFixture(t)
	ctx := context.Background()

	current := clock.now
	clock.now = current.AddDate(0, 0, -40)
	airDate := clock.now.AddDate(0, 0, -1).Format(time.DateOnly)
	calendarSubscription(t, s, actor, "a-completed", 100, 1)
	calendarSeed(t, s, 100, 1, tmdb.EpisodeSnapshot{EpisodeNumber: 1, AirDate: airDate})
	clock.now = current

	// Model an already queued job when Follow records terminal completion.
	if err := s.EnqueueDue(ctx, 1); err != nil {
		t.Fatal(err)
	}
	claimed, err := s.queue.Claim([]string{JobTypeSubscriptionCalendar})
	if err != nil || claimed == nil {
		t.Fatalf("expected original calendar job, got %+v %v", claimed, err)
	}
	if err := s.db.Model(&models.FollowSubscription{}).Where("id = ?", "a-completed").
		Updates(map[string]any{"status": models.FollowStatusCompleted, "next_run_at": nil}).Error; err != nil {
		t.Fatal(err)
	}
	if result := s.Run(ctx, nil, *claimed); result.ErrorCode != "" {
		t.Fatalf("old job should finish without reading TMDB: %+v", result)
	}
	if err := s.queue.Complete(claimed.Job.ID, claimed.LeaseToken); err != nil {
		t.Fatal(err)
	}
	q := CalendarQuery{Start: airDate, End: current.AddDate(0, 0, -2).Format(time.DateOnly), Timezone: "UTC"}
	page, err := s.Read(ctx, actor, q)
	if err != nil || len(page.Events) != 1 || page.StaleSeasons != 0 || page.MissingSeasons != 0 {
		t.Fatalf("historical schedule was incorrectly expired: %+v %v", page, err)
	}
	if result, err := s.RequestRefresh(ctx, actor, ""); err != nil || result.Queued != 0 {
		t.Fatalf("bulk refresh chose a terminal subscription: %+v %v", result, err)
	}
	clock.now = current.Add(2 * time.Minute)
	if result, err := s.RequestRefresh(ctx, actor, "a-completed"); err != nil || result.Queued != 1 {
		t.Fatalf("targeted refresh should allow a historical correction: %+v %v", result, err)
	}
	history, err := s.queue.Claim([]string{JobTypeSubscriptionCalendar})
	if err != nil || history == nil {
		t.Fatalf("expected targeted calendar job, got %+v %v", history, err)
	}
	s.load = func(context.Context, int64, int) (tmdb.SeasonSchedule, error) {
		return tmdb.SeasonSchedule{}, context.DeadlineExceeded
	}
	failed := s.Run(ctx, nil, *history)
	if failed.ErrorCode != "calendar_schedule_network" || failed.RetryAt == nil {
		t.Fatalf("history refresh did not arrange a safe retry: %+v", failed)
	}
	if err := s.queue.RetryLater(history.Job.ID, history.LeaseToken, failed.ErrorCode, failed.ErrorMessage, *failed.RetryAt); err != nil {
		t.Fatal(err)
	}
	targeted := q
	targeted.SubscriptionID = "a-completed"
	page, err = s.Read(ctx, actor, targeted)
	if err != nil || len(page.Events) != 1 || page.RefreshingSeasons != 0 || len(page.ScheduleIssues) != 1 ||
		page.ScheduleIssues[0].RetryAt == nil || !page.ScheduleIssues[0].RetryAt.Equal(*failed.RetryAt) {
		t.Fatalf("waiting historical retry did not preserve dates and retry state: %+v %v", page, err)
	}
	clock.now = *failed.RetryAt
	if err := s.queue.PromoteDueRetries(); err != nil {
		t.Fatal(err)
	}
	retry, err := s.queue.Claim([]string{JobTypeSubscriptionCalendar})
	if err != nil || retry == nil || retry.Job.ID != history.Job.ID {
		t.Fatalf("expected the same historical repair job, got %+v %v", retry, err)
	}
	s.load = func(context.Context, int64, int) (tmdb.SeasonSchedule, error) {
		return tmdb.SeasonSchedule{SeasonNumber: 1, Episodes: []tmdb.EpisodeSnapshot{{EpisodeNumber: 1, AirDate: airDate}}}, nil
	}
	if result := s.Run(ctx, nil, *retry); result.ErrorCode != "" {
		t.Fatalf("targeted refresh was not accepted: %+v", result)
	}
	if err := s.queue.Complete(retry.Job.ID, retry.LeaseToken); err != nil {
		t.Fatal(err)
	}
	if page, err := s.Read(ctx, actor, targeted); err != nil || len(page.ScheduleIssues) != 0 {
		t.Fatalf("historical correction kept a resolved failure: %+v %v", page, err)
	}

	other := models.User{Username: "still-following", UsernameNormalized: "still-following", DisplayName: "Other", PasswordHash: "unused", Status: models.UserStatusActive}
	if err := s.db.Create(&other).Error; err != nil {
		t.Fatal(err)
	}
	calendarSubscription(t, s, Actor{User: other}, "b-active-sibling", 100, 1)
	// Simulate an old successful head for a fully dated past-only season with
	// missing media, plus continuing seasons with future or unknown dates.
	clock.now = current.Add(-8 * time.Hour)
	for _, row := range []struct {
		id   int64
		sub  string
		date string
	}{
		{101, "c-settled-missing", current.AddDate(0, 0, -15).Format(time.DateOnly)},
		{102, "d-upcoming", current.AddDate(0, 0, 3).Format(time.DateOnly)},
		{103, "e-undated", ""},
		{104, "f-settled-with-retry", current.AddDate(0, 0, -15).Format(time.DateOnly)},
	} {
		calendarSubscription(t, s, actor, row.sub, row.id, 1)
		calendarSeed(t, s, row.id, 1, tmdb.EpisodeSnapshot{EpisodeNumber: 1, AirDate: row.date})
	}
	generation, err := s.generation()
	if err != nil {
		t.Fatal(err)
	}
	gotOlderDeadline := clock.now.Add(6 * time.Hour)
	settledKey := calendarSeasonKey(generation, 101, 1)
	for _, key := range []string{settledKey, calendarSeasonKey(generation, 100, 1)} {
		if err := s.db.Model(&models.CalendarSeason{}).Where("key = ?", key).Update("fresh_until", gotOlderDeadline).Error; err != nil {
			t.Fatal(err)
		}
	}
	// A due retry for a failed observation takes precedence over extending the old cache.
	retryKey := calendarSeasonKey(generation, 104, 1)
	if err := s.db.Model(&models.CalendarSeason{}).Where("key = ?", retryKey).
		Updates(map[string]any{"fresh_until": gotOlderDeadline, "last_error_code": "calendar_schedule_unavailable", "retry_at": current}).Error; err != nil {
		t.Fatal(err)
	}
	// A legacy completed record with a check still planned remains eligible.
	if err := s.db.Model(&models.FollowSubscription{}).Where("id = ?", "d-upcoming").
		Updates(map[string]any{"status": models.FollowStatusCompleted, "next_run_at": current.Add(time.Hour)}).Error; err != nil {
		t.Fatal(err)
	}
	// Expire the shared head beyond the quiet period to exercise that another
	// subscriber keeps it eligible even when the original one has finished.
	if err := s.db.Model(&models.CalendarSeason{}).Where("key = ?", calendarSeasonKey(generation, 100, 1)).
		Update("fetched_at", current.AddDate(0, 0, -40)).Error; err != nil {
		t.Fatal(err)
	}
	clock.now = current.Add(time.Hour)
	if err := s.EnqueueDue(ctx, 50); err != nil {
		t.Fatal(err)
	}
	var queued []string
	if err := s.db.Model(&models.Job{}).Where("job_type = ? AND status = ?", JobTypeSubscriptionCalendar, models.JobStatusQueued).
		Order("resource_key").Pluck("resource_key", &queued).Error; err != nil {
		t.Fatal(err)
	}
	want := []string{calendarSeasonKey(generation, 100, 1), calendarSeasonKey(generation, 102, 1), calendarSeasonKey(generation, 103, 1), retryKey}
	if !slices.Equal(queued, want) {
		t.Fatalf("queued calendar seasons = %v, want %v", queued, want)
	}
	var settled models.CalendarSeason
	if err := s.db.First(&settled, "key = ?", settledKey).Error; err != nil ||
		settled.FreshUntil == nil || !settled.FreshUntil.Equal(settled.FetchedAt.Add(7*24*time.Hour)) || settled.AiringFinished {
		t.Fatalf("past-only season lost its original check date or was marked finished: %+v %v", settled, err)
	}
}
