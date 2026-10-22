package services

import (
	"context"
	"encoding/json"
	"errors"
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
	ID                   string                    `json:"id,omitempty"`
	Title                string                    `json:"title,omitempty"`
	MediaType            string                    `json:"media_type"`
	TMDBID               int64                     `json:"tmdb_id"`
	Stage                string                    `json:"stage"`
	Status               string                    `json:"status"`
	DownloadTaskID       string                    `json:"download_task_id,omitempty"`
	FollowSubscriptionID string                    `json:"follow_subscription_id,omitempty"`
	TargetLibraryID      *uint                     `json:"target_library_id,omitempty"`
	CatalogTarget        *AcquisitionCatalogTarget `json:"catalog_target,omitempty"`
	TransferTaskID       string                    `json:"transfer_task_id,omitempty"`
	Progress             *float64                  `json:"progress,omitempty"`
	BytesCompleted       *int64                    `json:"bytes_completed,omitempty"`
	BytesTotal           *int64                    `json:"bytes_total,omitempty"`
	DownloadSpeed        *int64                    `json:"download_speed,omitempty"`
	ETASeconds           *int64                    `json:"eta_seconds,omitempty"`
	ProcessedFiles       int                       `json:"processed_files,omitempty"`
	TotalFiles           int                       `json:"total_files,omitempty"`
	LastErrorCode        string                    `json:"last_error_code"`
	Revision             uint64                    `json:"revision"`
	UpdatedAt            *time.Time                `json:"updated_at,omitempty"`
}

// AcquisitionCatalogTarget is navigation evidence, independent of the latest
// intake's progress. A show may exist while its next episode is still queued.
type AcquisitionCatalogTarget struct {
	LibraryID uint   `json:"library_id"`
	WorkToken string `json:"work_token"`
}

type AcquisitionPage struct {
	List     []AcquisitionStatus `json:"list"`
	Total    int64               `json:"total"`
	Page     int                 `json:"page"`
	PageSize int                 `json:"page_size"`
}

type AcquisitionService struct {
	db           *gorm.DB
	catalogStore *CatalogSnapshotStore
}

func NewAcquisitionService(db *gorm.DB) *AcquisitionService { return &AcquisitionService{db: db} }

func (s *AcquisitionService) SetCatalogSnapshotStore(store *CatalogSnapshotStore) {
	s.catalogStore = store
}

func (s *AcquisitionService) Get(actor Actor, mediaType string, tmdbID int64) (AcquisitionStatus, error) {
	if !actor.HasPermission(authz.PermissionDiscoveryRead) {
		return AcquisitionStatus{}, appError(CodePermissionDenied, "无权查看入库任务", nil)
	}
	mediaType = strings.TrimSpace(mediaType)
	if (mediaType != "movie" && mediaType != "tv") || tmdbID <= 0 {
		return AcquisitionStatus{}, appError(CodeInvalidRequest, "媒体身份无效", nil)
	}
	if err := s.reconcileDirectDownloads(actor.User.ID); err != nil {
		return AcquisitionStatus{}, err
	}
	var result AcquisitionStatus
	err := s.withProjection(actor, func(p *acquisitionProjection) error {
		var row models.MediaAcquisition
		if err := p.tx.Where("owner_id = ? AND media_type = ? AND tmdb_id = ?", actor.User.ID, mediaType, tmdbID).First(&row).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				result = AcquisitionStatus{MediaType: mediaType, TMDBID: tmdbID, Stage: "idle", Status: "idle"}
				return nil
			}
			return err
		}
		var err error
		result, err = p.project(row)
		return err
	})
	return result, err
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
	result := AcquisitionPage{List: []AcquisitionStatus{}, Page: page, PageSize: pageSize}
	err := s.withProjection(actor, func(p *acquisitionProjection) error {
		query := p.tx.Model(&models.MediaAcquisition{}).Where("owner_id = ?", actor.User.ID)
		if err := query.Session(&gorm.Session{}).Count(&result.Total).Error; err != nil {
			return err
		}
		var rows []models.MediaAcquisition
		if err := query.Order("updated_at DESC, id DESC").Offset((page - 1) * pageSize).Limit(pageSize).Find(&rows).Error; err != nil {
			return err
		}
		for _, row := range rows {
			item, err := p.project(row)
			if err != nil {
				return err
			}
			result.List = append(result.List, item)
		}
		return nil
	})
	return result, err
}

type acquisitionCatalogLookup struct {
	libraryID uint
	identity  acquisitionMediaIdentity
}

type acquisitionProjection struct {
	tx      *gorm.DB
	actor   Actor
	readers map[uint]*CatalogReader
	targets map[acquisitionCatalogLookup]*AcquisitionCatalogTarget
}

func (s *AcquisitionService) withProjection(actor Actor, read func(*acquisitionProjection) error) error {
	facade := &MediaLibraryService{db: s.db, catalogStore: s.catalogStore}
	return facade.withCatalogReadTx(context.Background(), func(tx *gorm.DB) error {
		return read(&acquisitionProjection{tx: tx, actor: actor, readers: map[uint]*CatalogReader{}, targets: map[acquisitionCatalogLookup]*AcquisitionCatalogTarget{}})
	})
}

// Task state, resource authorization and effective catalog facts share one read
// transaction. Library pins and exact identity lookups are reused within a page.
func (p *acquisitionProjection) project(row models.MediaAcquisition) (AcquisitionStatus, error) {
	status := acquisitionStatus(row)
	var download *models.DownloadTask
	var transfer *models.TransferTask
	if row.DownloadTaskID != "" {
		var task models.DownloadTask
		err := p.tx.Where("id = ? AND owner_id = ?", row.DownloadTaskID, row.OwnerID).First(&task).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return AcquisitionStatus{}, err
		}
		if errors.Is(err, gorm.ErrRecordNotFound) {
			status.Status = "unavailable"
		} else {
			download = &task
			status.Title = task.DisplayName
			visible, err := downloadResourcesVisible(p.tx, p.actor, task)
			if err != nil {
				return AcquisitionStatus{}, err
			}
			if !visible {
				status.Title = ""
			}
			status.Stage, status.Status, status.LastErrorCode = acquisitionDownloadState(task), task.Phase, task.LastErrorCode
			status.UpdatedAt = &task.UpdatedAt
			if task.TargetLibraryID != nil {
				status.TargetLibraryID = task.TargetLibraryID
			}
			if task.Phase == models.DownloadTaskStatusCompleted {
				status.Status = "publishing"
			}
			projectDownloadProgress(&status, task)
			var linked models.TransferTask
			err = p.tx.Where("download_task_id = ? AND owner_id = ?", task.ID, row.OwnerID).First(&linked).Error
			if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
				return AcquisitionStatus{}, err
			}
			if err == nil {
				transfer = &linked
				status.TransferTaskID, status.ProcessedFiles, status.TotalFiles = linked.ID, linked.ProcessedFiles, linked.TotalFiles
				// Canceling a pipeline retains its completed downstream transfer.
				// That historical success must not erase the parent's terminal error.
				if task.Phase != models.DownloadTaskStatusFailed && task.Phase != models.DownloadTaskStatusCancelled {
					status.Stage, status.Status, status.LastErrorCode = acquisitionTransferStage(linked), linked.Phase, linked.LastErrorCode
					status.UpdatedAt = &linked.UpdatedAt
					if linked.Phase == models.TransferTaskStatusCompleted {
						status.Status = "publishing"
						if linked.CatalogPublishedAt != nil {
							status.LastErrorCode = "acquisition_publication_unverified"
						}
					}
				}
			}
		}
	}
	if row.FollowSubscriptionID != "" && row.DownloadTaskID == "" {
		var follow models.FollowSubscription
		err := p.tx.Where("id = ? AND owner_id = ?", row.FollowSubscriptionID, row.OwnerID).First(&follow).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return AcquisitionStatus{}, err
		}
		if err == nil {
			status.Title, status.Stage, status.Status, status.LastErrorCode = follow.Title, "subscription", follow.Status, follow.LastErrorCode
		} else {
			status.Status = "unavailable"
		}
	}
	if status.TargetLibraryID != nil && !p.actor.CanResource(authz.PermissionMediaLibrariesRead, models.AuthorizationResourceMediaLibrary, uintID(*status.TargetLibraryID)) {
		status.TargetLibraryID = nil
		status.Title = ""
	}
	if status.TargetLibraryID != nil {
		target, reader, err := p.catalogTarget(*status.TargetLibraryID, row.MediaType, row.TMDBID)
		if err != nil {
			return AcquisitionStatus{}, err
		}
		status.CatalogTarget = target
		if target != nil && download != nil && download.Phase == models.DownloadTaskStatusCompleted && transfer != nil && transfer.Phase == models.TransferTaskStatusCompleted && transfer.CatalogPublishedAt != nil && transfer.LibraryID == target.LibraryID {
			published, err := acquisitionTransferPublished(p.tx, reader, *download, *transfer, row)
			if err != nil {
				return AcquisitionStatus{}, err
			}
			if published {
				status.Stage, status.Status, status.LastErrorCode = "library", models.TransferTaskStatusCompleted, ""
				status.UpdatedAt = transfer.CatalogPublishedAt
			}
		}
	}
	return status, nil
}

func (p *acquisitionProjection) catalogTarget(libraryID uint, mediaType string, tmdbID int64) (*AcquisitionCatalogTarget, *CatalogReader, error) {
	key := acquisitionCatalogLookup{libraryID, acquisitionMediaIdentity{mediaType, tmdbID}}
	if target, ok := p.targets[key]; ok {
		return target, p.readers[libraryID], nil
	}
	reader, ok := p.readers[libraryID]
	if !ok {
		if err := ensurePlayerMediaLibraryReadableTx(p.tx, p.actor, libraryID); err != nil {
			if ErrorCode(err) == CodePermissionDenied || errors.Is(err, gorm.ErrRecordNotFound) {
				p.targets[key] = nil
				return nil, nil, nil
			}
			return nil, nil, err
		}
		var err error
		reader, err = PinCatalogTx(p.tx, []uint{libraryID})
		if err != nil {
			return nil, nil, err
		}
		p.readers[libraryID] = reader
	}
	var entry models.MediaLibraryEntry
	err := reader.VisibleEntries().Select("work_key").Where("library_id = ? AND media_type = ? AND tmdb_id = ? AND match_status = ? AND work_key <> ''", libraryID, mediaType, tmdbID, mediaRecognitionStatusMatched).Order("work_key ASC").Limit(1).Take(&entry).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil, err
	}
	var target *AcquisitionCatalogTarget
	if err == nil {
		target = &AcquisitionCatalogTarget{LibraryID: libraryID, WorkToken: encodeCatalogToken(entry.WorkKey)}
	}
	p.targets[key] = target
	return target, reader, nil
}

// A receipt alone is not current availability, and another episode of the same
// show is not this transfer. Check every active managed video against effective
// entries in the exact destination, with its persisted path/provider/size proof.
func acquisitionTransferPublished(tx *gorm.DB, reader *CatalogReader, download models.DownloadTask, transfer models.TransferTask, row models.MediaAcquisition) (bool, error) {
	identity, ok := acquisitionIdentityForDownload(download)
	if !ok || identity.MediaType != row.MediaType || identity.TMDBID != row.TMDBID || download.TargetLibraryID == nil || *download.TargetLibraryID != transfer.LibraryID || download.TargetStorageID == nil {
		return false, nil
	}
	// A replacement source can contain the same relative name, size and TMDB ID.
	// Its catalog is valid navigation, but cannot verify an old intake's receipt.
	destination := tx.Table("media_libraries AS destination").
		Joins("JOIN storages AS storage ON storage.id = destination.storage_id").
		Where("destination.id = ? AND destination.storage_id = ? AND destination.relative_root = ? AND destination.provider_root_id = ? AND destination.enabled = ? AND storage.enabled = ? AND storage.type = ? AND storage.root_path = ?", transfer.LibraryID, *download.TargetStorageID, download.TargetRelativeRoot, download.TargetProviderRootID, true, true, download.TargetStorageType, download.TargetStorageRoot)
	if download.TargetConnectionID == nil {
		destination = destination.Where("storage.connection_id IS NULL")
	} else {
		destination = destination.Where("storage.connection_id = ?", *download.TargetConnectionID)
	}
	var matchingDestination int64
	if err := destination.Count(&matchingDestination).Error; err != nil {
		return false, err
	}
	if matchingDestination != 1 {
		return false, nil
	}
	allManaged := tx.Model(&models.MediaManagedItem{}).Where("library_id=? AND transfer_task_id=? AND download_task_id=? AND active=? AND managed=?", transfer.LibraryID, transfer.ID, download.ID, true, true)
	var managedCount int64
	if err := allManaged.Session(&gorm.Session{}).Count(&managedCount).Error; err != nil {
		return false, err
	}
	expected, evidenceErr := transferBatchExpectedFiles(transfer, download.TargetStorageType)
	if models.IsNativeCloudStorage(download.TargetStorageType) && (normalizeExecutionLocation(transfer.ExecutionLocation) == models.NodeLocationRemote || normalizeExecutionLocation(download.ExecutionLocation) == models.NodeLocationRemote) {
		var storage models.Storage
		if err := tx.First(&storage, *download.TargetStorageID).Error; err != nil {
			return false, err
		}
		var items []models.MediaManagedItem
		if err := allManaged.Session(&gorm.Session{}).Find(&items).Error; err != nil {
			return false, err
		}
		expected, evidenceErr = remoteUploadBatchEvidenceTx(tx, transfer, download, storage, items)
	}
	if evidenceErr != nil || expected <= 0 || int64(expected) != managedCount {
		return false, nil
	}
	var physical models.CatalogPhysicalWrite
	err := tx.Where("library_id=? AND owner_kind=? AND owner_id=? AND job_id=? AND state=?", transfer.LibraryID, CatalogPhysicalTransfer, transfer.ID, transfer.JobID, "settled").First(&physical).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	confirmed, err := acquisitionPublicationScopeCurrent(tx, reader, transfer, physical)
	if err != nil || !confirmed {
		return false, err
	}
	head, _ := reader.Head(transfer.LibraryID)
	if physical.SourceEpoch != head.SourceEpoch {
		return false, nil
	}
	owner, err := catalogPhysicalOwnerTx(tx, CatalogPhysicalWriteInput{LibraryID: transfer.LibraryID, OwnerKind: CatalogPhysicalTransfer, OwnerID: transfer.ID}, true, false)
	if err != nil || physical.SettledAt == nil || physical.OwnerDigest != owner.digest || physical.JobID != owner.jobID {
		return false, err
	}
	var job models.Job
	err = tx.First(&job, "id=?", transfer.JobID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var payload transferJobPayload
	if job.OwnerID == nil || *job.OwnerID != transfer.OwnerID || job.JobType != "transfer" || job.Status == models.JobStatusFailed || job.Status == models.JobStatusCancelled || json.Unmarshal([]byte(job.PayloadJSON), &payload) != nil || payload.TransferTaskID != transfer.ID {
		return false, nil
	}
	managed := tx.Model(&models.MediaManagedItem{}).Where("media_managed_items.library_id = ? AND media_managed_items.transfer_task_id = ? AND media_managed_items.download_task_id = ? AND media_managed_items.kind = ? AND media_managed_items.active = ? AND media_managed_items.managed = ?", transfer.LibraryID, transfer.ID, download.ID, models.MediaManagedItemKindVideo, true, true)
	var total int64
	if err := managed.Session(&gorm.Session{}).Count(&total).Error; err != nil {
		return false, err
	}
	if total == 0 {
		return false, nil
	}
	var present int64
	err = managed.Session(&gorm.Session{}).
		Joins("JOIN (?) AS published ON published.library_id = media_managed_items.library_id AND published.relative_path = media_managed_items.relative_path AND published.size = media_managed_items.size AND (media_managed_items.provider_item_id = '' OR published.provider_id = media_managed_items.provider_item_id)", reader.VisibleEntries()).
		Where("media_managed_items.identity_revision = ? AND published.media_type = ? AND published.tmdb_id = ? AND published.match_status = ? AND published.work_key <> ''", download.IdentityRevision, row.MediaType, row.TMDBID, mediaRecognitionStatusMatched).
		Count(&present).Error
	return total == present, err
}

func projectDownloadProgress(status *AcquisitionStatus, task models.DownloadTask) {
	status.Progress = cloneFloat64(task.Progress)
	status.BytesCompleted = cloneInt64(task.BytesCompleted)
	status.BytesTotal = cloneInt64(task.BytesTotal)
	status.DownloadSpeed = cloneInt64(task.DownloadSpeed)
	status.ETASeconds = cloneInt64(task.ETASeconds)
}

// Persisted only by the successful catalog-publication transaction. Pointers
// distinguish a complete first-generation (zero) proof from old/partial JSON.
type acquisitionPublicationScope struct {
	Version           *uint   `json:"version"`
	LibraryID         *uint   `json:"library_id"`
	SourceEpoch       *uint64 `json:"source_epoch"`
	RootGeneration    *uint64 `json:"root_generation"`
	SourceFingerprint *string `json:"source_fingerprint"`
}

func stampAcquisitionPublicationTx(tx *gorm.DB, run *models.MediaLibraryScanRun) error {
	if run.Kind != "transfer_batch" {
		return nil
	}
	var checkpoint map[string]json.RawMessage
	if err := json.Unmarshal([]byte(run.CheckpointJSON), &checkpoint); err != nil {
		return err
	}
	var transferID string
	if json.Unmarshal(checkpoint["transfer_batch_id"], &transferID) != nil || transferID == "" {
		return ErrCatalogInvalid
	}
	library, storage, _, err := catalogConversionContextTx(tx, run.LibraryID)
	if err != nil {
		return err
	}
	var epoch uint64
	if err := tx.Model(&models.CatalogHead{}).Select("source_epoch").Where("library_id=?", library.ID).Scan(&epoch).Error; err != nil {
		return err
	}
	version, fingerprint := uint(1), catalogSourceFingerprint(library, storage)
	scope, err := json.Marshal(acquisitionPublicationScope{&version, &library.ID, &epoch, &library.ExclusionEpoch, &fingerprint})
	if err != nil {
		return err
	}
	checkpoint["acquisition_publication_scope"] = scope
	raw, err := json.Marshal(checkpoint)
	if err != nil {
		return err
	}
	run.CheckpointJSON = string(raw)
	return nil
}

func acquisitionPublicationScopeCurrent(tx *gorm.DB, reader *CatalogReader, transfer models.TransferTask, physical models.CatalogPhysicalWrite) (bool, error) {
	var run models.MediaLibraryScanRun
	err := tx.Where("library_id=? AND kind=? AND status=? AND catalog_published_at IS NOT NULL AND CASE WHEN json_valid(checkpoint_json) THEN json_extract(checkpoint_json, '$.transfer_batch_id') ELSE '' END=?", transfer.LibraryID, "transfer_batch", "success", transfer.ID).Order("id DESC").First(&run).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var checkpoint struct {
		Scope acquisitionPublicationScope `json:"acquisition_publication_scope"`
	}
	if json.Unmarshal([]byte(run.CheckpointJSON), &checkpoint) != nil {
		return false, nil
	}
	scope := checkpoint.Scope
	if scope.Version == nil || *scope.Version != 1 || scope.LibraryID == nil || *scope.LibraryID != transfer.LibraryID || scope.SourceEpoch == nil || scope.RootGeneration == nil || scope.SourceFingerprint == nil || *scope.SourceFingerprint == "" {
		return false, nil
	}
	library, storage, _, err := catalogConversionContextTx(tx, transfer.LibraryID)
	if err != nil {
		return false, err
	}
	head, _ := reader.Head(transfer.LibraryID)
	currentFingerprint := catalogSourceFingerprint(library, storage)
	return *scope.SourceEpoch == head.SourceEpoch && *scope.RootGeneration == library.ExclusionEpoch && *scope.SourceFingerprint == currentFingerprint && physical.SourceEpoch == head.SourceEpoch && physical.SourceFingerprint == currentFingerprint && (head.Mode != "versioned" || head.SourceFingerprint == currentFingerprint), nil
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
		return "import"
	default:
		return "import"
	}
}

func acquisitionStatus(row models.MediaAcquisition) AcquisitionStatus {
	updated := row.UpdatedAt
	return AcquisitionStatus{ID: row.ID, MediaType: row.MediaType, TMDBID: row.TMDBID, Stage: row.Stage, Status: row.Status, DownloadTaskID: row.DownloadTaskID, FollowSubscriptionID: row.FollowSubscriptionID, TargetLibraryID: row.TargetLibraryID, LastErrorCode: row.LastErrorCode, Revision: row.Revision, UpdatedAt: &updated}
}
