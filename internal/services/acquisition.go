package services

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type AcquisitionStatus struct {
	ID                   string     `json:"id,omitempty"`
	MediaType            string     `json:"media_type"`
	TMDBID               int64      `json:"tmdb_id"`
	Stage                string     `json:"stage"`
	Status               string     `json:"status"`
	DownloadTaskID       string     `json:"download_task_id,omitempty"`
	FollowSubscriptionID string     `json:"follow_subscription_id,omitempty"`
	TargetLibraryID      *uint      `json:"target_library_id,omitempty"`
	TransferTaskID       string     `json:"transfer_task_id,omitempty"`
	ProcessedFiles       int        `json:"processed_files,omitempty"`
	TotalFiles           int        `json:"total_files,omitempty"`
	LastErrorCode        string     `json:"last_error_code"`
	Revision             uint64     `json:"revision"`
	UpdatedAt            *time.Time `json:"updated_at,omitempty"`
}

type AcquisitionService struct{ db *gorm.DB }

func NewAcquisitionService(db *gorm.DB) *AcquisitionService { return &AcquisitionService{db: db} }

func (s *AcquisitionService) Get(actor Actor, mediaType string, tmdbID int64) (AcquisitionStatus, error) {
	mediaType = strings.TrimSpace(mediaType)
	if (mediaType != "movie" && mediaType != "tv") || tmdbID <= 0 {
		return AcquisitionStatus{}, appError(CodeInvalidRequest, "媒体身份无效", nil)
	}
	var row models.MediaAcquisition
	if err := s.db.Where("owner_id = ? AND media_type = ? AND tmdb_id = ?", actor.User.ID, mediaType, tmdbID).First(&row).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return AcquisitionStatus{MediaType: mediaType, TMDBID: tmdbID, Stage: "idle", Status: "idle"}, nil
		}
		return AcquisitionStatus{}, err
	}
	// Re-project current durable domain facts so process restarts never strand
	// the Player on a stale in-memory state.
	if row.DownloadTaskID != "" {
		var task models.DownloadTask
		if err := s.db.First(&task, "id = ?", row.DownloadTaskID).Error; err == nil {
			row.Stage, row.Status, row.LastErrorCode = acquisitionDownloadState(task), task.Phase, task.LastErrorCode
			if row.TargetLibraryID == nil {
				row.TargetLibraryID = task.TargetLibraryID
			}
			var transfer models.TransferTask
			if err := s.db.Where("download_task_id = ?", task.ID).First(&transfer).Error; err == nil {
				row.Stage = acquisitionTransferStage(transfer)
				row.Status = transfer.Phase
				row.LastErrorCode = transfer.LastErrorCode
				status := acquisitionStatus(row)
				status.TransferTaskID = transfer.ID
				status.ProcessedFiles = transfer.ProcessedFiles
				status.TotalFiles = transfer.TotalFiles
				if transfer.FinishedAt != nil && transfer.Phase == models.TransferTaskStatusCompleted {
					status.UpdatedAt = transfer.FinishedAt
				}
				if status.TargetLibraryID != nil && !actor.CanResource(authz.PermissionMediaLibrariesRead, models.AuthorizationResourceMediaLibrary, uintID(*status.TargetLibraryID)) {
					status.TargetLibraryID = nil
				}
				return status, nil
			}
		}
	}
	if row.FollowSubscriptionID != "" && row.DownloadTaskID == "" {
		var follow models.FollowSubscription
		if err := s.db.First(&follow, "id = ?", row.FollowSubscriptionID).Error; err == nil {
			row.Stage, row.Status, row.LastErrorCode = "subscription", follow.Status, follow.LastErrorCode
		}
	}
	status := acquisitionStatus(row)
	if status.TargetLibraryID != nil && !actor.CanResource(authz.PermissionMediaLibrariesRead, models.AuthorizationResourceMediaLibrary, uintID(*status.TargetLibraryID)) {
		status.TargetLibraryID = nil
	}
	return status, nil
}

func (s *AcquisitionService) RecordDownload(ownerID uint, summary DownloadTaskSummary) error {
	if summary.ScrapeTMDBID == nil || *summary.ScrapeTMDBID <= 0 || (summary.ScrapeMediaType != "movie" && summary.ScrapeMediaType != "tv") {
		return nil
	}
	snapshot, _ := json.Marshal(map[string]any{"downloader_id": summary.DownloaderID, "target_library_id": summary.TargetLibraryID, "profile_id": summary.ProfileID, "route_kind": summary.RouteKind})
	row := models.MediaAcquisition{ID: uuid.NewString(), OwnerID: ownerID, MediaType: summary.ScrapeMediaType, TMDBID: *summary.ScrapeTMDBID, Stage: "download", Status: summary.Phase, DownloadTaskID: summary.ID, TargetLibraryID: summary.TargetLibraryID, LastErrorCode: summary.LastErrorCode, FrozenSnapshotJSON: string(snapshot), Revision: 1}
	return s.upsert(row, map[string]any{"stage": row.Stage, "status": row.Status, "download_task_id": row.DownloadTaskID, "target_library_id": row.TargetLibraryID, "last_error_code": row.LastErrorCode, "frozen_snapshot_json": row.FrozenSnapshotJSON, "revision": gorm.Expr("media_acquisitions.revision + 1"), "updated_at": time.Now().UTC()})
}

func (s *AcquisitionService) RecordFollow(summary FollowSummary) error {
	snapshot, _ := json.Marshal(summary.Snapshot)
	row := models.MediaAcquisition{ID: uuid.NewString(), OwnerID: summary.OwnerID, MediaType: summary.MediaType, TMDBID: summary.TMDBID, Stage: "subscription", Status: summary.Status, FollowSubscriptionID: summary.ID, TargetLibraryID: &summary.Snapshot.MediaLibraryID, LastErrorCode: summary.LastErrorCode, FrozenSnapshotJSON: string(snapshot), Revision: 1}
	return s.upsert(row, map[string]any{"stage": row.Stage, "status": row.Status, "follow_subscription_id": row.FollowSubscriptionID, "target_library_id": row.TargetLibraryID, "last_error_code": row.LastErrorCode, "frozen_snapshot_json": row.FrozenSnapshotJSON, "revision": gorm.Expr("media_acquisitions.revision + 1"), "updated_at": time.Now().UTC()})
}

func (s *AcquisitionService) upsert(row models.MediaAcquisition, updates map[string]any) error {
	now := time.Now().UTC()
	row.CreatedAt, row.UpdatedAt = now, now
	return s.db.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "owner_id"}, {Name: "media_type"}, {Name: "tmdb_id"}}, DoUpdates: clause.Assignments(updates)}).Create(&row).Error
}

func acquisitionDownloadState(task models.DownloadTask) string {
	if task.Phase == models.DownloadTaskStatusCompleted {
		return "import"
	}
	return "download"
}

func acquisitionTransferStage(task models.TransferTask) string {
	switch task.Phase {
	case models.TransferTaskStatusQueued, models.TransferTaskStatusPlanning, models.TransferTaskStatusCheckingDirectories, models.TransferTaskStatusCreatingDirectories, models.TransferTaskStatusCheckingConflicts:
		return "organize"
	case models.TransferTaskStatusCompleted:
		return "library"
	default:
		return "import"
	}
}

func acquisitionStatus(row models.MediaAcquisition) AcquisitionStatus {
	updated := row.UpdatedAt
	return AcquisitionStatus{ID: row.ID, MediaType: row.MediaType, TMDBID: row.TMDBID, Stage: row.Stage, Status: row.Status, DownloadTaskID: row.DownloadTaskID, FollowSubscriptionID: row.FollowSubscriptionID, TargetLibraryID: row.TargetLibraryID, LastErrorCode: row.LastErrorCode, Revision: row.Revision, UpdatedAt: &updated}
}
