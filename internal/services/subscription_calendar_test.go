package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/metadata/tmdb"
)

func calendarFixture(t *testing.T) (*SubscriptionCalendarService, Actor, *fakeQueueClock) {
	t.Helper()
	queue, actor, clock := queueFixture(t)
	actor.Permissions[authz.PermissionFollowsReadOwn] = struct{}{}
	actor.Permissions[authz.PermissionMediaLibrariesRead] = struct{}{}
	s := NewSubscriptionCalendarService(queue.db, queue, nil, nil)
	s.now = clock.Now
	s.load = func(context.Context, int64, int) (tmdb.SeasonSchedule, error) {
		t.Fatal("a read must never fetch metadata")
		return tmdb.SeasonSchedule{}, nil
	}
	return s, actor, clock
}
func calendarSubscription(t *testing.T, s *SubscriptionCalendarService, actor Actor, id string, tmdbID int64, seasons ...int) {
	t.Helper()
	raw, _ := json.Marshal(FollowExecutionSnapshot{Version: 2, Seasons: seasons, MediaLibraryID: 1, SiteIDs: []uint{}})
	record := models.FollowSubscription{ID: id, OwnerID: actor.User.ID, MediaType: "tv", TMDBID: tmdbID, Title: "测试订阅" + id, Status: models.FollowStatusActive, Revision: 1, ExecutionSnapshotJSON: string(raw), CreatedAt: s.now(), UpdatedAt: s.now()}
	if err := s.db.Create(&record).Error; err != nil {
		t.Fatal(err)
	}
	for _, season := range seasons {
		if err := s.db.Create(&models.FollowSubscriptionSeason{SubscriptionID: id, OwnerID: actor.User.ID, TMDBID: tmdbID, SeasonNumber: season}).Error; err != nil {
			t.Fatal(err)
		}
	}
}
func calendarSeed(t *testing.T, s *SubscriptionCalendarService, id int64, season int, episodes ...tmdb.EpisodeSnapshot) {
	t.Helper()
	generation, err := s.generation()
	if err != nil {
		t.Fatal(err)
	}
	key := calendarSeasonKey(generation, id, season)
	if err := s.db.Create(&models.CalendarSeason{Key: key, Generation: generation, TMDBID: id, SeasonNumber: season, Language: calendarLanguage}).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.publish(context.Background(), calendarJobPayload{Key: key, Generation: generation, TMDBID: id, Season: season}, tmdb.SeasonSchedule{SeasonNumber: season, Episodes: episodes}, nil); err != nil {
		t.Fatal(err)
	}
}
func calendarMonthQuery() CalendarQuery {
	return CalendarQuery{Start: "2026-08-01", End: "2026-09-01", Timezone: "Asia/Shanghai"}
}

func TestSubscriptionCalendarReadsLocalSelectedSeasonsAndDates(t *testing.T) {
	s, actor, _ := calendarFixture(t)
	calendarSubscription(t, s, actor, "sub", 100, 1)
	calendarSeed(t, s, 100, 1, tmdb.EpisodeSnapshot{EpisodeNumber: 1, Name: "日期不漂移", AirDate: "2026-08-13"}, tmdb.EpisodeSnapshot{EpisodeNumber: 2, Name: "待定"})
	otherUser := models.User{Username: "other-calendar", UsernameNormalized: "other-calendar", DisplayName: "Other", PasswordHash: "unused", Status: models.UserStatusActive}
	if err := s.db.Create(&otherUser).Error; err != nil {
		t.Fatal(err)
	}
	calendarSubscription(t, s, Actor{User: otherUser}, "other-sub", 100, 0)
	calendarSeed(t, s, 100, 0, tmdb.EpisodeSnapshot{EpisodeNumber: 1, Name: "未订阅特别篇", AirDate: "2026-08-13"})
	page, err := s.Read(context.Background(), actor, calendarMonthQuery())
	if err != nil || len(page.Events) != 1 || page.Events[0].AirDate != "2026-08-13" || page.Events[0].SeasonNumber != 1 || page.Events[0].LibraryStatus != "unknown" || page.UndatedCount != 1 {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	q := calendarMonthQuery()
	q.Timezone = "America/Los_Angeles"
	other, err := s.Read(context.Background(), actor, q)
	if err != nil || other.Events[0].AirDate != page.Events[0].AirDate {
		t.Fatalf("date drift: %+v %v", other, err)
	}
	actor.Permissions[authz.PermissionFollowsReadAll] = struct{}{}
	all, err := s.Read(context.Background(), actor, q)
	if err != nil || all.Scope != "all" || len(all.Events) != 2 {
		t.Fatalf("all scope=%+v %v", all, err)
	}
	actor.ResourceRules = []AuthorizationRule{{PermissionCode: authz.PermissionMediaLibrariesRead, ResourceType: models.AuthorizationResourceMediaLibrary, ResourceID: "1", Effect: models.AuthorizationEffectDeny}}
	hidden, err := s.Read(context.Background(), actor, q)
	if err != nil || len(hidden.Events) != 0 || len(hidden.Subscriptions) != 0 {
		t.Fatalf("revoked library leaked: %+v %v", hidden, err)
	}
	actor.Permissions = map[string]struct{}{}
	if _, err := s.Read(context.Background(), actor, q); ErrorCode(err) != CodePermissionDenied {
		t.Fatalf("err=%v", err)
	}
}

func TestSubscriptionCalendarProjectionBudgetBoundsSharedSeasonMultiplication(t *testing.T) {
	s, actor, _ := calendarFixture(t)
	actor.Permissions[authz.PermissionFollowsReadAll] = struct{}{}
	for _, id := range []string{"sub-a", "sub-b", "sub-c"} {
		owner := models.User{Username: id, UsernameNormalized: id, DisplayName: id, PasswordHash: "unused", Status: models.UserStatusActive}
		if err := s.db.Create(&owner).Error; err != nil {
			t.Fatal(err)
		}
		calendarSubscription(t, s, Actor{User: owner}, id, 100, 1)
	}
	episodes := make([]tmdb.EpisodeSnapshot, tmdb.MaxCalendarEpisodes)
	for i := range episodes {
		episodes[i] = tmdb.EpisodeSnapshot{EpisodeNumber: i + 1, AirDate: "2026-08-13"}
	}
	calendarSeed(t, s, 100, 1, episodes...)
	page, err := s.Read(context.Background(), actor, calendarMonthQuery())
	if err != nil || !page.Partial || !page.HasMore {
		t.Fatalf("budget=%+v %v", page, err)
	}
	found := false
	for _, reason := range page.Reasons {
		found = found || reason == "event_budget"
	}
	if !found {
		t.Fatalf("shared season silently truncated: %+v", page.Reasons)
	}
}

func TestSubscriptionCalendarPublicationRollsBackAndLostACKDoesNotReplaceAgain(t *testing.T) {
	s, actor, _ := calendarFixture(t)
	calendarSubscription(t, s, actor, "sub", 100, 1)
	calendarSeed(t, s, 100, 1, tmdb.EpisodeSnapshot{EpisodeNumber: 1, Name: "旧排期", AirDate: "2026-08-13"})
	if _, err := s.RequestRefresh(context.Background(), actor, ""); err != nil {
		t.Fatal(err)
	}
	claimed, err := s.queue.Claim([]string{JobTypeSubscriptionCalendar})
	if err != nil || claimed == nil {
		t.Fatalf("claim=%+v %v", claimed, err)
	}
	var payload calendarJobPayload
	_ = json.Unmarshal([]byte(claimed.Job.PayloadJSON), &payload)
	invalid := tmdb.SeasonSchedule{SeasonNumber: 1, Episodes: []tmdb.EpisodeSnapshot{{EpisodeNumber: 2, Name: "未提交"}, {EpisodeNumber: 2}}}
	if err := s.publish(context.Background(), payload, invalid, claimed); err == nil {
		t.Fatal("duplicate episode fixture must fail publication")
	}
	var previous models.CalendarEpisode
	if err := s.db.First(&previous).Error; err != nil || previous.EpisodeNumber != 1 || previous.Name != "旧排期" {
		t.Fatalf("partial replacement escaped rollback: %+v %v", previous, err)
	}
	s.load = func(context.Context, int64, int) (tmdb.SeasonSchedule, error) {
		return tmdb.SeasonSchedule{SeasonNumber: 1, Episodes: []tmdb.EpisodeSnapshot{{EpisodeNumber: 2, Name: "完整新排期", AirDate: "2026-08-14"}}}, nil
	}
	if result := s.Run(context.Background(), nil, *claimed); result.ErrorCode != "" {
		t.Fatalf("publish=%+v", result)
	}
	var first models.CalendarSeason
	if err := s.db.First(&first).Error; err != nil {
		t.Fatal(err)
	}
	// Simulate losing the worker completion acknowledgement after committing cache.
	if result := s.Run(context.Background(), nil, *claimed); result.ErrorCode != "" {
		t.Fatalf("lost ack=%+v", result)
	}
	var after models.CalendarSeason
	if err := s.db.First(&after).Error; err != nil || after.Revision != first.Revision {
		t.Fatalf("lost ACK repeated publication: %+v %v", after, err)
	}
}

func TestSubscriptionCalendarPaginatesBeyondFirstHundredAndFencesCursor(t *testing.T) {
	s, actor, _ := calendarFixture(t)
	for n := 1; n <= 105; n++ {
		id := fmt.Sprintf("sub-%03d", n)
		calendarSubscription(t, s, actor, id, int64(n), 1)
		calendarSeed(t, s, int64(n), 1, tmdb.EpisodeSnapshot{EpisodeNumber: 1, AirDate: "2026-08-13"})
	}
	q := calendarMonthQuery()
	q.PageSize = 100
	first, err := s.Read(context.Background(), actor, q)
	if err != nil || len(first.Events) != 100 || !first.HasMore || len(first.Subscriptions) != 105 {
		t.Fatalf("count=%d subs=%d more=%v err=%v", len(first.Events), len(first.Subscriptions), first.HasMore, err)
	}
	q.Cursor = first.NextCursor
	last, err := s.Read(context.Background(), actor, q)
	if err != nil || len(last.Events) != 5 || last.HasMore {
		t.Fatalf("last=%+v err=%v", last, err)
	}
	if err := s.db.Model(&models.FollowSubscription{}).Where("id = ?", "sub-001").Update("revision", 2).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read(context.Background(), actor, q); ErrorCode(err) != CodeConflict {
		t.Fatalf("old cursor must not splice new schedule: %v", err)
	}
}

func TestSubscriptionCalendarRefreshDeduplicatesWithoutAdvancingRunningJob(t *testing.T) {
	s, actor, clock := calendarFixture(t)
	calendarSubscription(t, s, actor, "sub", 100, 1)
	result, err := s.RequestRefresh(context.Background(), actor, "")
	if err != nil || result.Queued != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	claimed, err := s.queue.Claim([]string{JobTypeSubscriptionCalendar})
	if err != nil || claimed == nil {
		t.Fatalf("claim=%+v %v", claimed, err)
	}
	// Model the live scheduler's lease renewal while metadata work is running.
	if err := s.db.Model(&models.Job{}).Where("id = ?", claimed.Job.ID).Update("lease_expires_at", clock.now.Add(3*time.Minute)).Error; err != nil {
		t.Fatal(err)
	}
	clock.now = clock.now.Add(2 * time.Minute)
	result, err = s.RequestRefresh(context.Background(), actor, "")
	if err != nil || result.AlreadyQueued != 1 {
		t.Fatalf("duplicate=%+v %v", result, err)
	}
	var after models.Job
	if err := s.db.First(&after, "id = ?", claimed.Job.ID).Error; err != nil {
		t.Fatal(err)
	}
	if after.Generation != claimed.Job.Generation || after.Revision != claimed.Job.Revision {
		t.Fatal("duplicate refresh advanced the running job generation")
	}
	s.load = func(context.Context, int64, int) (tmdb.SeasonSchedule, error) {
		return tmdb.SeasonSchedule{SeasonNumber: 1, Episodes: []tmdb.EpisodeSnapshot{{EpisodeNumber: 1, AirDate: "2026-08-13"}}}, nil
	}
	if result := s.Run(context.Background(), nil, *claimed); result.ErrorCode != "" {
		t.Fatalf("run=%+v", result)
	}
	page, err := s.Read(context.Background(), actor, calendarMonthQuery())
	if err != nil || len(page.Events) != 1 {
		t.Fatalf("page=%+v err=%v", page, err)
	}
}

func TestSubscriptionCalendarRetiredWorkerCannotPublishOrRecordFailure(t *testing.T) {
	for _, outcome := range []string{"expired", "replaced", "failed"} {
		t.Run(outcome, func(t *testing.T) {
			s, actor, clock := calendarFixture(t)
			calendarSubscription(t, s, actor, "sub", 100, 1)
			calendarSeed(t, s, 100, 1, tmdb.EpisodeSnapshot{EpisodeNumber: 1, AirDate: "2026-08-13"})
			if _, err := s.RequestRefresh(context.Background(), actor, ""); err != nil {
				t.Fatal(err)
			}
			claimed, err := s.queue.Claim([]string{JobTypeSubscriptionCalendar})
			if err != nil || claimed == nil {
				t.Fatalf("claim=%+v %v", claimed, err)
			}
			s.load = func(context.Context, int64, int) (tmdb.SeasonSchedule, error) {
				if outcome == "expired" {
					clock.now = clock.now.Add(61 * time.Second)
				} else if err := s.db.Model(&models.Job{}).Where("id = ?", claimed.Job.ID).Update("lease_token_hash", "replacement-owner").Error; err != nil {
					t.Fatal(err)
				}
				if outcome == "failed" {
					return tmdb.SeasonSchedule{}, errors.New("late upstream failure")
				}
				return tmdb.SeasonSchedule{SeasonNumber: 1, Episodes: []tmdb.EpisodeSnapshot{{EpisodeNumber: 2, AirDate: "2026-08-14"}}}, nil
			}
			if result := s.Run(context.Background(), nil, *claimed); result.ErrorCode != "" {
				t.Fatalf("retired work=%+v", result)
			}
			var episode models.CalendarEpisode
			if err := s.db.First(&episode).Error; err != nil || episode.EpisodeNumber != 1 {
				t.Fatalf("retired worker replaced cache: %+v %v", episode, err)
			}
			var head models.CalendarSeason
			if err := s.db.First(&head).Error; err != nil || head.LastErrorCode != "" || head.RetryAt != nil {
				t.Fatalf("retired worker changed failure state: %+v %v", head, err)
			}
		})
	}
}

func TestSubscriptionCalendarFailureKeepsSnapshotAndRejectsOldGeneration(t *testing.T) {
	s, actor, clock := calendarFixture(t)
	calendarSubscription(t, s, actor, "sub", 100, 1)
	calendarSeed(t, s, 100, 1, tmdb.EpisodeSnapshot{EpisodeNumber: 1, AirDate: "2026-08-13"})
	clock.now = clock.now.Add(7 * time.Hour)
	s.load = func(context.Context, int64, int) (tmdb.SeasonSchedule, error) {
		return tmdb.SeasonSchedule{}, errors.New("private upstream detail")
	}
	if _, err := s.RequestRefresh(context.Background(), actor, ""); err != nil {
		t.Fatal(err)
	}
	claimed, err := s.queue.Claim([]string{JobTypeSubscriptionCalendar})
	if err != nil || claimed == nil {
		t.Fatalf("claim=%+v %v", claimed, err)
	}
	if result := s.Run(context.Background(), nil, *claimed); result.ErrorCode != "calendar_schedule_unavailable" {
		t.Fatalf("result=%+v", result)
	}
	page, err := s.Read(context.Background(), actor, calendarMonthQuery())
	if err != nil || len(page.Events) != 1 || page.StaleSeasons != 1 {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	var payload calendarJobPayload
	_ = json.Unmarshal([]byte(claimed.Job.PayloadJSON), &payload)
	if err := s.db.Model(&models.MetadataSettings{}).Where("id = ?", 1).Update("revision", payload.Generation+1).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.publish(context.Background(), payload, tmdb.SeasonSchedule{SeasonNumber: 1, Episodes: []tmdb.EpisodeSnapshot{{EpisodeNumber: 2, AirDate: "2026-08-14"}}}, nil); !errors.Is(err, errCalendarSuperseded) {
		t.Fatalf("old publication err=%v", err)
	}
	var episode models.CalendarEpisode
	if err := s.db.First(&episode, "season_key = ?", payload.Key).Error; err != nil || episode.EpisodeNumber != 1 {
		t.Fatalf("old generation overwrote snapshot: %+v %v", episode, err)
	}
}

func TestSubscriptionCalendarBoundsUnknownDatesAndRefreshRange(t *testing.T) {
	s, actor, _ := calendarFixture(t)
	calendarSubscription(t, s, actor, "sub", 100, 1)
	episodes := make([]tmdb.EpisodeSnapshot, 120)
	for i := range episodes {
		episodes[i] = tmdb.EpisodeSnapshot{EpisodeNumber: i + 1}
	}
	calendarSeed(t, s, 100, 1, episodes...)
	page, err := s.Read(context.Background(), actor, calendarMonthQuery())
	if err != nil || len(page.Undated) != 100 || page.UndatedCount != 120 || !page.Partial {
		t.Fatalf("undated=%d count=%d partial=%v err=%v", len(page.Undated), page.UndatedCount, page.Partial, err)
	}
	q := calendarMonthQuery()
	q.End = "2026-11-01"
	if _, err := s.Read(context.Background(), actor, q); ErrorCode(err) != CodeInvalidRequest {
		t.Fatalf("range err=%v", err)
	}
	delete(actor.Permissions, authz.PermissionMediaLibrariesRead)
	page, err = s.Read(context.Background(), actor, calendarMonthQuery())
	if err != nil || len(page.Events) != 0 || len(page.Undated) != 0 || len(page.Subscriptions) != 0 {
		t.Fatalf("restricted subscription leaked: %+v %v", page, err)
	}
}

func TestSubscriptionCalendarUsesCurrentCatalogPresenceWithoutMetadataRequests(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("calendar GET performed external metadata I/O") }))
	defer upstream.Close()
	coverage, actor, completeID, partialID := coverageFixture(t, upstream, now)
	user := models.User{ID: actor.User.ID, Username: "calendar", UsernameNormalized: "calendar", DisplayName: "Calendar", PasswordHash: "unused", Status: models.UserStatusActive}
	if err := coverage.db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	actor.Permissions[authz.PermissionFollowsReadOwn] = struct{}{}
	s := NewSubscriptionCalendarService(coverage.db, nil, coverage.metadata, coverage)
	s.now = func() time.Time { return now }
	calendarSubscription(t, s, actor, "sub", 100, 1)
	calendarSeed(t, s, 100, 1, tmdb.EpisodeSnapshot{EpisodeNumber: 1, AirDate: "2026-08-13"}, tmdb.EpisodeSnapshot{EpisodeNumber: 2, AirDate: "2026-08-14"})
	season, episode, id := 1, 1, int64(100)
	entry := models.MediaLibraryEntry{LibraryID: partialID, RelativePath: "/show/one.mkv", ProviderID: "private-id", MediaType: "tv", Title: "测试剧", WorkKey: "series:tmdb:100", Season: &season, Episode: &episode, MatchStatus: mediaRecognitionStatusMatched, TMDBID: &id, LastGeneration: 1}
	if err := coverage.db.Create(&entry).Error; err != nil {
		t.Fatal(err)
	}
	if err := coverage.db.Model(&models.MediaLibrary{}).Where("id = ?", completeID).Update("enabled", false).Error; err != nil {
		t.Fatal(err)
	}
	if err := coverage.db.Model(&models.MediaLibraryScanRun{}).Where("library_id = ?", partialID).Update("partial", true).Error; err != nil {
		t.Fatal(err)
	}
	page, err := s.Read(context.Background(), actor, calendarMonthQuery())
	if err != nil || len(page.Events) != 2 || page.Events[0].LibraryStatus != "present" || page.Events[1].LibraryStatus != "unknown" {
		t.Fatalf("current facts=%+v err=%v", page, err)
	}
}
