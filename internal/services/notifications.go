package services

import (
	"context"
	"net/url"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type UserNotification struct {
	ID         string    `json:"id"`
	Title      string    `json:"title"`
	Status     string    `json:"status"`
	Occurrence uint64    `json:"occurrence"`
	Read       bool      `json:"read"`
	Resolved   bool      `json:"resolved"`
	Link       string    `json:"link"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type notificationRow struct {
	models.Job
	Occurrence     uint64
	ReadOccurrence uint64
}

// Event ID is stable across progress/heartbeat changes, and advances on the
// next failure/action request. Legacy jobs without events use generation.
const notificationOccurrence = "COALESCE((SELECT MAX(e.id) FROM job_status_events e WHERE e.job_id = jobs.id AND e.to_status IN ('failed','waiting_user_action')), jobs.generation)"

func (s *QueueService) notificationQuery(ctx context.Context, actor Actor) (*gorm.DB, error) {
	query, err := readableJobsQuery(s.db.WithContext(ctx), actor)
	if err != nil {
		return nil, err
	}
	return query.Joins("LEFT JOIN notification_receipts r ON r.job_id = jobs.id AND r.user_id = ?", actor.User.ID).
		Where("jobs.status IN ('failed','waiting_user_action') OR r.job_id IS NOT NULL"), nil
}

func (s *QueueService) Notifications(ctx context.Context, actor Actor, page, size int) (BrowserMediaPage[UserNotification], error) {
	result := BrowserMediaPage[UserNotification]{List: []UserNotification{}, Page: page, PageSize: size}
	if err := validateMediaStatePage(page, size); err != nil {
		return result, err
	}
	query, err := s.notificationQuery(ctx, actor)
	if err != nil {
		return result, err
	}
	if err := query.Count(&result.Total).Error; err != nil {
		return result, err
	}
	var rows []notificationRow
	if err := query.Select("jobs.id,jobs.display_name,jobs.status,jobs.updated_at," + notificationOccurrence + " AS occurrence,COALESCE(r.occurrence,0) AS read_occurrence").Order("jobs.updated_at DESC,jobs.id").Limit(size).Offset((page - 1) * size).Scan(&rows).Error; err != nil {
		return result, err
	}
	for _, row := range rows {
		result.List = append(result.List, UserNotification{ID: row.ID, Title: safeLabel(row.DisplayName, 256), Status: row.Status, Occurrence: row.Occurrence, Read: row.ReadOccurrence == row.Occurrence, Resolved: row.Status != "failed" && row.Status != "waiting_user_action", Link: "/automation/tasks?job_id=" + url.QueryEscape(row.ID), UpdatedAt: row.UpdatedAt})
	}
	result.HasMore = int64(page*size) < result.Total
	return result, nil
}

func (s *QueueService) AcknowledgeNotification(ctx context.Context, actor Actor, id string, occurrence uint64) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		local := *s
		local.db = tx
		query, err := local.notificationQuery(ctx, actor)
		if err != nil {
			return err
		}
		var row notificationRow
		if err := query.Select("jobs.id,"+notificationOccurrence+" AS occurrence").Where("jobs.id = ?", id).Scan(&row).Error; err != nil {
			return err
		}
		if row.ID == "" {
			return appError(CodeNotFound, "通知不存在", nil)
		}
		if occurrence == 0 || row.Occurrence != occurrence {
			return appError(CodeConflict, "通知已更新，请刷新后重试", nil)
		}
		receipt := models.NotificationReceipt{UserID: actor.User.ID, JobID: id, Occurrence: occurrence, ReadAt: time.Now().UTC()}
		return tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "user_id"}, {Name: "job_id"}}, DoUpdates: clause.AssignmentColumns([]string{"occurrence", "read_at"})}).Create(&receipt).Error
	})
}
