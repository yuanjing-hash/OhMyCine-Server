package services

import (
	"context"
	"errors"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const calendarMaintenanceBatch = 256
const calendarMaintenanceCursor = "seasons-v1"

// Scan a bounded keyset page rather than permanently revisiting the first
// subscriptions. A season-level bookmark lets a large subscription yield after
// enqueueing a few seasons and resume the remainder after restart.
func (s *SubscriptionCalendarService) enqueueDueRoundRobin(ctx context.Context, limit int) (resultErr error) {
	if limit <= 0 {
		return nil
	}
	s.maintenanceMu.Lock()
	defer s.maintenanceMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := s.pruneUnused(ctx); err != nil {
		return err
	}
	generation, err := s.generation()
	if err != nil {
		return err
	}
	bookmark := models.CalendarRefreshCursor{Name: calendarMaintenanceCursor, Generation: generation, SeasonNumber: -1}
	var saved models.CalendarRefreshCursor
	if err := s.db.WithContext(ctx).First(&saved, "name = ?", calendarMaintenanceCursor).Error; err == nil {
		if saved.Generation == generation && len(saved.SubscriptionID) <= 36 && saved.SeasonNumber >= -1 && saved.SeasonNumber <= 10000 {
			bookmark = saved
		}
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	progressed := false
	defer func() {
		if !progressed {
			return
		}
		// Only bounded local bookkeeping survives a request timeout. No provider
		// work is extended, and active jobs keep their original identities.
		writeCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		defer stop()
		bookmark.UpdatedAt = s.now()
		err := s.db.WithContext(writeCtx).Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "name"}}, DoUpdates: clause.AssignmentColumns([]string{"generation", "subscription_id", "season_number", "updated_at"})}).Create(&bookmark).Error
		if resultErr == nil {
			resultErr = err
		}
	}()
	var seasons []models.FollowSubscriptionSeason
	query := s.db.WithContext(ctx).Model(&models.FollowSubscriptionSeason{}).
		Where("EXISTS (SELECT 1 FROM follow_subscriptions f WHERE f.id = follow_subscription_seasons.subscription_id)")
	if bookmark.SubscriptionID != "" {
		query = query.Where("subscription_id > ? OR (subscription_id = ? AND season_number > ?)", bookmark.SubscriptionID, bookmark.SubscriptionID, bookmark.SeasonNumber)
	}
	if err := query.Order("subscription_id,season_number").Limit(calendarMaintenanceBatch).Find(&seasons).Error; err != nil {
		return err
	}
	if len(seasons) == 0 {
		bookmark.SubscriptionID, bookmark.SeasonNumber = "", -1
		progressed = true
		return nil // next poll starts the next round; no unbounded wrap loop
	}
	ids := make([]string, 0, len(seasons))
	seenIDs := map[string]bool{}
	for _, item := range seasons {
		if !seenIDs[item.SubscriptionID] {
			ids = append(ids, item.SubscriptionID)
			seenIDs[item.SubscriptionID] = true
		}
	}
	var records []models.FollowSubscription
	if err := s.db.WithContext(ctx).Where("id IN ?", ids).Find(&records).Error; err != nil {
		return err
	}
	valid := map[string]FollowSummary{}
	for _, record := range records {
		item, err := followSummary(record)
		if err == nil {
			valid[record.ID] = item
		} // one damaged subscription cannot block unrelated metadata maintenance
	}
	queued, seenKeys := 0, map[string]bool{}
	for _, item := range seasons {
		if queued >= min(limit, 50) {
			break
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		sub, ok := valid[item.SubscriptionID]
		selected := false
		for _, season := range sub.Snapshot.Seasons {
			if season == item.SeasonNumber {
				selected = true
				break
			}
		}
		if ok && selected && sub.TMDBID == item.TMDBID {
			key := calendarSeasonKey(generation, item.TMDBID, item.SeasonNumber)
			if !seenKeys[key] {
				seenKeys[key] = true
				sub.Snapshot.Seasons = []int{item.SeasonNumber}
				result, err := s.enqueueRows(ctx, []FollowSummary{sub}, false, min(limit, 50)-queued)
				if err != nil {
					return err
				}
				queued += result.Queued
			}
		}
		bookmark.SubscriptionID, bookmark.SeasonNumber = item.SubscriptionID, item.SeasonNumber
		progressed = true
	}
	return nil
}
