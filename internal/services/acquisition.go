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

const acquisitionReconcileDownloadLimit = 500

type acquisitionMediaIdentity struct {
	MediaType string
	TMDBID    int64
}

type acquisitionDownloadCandidate struct {
	Task     models.DownloadTask
	Identity acquisitionMediaIdentity
}

type AcquisitionStatus struct {
	ID                   string     `json:"id,omitempty"`
	Title                string     `json:"title,omitempty"`
	MediaType            string     `json:"media_type"`
	TMDBID               int64      `json:"tmdb_id"`
	Stage                string     `json:"stage"`
	Status               string     `json:"status"`
	DownloadTaskID       string     `json:"download_task_id,omitempty"`
	FollowSubscriptionID string     `json:"follow_subscription_id,omitempty"`
	TargetLibraryID      *uint      `json:"target_library_id,omitempty"`
	TransferTaskID       string     `json:"transfer_task_id,omitempty"`
	Progress             *float64   `json:"progress,omitempty"`
	BytesCompleted       *int64     `json:"bytes_completed,omitempty"`
	BytesTotal           *int64     `json:"bytes_total,omitempty"`
	DownloadSpeed        *int64     `json:"download_speed,omitempty"`
	ETASeconds           *int64     `json:"eta_seconds,omitempty"`
	ProcessedFiles       int        `json:"processed_files,omitempty"`
	TotalFiles           int        `json:"total_files,omitempty"`
	LastErrorCode        string     `json:"last_error_code"`
	Revision             uint64     `json:"revision"`
	UpdatedAt            *time.Time `json:"updated_at,omitempty"`
}

type AcquisitionPage struct {
	List     []AcquisitionStatus `json:"list"`
	Total    int64               `json:"total"`
	Page     int                 `json:"page"`
	PageSize int                 `json:"page_size"`
}

type AcquisitionService struct{ db *gorm.DB }

func NewAcquisitionService(db *gorm.DB) *AcquisitionService { return &AcquisitionService{db: db} }

func (s *AcquisitionService) Get(actor Actor, mediaType string, tmdbID int64) (AcquisitionStatus, error) {
	mediaType = strings.TrimSpace(mediaType)
	if (mediaType != "movie" && mediaType != "tv") || tmdbID <= 0 {
		return AcquisitionStatus{}, appError(CodeInvalidRequest, "媒体身份无效", nil)
	}
	if err := s.reconcileDirectDownloads(actor.User.ID); err != nil {
		return AcquisitionStatus{}, err
	}
	var row models.MediaAcquisition
	if err := s.db.Where("owner_id = ? AND media_type = ? AND tmdb_id = ?", actor.User.ID, mediaType, tmdbID).First(&row).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return AcquisitionStatus{MediaType: mediaType, TMDBID: tmdbID, Stage: "idle", Status: "idle"}, nil
		}
		return AcquisitionStatus{}, err
	}
	return s.project(actor, row)
}

func (s *AcquisitionService) List(actor Actor, page, pageSize int) (AcquisitionPage, error) {
	if !actor.HasPermission(authz.PermissionDiscoveryRead) {
		return AcquisitionPage{}, appError(CodePermissionDenied, "无权查看入库任务", nil)
	}
	if page < 1 || page > 100000 || pageSize < 1 || pageSize > 100 {
		return AcquisitionPage{}, appError(CodeInvalidRequest, "入库任务分页参数无效", nil)
	}
	if err := s.reconcileDirectDownloads(actor.User.ID); err != nil {
		return AcquisitionPage{}, err
	}
	query := s.db.Model(&models.MediaAcquisition{}).Where("owner_id = ?", actor.User.ID)
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return AcquisitionPage{}, err
	}
	var rows []models.MediaAcquisition
	if err := query.Order("updated_at DESC, id DESC").Offset((page - 1) * pageSize).Limit(pageSize).Find(&rows).Error; err != nil {
		return AcquisitionPage{}, err
	}
	items := make([]AcquisitionStatus, 0, len(rows))
	for _, row := range rows {
		item, err := s.project(actor, row)
		if err != nil {
			return AcquisitionPage{}, err
		}
		items = append(items, item)
	}
	return AcquisitionPage{List: items, Total: total, Page: page, PageSize: pageSize}, nil
}

// project rebuilds current state from durable task facts so process restarts
// never strand Player clients on a stale acquisition row.
func (s *AcquisitionService) project(actor Actor, row models.MediaAcquisition) (AcquisitionStatus, error) {
	title := ""
	if row.DownloadTaskID != "" {
		var task models.DownloadTask
		if err := s.db.First(&task, "id = ?", row.DownloadTaskID).Error; err == nil {
			title = task.DisplayName
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
				status.Title = title
				projectDownloadProgress(&status, task)
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
			title = follow.Title
			row.Stage, row.Status, row.LastErrorCode = "subscription", follow.Status, follow.LastErrorCode
		}
	}
	status := acquisitionStatus(row)
	status.Title = title
	if row.DownloadTaskID != "" {
		var task models.DownloadTask
		if err := s.db.First(&task, "id = ?", row.DownloadTaskID).Error; err == nil {
			projectDownloadProgress(&status, task)
		}
	}
	if status.TargetLibraryID != nil && !actor.CanResource(authz.PermissionMediaLibrariesRead, models.AuthorizationResourceMediaLibrary, uintID(*status.TargetLibraryID)) {
		status.TargetLibraryID = nil
	}
	return status, nil
}

func projectDownloadProgress(status *AcquisitionStatus, task models.DownloadTask) {
	status.Progress = cloneFloat64(task.Progress)
	status.BytesCompleted = cloneInt64(task.BytesCompleted)
	status.BytesTotal = cloneInt64(task.BytesTotal)
	status.DownloadSpeed = cloneInt64(task.DownloadSpeed)
	status.ETASeconds = cloneInt64(task.ETASeconds)
}

func (s *AcquisitionService) RecordDownload(ownerID uint, summary DownloadTaskSummary) error {
	if ownerID == 0 || strings.TrimSpace(summary.ID) == "" {
		return nil
	}
	var task models.DownloadTask
	if err := s.db.Where("id = ? AND owner_id = ?", strings.TrimSpace(summary.ID), ownerID).First(&task).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil
		}
		return err
	}
	identity, ok := acquisitionIdentityForDownload(task)
	if !ok {
		return nil
	}
	return s.recordDownloadTask(task, identity)
}

// reconcileDirectDownloads repairs projections missed by Server versions that
// only inspected scrape fields at task creation time. The bounded query is
// deliberately limited to poster-originated identities so ordinary download
// history does not unexpectedly become Player acquisition history.
func (s *AcquisitionService) reconcileDirectDownloads(ownerID uint) error {
	if ownerID == 0 {
		return nil
	}
	var tasks []models.DownloadTask
	if err := s.db.Where("owner_id = ? AND identity_source = ? AND identity_status = ? AND identity_revision > 0", ownerID, mediaIdentitySourceDirectID, mediaIdentityStatusVerified).
		Order("created_at DESC, id DESC").Limit(acquisitionReconcileDownloadLimit).Find(&tasks).Error; err != nil {
		return err
	}
	seen := make(map[acquisitionMediaIdentity]struct{}, len(tasks))
	candidates := make([]acquisitionDownloadCandidate, 0, len(tasks))
	movieIDs := make([]int64, 0, len(tasks))
	tvIDs := make([]int64, 0, len(tasks))
	for _, task := range tasks {
		identity, ok := acquisitionIdentityFromSnapshot(task)
		if !ok {
			continue
		}
		if _, exists := seen[identity]; exists {
			continue
		}
		seen[identity] = struct{}{}
		candidates = append(candidates, acquisitionDownloadCandidate{Task: task, Identity: identity})
		if identity.MediaType == "movie" {
			movieIDs = append(movieIDs, identity.TMDBID)
		} else {
			tvIDs = append(tvIDs, identity.TMDBID)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	query := s.db.Where("owner_id = ?", ownerID)
	switch {
	case len(movieIDs) > 0 && len(tvIDs) > 0:
		query = query.Where("(media_type = ? AND tmdb_id IN ?) OR (media_type = ? AND tmdb_id IN ?)", "movie", movieIDs, "tv", tvIDs)
	case len(movieIDs) > 0:
		query = query.Where("media_type = ? AND tmdb_id IN ?", "movie", movieIDs)
	default:
		query = query.Where("media_type = ? AND tmdb_id IN ?", "tv", tvIDs)
	}
	var existingRows []models.MediaAcquisition
	if err := query.Find(&existingRows).Error; err != nil {
		return err
	}
	existingByIdentity := make(map[acquisitionMediaIdentity]models.MediaAcquisition, len(existingRows))
	for _, row := range existingRows {
		existingByIdentity[acquisitionMediaIdentity{MediaType: row.MediaType, TMDBID: row.TMDBID}] = row
	}
	for _, candidate := range candidates {
		if existing, ok := existingByIdentity[candidate.Identity]; ok && existing.DownloadTaskID == candidate.Task.ID {
			continue
		}
		if err := s.recordDownloadTask(candidate.Task, candidate.Identity); err != nil {
			return err
		}
	}
	return nil
}

func acquisitionIdentityForDownload(task models.DownloadTask) (acquisitionMediaIdentity, bool) {
	mediaType := strings.TrimSpace(task.ScrapeMediaType)
	if task.ScrapeTMDBID != nil && *task.ScrapeTMDBID > 0 && (mediaType == "movie" || mediaType == "tv") {
		return acquisitionMediaIdentity{MediaType: mediaType, TMDBID: *task.ScrapeTMDBID}, true
	}
	return acquisitionIdentityFromSnapshot(task)
}

func acquisitionIdentityFromSnapshot(task models.DownloadTask) (acquisitionMediaIdentity, bool) {
	if task.IdentityRevision == 0 || strings.TrimSpace(task.IdentitySnapshotJSON) == "" {
		return acquisitionMediaIdentity{}, false
	}
	snapshot, err := decodeMediaIdentity(task.IdentitySnapshotJSON)
	if err != nil || snapshot.Revision != task.IdentityRevision || snapshot.Source != task.IdentitySource || snapshot.Status != task.IdentityStatus || snapshot.Locked != task.IdentityLocked || snapshot.TMDBID == nil || *snapshot.TMDBID <= 0 {
		return acquisitionMediaIdentity{}, false
	}
	mediaType := strings.TrimSpace(snapshot.MediaType)
	if mediaType != "movie" && mediaType != "tv" {
		return acquisitionMediaIdentity{}, false
	}
	if !validDownloadIdentityBinding(DownloadRecognitionIdentity{TMDBID: *snapshot.TMDBID, MediaType: mediaType, Source: snapshot.Source, Status: snapshot.Status, Locked: snapshot.Locked, Season: cloneInt(snapshot.Season), Episode: cloneInt(snapshot.Episode)}) {
		return acquisitionMediaIdentity{}, false
	}
	return acquisitionMediaIdentity{MediaType: mediaType, TMDBID: *snapshot.TMDBID}, true
}

func (s *AcquisitionService) recordDownloadTask(task models.DownloadTask, identity acquisitionMediaIdentity) error {
	snapshot, err := json.Marshal(map[string]any{"downloader_id": task.DownloaderID, "target_library_id": task.TargetLibraryID, "profile_id": task.ProfileID, "route_kind": task.TransferRouteKind})
	if err != nil {
		return err
	}
	frozenSnapshot := string(snapshot)
	return s.db.Transaction(func(tx *gorm.DB) error {
		var existing models.MediaAcquisition
		err := tx.Where("owner_id = ? AND media_type = ? AND tmdb_id = ?", task.OwnerID, identity.MediaType, identity.TMDBID).First(&existing).Error
		if err != nil && err != gorm.ErrRecordNotFound {
			return err
		}
		if err == nil {
			if existing.DownloadTaskID == task.ID && optionalUintEqual(existing.TargetLibraryID, task.TargetLibraryID) && existing.FrozenSnapshotJSON == frozenSnapshot {
				return nil
			}
			if keepExistingAcquisitionDownload(tx, existing.DownloadTaskID, task) {
				return nil
			}
		}
		now := time.Now().UTC()
		row := models.MediaAcquisition{ID: uuid.NewString(), OwnerID: task.OwnerID, MediaType: identity.MediaType, TMDBID: identity.TMDBID, Stage: acquisitionDownloadState(task), Status: task.Phase, DownloadTaskID: task.ID, TargetLibraryID: task.TargetLibraryID, LastErrorCode: task.LastErrorCode, FrozenSnapshotJSON: frozenSnapshot, Revision: 1, CreatedAt: now, UpdatedAt: now}
		updates := map[string]any{"stage": row.Stage, "status": row.Status, "download_task_id": row.DownloadTaskID, "target_library_id": row.TargetLibraryID, "last_error_code": row.LastErrorCode, "frozen_snapshot_json": row.FrozenSnapshotJSON, "revision": gorm.Expr("media_acquisitions.revision + 1"), "updated_at": now}
		return tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "owner_id"}, {Name: "media_type"}, {Name: "tmdb_id"}}, DoUpdates: clause.Assignments(updates)}).Create(&row).Error
	})
}

func optionalUintEqual(left, right *uint) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func keepExistingAcquisitionDownload(tx *gorm.DB, existingTaskID string, candidate models.DownloadTask) bool {
	if strings.TrimSpace(existingTaskID) == "" {
		return false
	}
	var existing models.DownloadTask
	if err := tx.Select("id", "owner_id", "created_at").First(&existing, "id = ?", existingTaskID).Error; err != nil || existing.OwnerID != candidate.OwnerID {
		return false
	}
	if existing.CreatedAt.After(candidate.CreatedAt) {
		return true
	}
	return existing.CreatedAt.Equal(candidate.CreatedAt) && existing.ID >= candidate.ID
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
