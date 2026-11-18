package services

import (
	"context"
	"errors"
	"time"

	serverlog "github.com/yuanjing-hash/OhMyCine-Server/internal/logging"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/metadata/tmdb"
	"gorm.io/gorm"
)

const (
	calendarQuietAge      = 7 * 24 * time.Hour
	calendarQuietInterval = 7 * 24 * time.Hour

	calendarIssueUnavailable = "schedule_unavailable"
	calendarIssueCredentials = "schedule_credentials"
	calendarIssueNetwork     = "schedule_network"
	calendarIssueRejected    = "schedule_provider_rejected"
	calendarIssueResponse    = "schedule_invalid_response"
	calendarIssueStorage     = "schedule_storage_unavailable"
)

type calendarRefreshRequest struct {
	manual                bool
	historySubscriptionID string
}

type CalendarScheduleIssue struct {
	Reason  string     `json:"reason"`
	Seasons int        `json:"seasons"`
	RetryAt *time.Time `json:"retry_at,omitempty"`
}

func calendarSubscriptionCompleted(sub FollowSummary) bool {
	return sub.Status == models.FollowStatusCompleted && sub.NextRunAt == nil
}

func calendarRefreshReferenced(tx *gorm.DB, payload calendarJobPayload) (bool, error) {
	q := tx.Table("follow_subscription_seasons AS fs").
		Joins("JOIN follow_subscriptions f ON f.id = fs.subscription_id AND f.tmdb_id = fs.tmdb_id").
		Where("fs.tmdb_id = ? AND fs.season_number = ?", payload.TMDBID, payload.Season)
	// A targeted request may repair a completed subscription's saved history.
	// A completed subscriber by itself never keeps an automatic job alive.
	if payload.HistorySubscriptionID != "" {
		q = q.Where("f.id = ?", payload.HistorySubscriptionID)
	} else {
		q = q.Where("(f.status <> ? OR f.next_run_at IS NOT NULL)", models.FollowStatusCompleted)
	}
	var matched int
	err := q.Select("1").Limit(1).Scan(&matched).Error
	return matched > 0, err
}

// Calendar snapshots carry validated, unique episode numbers. A canonical
// sequence with only past dates can use a quiet cadence without declaring the
// provider's series or the subscriber's library complete.
func calendarPastOnly(episodes []tmdb.EpisodeSnapshot, cutoff time.Time) bool {
	if len(episodes) == 0 {
		return false
	}
	cutoffDate := cutoff.UTC().Format(time.DateOnly)
	for _, episode := range episodes {
		if episode.EpisodeNumber < 1 || episode.EpisodeNumber > len(episodes) ||
			!calendarAiredBefore(episode.AirDate, cutoffDate) {
			return false
		}
	}
	return true
}

func calendarAiredBefore(value, cutoffDate string) bool {
	_, err := time.Parse(time.DateOnly, value)
	return err == nil && value < cutoffDate
}

// Extend the old six-hour deadline from the actual fetch. This path does not
// fetch metadata or renew the observation date and therefore cannot postpone
// the next check forever. A current failure keeps its own retry schedule.
func calendarLegacyQuietUntil(db *gorm.DB, cached models.CalendarSeason, now time.Time) (time.Time, error) {
	if cached.LastErrorCode != "" || !cached.Complete || cached.FetchedAt == nil || cached.EpisodeCount < 1 || cached.EpisodeCount > tmdb.MaxCalendarEpisodes {
		return time.Time{}, nil
	}
	quietUntil := cached.FetchedAt.Add(calendarQuietInterval)
	if !now.Before(quietUntil) {
		return time.Time{}, nil
	}
	var episodes []models.CalendarEpisode
	if err := db.Select("episode_number", "air_date").Where("season_key = ?", cached.Key).
		Order("episode_number").Limit(cached.EpisodeCount + 1).Find(&episodes).Error; err != nil {
		return time.Time{}, err
	}
	if len(episodes) != cached.EpisodeCount {
		return time.Time{}, nil
	}
	cutoffDate := now.Add(-calendarQuietAge).UTC().Format(time.DateOnly)
	for index, episode := range episodes {
		if episode.EpisodeNumber != index+1 || !calendarAiredBefore(episode.AirDate, cutoffDate) {
			return time.Time{}, nil
		}
	}
	return quietUntil, nil
}

// Failure of this supplementary lookup leaves the successful season read
// usable. The optional proof shares the calendar's existing worker deadline.
func (s *SubscriptionCalendarService) confirmAiringFinished(ctx context.Context, id int64, schedule tmdb.SeasonSchedule) (finished, checked bool) {
	now := s.now()
	if !calendarPastOnly(schedule.Episodes, now) {
		return false, true
	}
	if s.loadSeries == nil {
		return false, false
	}
	proofCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	series, err := s.loadSeries(proofCtx, id)
	if err != nil || series.TMDBID != id || series.MediaType != "tv" {
		return false, false
	}
	for _, season := range series.Seasons {
		if season.SeasonNumber == schedule.SeasonNumber {
			finished := coverageSeasonFinished(series, season, schedule.Episodes, false, now)
			return finished, finished || series.Status != ""
		}
	}
	return false, false
}

// An explicit historical correction can keep prior proof through a failed
// supplementary lookup only while the validated, complete dates stay identical.
func calendarEpisodeDatesUnchanged(db *gorm.DB, key string, episodes []tmdb.EpisodeSnapshot) bool {
	var cached []models.CalendarEpisode
	if err := db.Select("episode_number", "tmdb_id", "air_date").Where("season_key = ?", key).
		Order("episode_number").Limit(len(episodes) + 1).Find(&cached).Error; err != nil || len(cached) != len(episodes) {
		return false
	}
	for i, episode := range episodes {
		old := cached[i]
		if old.EpisodeNumber != episode.EpisodeNumber || old.TMDBID != episode.TMDBID || old.AirDate != episode.AirDate {
			return false
		}
	}
	return true
}

func calendarLoadFailure(err error) string {
	switch ErrorCode(err) {
	case CodeTMDBTokenInvalid, CodeTMDBUnavailable:
		return calendarIssueCredentials
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return calendarIssueNetwork
	}
	switch tmdb.ErrorCode(err) {
	case tmdb.ErrorAuthFailed:
		return calendarIssueCredentials
	case tmdb.ErrorNetworkUnavailable:
		return calendarIssueNetwork
	case tmdb.ErrorRequestFailed:
		return calendarIssueRejected
	case tmdb.ErrorInvalidResponse:
		var typed *tmdb.ClientError
		if errors.As(err, &typed) {
			return calendarIssueResponse
		}
	}
	return calendarIssueUnavailable
}

func calendarSafeIssueReason(code string) string {
	switch code {
	case calendarIssueCredentials, calendarIssueNetwork, calendarIssueRejected, calendarIssueResponse, calendarIssueStorage:
		return code
	default:
		return calendarIssueUnavailable
	}
}

func calendarIssueDetails(reason string) (string, time.Duration) {
	switch reason {
	case calendarIssueCredentials:
		return "TMDB 凭据或接口配置不可用", 6 * time.Hour
	case calendarIssueNetwork:
		return "连接 TMDB 超时或暂时不可达", 15 * time.Minute
	case calendarIssueRejected:
		return "TMDB 未接受排期请求", time.Hour
	case calendarIssueResponse:
		return "TMDB 返回的季集排期无法核验", time.Hour
	case calendarIssueStorage:
		return "本地排期保存失败", time.Hour
	default:
		return "播出计划暂时无法更新", 15 * time.Minute
	}
}

func (s *SubscriptionCalendarService) refreshFailed(ctx context.Context, claimed ClaimedJob, payload calendarJobPayload, reason string, fields map[string]any) WorkerResult {
	message, delay := calendarIssueDetails(reason)
	var retryAt time.Time
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		current, err := s.currentPublication(tx, payload, &claimed)
		if err != nil {
			return err
		}
		if current.LastErrorCode != "" {
			delay = max(delay, time.Hour)
		}
		now := s.now()
		retryAt = now.Add(delay)
		if current.Complete && current.FetchedAt != nil {
			message += "，已保留之前的排期"
		}
		return tx.Model(&models.CalendarSeason{}).Where("key = ? AND generation = ? AND revision = ?", payload.Key, payload.Generation, payload.Revision).
			Updates(map[string]any{"retry_at": retryAt, "last_error_code": reason, "updated_at": now}).Error
	})
	if errors.Is(err, errCalendarSuperseded) || ctx.Err() != nil {
		return WorkerResult{}
	}
	if err != nil {
		return WorkerResult{ErrorCode: "calendar_storage_failed", ErrorMessage: "排期更新状态保存失败"}
	}
	fields["error_code"] = reason
	fields["retry_at"] = retryAt
	serverlog.RecordBusiness(ctx, s.log, serverlog.OperationMetadataSnapshot, serverlog.BusinessEvent{Code: "calendar.refresh.failed", Stage: "metadata", Outcome: "failed", Message: message, Fields: fields})
	return WorkerResult{ErrorCode: "calendar_" + reason, ErrorMessage: message, RetryAt: &retryAt}
}
