package services

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	serverlog "github.com/yuanjing-hash/OhMyCine-Server/internal/logging"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/metadata/tmdb"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const JobTypeSubscriptionCalendar = "subscription_calendar_refresh"
const calendarLanguage = "zh-CN"
const maxCalendarSubscriptions = 2000
const maxCalendarSeasons = 4000
const maxCalendarProjectedEvents = 10000

var errCalendarSuperseded = errors.New("calendar observation superseded")

type CalendarQuery struct {
	Start          string
	End            string
	Timezone       string
	Status         string
	SubscriptionID string
	Cursor         string
	PageSize       int
}
type CalendarEvent struct {
	ID                 string `json:"id"`
	SubscriptionID     string `json:"subscription_id"`
	TMDBID             int64  `json:"tmdb_id"`
	Title              string `json:"title"`
	PosterRef          string `json:"poster_ref,omitempty"`
	SubscriptionStatus string `json:"subscription_status"`
	SeasonNumber       int    `json:"season_number"`
	EpisodeNumber      int    `json:"episode_number"`
	EpisodeTitle       string `json:"episode_title,omitempty"`
	AirDate            string `json:"air_date,omitempty"`
	LibraryStatus      string `json:"library_status"`
	Activity           string `json:"activity"`
	ScheduleState      string `json:"schedule_state"`
}
type CalendarSubscription struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	PosterRef string `json:"poster_ref,omitempty"`
	Status    string `json:"status"`
	Seasons   []int  `json:"seasons"`
}
type CalendarPage struct {
	Events            []CalendarEvent        `json:"events"`
	Undated           []CalendarEvent        `json:"undated"`
	Subscriptions     []CalendarSubscription `json:"subscriptions"`
	HasMore           bool                   `json:"has_more"`
	NextCursor        string                 `json:"next_cursor,omitempty"`
	Partial           bool                   `json:"partial"`
	Reasons           []string               `json:"reasons"`
	MissingSeasons    int                    `json:"missing_seasons"`
	StaleSeasons      int                    `json:"stale_seasons"`
	RefreshingSeasons int                    `json:"refreshing_seasons"`
	UndatedCount      int                    `json:"undated_count"`
	DateBasis         string                 `json:"date_basis"`
	Scope             string                 `json:"scope"`
	GeneratedAt       time.Time              `json:"generated_at"`
	Timezone          string                 `json:"timezone"`
	Start             string                 `json:"start"`
	End               string                 `json:"end"`
}
type CalendarRefreshResult struct {
	Queued        int  `json:"queued"`
	AlreadyQueued int  `json:"already_queued"`
	CoolingDown   int  `json:"cooling_down"`
	Partial       bool `json:"partial"`
}
type calendarJobPayload struct {
	Key        string `json:"key"`
	Generation uint64 `json:"generation"`
	TMDBID     int64  `json:"tmdb_id"`
	Season     int    `json:"season"`
	Revision   uint64 `json:"revision"`
}
type calendarCursor struct {
	Scope    string `json:"s"`
	Revision string `json:"r"`
	Offset   int    `json:"o"`
}

type SubscriptionCalendarService struct {
	db            *gorm.DB
	queue         *QueueService
	metadata      *MetadataSettingsService
	coverage      *MediaCoverageService
	now           func() time.Time
	load          func(context.Context, int64, int) (tmdb.SeasonSchedule, error)
	enqueueMu     sync.Mutex
	maintenanceMu sync.Mutex
	refreshSlots  chan struct{}
	log           zerolog.Logger
}

func NewSubscriptionCalendarService(db *gorm.DB, queue *QueueService, metadata *MetadataSettingsService, coverage *MediaCoverageService) *SubscriptionCalendarService {
	s := &SubscriptionCalendarService{db: db, queue: queue, metadata: metadata, coverage: coverage, now: func() time.Time { return time.Now().UTC() }, refreshSlots: make(chan struct{}, 2), log: zerolog.Nop()}
	s.load = func(ctx context.Context, id int64, season int) (tmdb.SeasonSchedule, error) {
		if metadata == nil {
			return tmdb.SeasonSchedule{}, appError(CodeTMDBUnavailable, "排期元数据尚未配置", nil)
		}
		client, err := metadata.Client()
		if err != nil {
			return tmdb.SeasonSchedule{}, err
		}
		return client.GetTVSeasonSchedule(ctx, id, season, calendarLanguage)
	}
	return s
}
func (s *SubscriptionCalendarService) SetLogger(log zerolog.Logger) { s.log = log }
func (s *SubscriptionCalendarService) generation() (uint64, error) {
	var settings models.MetadataSettings
	if err := s.db.Select("revision").First(&settings, 1).Error; err != nil {
		return 0, err
	}
	return settings.Revision, nil
}
func calendarSeasonKey(generation uint64, id int64, season int) string {
	return fmt.Sprintf("v1:%d:%s:%d:%d", generation, calendarLanguage, id, season)
}
func calendarReadAllowed(actor Actor) bool {
	return actor.Can(authz.PermissionFollowsReadOwn) || actor.Can(authz.PermissionFollowsReadAll)
}

// subscriptionRows uses keyset order and never assumes the first list page is
// the full authorized subscription set. It performs no metadata network I/O.
func (s *SubscriptionCalendarService) subscriptionRows(ctx context.Context, actor Actor, query CalendarQuery) ([]FollowSummary, bool, error) {
	if !calendarReadAllowed(actor) {
		return nil, false, appError(CodePermissionDenied, "无权查看订阅日历", nil)
	}
	q := s.db.WithContext(ctx).Model(&models.FollowSubscription{})
	if !actor.Can(authz.PermissionFollowsReadAll) {
		q = q.Where("owner_id = ?", actor.User.ID)
	}
	if query.Status != "" {
		q = q.Where("status = ?", query.Status)
	}
	if query.SubscriptionID != "" {
		q = q.Where("id = ?", query.SubscriptionID)
	}
	var rows []models.FollowSubscription
	if err := q.Order("id").Limit(maxCalendarSubscriptions + 1).Find(&rows).Error; err != nil {
		return nil, false, err
	}
	partial := len(rows) > maxCalendarSubscriptions
	if partial {
		rows = rows[:maxCalendarSubscriptions]
	}
	result := make([]FollowSummary, 0, len(rows))
	for _, row := range rows {
		item, err := followSummaryForActor(actor, row)
		if err != nil {
			return nil, false, err
		}
		// Follow intentionally hides the work after target-library revocation.
		// Never rehydrate its title or episode facts from the shared cache.
		if item.Snapshot.MediaLibraryID == 0 {
			continue
		}
		result = append(result, item)
	}
	return result, partial, nil
}
func validateCalendarQuery(q CalendarQuery) (CalendarQuery, error) {
	start, e1 := time.Parse("2006-01-02", q.Start)
	end, e2 := time.Parse("2006-01-02", q.End)
	if e1 != nil || e2 != nil || !end.After(start) || end.Sub(start) > 42*24*time.Hour {
		return q, appError(CodeInvalidRequest, "请选择不超过42天的有效日期范围", nil)
	}
	if q.Timezone == "" {
		q.Timezone = "UTC"
	}
	if _, err := time.LoadLocation(q.Timezone); err != nil {
		return q, appError(CodeInvalidRequest, "日历时区无效", nil)
	}
	if q.Status != "" && q.Status != models.FollowStatusActive && q.Status != models.FollowStatusPaused && q.Status != models.FollowStatusCompleted && q.Status != models.FollowStatusBlocked {
		return q, appError(CodeInvalidRequest, "订阅状态无效", nil)
	}
	if len(q.SubscriptionID) > 36 || len(q.Cursor) > 2048 {
		return q, appError(CodeInvalidRequest, "日历筛选参数无效", nil)
	}
	if q.PageSize == 0 {
		q.PageSize = 200
	}
	if q.PageSize < 1 || q.PageSize > 500 {
		return q, appError(CodeInvalidRequest, "日历分页大小无效", nil)
	}
	return q, nil
}
func (s *SubscriptionCalendarService) Read(ctx context.Context, actor Actor, query CalendarQuery) (CalendarPage, error) {
	q, err := validateCalendarQuery(query)
	if err != nil {
		return CalendarPage{}, err
	}
	rows, partial, err := s.subscriptionRows(ctx, actor, q)
	if err != nil {
		return CalendarPage{}, err
	}
	generation, err := s.generation()
	if err != nil {
		return CalendarPage{}, err
	}
	now := s.now()
	page := CalendarPage{Events: []CalendarEvent{}, Undated: []CalendarEvent{}, Subscriptions: []CalendarSubscription{}, Reasons: []string{}, Partial: partial, DateBasis: "expected_air_date", Scope: "own", GeneratedAt: now, Timezone: q.Timezone, Start: q.Start, End: q.End}
	if actor.Can(authz.PermissionFollowsReadAll) {
		page.Scope = "all"
	}
	if partial {
		page.Reasons = append(page.Reasons, "subscription_budget")
	}
	scopeRaw, _ := json.Marshal([]any{actor.User.ID, actor.Permissions, actor.DeniedPermissions, actor.ResourceRules, actor.ResourceAccessPolicies, q.Start, q.End, q.Timezone, q.Status, q.SubscriptionID})
	scopeHash := sha256.Sum256(scopeRaw)
	scope := hex.EncodeToString(scopeHash[:])
	versions := []string{}
	for _, sub := range rows {
		page.Subscriptions = append(page.Subscriptions, CalendarSubscription{ID: sub.ID, Title: sub.Title, PosterRef: sub.PosterRef, Status: sub.Status, Seasons: sub.Snapshot.Seasons})
		versions = append(versions, fmt.Sprintf("%s:%d", sub.ID, sub.Revision))
	}
	events := []CalendarEvent{}
	seasonCount := 0
	type workFacts struct {
		entries  []models.MediaLibraryEntry
		reliable bool
	}
	facts := map[int64]workFacts{}
	keys := make([]string, 0)
	seenKeys := map[string]bool{}
	for _, sub := range rows {
		for _, season := range sub.Snapshot.Seasons {
			key := calendarSeasonKey(generation, sub.TMDBID, season)
			if !seenKeys[key] && len(keys) < maxCalendarSeasons {
				keys = append(keys, key)
				seenKeys[key] = true
			}
		}
	}
	heads, episodeRows, running, bounded, err := s.scheduleFacts(ctx, keys, q.Start, q.End)
	if err != nil {
		return CalendarPage{}, err
	}
	if bounded {
		page.Partial = true
		page.Reasons = appendUniqueString(page.Reasons, "schedule_budget")
	}
	if s.coverage != nil && actor.Can(authz.PermissionMediaLibrariesRead) {
		identities := make([]int64, 0, len(rows))
		seen := map[int64]bool{}
		for _, sub := range rows {
			if !seen[sub.TMDBID] {
				identities = append(identities, sub.TMDBID)
				seen[sub.TMDBID] = true
			}
		}
		_, entries, reliable, scanState, readErr := s.coverage.coverageCatalogMany(ctx, actor, "tv", identities, 50000)
		if readErr != nil {
			return CalendarPage{}, readErr
		}
		for _, id := range identities {
			facts[id] = workFacts{reliable: reliable}
		}
		for _, entry := range entries {
			if entry.TMDBID != nil {
				value := facts[*entry.TMDBID]
				value.entries = append(value.entries, entry)
				facts[*entry.TMDBID] = value
			}
		}
		if scanState == "bounded" {
			page.Partial = true
			page.Reasons = appendUniqueString(page.Reasons, "library_budget")
		}
	}
	subscriptionIDs := make([]string, 0, len(rows))
	for _, sub := range rows {
		subscriptionIDs = append(subscriptionIDs, sub.ID)
	}
	activities, activityBounded, err := s.episodeActivityFacts(ctx, actor, subscriptionIDs)
	if err != nil {
		return CalendarPage{}, err
	}
	if activityBounded {
		page.Partial = true
		page.Reasons = appendUniqueString(page.Reasons, "activity_budget")
	}
projection:
	for _, sub := range rows {
		wf := facts[sub.TMDBID]
		presence := map[[2]int]bool{}
		for _, entry := range wf.entries {
			if entry.Season != nil && entry.Episode != nil {
				presence[[2]int{*entry.Season, *entry.Episode}] = true
			}
		}
		for _, season := range sub.Snapshot.Seasons {
			seasonCount++
			if seasonCount > maxCalendarSeasons {
				page.Partial = true
				page.Reasons = appendUniqueString(page.Reasons, "season_budget")
				break
			}
			key := calendarSeasonKey(generation, sub.TMDBID, season)
			if running[key] {
				page.RefreshingSeasons++
			}
			cached, found := heads[key]
			if !found || !cached.Complete || cached.FetchedAt == nil || now.Sub(*cached.FetchedAt) > 30*24*time.Hour {
				page.MissingSeasons++
				page.Partial = true
				page.Reasons = appendUniqueString(page.Reasons, "schedule_pending")
				versions = append(versions, key+":missing")
				continue
			}
			versions = append(versions, fmt.Sprintf("%s:%d", key, cached.Revision))
			state := "fresh"
			if cached.FreshUntil == nil || !now.Before(*cached.FreshUntil) {
				state = "stale"
				page.StaleSeasons++
				page.Partial = true
				page.Reasons = appendUniqueString(page.Reasons, "schedule_stale")
			}
			if cached.LastErrorCode != "" {
				page.Reasons = appendUniqueString(page.Reasons, "schedule_unavailable")
			}
			for _, episode := range episodeRows[key] {
				if len(events)+page.UndatedCount >= maxCalendarProjectedEvents {
					page.Partial = true
					page.Reasons = appendUniqueString(page.Reasons, "event_budget")
					break projection
				}
				library := "unknown"
				if presence[[2]int{season, episode.EpisodeNumber}] {
					library = "present"
				} else if wf.reliable {
					library = "missing"
				}
				activity := activities[sub.ID][[2]int{season, episode.EpisodeNumber}]
				if activity == "" {
					activity = "none"
					if activityBounded {
						activity = "unknown"
					}
				}
				event := CalendarEvent{ID: fmt.Sprintf("%s:%d:%d", sub.ID, season, episode.EpisodeNumber), SubscriptionID: sub.ID, TMDBID: sub.TMDBID, Title: sub.Title, PosterRef: sub.PosterRef, SubscriptionStatus: sub.Status, SeasonNumber: season, EpisodeNumber: episode.EpisodeNumber, EpisodeTitle: episode.Name, AirDate: episode.AirDate, LibraryStatus: library, Activity: activity, ScheduleState: state}
				if episode.AirDate == "" {
					page.UndatedCount++
					if len(page.Undated) < 100 {
						page.Undated = append(page.Undated, event)
					}
				} else {
					events = append(events, event)
				}
			}
		}
	}
	if page.UndatedCount > len(page.Undated) {
		page.Partial = true
		page.Reasons = appendUniqueString(page.Reasons, "undated_budget")
	}
	sort.Slice(events, func(i, j int) bool {
		a, b := events[i], events[j]
		if a.AirDate != b.AirDate {
			return a.AirDate < b.AirDate
		}
		if a.Title != b.Title {
			return a.Title < b.Title
		}
		if a.SeasonNumber != b.SeasonNumber {
			return a.SeasonNumber < b.SeasonNumber
		}
		if a.EpisodeNumber != b.EpisodeNumber {
			return a.EpisodeNumber < b.EpisodeNumber
		}
		return a.ID < b.ID
	})
	versionRaw, _ := json.Marshal([]any{versions, events, page.Undated})
	versionHash := sha256.Sum256(versionRaw)
	revision := hex.EncodeToString(versionHash[:])
	offset := 0
	if q.Cursor != "" {
		var cursor calendarCursor
		raw, decodeErr := base64.RawURLEncoding.DecodeString(q.Cursor)
		if decodeErr != nil || json.Unmarshal(raw, &cursor) != nil || cursor.Scope != scope || cursor.Revision != revision || cursor.Offset < 0 || cursor.Offset > len(events) {
			return CalendarPage{}, appError(CodeConflict, "日历内容已更新，请刷新当前日期范围", nil)
		}
		offset = cursor.Offset
	}
	end := min(len(events), offset+q.PageSize)
	page.Events = events[offset:end]
	page.HasMore = end < len(events)
	if page.HasMore {
		raw, _ := json.Marshal(calendarCursor{Scope: scope, Revision: revision, Offset: end})
		page.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
	}
	return page, nil
}

// Headers and date rows are read from one query-only snapshot. The range is
// bounded before joining user subscriptions; a shared season is read only once.
func (s *SubscriptionCalendarService) scheduleFacts(ctx context.Context, keys []string, start, end string) (map[string]models.CalendarSeason, map[string][]models.CalendarEpisode, map[string]bool, bool, error) {
	heads := map[string]models.CalendarSeason{}
	episodes := map[string][]models.CalendarEpisode{}
	running := map[string]bool{}
	var store *CatalogSnapshotStore
	if s.coverage != nil {
		store = s.coverage.catalogStore
	}
	facade := &MediaLibraryService{db: s.db, catalogStore: store}
	bounded := false
	count := 0
	err := facade.withCatalogReadTx(ctx, func(tx *gorm.DB) error {
		for offset := 0; offset < len(keys); offset += 500 {
			chunk := keys[offset:min(offset+500, len(keys))]
			var batch []models.CalendarSeason
			if err := tx.Where("key IN ?", chunk).Find(&batch).Error; err != nil {
				return err
			}
			for _, head := range batch {
				heads[head.Key] = head
			}
			var active []string
			if err := tx.Model(&models.Job{}).Where("job_type = ? AND resource_key IN ? AND status IN ?", JobTypeSubscriptionCalendar, chunk, activeJobStatuses()).Distinct("resource_key").Pluck("resource_key", &active).Error; err != nil {
				return err
			}
			for _, key := range active {
				running[key] = true
			}
			if bounded {
				continue
			}
			var rows []models.CalendarEpisode
			if err := tx.Where("season_key IN ? AND ((air_date >= ? AND air_date < ?) OR air_date = '')", chunk, start, end).Order("season_key,air_date,episode_number").Limit(50001 - count).Find(&rows).Error; err != nil {
				return err
			}
			if count+len(rows) > 50000 {
				rows = rows[:50000-count]
				bounded = true
			}
			count += len(rows)
			for _, episode := range rows {
				episodes[episode.SeasonKey] = append(episodes[episode.SeasonKey], episode)
			}
		}
		return nil
	})
	return heads, episodes, running, bounded, err
}

func (s *SubscriptionCalendarService) RequestRefresh(ctx context.Context, actor Actor, subscriptionID string) (CalendarRefreshResult, error) {
	if len(subscriptionID) > 36 || strings.TrimSpace(subscriptionID) != subscriptionID {
		return CalendarRefreshResult{}, appError(CodeInvalidRequest, "订阅选择无效", nil)
	}
	rows, partial, err := s.subscriptionRows(ctx, actor, CalendarQuery{SubscriptionID: subscriptionID})
	if err != nil {
		return CalendarRefreshResult{}, err
	}
	result, err := s.enqueueRows(ctx, rows, true, 50)
	result.Partial = result.Partial || partial
	return result, err
}

// EnqueueDue is called by the existing unified scheduler maintenance pass.
// Cache maintenance has no own goroutine, no second Cron and no HTTP side effects.
func (s *SubscriptionCalendarService) EnqueueDue(ctx context.Context, limit int) error {
	return s.enqueueDueRoundRobin(ctx, limit)
}
func (s *SubscriptionCalendarService) enqueueRows(ctx context.Context, rows []FollowSummary, force bool, limit int) (CalendarRefreshResult, error) {
	result := CalendarRefreshResult{}
	if s.queue == nil {
		return result, appError(CodeQueueWorkerUnavailable, "排期更新服务暂不可用", nil)
	}
	s.enqueueMu.Lock()
	defer s.enqueueMu.Unlock()
	generation, err := s.generation()
	if err != nil {
		return result, err
	}
	now := s.now()
	seen := map[string]bool{}
	for _, sub := range rows {
		for _, season := range sub.Snapshot.Seasons {
			key := calendarSeasonKey(generation, sub.TMDBID, season)
			if seen[key] {
				continue
			}
			seen[key] = true
			var cached models.CalendarSeason
			err := s.db.WithContext(ctx).First(&cached, "key = ?", key).Error
			if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
				return result, err
			}
			if !force && cached.FreshUntil != nil && now.Before(*cached.FreshUntil) {
				continue
			}
			if (cached.RetryAt != nil && now.Before(*cached.RetryAt)) || (cached.LastRequestedAt != nil && now.Sub(*cached.LastRequestedAt) < time.Minute) {
				result.CoolingDown++
				continue
			}
			var active int64
			if err := s.db.WithContext(ctx).Model(&models.Job{}).Where("job_type = ? AND resource_key = ? AND status IN ?", JobTypeSubscriptionCalendar, key, activeJobStatuses()).Count(&active).Error; err != nil {
				return result, err
			}
			if active > 0 {
				result.AlreadyQueued++
				continue
			}
			if result.Queued >= limit {
				result.Partial = true
				continue
			}
			if cached.Key == "" {
				var heads int64
				if err := s.db.WithContext(ctx).Model(&models.CalendarSeason{}).Count(&heads).Error; err != nil {
					return result, err
				}
				if heads >= 2048 {
					result.Partial = true
					continue
				}
				cached = models.CalendarSeason{Key: key, Generation: generation, TMDBID: sub.TMDBID, SeasonNumber: season, Language: calendarLanguage, UpdatedAt: now}
				if err := s.db.Clauses(clause.OnConflict{DoNothing: true}).Create(&cached).Error; err != nil {
					return result, err
				}
			}
			title := []rune(serverlog.BusinessDisplayName(sub.Title))
			if len(title) > 50 {
				title = append(title[:50], '…')
			}
			_, err = s.queue.EnqueueWith(EnqueueJobInput{System: true, JobType: JobTypeSubscriptionCalendar, Priority: -20, DisplayName: fmt.Sprintf("更新播出计划 · %s S%02d", string(title), season), Provider: "metadata", ResourceKey: key, Payload: calendarJobPayload{Key: key, Generation: generation, TMDBID: sub.TMDBID, Season: season, Revision: cached.Revision}}, func(tx *gorm.DB, _ models.Job) error {
				return tx.Model(&models.CalendarSeason{}).Where("key = ?", key).Updates(map[string]any{"last_requested_at": now, "updated_at": now}).Error
			})
			if err != nil {
				return result, err
			}
			result.Queued++
		}
	}
	return result, nil
}

func (s *SubscriptionCalendarService) Run(ctx context.Context, _ JobRuntime, claimed ClaimedJob) WorkerResult {
	var payload calendarJobPayload
	if json.Unmarshal([]byte(claimed.Job.PayloadJSON), &payload) != nil || payload.Key != calendarSeasonKey(payload.Generation, payload.TMDBID, payload.Season) {
		return WorkerResult{ErrorCode: CodeInvalidRequest, ErrorMessage: "排期更新参数无效"}
	}
	select {
	case s.refreshSlots <- struct{}{}:
		defer func() { <-s.refreshSlots }()
	case <-ctx.Done():
		return WorkerResult{ErrorCode: "calendar_cancelled", ErrorMessage: "排期更新已取消"}
	}
	generation, err := s.generation()
	if err != nil {
		return WorkerResult{ErrorCode: "calendar_storage_failed", ErrorMessage: "排期状态读取失败"}
	}
	if generation != payload.Generation {
		return WorkerResult{}
	}
	var referenced int64
	if err := s.db.WithContext(ctx).Model(&models.FollowSubscriptionSeason{}).Where("tmdb_id = ? AND season_number = ?", payload.TMDBID, payload.Season).Count(&referenced).Error; err != nil {
		return WorkerResult{ErrorCode: "calendar_storage_failed", ErrorMessage: "排期范围读取失败"}
	}
	if referenced == 0 {
		return WorkerResult{}
	}
	readCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	fields := map[string]any{"job_id": claimed.Job.ID, "display_name": claimed.Job.DisplayName, "season": payload.Season}
	serverlog.RecordBusiness(ctx, s.log, serverlog.OperationMetadataSnapshot, serverlog.BusinessEvent{Code: "calendar.refresh.start", Stage: "metadata", Outcome: "started", Message: "开始更新" + serverlog.BusinessDisplayName(claimed.Job.DisplayName), Fields: fields})
	schedule, err := s.load(readCtx, payload.TMDBID, payload.Season)
	if err != nil {
		if saveErr := s.recordFailure(ctx, claimed, payload, "schedule_unavailable", 15*time.Minute); saveErr != nil {
			if errors.Is(saveErr, errCalendarSuperseded) {
				return WorkerResult{}
			}
			return WorkerResult{ErrorCode: "calendar_storage_failed", ErrorMessage: "排期更新状态保存失败"}
		}
		serverlog.RecordBusiness(ctx, s.log, serverlog.OperationMetadataSnapshot, serverlog.BusinessEvent{Code: "calendar.refresh.failed", Stage: "metadata", Outcome: "failed", Message: "播出计划更新暂未完成，已保留之前保存的排期并安排稍后重试", Fields: fields})
		return WorkerResult{ErrorCode: "calendar_schedule_unavailable", ErrorMessage: "播出计划暂时无法更新，已保留之前的排期"}
	}
	if len(schedule.Episodes) > tmdb.MaxCalendarEpisodes || schedule.SeasonNumber != payload.Season {
		return WorkerResult{ErrorCode: "calendar_response_invalid", ErrorMessage: "播出计划响应不完整"}
	}
	if err := s.publish(ctx, payload, schedule, &claimed); err != nil {
		if errors.Is(err, errCalendarSuperseded) {
			serverlog.RecordBusiness(ctx, s.log, serverlog.OperationMetadataSnapshot, serverlog.BusinessEvent{Code: "calendar.refresh.superseded", Stage: "publish", Outcome: "skipped", Message: "播出计划的配置或缓存版本已更新，本次旧结果未发布", Fields: fields})
			return WorkerResult{}
		}
		_ = s.recordFailure(ctx, claimed, payload, "schedule_storage_unavailable", time.Hour)
		return WorkerResult{ErrorCode: "calendar_storage_failed", ErrorMessage: "播出计划保存失败，已保留之前的排期"}
	}
	fields["episode_count"] = len(schedule.Episodes)
	serverlog.RecordBusiness(ctx, s.log, serverlog.OperationMetadataSnapshot, serverlog.BusinessEvent{Code: "calendar.refresh.completed", Stage: "publish", Outcome: "completed", Message: fmt.Sprintf("播出计划已更新，共核对%d集；入库状态继续以当前媒体库为准", len(schedule.Episodes)), Fields: fields})
	return WorkerResult{}
}

// currentPublication fences both successful and failed observations in the
// same short write transaction. A retired attempt cannot change the cache.
func (s *SubscriptionCalendarService) currentPublication(tx *gorm.DB, payload calendarJobPayload, claimed *ClaimedJob) error {
	if claimed != nil {
		if s.queue == nil {
			return errCalendarSuperseded
		}
		job, err := s.queue.verifyLease(tx, claimed.Job.ID, claimed.LeaseToken)
		if ErrorCode(err) == CodeQueueLeaseInvalid || ErrorCode(err) == CodeNotFound {
			return errCalendarSuperseded
		}
		if err != nil {
			return err
		}
		if job.Generation != claimed.Job.Generation || job.StartedGeneration != claimed.Job.StartedGeneration {
			return errCalendarSuperseded
		}
	}
	var settings models.MetadataSettings
	if err := tx.Select("revision").First(&settings, 1).Error; err != nil {
		return err
	}
	if settings.Revision != payload.Generation {
		return errCalendarSuperseded
	}
	var references int64
	if err := tx.Model(&models.FollowSubscriptionSeason{}).Where("tmdb_id = ? AND season_number = ?", payload.TMDBID, payload.Season).Count(&references).Error; err != nil {
		return err
	}
	if references == 0 {
		return errCalendarSuperseded
	}
	var current models.CalendarSeason
	if err := tx.First(&current, "key = ?", payload.Key).Error; err != nil {
		return err
	}
	if current.Revision != payload.Revision || current.Generation != payload.Generation {
		return errCalendarSuperseded
	}
	return nil
}
func (s *SubscriptionCalendarService) recordFailure(ctx context.Context, claimed ClaimedJob, payload calendarJobPayload, code string, delay time.Duration) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := s.currentPublication(tx, payload, &claimed); err != nil {
			return err
		}
		return tx.Model(&models.CalendarSeason{}).Where("key = ? AND generation = ? AND revision = ?", payload.Key, payload.Generation, payload.Revision).Updates(map[string]any{"retry_at": s.now().Add(delay), "last_error_code": code, "updated_at": s.now()}).Error
	})
}
func (s *SubscriptionCalendarService) publish(ctx context.Context, payload calendarJobPayload, schedule tmdb.SeasonSchedule, claimed *ClaimedJob) error {
	rows := make([]models.CalendarEpisode, 0, len(schedule.Episodes))
	payloadBytes := int64(0)
	now := s.now()
	old := len(schedule.Episodes) > 0
	for _, episode := range schedule.Episodes {
		rows = append(rows, models.CalendarEpisode{SeasonKey: payload.Key, EpisodeNumber: episode.EpisodeNumber, TMDBID: episode.TMDBID, Name: episode.Name, AirDate: episode.AirDate})
		payloadBytes += int64(len(episode.Name) + len(episode.AirDate) + 40)
		if episode.AirDate == "" || episode.AirDate >= now.AddDate(0, 0, -30).Format("2006-01-02") {
			old = false
		}
	}
	ttl := 6 * time.Hour
	if old {
		ttl = 7 * 24 * time.Hour
	}
	fresh := now.Add(ttl)
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := s.currentPublication(tx, payload, claimed); err != nil {
			return err
		}
		var heads int64
		var episodeCount int64
		var bytes int64
		if err := tx.Model(&models.CalendarSeason{}).Count(&heads).Error; err != nil {
			return err
		}
		if err := tx.Model(&models.CalendarEpisode{}).Where("season_key <> ?", payload.Key).Count(&episodeCount).Error; err != nil {
			return err
		}
		if err := tx.Model(&models.CalendarSeason{}).Where("key <> ?", payload.Key).Select("COALESCE(SUM(payload_bytes),0)").Scan(&bytes).Error; err != nil {
			return err
		}
		if heads > 2048 || episodeCount+int64(len(rows)) > 500000 || bytes+payloadBytes > 128<<20 {
			return appError(CodeInvalidRequest, "排期缓存容量已达上限", nil)
		}
		if err := tx.Where("season_key = ?", payload.Key).Delete(&models.CalendarEpisode{}).Error; err != nil {
			return err
		}
		if len(rows) > 0 {
			if err := tx.CreateInBatches(&rows, 200).Error; err != nil {
				return err
			}
		}
		return tx.Model(&models.CalendarSeason{}).Where("key = ? AND generation = ? AND revision = ?", payload.Key, payload.Generation, payload.Revision).Updates(map[string]any{"revision": gorm.Expr("revision + 1"), "fetched_at": now, "fresh_until": fresh, "retry_at": nil, "last_error_code": "", "complete": true, "episode_count": len(rows), "payload_bytes": payloadBytes, "updated_at": now}).Error
	})
}

func appendUniqueString(values []string, value string) []string {
	for _, v := range values {
		if v == value {
			return values
		}
	}
	return append(values, value)
}

// Retire only bounded public cache rows no longer addressed by the current
// metadata generation or any subscribed season. Business records are untouched.
func (s *SubscriptionCalendarService) pruneUnused(ctx context.Context) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var settings models.MetadataSettings
		if err := tx.Select("revision").First(&settings, 1).Error; err != nil {
			return err
		}
		var rows []models.CalendarSeason
		if err := tx.Where("generation <> ? OR NOT EXISTS (SELECT 1 FROM follow_subscription_seasons fs WHERE fs.tmdb_id = calendar_seasons.tmdb_id AND fs.season_number = calendar_seasons.season_number)", settings.Revision).Order("updated_at,key").Limit(50).Find(&rows).Error; err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		keys := make([]string, 0, len(rows))
		for _, row := range rows {
			keys = append(keys, row.Key)
		}
		if err := tx.Where("season_key IN ?", keys).Delete(&models.CalendarEpisode{}).Error; err != nil {
			return err
		}
		return tx.Where("key IN ?", keys).Delete(&models.CalendarSeason{}).Error
	})
}
