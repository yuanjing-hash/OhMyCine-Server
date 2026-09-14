package services

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

const JobTypeMediaLibraryRetirement = "media_library_retirement"

func mediaLibraryRetirementResourceKey(libraryID uint) string {
	// Retirement must be claimable while an ordinary library worker still owns
	// its resource lane; claiming retirement is what asks that worker to stop.
	return "media-library-retirement:" + strconv.FormatUint(uint64(libraryID), 10)
}

type MediaLibraryDeletionResult struct {
	Deleted bool   `json:"deleted"`
	Status  string `json:"status,omitempty"`
	JobID   string `json:"job_id,omitempty"`
}
type MediaLibraryRetirementSummary struct {
	Status    string `json:"status"`
	JobID     string `json:"job_id"`
	ErrorCode string `json:"error_code,omitempty"`
}

func retirementSummary(row models.MediaLibraryRetirement) *MediaLibraryRetirementSummary {
	return &MediaLibraryRetirementSummary{Status: "deleting", JobID: row.JobID, ErrorCode: row.LastErrorCode}
}
func retirementResult(row models.MediaLibraryRetirement) MediaLibraryDeletionResult {
	if row.Phase == "completed" {
		return MediaLibraryDeletionResult{Deleted: true}
	}
	return MediaLibraryDeletionResult{Status: "deleting", JobID: row.JobID}
}

var errRetirementAlreadyAccepted = errors.New("library retirement already accepted")

// DeleteRequest is the single management deletion path. Every library uses the
// same durable, bounded retirement flow, including a library that has not yet
// published its first CatalogHead.
func (s *MediaLibraryService) DeleteRequest(ctx context.Context, actor Actor, id uint, request RequestContext) (MediaLibraryDeletionResult, error) {
	if !actor.CanResource(authz.PermissionMediaLibrariesDelete, models.AuthorizationResourceMediaLibrary, uintID(id)) {
		return MediaLibraryDeletionResult{}, appError(CodePermissionDenied, "无权删除这个媒体库", nil)
	}
	var retirement *models.MediaLibraryRetirement
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var err error
		retirement, err = mediaLibraryRetirementTx(tx, id)
		if err != nil || retirement != nil {
			return err
		}
		if err := tx.First(&models.MediaLibrary{}, id).Error; err != nil {
			return mediaLibraryNotFound(err)
		}
		return nil
	})
	if err != nil {
		return MediaLibraryDeletionResult{}, err
	}
	if retirement != nil {
		return retirementResult(*retirement), nil
	}
	if s.queue == nil {
		return MediaLibraryDeletionResult{}, appError(CodeConflict, "媒体库移除服务尚未就绪，请稍后重试", nil)
	}
	row := models.MediaLibraryRetirement{ID: uuid.NewString(), LibraryID: id, ActorID: actor.User.ID, Phase: "queued", Revision: 1, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	_, err = s.queue.EnqueueWith(EnqueueJobInput{OwnerID: actor.User.ID, JobType: JobTypeMediaLibraryRetirement, DisplayName: "移除媒体库", Provider: "media_library", ResourceKey: mediaLibraryRetirementResourceKey(id), Payload: map[string]any{"retirement_id": row.ID}}, func(tx *gorm.DB, job models.Job) error {
		if !actor.CanResource(authz.PermissionMediaLibrariesDelete, models.AuthorizationResourceMediaLibrary, uintID(id)) {
			return appError(CodePermissionDenied, "无权删除这个媒体库", nil)
		}
		current, err := mediaLibraryRetirementTx(tx, id)
		if err != nil {
			return err
		}
		if current != nil {
			row = *current
			return errRetirementAlreadyAccepted
		}
		var library models.MediaLibrary
		if err := tx.First(&library, id).Error; err != nil {
			return mediaLibraryNotFound(err)
		}
		if library.DefaultIngestConnectionID != nil {
			if err := requireNoEnabledLifeEventListener(ctx, tx, *library.DefaultIngestConnectionID); err != nil {
				return err
			}
		}
		var head models.CatalogHead
		if err := tx.First(&head, "library_id=?", id).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		row.JobID = job.ID
		row.SourceEpoch = head.SourceEpoch
		row.SourceFingerprint = head.SourceFingerprint
		row.ConfigFingerprint = head.ConfigFingerprint
		if err := tx.Create(&row).Error; err != nil {
			return err
		}
		var jobIDs []string
		if err := retirementLibraryJobs(tx, id).Order("id").Pluck("id", &jobIDs).Error; err != nil {
			return err
		}
		owned := make([]models.MediaLibraryRetirementJob, 0, len(jobIDs))
		for _, jobID := range jobIDs {
			owned = append(owned, models.MediaLibraryRetirementJob{RetirementID: row.ID, JobID: jobID, CreatedAt: row.CreatedAt})
		}
		if len(owned) > 0 {
			if err := tx.CreateInBatches(&owned, CatalogBatchRows).Error; err != nil {
				return err
			}
		}
		if err := tx.Model(&models.MediaLibrary{}).Where("id=?", id).Updates(map[string]any{"enabled": false, "updated_at": time.Now().UTC()}).Error; err != nil {
			return err
		}
		return s.audit.Record(tx, &actor.User.ID, "media_library.retirement.request", "media_library", uintID(id), "success", map[string]any{"job_id": job.ID, "files_preserved": true}, request)
	})
	if err != nil && !errors.Is(err, errRetirementAlreadyAccepted) {
		return MediaLibraryDeletionResult{}, err
	}
	return retirementResult(row), nil
}
