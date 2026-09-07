package services

import (
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

type JobHistoryPage[T any] struct {
	List     []T   `json:"list"`
	Total    int64 `json:"total"`
	Page     int   `json:"page"`
	PageSize int   `json:"page_size"`
}

func jobHistoryPage[T any](db *gorm.DB, id, order string, page, size int) (JobHistoryPage[T], error) {
	result := JobHistoryPage[T]{List: []T{}, Page: page, PageSize: size}
	if size < 1 || size > 200 || page < 1 || page-1 > int(^uint(0)>>1)/size {
		return result, appError(CodeInvalidRequest, "分页参数无效", nil)
	}
	var model T
	query := db.Model(&model).Where("job_id = ?", id)
	if err := query.Session(&gorm.Session{}).Count(&result.Total).Error; err != nil {
		return result, err
	}
	err := query.Session(&gorm.Session{}).Order(order).Offset((page - 1) * size).Limit(size).Find(&result.List).Error
	return result, err
}
func (s *QueueService) AttemptsPage(actor Actor, id string, page, size int) (JobHistoryPage[models.JobAttempt], error) {
	if _, err := s.Get(actor, id); err != nil {
		return JobHistoryPage[models.JobAttempt]{}, err
	}
	return jobHistoryPage[models.JobAttempt](s.db, id, "attempt_number DESC", page, size)
}
func (s *QueueService) TimelinePage(actor Actor, id string, page, size int) (JobHistoryPage[models.JobStatusEvent], error) {
	if _, err := s.Get(actor, id); err != nil {
		return JobHistoryPage[models.JobStatusEvent]{}, err
	}
	return jobHistoryPage[models.JobStatusEvent](s.db, id, "id DESC", page, size)
}
