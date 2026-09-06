package services

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

const JobTypeMediaLibraryRetirement = "media_library_retirement"

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

// The shared durable physical admission guard is installed with the converter /
// Transfer integration. Missing proof is a refusal, never permission to delete.
func (s *MediaLibraryService) SetRetirementPhysicalGuard(guard func(*gorm.DB, uint) error) {
	s.retirementPhysicalGuard = guard
}

var errRetirementAlreadyAccepted = errors.New("library retirement already accepted")

// DeleteRequest is management-only. Legacy Delete retains its synchronous
// contract and refuses snapshot-backed libraries rather than falsely succeeding.
func (s *MediaLibraryService) DeleteRequest(ctx context.Context, actor Actor, id uint, request RequestContext) (MediaLibraryDeletionResult, error) {
	if !actor.CanResource(authz.PermissionMediaLibrariesDelete, models.AuthorizationResourceMediaLibrary, uintID(id)) {
		return MediaLibraryDeletionResult{}, appError(CodePermissionDenied, "无权删除这个媒体库", nil)
	}
	var retirement *models.MediaLibraryRetirement
	var needsAsync bool
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var err error
		retirement, err = mediaLibraryRetirementTx(tx, id)
		if err != nil || retirement != nil {
			return err
		}
		var library models.MediaLibrary
		if err := tx.First(&library, id).Error; err != nil {
			return mediaLibraryNotFound(err)
		}
		var count int64
		if err := tx.Model(&models.CatalogHead{}).Where("library_id=?", id).Count(&count).Error; err != nil {
			return err
		}
		needsAsync = count > 0
		return nil
	})
	if err != nil {
		return MediaLibraryDeletionResult{}, err
	}
	if retirement != nil {
		return retirementResult(*retirement), nil
	}
	if !needsAsync {
		if err := s.Delete(actor, id, request); err != nil {
			return MediaLibraryDeletionResult{}, err
		}
		return MediaLibraryDeletionResult{Deleted: true}, nil
	}
	if s.queue == nil || s.retirementPhysicalGuard == nil {
		return MediaLibraryDeletionResult{}, appError(CodeConflict, "媒体库移除服务尚未就绪，请稍后重试", nil)
	}
	row := models.MediaLibraryRetirement{ID: uuid.NewString(), LibraryID: id, ActorID: actor.User.ID, Phase: "queued", Revision: 1, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	_, err = s.queue.EnqueueWith(EnqueueJobInput{OwnerID: actor.User.ID, JobType: JobTypeMediaLibraryRetirement, DisplayName: "移除媒体库索引", Provider: "media_library", ResourceKey: mediaArtifactResourceKey(id), Payload: map[string]any{"retirement_id": row.ID}}, func(tx *gorm.DB, job models.Job) error {
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
		// Check before changing enabled/head/source: a refused physical task
		// must retain every original recovery fence.
		if err := s.retirementPhysicalGuard(tx, id); err != nil {
			return err
		}
		var head models.CatalogHead
		if err := tx.First(&head, "library_id=?", id).Error; err != nil {
			return err
		}
		row.JobID = job.ID
		row.SourceEpoch = head.SourceEpoch
		row.SourceFingerprint = head.SourceFingerprint
		row.ConfigFingerprint = head.ConfigFingerprint
		if err := tx.Create(&row).Error; err != nil {
			return err
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
