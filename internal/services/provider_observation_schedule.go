package services

import (
	"context"
	"errors"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func observationIsHot(scope models.ProviderObservationScope, dir models.ProviderObservationItem, now time.Time) bool {
	if scope.Kind == "intake" || dir.ResourceID == scope.RootID {
		return true
	}
	if dir.CompletedRound < dir.QueueRound {
		return false
	}
	return dir.HotUntil != nil && dir.HotUntil.After(now)
}

func (s *ProviderObservationService) nextDirectory(ctx context.Context, scope models.ProviderObservationScope, now time.Time) (models.ProviderObservationItem, error) {
	var hot, cold models.ProviderObservationItem
	load := func(wantHot bool, out *models.ProviderObservationItem) error {
		q := observationCurrent(s.db.WithContext(ctx), scope.ID).Where("is_dir = ? AND excluded = ? AND absent_parent_round = 0 AND downlisted_at IS NULL AND due_at <= ?", true, false, now)
		condition := "(resource_id = ? OR (completed_round >= queue_round AND hot_until IS NOT NULL AND hot_until > ?))"
		if scope.Kind == "intake" {
			if !wantHot {
				return gorm.ErrRecordNotFound
			}
		} else if wantHot {
			q = q.Where(condition, scope.RootID, now)
		} else {
			q = q.Where("NOT "+condition, scope.RootID, now)
		}
		return q.Order("due_at, resource_id").First(out).Error
	}
	hotErr, coldErr := load(true, &hot), load(false, &cold)
	if hotErr != nil && !errors.Is(hotErr, gorm.ErrRecordNotFound) {
		return hot, hotErr
	}
	if coldErr != nil && !errors.Is(coldErr, gorm.ErrRecordNotFound) {
		return cold, coldErr
	}
	if hotErr != nil && coldErr != nil {
		return hot, ErrObservationDeferred
	}
	if hotErr != nil {
		return cold, nil
	}
	if coldErr != nil {
		return hot, nil
	}
	var budget models.ProviderObservationAccountBudget
	err := s.db.WithContext(ctx).First(&budget, "account_key = ?", scope.AccountKey).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return hot, err
	}
	if budget.ColdRequests <= budget.HotRequests {
		return cold, nil
	}
	return hot, nil
}

func (s *ProviderObservationService) oppositeDemandTx(tx *gorm.DB, account string, hot bool, now time.Time) (bool, error) {
	query := tx.Table("provider_observation_items AS i").Joins("JOIN provider_observation_scopes AS s ON s.id = i.scope_id").
		Where("s.account_key = ? AND s.enabled = ? AND i.batch_id = '' AND i.is_dir = ? AND i.excluded = ? AND i.absent_parent_round = 0 AND i.downlisted_at IS NULL AND i.due_at <= ?", account, true, true, false, now)
	hotSQL := "(s.kind = 'intake' OR i.resource_id = s.root_id OR (i.completed_round >= i.queue_round AND i.hot_until IS NOT NULL AND i.hot_until > ?))"
	if hot {
		query = query.Where("NOT "+hotSQL, now)
	} else {
		query = query.Where(hotSQL, now)
	}
	var count int64
	err := query.Count(&count).Error
	return count > 0, err
}

func (s *ProviderObservationService) claimAccount(ctx context.Context, scope models.ProviderObservationScope, hot bool, token string) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := s.validScopeTx(tx, scope); err != nil {
			return err
		}
		now := s.now()
		row := models.ProviderObservationAccountBudget{AccountKey: scope.AccountKey, WindowStart: now, UpdatedAt: now}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error; err != nil {
			return err
		}
		if err := tx.First(&row, "account_key = ?", scope.AccountKey).Error; err != nil {
			return err
		}
		if row.CooldownUntil.After(now) || row.NextRequestAt.After(now) || (row.LeaseToken != "" && row.LeaseUntil.After(now)) {
			return ErrObservationDeferred
		}
		if now.Sub(row.WindowStart) >= time.Hour {
			row.WindowStart = now
			row.HotRequests = 0
			row.ColdRequests = 0
		}
		if row.HotRequests+row.ColdRequests >= observationHourlyBudget {
			return ErrObservationDeferred
		}
		demand, err := s.oppositeDemandTx(tx, scope.AccountKey, hot, now)
		if err != nil {
			return err
		}
		if demand && ((hot && row.HotRequests >= observationHourlyBudget/2) || (!hot && row.ColdRequests >= observationHourlyBudget/2)) {
			return ErrObservationDeferred
		}
		row.LeaseToken = token
		row.LeaseUntil = now.Add(observationSliceDuration + 5*time.Second)
		row.LastScopeID = scope.ID
		row.UpdatedAt = now
		if err = tx.Save(&row).Error; err != nil {
			return err
		}
		return tx.Model(&models.ProviderObservationScope{}).Where("id = ?", scope.ID).Updates(map[string]any{"last_scheduled_at": now, "updated_at": now}).Error
	})
}

func (s *ProviderObservationService) reserveRequest(ctx context.Context, account, token string, hot bool) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row models.ProviderObservationAccountBudget
		if err := tx.First(&row, "account_key = ?", account).Error; err != nil {
			return err
		}
		now := s.now()
		if row.LeaseToken != token || !row.LeaseUntil.After(now) || row.CooldownUntil.After(now) || row.NextRequestAt.After(now) || row.HotRequests+row.ColdRequests >= observationHourlyBudget {
			return ErrObservationDeferred
		}
		if hot {
			row.HotRequests++
		} else {
			row.ColdRequests++
		}
		row.NextRequestAt = now.Add(observationRequestInterval)
		row.UpdatedAt = now
		return tx.Save(&row).Error
	})
}

func (s *ProviderObservationService) releaseAccount(ctx context.Context, account, token string) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_ = s.db.WithContext(ctx).Model(&models.ProviderObservationAccountBudget{}).Where("account_key = ? AND lease_token = ?", account, token).Updates(map[string]any{"lease_token": "", "lease_until": time.Time{}}).Error
}

// Cooldown is shared across the account's adapters, including interactive
// callers. Retry-After cannot be bypassed by choosing a different scope/route.
func (s *ProviderObservationService) Cooldown(ctx context.Context, account string, delay time.Duration) error {
	if account == "" {
		return ErrObservationInvalid
	}
	if delay <= 0 {
		delay = time.Minute
	}
	if delay > 24*time.Hour {
		delay = 24 * time.Hour
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		now := s.now()
		row := models.ProviderObservationAccountBudget{AccountKey: account, WindowStart: now, UpdatedAt: now}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error; err != nil {
			return err
		}
		if err := tx.First(&row, "account_key = ?", account).Error; err != nil {
			return err
		}
		until := now.Add(delay)
		if row.CooldownUntil.Before(until) {
			return tx.Model(&row).Updates(map[string]any{"cooldown_until": until, "updated_at": now}).Error
		}
		return nil
	})
}

type ProviderObservationStatus struct {
	Mode                 string     `json:"mode"`
	LastCompleteAt       *time.Time `json:"last_complete_at"`
	PendingDirectories   int64      `json:"pending_directories"`
	MissingCandidates    int64      `json:"missing_candidates"`
	PendingPublications  int64      `json:"pending_publications"`
	PathRepairs          int64      `json:"path_repairs"`
	PendingConfirmations int64      `json:"pending_confirmations"`
	ErrorCode            string     `json:"error_code,omitempty"`
	NextRetryAt          *time.Time `json:"next_retry_at,omitempty"`
	CooldownUntil        *time.Time `json:"cooldown_until,omitempty"`
	EstimatedCoverageAt  *time.Time `json:"estimated_coverage_at,omitempty"`
	EstimateIsLowerBound bool       `json:"estimate_is_lower_bound"`
	OldestPendingAt      *time.Time `json:"oldest_pending_at,omitempty"`
	Delayed              bool       `json:"delayed"`
}

// The estimate is explicitly a lower bound: each unseen directory costs at
// least one request; actual pagination/limits can extend it. Never invent a
// completed coverage timestamp from elapsed wall time or an empty queue slice.
func (s *ProviderObservationService) Status(ctx context.Context, id string) (ProviderObservationStatus, error) {
	out := ProviderObservationStatus{Mode: "bounded_observation", EstimateIsLowerBound: true}
	var scope models.ProviderObservationScope
	if err := s.db.WithContext(ctx).First(&scope, "id = ? AND enabled = ?", id, true).Error; err != nil {
		return out, err
	}
	out.LastCompleteAt = scope.LastCompleteAt
	out.ErrorCode, out.NextRetryAt = scope.LastErrorCode, scope.NextRetryAt
	if err := s.db.WithContext(ctx).Model(&models.ProviderObservationBatch{}).Where("scope_id = ? AND revision > published_revision", id).Count(&out.PendingPublications).Error; err != nil {
		return out, err
	}
	if err := s.db.WithContext(ctx).Model(&models.ProviderObservationBatch{}).Where("scope_id = ? AND state = 'confirming'", id).Count(&out.PendingConfirmations).Error; err != nil {
		return out, err
	}
	if err := observationCurrent(s.db.WithContext(ctx), id).Model(&models.ProviderObservationItem{}).Where("path_repair_pending = ?", true).Count(&out.PathRepairs).Error; err != nil {
		return out, err
	}
	base := observationCurrent(s.db.WithContext(ctx), id).Model(&models.ProviderObservationItem{})
	if err := base.Session(&gorm.Session{}).Where("is_dir = ? AND excluded = ? AND absent_parent_round = 0 AND completed_round < queue_round", true, false).Count(&out.PendingDirectories).Error; err != nil {
		return out, err
	}
	if err := observationCurrent(s.db.WithContext(ctx), id).Model(&models.ProviderObservationItem{}).Where("missing_since IS NOT NULL AND downlisted_at IS NULL AND excluded = ?", false).Count(&out.MissingCandidates).Error; err != nil {
		return out, err
	}
	var oldest models.ProviderObservationItem
	err := observationCurrent(s.db.WithContext(ctx), id).Where("is_dir = ? AND excluded = ? AND absent_parent_round = 0 AND completed_round < queue_round", true, false).Order("due_at").First(&oldest).Error
	if err == nil {
		out.OldestPendingAt = &oldest.DueAt
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return out, err
	}
	var budget models.ProviderObservationAccountBudget
	err = s.db.WithContext(ctx).First(&budget, "account_key = ?", scope.AccountKey).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return out, err
	}
	now := s.now()
	start := now
	if budget.CooldownUntil.After(now) {
		out.CooldownUntil = &budget.CooldownUntil
		start = budget.CooldownUntil
	}
	if budget.HotRequests+budget.ColdRequests >= observationHourlyBudget && budget.WindowStart.Add(time.Hour).After(start) {
		start = budget.WindowStart.Add(time.Hour)
		out.NextRetryAt = &start
	}
	if out.NextRetryAt != nil && out.NextRetryAt.After(start) {
		start = *out.NextRetryAt
	}
	if out.PendingDirectories > 0 {
		// Cold coverage owns at least half the account budget, shared fairly with
		// other libraries. Sum their outstanding directories for honest backlog.
		var accountPending int64
		if err = s.db.WithContext(ctx).Table("provider_observation_items AS i").Joins("JOIN provider_observation_scopes AS s ON s.id = i.scope_id").Where("s.account_key = ? AND s.enabled = ? AND i.batch_id = '' AND i.is_dir = ? AND i.excluded = ? AND i.absent_parent_round = 0 AND i.completed_round < i.queue_round", scope.AccountKey, true, true, false).Count(&accountPending).Error; err != nil {
			return out, err
		}
		eta := start.Add(time.Duration(accountPending) * 12 * time.Second)
		out.EstimatedCoverageAt = &eta
		out.Delayed = eta.After(scope.CoverageStartedAt.Add(24*time.Hour)) || now.After(scope.CoverageStartedAt.Add(24*time.Hour))
	}
	return out, nil
}
