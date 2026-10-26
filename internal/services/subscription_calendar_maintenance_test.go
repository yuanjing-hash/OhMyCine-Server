package services

import (
	"context"
	"encoding/json"
	"fmt"
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
