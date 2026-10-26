package services

import (
	"context"
	"sort"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

type DashboardActivity struct {
	GeneratedAt time.Time                               `json:"generated_at"`
	Active      PlayerOverviewSection[JobDTO]           `json:"active_tasks"`
	Waiting     PlayerOverviewSection[JobDTO]           `json:"waiting_tasks"`
	Attention   PlayerOverviewSection[JobDTO]           `json:"attention_tasks"`
	Recent      PlayerOverviewSection[BrowserMediaItem] `json:"recently_added"`
	Capacities  PlayerOverviewSection[StorageCapacity]  `json:"capacities"`
}

// DashboardActivity reuses the task resource projection and catalog ordering.
// Reading it never probes a provider or executes any domain workflow.
func (s *AdminService) DashboardActivity(ctx context.Context, actor Actor) (DashboardActivity, error) {
	if !actor.Can(authz.PermissionDashboardRead) {
		return DashboardActivity{}, appError(CodePermissionDenied, "无权查看仪表盘", nil)
	}
	result := DashboardActivity{GeneratedAt: time.Now().UTC()}
	queue := NewQueueService(s.db.WithContext(ctx), nil)
	tasks := func(statuses []string) PlayerOverviewSection[JobDTO] {
		return loadPlayerOverviewSection(func() ([]JobDTO, bool, error) {
			items := make([]JobDTO, 0, 8)
			var total int64
			for _, status := range statuses {
				page, err := queue.List(actor, JobListFilter{Status: status, Page: 1, PageSize: 8})
				if err != nil {
					return nil, false, err
				}
				total += page.Total
				items = append(items, page.List...)
			}
			sort.SliceStable(items, func(i, j int) bool {
				if !items[i].UpdatedAt.Equal(items[j].UpdatedAt) {
					return items[i].UpdatedAt.After(items[j].UpdatedAt)
				}
				return items[i].ID < items[j].ID
			})
			if len(items) > 8 {
				items = items[:8]
			}
			return items, total > int64(len(items)), nil
		})
	}
	result.Active = tasks([]string{models.JobStatusRunning})
	result.Waiting = tasks([]string{models.JobStatusQueued, models.JobStatusRetryWait, models.JobStatusPaused})
	result.Attention = tasks([]string{models.JobStatusFailed, models.JobStatusWaitingUserAction})
	result.Recent = loadPlayerOverviewSection(func() ([]BrowserMediaItem, bool, error) {
		libraries := &MediaLibraryService{db: s.db.WithContext(ctx), catalogStore: s.catalogStore}
		visible, err := libraries.PlayerLibraries(actor)
		if err != nil {
			return nil, false, err
		}
		reader := newPlayerOverviewService(nil, nil, libraries)
		items, total, err := reader.catalogCandidates(actor, visible)
		if err != nil {
			return nil, false, err
		}
		if len(items) > 8 {
			items = items[:8]
		}
		return browserMediaItems(items), total > len(items), nil
	})
	result.Capacities = loadPlayerOverviewSection(func() ([]StorageCapacity, bool, error) {
		if !actor.Can(authz.PermissionStoragesRead) {
			return nil, false, appError(CodePermissionDenied, "无权查看存储", nil)
		}
		var rows []models.Storage
		if err := s.db.WithContext(ctx).Order("name_normalized,id").Limit(51).Find(&rows).Error; err != nil {
			return nil, false, err
		}
		more := len(rows) > 50
		if more {
			rows = rows[:50]
		}
		items := make([]StorageCapacity, 0, len(rows))
		for _, row := range rows {
			items = append(items, storageCapacity(row, time.Now().UTC()))
		}
		return items, more, nil
	})
	return result, nil
}

type DashboardActorSummary struct {
	Initialized      bool   `json:"initialized"`
	RecoveryRequired bool   `json:"recovery_required"`
	Users            *int64 `json:"users"`
	ActiveUsers      *int64 `json:"active_users"`
	Roles            *int64 `json:"roles"`
	AuditEvents      *int64 `json:"audit_events"`
}

// Management counters remain available to authorized administrators only.
// Dashboard permission itself is not a grant to enumerate users or audit logs.
func (s *AdminService) DashboardForActor(ctx context.Context, actor Actor) (DashboardActorSummary, error) {
	if !actor.Can(authz.PermissionDashboardRead) {
		return DashboardActorSummary{}, appError(CodePermissionDenied, "无权查看仪表盘", nil)
	}
	var result DashboardActorSummary
	db := s.db.WithContext(ctx)
	var users, owners int64
	if err := db.Model(&models.User{}).Count(&users).Error; err != nil {
		return result, err
	}
	if err := db.Model(&models.User{}).Where("is_owner = ?", true).Count(&owners).Error; err != nil {
		return result, err
	}
	result.Initialized, result.RecoveryRequired = users > 0, users > 0 && owners == 0
	if actor.Can(authz.PermissionUsersRead) {
		result.Users = &users
		var active int64
		if err := db.Model(&models.User{}).Where("status = ?", models.UserStatusActive).Count(&active).Error; err != nil {
			return result, err
		}
		result.ActiveUsers = &active
	}
	if actor.Can(authz.PermissionRolesRead) {
		var roles int64
		if err := db.Model(&models.Role{}).Count(&roles).Error; err != nil {
			return result, err
		}
		result.Roles = &roles
	}
	if actor.Can(authz.PermissionAuditRead) {
		var events int64
		if err := db.Model(&models.AuditLog{}).Count(&events).Error; err != nil {
			return result, err
		}
		result.AuditEvents = &events
	}
	return result, nil
}
