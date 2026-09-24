package services

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	serverlog "github.com/yuanjing-hash/OhMyCine-Server/internal/logging"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/medialibrary"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	storagefs "github.com/yuanjing-hash/OhMyCine-Server/internal/storage"
	cloudpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/metadata/nfo"
	"gorm.io/gorm"
)

const JobTypeMediaArtifact = "media_artifact"
const artifactTargetCloudCleanup = "cloud_empty_cleanup"

func mediaArtifactResourceKey(libraryID uint) string {
	return fmt.Sprintf("media-artifact-library:%d", libraryID)
}

type mediaArtifactPolicy struct {
	DeletedWorkGuards         []deletedRecognitionScope `json:"deleted_work_guards,omitempty"`
	CloudEmptyCleanupEnabled  bool                      `json:"cloud_empty_cleanup_enabled,omitempty"`
	CloudCleanupDirectories   []cloudCleanupDirectory   `json:"cloud_cleanup_directories,omitempty"`
	SourceBoundaryFingerprint string                    `json:"source_boundary_fingerprint,omitempty"`
	BatchSourceFingerprint    string                    `json:"batch_source_fingerprint,omitempty"`
	ChangeRevisions           []uint64                  `json:"change_revisions,omitempty"`
	ScopeVersion              int                       `json:"scope_version,omitempty"`
	EntryIDs                  []uint                    `json:"entry_ids,omitempty"`
	RecognitionIDs            []uint                    `json:"recognition_ids,omitempty"`
	SourceAssetIDs            []uint                    `json:"source_asset_ids,omitempty"`
	DeletedManifestIDs        []uint                    `json:"deleted_manifest_ids,omitempty"`
	LibraryID                 uint                      `json:"library_id"`
	Generation                uint64                    `json:"generation"`
	StorageID                 uint                      `json:"storage_id"`
	StorageType               string                    `json:"storage_type"`
	ConnectionID              uint                      `json:"connection_id,omitempty"`
	ProjectionRoot            string                    `json:"projection_root"`
	ProjectionRootIdentity    string                    `json:"projection_root_identity,omitempty"`
	TargetKind                string                    `json:"target_kind"`
	STRMEnabled               bool                      `json:"strm_enabled"`
	Metadata                  bool                      `json:"metadata_artifacts_enabled"`
	AssetExtensions           []string                  `json:"asset_extensions,omitempty"`
	ScanRunID                 uint                      `json:"scan_run_id,omitempty"`
	ScanKind                  string                    `json:"scan_kind,omitempty"`
	ScanPartial               bool                      `json:"scan_partial,omitempty"`
	CleanupEligible           bool                      `json:"cleanup_eligible,omitempty"`
	RefreshSerial             int64                     `json:"refresh_serial,omitempty"`
	CatalogBindingID          string                    `json:"catalog_binding_id,omitempty"`
	CatalogScopeMode          string                    `json:"catalog_scope_mode,omitempty"`
}

type mediaArtifactJobPayload struct {
	ArtifactRunID string `json:"artifact_run_id"`
}

type ArtifactCleanupResult struct {
	Removed   int
	ErrorCode string
	Skipped   bool
}

type MediaArtifactCleanup interface {
	AutoCleanup(context.Context, string) ArtifactCleanupResult
}

type MediaArtifactService struct {
	db           *gorm.DB
	queue        *QueueService
	signedProxy  *SignedProxyService
	metadata     *MetadataSettingsService
	connections  *ConnectionService
	cleanup      MediaArtifactCleanup
	changes      *MediaChangeService
	log          zerolog.Logger
	catalogStore *CatalogSnapshotStore
	localSlots   chan struct{}
}

func (s *MediaArtifactService) SetCatalogSnapshotStore(store *CatalogSnapshotStore) {
	s.catalogStore = store
}

func NewMediaArtifactService(db *gorm.DB, queue *QueueService, signedProxy *SignedProxyService, log zerolog.Logger) *MediaArtifactService {
	return &MediaArtifactService{db: db, queue: queue, signedProxy: signedProxy, log: log, localSlots: make(chan struct{}, 8)}
}

func (s *MediaArtifactService) SetMetadataSettingsService(metadata *MetadataSettingsService) {
	s.metadata = metadata
}

func (s *MediaArtifactService) SetConnectionService(connections *ConnectionService) {
	s.connections = connections
}

func (s *MediaArtifactService) SetCleanupService(cleanup MediaArtifactCleanup) {
	s.cleanup = cleanup
}
func (s *MediaArtifactService) SetMediaChangeService(changes *MediaChangeService) {
	s.changes = changes
}

// ScheduleGeneration binds one immutable run to its own queue payload. Library
// resource serialization is retained, but a worker never borrows another run.
func (s *MediaArtifactService) ScheduleGeneration(libraryID uint, generation uint64) error {
	return s.scheduleGeneration(libraryID, generation, false)
}

// RefreshGeneration reruns an already settled generation after recognition.
// It never replaces a live or unresolved owner's policy. Callers retain their
// durable scan/binding follow-up marker when a conflicting refresh is rejected.
func (s *MediaArtifactService) RefreshGeneration(libraryID uint, generation uint64) error {
	return s.scheduleGeneration(libraryID, generation, true)
}

func (s *MediaArtifactService) scheduleGeneration(libraryID uint, generation uint64, refresh bool) error {
	if s == nil || s.queue == nil || libraryID == 0 || generation == 0 {
		return appError(CodeInvalidRequest, "媒体产物任务参数无效", nil)
	}
	var library models.MediaLibrary
	if err := s.db.First(&library, libraryID).Error; err != nil {
		return mediaLibraryNotFound(err)
	}
	var storage models.Storage
	if err := s.db.First(&storage, library.StorageID).Error; err != nil {
		return err
	}
	var extraAssetExtensions []string
	_ = json.Unmarshal([]byte(library.STRMAssetExtraExtensionsJSON), &extraAssetExtensions)
	policy := mediaArtifactPolicy{LibraryID: library.ID, Generation: generation, StorageID: library.StorageID, StorageType: storage.Type, Metadata: library.MetadataArtifactsEnabled, AssetExtensions: effectiveSourceAssetExtensions(extraAssetExtensions), SourceBoundaryFingerprint: catalogSourceFingerprint(library, storage)}
	var scanRun models.MediaLibraryScanRun
	if err := s.db.Where("library_id = ? AND generation = ?", library.ID, generation).Order("id DESC").First(&scanRun).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	if library.CloudEmptyCleanupEnabled && storage.Type == models.StorageTypePan115 && scanRun.CatalogPublishedAt != nil && (scanRun.Status == "success" || scanRun.Status == "catalog_ready") {
		var checkpoint struct {
			Directories []cloudCleanupDirectory `json:"cloud_cleanup_directories"`
		}
		if err := json.Unmarshal([]byte(scanRun.CheckpointJSON), &checkpoint); err != nil {
			return err
		}
		policy.CloudCleanupDirectories = checkpoint.Directories
		policy.CloudEmptyCleanupEnabled = len(checkpoint.Directories) > 0
	}
	switch storage.Type {
	case models.StorageTypeLocal:
		if !library.MetadataArtifactsEnabled {
			return nil
		}
		root, resolveErr := medialibrary.ResolveRoot(storage.RootPath, library.RelativeRoot)
		if resolveErr != nil {
			return appError(CodeInvalidRequest, "本地媒体库产物目录无效", nil)
		}
		canonical, canonicalErr := (storagefs.LocalDriver{}).CanonicalizeRoot(root)
		if canonicalErr != nil {
			return appError(CodeInvalidRequest, "本地媒体库产物目录不可用", nil)
		}
		policy.ProjectionRoot, policy.TargetKind = canonical, models.MediaArtifactTargetLocalAdjacent
	default:
		if !library.STRMEnabled || !library.SignedProxyEnabled || s.signedProxy == nil {
			if !policy.CloudEmptyCleanupEnabled {
				return nil
			}
			policy.Metadata = false
			policy.TargetKind = artifactTargetCloudCleanup
		}
		if storage.ConnectionID == nil || *storage.ConnectionID == 0 {
			return appError(CodeConnectionUnavailable, "媒体库连接不可用", nil)
		}
		policy.ConnectionID = *storage.ConnectionID
		if policy.TargetKind != artifactTargetCloudCleanup {
			policy.ProjectionRoot, policy.TargetKind, policy.STRMEnabled = library.STRMLocalRoot, models.MediaArtifactTargetLocalProjection, true
		}
	}
	if policy.TargetKind != artifactTargetCloudCleanup {
		canonicalRoot, rootIdentity, err := canonicalProjectionRoot(policy.ProjectionRoot)
		if err != nil {
			return appError(CodeInvalidRequest, "媒体产物目录不可用", nil)
		}
		policy.ProjectionRoot, policy.ProjectionRootIdentity = canonicalRoot, rootIdentity
	}
	if err := s.db.Where("library_id = ? AND generation = ?", library.ID, generation).Order("id DESC").First(&scanRun).Error; err == nil {
		policy.ScanRunID = scanRun.ID
		policy.ScanKind = scanRun.Kind
		policy.ScanPartial = scanRun.Partial
		policy.CleanupEligible = (policy.TargetKind == models.MediaArtifactTargetLocalProjection || policy.TargetKind == models.MediaArtifactTargetLocalAdjacent) && scanRun.Status == "success" && !scanRun.Partial && automaticCleanupScanKind(scanRun.Kind)
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	if refresh {
		policy.RefreshSerial = time.Now().UTC().UnixNano()
	}
	if err := s.bindScheduledArtifactPolicy(&policy); err != nil {
		return err
	}
	if err := s.freezeLegacyArtifactScope(&policy); err != nil {
		return err
	}
	if err := s.db.Transaction(func(tx *gorm.DB) error { return freezeArtifactDeletedWorkGuardsTx(tx, &policy) }); err != nil {
		return err
	}
	policyJSON, err := json.Marshal(policy)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	run := models.MediaArtifactRun{ID: uuid.NewString(), LibraryID: library.ID, Generation: generation, CatalogBindingID: policy.CatalogBindingID, PolicyJSON: string(policyJSON), Status: models.MediaArtifactStatusQueued, CleanupStatus: models.MediaArtifactCleanupPending, CreatedAt: now, UpdatedAt: now}
	var existing models.MediaArtifactRun
	lookup := s.db.Where("library_id = ? AND generation = ?", library.ID, generation)
	if policy.CatalogBindingID != "" {
		lookup = s.db.Where("library_id = ? AND catalog_binding_id = ?", library.ID, policy.CatalogBindingID)
	} else {
		lookup = legacyArtifactRunScopeQuery(lookup.Where("catalog_binding_id = ''"), policy)
	}
	lookupErr := lookup.First(&existing).Error
	if lookupErr != nil && !errors.Is(lookupErr, gorm.ErrRecordNotFound) {
		return lookupErr
	}
	if lookupErr == nil {
		run = existing
		if !refresh && existing.Status == models.MediaArtifactStatusCompleted && artifactPoliciesEquivalent(existing.PolicyJSON, policy) {
			return nil
		}
		if existing.Status == models.MediaArtifactStatusRunning {
			if !refresh && artifactPoliciesEquivalent(existing.PolicyJSON, policy) {
				return nil
			}
			return catalogPhysicalUnsettledError()
		}
		if existing.JobID != nil && artifactPoliciesEquivalent(existing.PolicyJSON, policy) && existing.Status != models.MediaArtifactStatusCompleted && !refresh {
			var active int64
			if err := s.db.Model(&models.Job{}).Where("id = ? AND status IN ?", *existing.JobID, activeJobStatuses()).Count(&active).Error; err != nil {
				return err
			}
			if active > 0 {
				return nil
			}
		}
	}
	_, err = s.queue.EnqueueLatestWith(EnqueueJobInput{System: true, JobType: JobTypeMediaArtifact, Priority: 100, DisplayName: fmt.Sprintf("媒体产物 · 媒体库 %d", library.ID), Provider: "media_library", ResourceKey: mediaArtifactResourceKey(library.ID), CoalescingKey: "run:" + run.ID, Payload: mediaArtifactJobPayload{ArtifactRunID: run.ID}}, func(tx *gorm.DB, job models.Job) error {
		if err := AssertCatalogPhysicalAdmissionTx(tx, library.ID); err != nil {
			return err
		}
		var current models.MediaArtifactRun
		var admitted *models.CatalogPhysicalWrite
		currentQuery := tx.Where("library_id = ? AND generation = ?", library.ID, generation)
		if policy.CatalogBindingID != "" {
			currentQuery = tx.Where("library_id = ? AND catalog_binding_id = ?", library.ID, policy.CatalogBindingID)
		} else {
			currentQuery = legacyArtifactRunScopeQuery(currentQuery.Where("catalog_binding_id = ''"), policy)
		}
		currentErr := currentQuery.First(&current).Error
		if currentErr == nil {
			if lookupErr != nil || current.ID != existing.ID || current.PolicyJSON != existing.PolicyJSON || current.Status == models.MediaArtifactStatusRunning {
				return catalogPhysicalUnsettledError()
			}
			var receipt models.CatalogPhysicalWrite
			receiptErr := tx.Where("owner_kind=? AND owner_id=?", CatalogPhysicalArtifact, current.ID).First(&receipt).Error
			if receiptErr != nil && !errors.Is(receiptErr, gorm.ErrRecordNotFound) {
				return receiptErr
			}
			if receiptErr == nil && receipt.State != "settled" {
				if receipt.State != "admitted" {
					return catalogPhysicalUnsettledError()
				}
				admitted = &receipt
			}
		} else if !errors.Is(currentErr, gorm.ErrRecordNotFound) {
			return currentErr
		}
		if policy.CatalogBindingID != "" {
			if err := s.validateArtifactBindingTx(tx, policy, nil, nil, false); err != nil {
				return err
			}
			var binding models.CatalogArtifactBinding
			if err := tx.First(&binding, "id = ?", policy.CatalogBindingID).Error; err != nil {
				return err
			}
			if binding.State == "completed" && refresh {
				snapshot, err := artifactCatalogSnapshot(binding)
				if err != nil {
					return err
				}
				for _, layer := range snapshot.Layers {
					if err := AcquireCatalogReferenceTx(tx, layer.SnapshotID, "artifact", binding.ID); err != nil {
						return err
					}
				}
				if err := tx.Model(&binding).Updates(map[string]any{"state": "scheduled", "finalize_after_id": 0, "updated_at": now}).Error; err != nil {
					return err
				}
			}
			if err := tx.Model(&models.CatalogArtifactBinding{}).Where("id = ? AND state IN ?", policy.CatalogBindingID, []string{"pending", "failed", "scheduled"}).Updates(map[string]any{"state": "scheduled", "updated_at": now}).Error; err != nil {
				return err
			}
		}
		if currentErr == nil {
			update := func(tx *gorm.DB) error {
				existing := current
				run = current
				if err := tx.Model(&existing).Update("job_id", job.ID).Error; err != nil {
					return err
				}
				if refresh || existing.PolicyJSON != string(policyJSON) {
					updates := map[string]any{"policy_json": string(policyJSON), "updated_at": now}
					if existing.Status != models.MediaArtifactStatusRunning {
						updates["status"] = models.MediaArtifactStatusQueued
						updates["expected_count"] = 0
						updates["written_count"] = 0
						updates["updated_count"] = 0
						updates["removed_count"] = 0
						updates["skipped_count"] = 0
						updates["failed_count"] = 0
						updates["error_code"] = ""
						updates["cleanup_status"] = models.MediaArtifactCleanupPending
						updates["cleanup_error_code"] = ""
						updates["cleanup_at"] = nil
						updates["started_at"] = nil
						updates["finished_at"] = nil
						run.Status = models.MediaArtifactStatusQueued
					}
					run.PolicyJSON = string(policyJSON)
					if err := tx.Model(&existing).Updates(updates).Error; err != nil {
						return err
					}
					return artifactScheduledLibraryQuery(tx, policy).Updates(map[string]any{"artifact_generation": generation, "artifact_status": models.MediaArtifactStatusQueued, "artifact_error": "", "artifact_updated_at": now}).Error
				}
				return nil
			}
			if admitted != nil {
				return refreshAdmittedCatalogArtifactTx(tx, current, *admitted, update)
			}
			return update(tx)
		}
		run.JobID = &job.ID
		if err := RegisterCatalogPhysicalOwnerTx(tx, CatalogPhysicalWriteInput{LibraryID: library.ID, OwnerKind: CatalogPhysicalArtifact, OwnerID: run.ID}, func(tx *gorm.DB) error {
			return tx.Create(&run).Error
		}); err != nil {
			return err
		}
		return artifactScheduledLibraryQuery(tx, policy).Updates(map[string]any{"artifact_generation": generation, "artifact_status": models.MediaArtifactStatusQueued, "artifact_error": "", "artifact_updated_at": now}).Error
	})
	return err
}

func artifactPoliciesEquivalent(raw string, policy mediaArtifactPolicy) bool {
	var previous mediaArtifactPolicy
	if json.Unmarshal([]byte(raw), &previous) != nil {
		return false
	}
	previous.RefreshSerial, policy.RefreshSerial = 0, 0
	left, _ := json.Marshal(previous)
	right, _ := json.Marshal(policy)
	return string(left) == string(right)
}

func (s *MediaArtifactService) failRun(runID, code string) error {
	now := time.Now().UTC()
	return s.db.Transaction(func(tx *gorm.DB) error {
		var run models.MediaArtifactRun
		if err := tx.First(&run, "id = ?", runID).Error; err != nil {
			return err
		}
		if err := tx.Model(&run).Updates(map[string]any{"status": models.MediaArtifactStatusFailed, "error_code": code, "cleanup_status": models.MediaArtifactCleanupSkipped, "cleanup_at": now, "finished_at": now, "updated_at": now}).Error; err != nil {
			return err
		}
		return tx.Model(&models.MediaLibrary{}).Where("id = ? AND artifact_generation = ?", run.LibraryID, run.Generation).Updates(map[string]any{"artifact_status": models.MediaArtifactStatusFailed, "artifact_error": code, "artifact_updated_at": now}).Error
	})
}

type MediaArtifactWorker struct{ service *MediaArtifactService }

func NewMediaArtifactWorker(service *MediaArtifactService) *MediaArtifactWorker {
	return &MediaArtifactWorker{service: service}
}

func withArtifactScanContext(event *zerolog.Event, policy mediaArtifactPolicy, phase string) *zerolog.Event {
	if policy.ScanRunID != 0 {
		event.Uint("scan_run_id", policy.ScanRunID)
	}
	if policy.ScanKind != "" {
		event.Str("scan_kind", policy.ScanKind)
	}
	return event.Str("phase", phase)
}

func (w *MediaArtifactWorker) Run(ctx context.Context, runtime JobRuntime, job ClaimedJob) WorkerResult {
	if w == nil || w.service == nil {
		return WorkerResult{ErrorCode: "artifact_service_unavailable", ErrorMessage: "媒体产物服务不可用"}
	}
	var payload mediaArtifactJobPayload
	if err := json.Unmarshal([]byte(job.Job.PayloadJSON), &payload); err != nil || strings.TrimSpace(payload.ArtifactRunID) == "" {
		return WorkerResult{ErrorCode: "artifact_payload_invalid", ErrorMessage: "媒体产物任务参数无效"}
	}
	run, policy, err := w.service.loadRun(payload.ArtifactRunID)
	if err != nil {
		return WorkerResult{ErrorCode: "artifact_run_missing", ErrorMessage: "媒体产物记录不存在"}
	}
	if run.Status == models.MediaArtifactStatusCompleted {
		if superseded, err := w.service.artifactPolicySuperseded(policy); err != nil {
			return WorkerResult{ErrorCode: "artifact_state_unavailable", ErrorMessage: "媒体产物状态不可用"}
		} else if superseded {
			return w.service.recoverSupersededArtifactExecution(ctx, job, run, policy)
		}
		permit, err := enterCatalogPhysicalWrite(ctx, w.service.db, CatalogPhysicalWriteInput{LibraryID: run.LibraryID, OwnerKind: CatalogPhysicalArtifact, OwnerID: run.ID, Job: &job, ArtifactReceiptVersion: 1})
		if err != nil {
			return artifactPhysicalRecoveryPending("artifact_write_admission_failed")
		}
		defer w.service.finishArtifactExecutionOnExit(permit, policy)
		return w.service.finishArtifactGeneration(ctx, job, permit, run, policy)
	}
	if run.Status == models.MediaArtifactStatusSuperseded {
		return w.service.recoverSupersededArtifactExecution(ctx, job, run, policy)
	}
	if superseded, checkErr := w.service.artifactPolicySuperseded(policy); checkErr != nil {
		return WorkerResult{ErrorCode: "artifact_state_unavailable", ErrorMessage: "媒体产物状态不可用"}
	} else if superseded {
		return w.service.recoverSupersededArtifactExecution(ctx, job, run, policy)
	}
	started := time.Now()
	now := time.Now().UTC()
	if err := w.service.catalogArtifactWriteTx(ctx, func(tx *gorm.DB) error {
		if _, err := w.service.queue.verifyLease(tx, job.Job.ID, job.LeaseToken); err != nil {
			return err
		}
		var currentRun models.MediaArtifactRun
		if err := tx.First(&currentRun, "id = ?", run.ID).Error; err != nil {
			return err
		}
		if currentRun.JobID == nil || *currentRun.JobID != job.Job.ID || currentRun.PolicyJSON != run.PolicyJSON {
			return ErrCatalogFence
		}
		if policy.CatalogBindingID != "" {
			if err := w.service.validateArtifactBindingTx(tx, policy, nil, &job, true); err != nil {
				return err
			}
			var current models.MediaArtifactRun
			if err := tx.First(&current, "id = ?", run.ID).Error; err != nil {
				return err
			}
			if current.PolicyJSON != run.PolicyJSON {
				return ErrCatalogFence
			}
		}
		if err := tx.Model(&models.MediaArtifactRun{}).Where("id = ?", run.ID).Updates(map[string]any{"status": models.MediaArtifactStatusRunning, "started_at": now, "finished_at": nil, "error_code": "", "updated_at": now}).Error; err != nil {
			return err
		}
		result := tx.Model(&models.MediaLibrary{}).Where("id = ? AND artifact_generation = ?", run.LibraryID, run.Generation).Updates(map[string]any{"artifact_status": models.MediaArtifactStatusRunning, "artifact_error": "", "artifact_updated_at": now})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 && policy.ScopeVersion != 1 {
			return errors.New("artifact generation changed before start")
		}
		return nil
	}); err != nil {
		return WorkerResult{ErrorCode: "artifact_state_persist_failed", ErrorMessage: "媒体产物状态保存失败"}
	}
	withArtifactScanContext(serverlog.OperationMediaArtifact.Event(w.service.log.Info()).Uint("library_id", run.LibraryID).Str("task_id", run.ID).Uint64("generation", run.Generation), policy, "artifact_running").Msg(serverlog.OperationMediaArtifact.Message("开始"))
	if policy.STRMEnabled {
		withArtifactScanContext(serverlog.OperationSTRMGeneration.Event(w.service.log.Info()).Uint("library_id", run.LibraryID).Str("task_id", run.ID).Uint64("generation", run.Generation), policy, "artifact_running").Msg(serverlog.OperationSTRMGeneration.Message("开始"))
	}
	permit, err := enterCatalogPhysicalWrite(ctx, w.service.db, CatalogPhysicalWriteInput{LibraryID: run.LibraryID, OwnerKind: CatalogPhysicalArtifact, OwnerID: run.ID, Job: &job, ArtifactReceiptVersion: 1})
	if err != nil {
		return artifactPhysicalRecoveryPending("artifact_write_admission_failed")
	}
	defer w.service.finishArtifactExecutionOnExit(permit, policy)
	if err := w.service.reconcileArtifactExecution(ctx, permit, policy); err != nil {
		return artifactPhysicalRecoveryPending("artifact_write_reconciliation_pending")
	}
	var result WorkerResult
	if policy.CatalogBindingID != "" {
		result = w.service.generateBoundArtifacts(ctx, runtime, job, permit, run, policy)
	} else if policy.TargetKind == artifactTargetCloudCleanup {
		result = w.service.completeCloudOnlyArtifactGeneration(ctx, job, run, policy)
	} else {
		result = w.service.generateArtifacts(ctx, runtime, job, permit, run, policy)
	}
	_ = w.service.db.First(&run, "id = ?", run.ID).Error
	if superseded, checkErr := w.service.artifactPolicySuperseded(policy); checkErr == nil && (superseded || run.Status == models.MediaArtifactStatusSuperseded) {
		if err := w.service.settleSupersededArtifactExecution(permit, policy); err != nil {
			return artifactPhysicalRecoveryPending("artifact_superseded_reconciliation_pending")
		}
		return WorkerResult{}
	}
	if result.ErrorCode == "" && result.RetryAt == nil && run.Status == models.MediaArtifactStatusCompleted {
		result = w.service.finishArtifactGeneration(ctx, job, permit, run, policy)
	}
	if run.Status == models.MediaArtifactStatusSuperseded {
		withArtifactScanContext(serverlog.OperationMediaArtifact.Event(w.service.log.Info()).Uint("library_id", run.LibraryID).Str("task_id", run.ID).Uint64("generation", run.Generation).Int64("duration_ms", time.Since(started).Milliseconds()), policy, "superseded").Msg(serverlog.OperationMediaArtifact.Message("已由更新 generation 接管"))
		return WorkerResult{}
	}
	if result.ErrorCode != "" {
		withArtifactScanContext(serverlog.OperationMediaArtifact.Event(w.service.log.Error()).Uint("library_id", run.LibraryID).Str("task_id", run.ID).Uint64("generation", run.Generation).Str("error_code", result.ErrorCode).Int64("duration_ms", time.Since(started).Milliseconds()), policy, "artifact_failed").Msg(serverlog.OperationMediaArtifact.Message("失败"))
		if policy.STRMEnabled {
			withArtifactScanContext(serverlog.OperationSTRMGeneration.Event(w.service.log.Error()).Uint("library_id", run.LibraryID).Str("task_id", run.ID).Uint64("generation", run.Generation).Str("error_code", result.ErrorCode).Int64("duration_ms", time.Since(started).Milliseconds()), policy, "artifact_failed").Msg(serverlog.OperationSTRMGeneration.Message("失败"))
		}
		return result
	}
	withArtifactScanContext(serverlog.OperationMediaArtifact.Event(w.service.log.Info()).Uint("library_id", run.LibraryID).Str("task_id", run.ID).Uint64("generation", run.Generation).Int("written", run.WrittenCount).Int("updated", run.UpdatedCount).Int("skipped", run.SkippedCount).Int64("duration_ms", time.Since(started).Milliseconds()), policy, "artifact_completed").Msg(serverlog.OperationMediaArtifact.Message("完成"))
	if policy.STRMEnabled {
		withArtifactScanContext(serverlog.OperationSTRMGeneration.Event(w.service.log.Info()).Uint("library_id", run.LibraryID).Str("task_id", run.ID).Uint64("generation", run.Generation).Int("written", run.WrittenCount).Int("updated", run.UpdatedCount).Int("skipped", run.SkippedCount).Int64("duration_ms", time.Since(started).Milliseconds()), policy, "artifact_completed").Msg(serverlog.OperationSTRMGeneration.Message("完成"))
	}
	return WorkerResult{}
}

func (s *MediaArtifactService) loadRun(runID string) (models.MediaArtifactRun, mediaArtifactPolicy, error) {
	var run models.MediaArtifactRun
	if err := s.db.First(&run, "id = ?", runID).Error; err != nil {
		return models.MediaArtifactRun{}, mediaArtifactPolicy{}, err
	}
	var policy mediaArtifactPolicy
	if err := json.Unmarshal([]byte(run.PolicyJSON), &policy); err != nil || policy.LibraryID != run.LibraryID || policy.Generation != run.Generation || (!policy.STRMEnabled && !policy.Metadata && (!policy.CloudEmptyCleanupEnabled || len(policy.CloudCleanupDirectories) == 0)) || (policy.TargetKind != models.MediaArtifactTargetLocalAdjacent && policy.TargetKind != models.MediaArtifactTargetLocalProjection && policy.TargetKind != artifactTargetCloudCleanup) {
		return models.MediaArtifactRun{}, mediaArtifactPolicy{}, errors.New("artifact policy is invalid")
	}
	if artifactRequiresBoundedScope(policy) && policy.CatalogBindingID == "" && policy.ScopeVersion != 1 {
		return models.MediaArtifactRun{}, mediaArtifactPolicy{}, errors.New("artifact batch scope is missing; obsolete task must be deleted")
	}
	return run, policy, nil
}

func (s *MediaArtifactService) completeCloudOnlyArtifactGeneration(ctx context.Context, claim ClaimedJob, run models.MediaArtifactRun, policy mediaArtifactPolicy) WorkerResult {
	err := s.catalogArtifactWriteTx(ctx, func(tx *gorm.DB) error {
		if _, err := s.queue.verifyLease(tx, claim.Job.ID, claim.LeaseToken); err != nil {
			return err
		}
		var current models.MediaArtifactRun
		var library models.MediaLibrary
		var storage models.Storage
		if err := tx.First(&current, "id = ?", run.ID).Error; err != nil {
			return err
		}
		if current.PolicyJSON != run.PolicyJSON || current.JobID == nil || *current.JobID != claim.Job.ID {
			return ErrCatalogFence
		}
		if err := tx.First(&library, run.LibraryID).Error; err != nil {
			return err
		}
		if err := tx.First(&storage, library.StorageID).Error; err != nil {
			return err
		}
		if !artifactPolicyMatchesLibrary(policy, library) || catalogSourceFingerprint(library, storage) != policy.SourceBoundaryFingerprint {
			return ErrCatalogFence
		}
		now := time.Now().UTC()
		if err := tx.Model(&current).Updates(map[string]any{"status": models.MediaArtifactStatusCompleted, "finished_at": now, "updated_at": now, "error_code": ""}).Error; err != nil {
			return err
		}
		return tx.Model(&library).Where("artifact_generation = ?", run.Generation).Updates(map[string]any{"artifact_applied_generation": run.Generation, "artifact_status": models.MediaArtifactStatusCompleted, "artifact_updated_at": now, "artifact_error": ""}).Error
	})
	if err != nil {
		return WorkerResult{ErrorCode: "artifact_state_persist_failed", ErrorMessage: "云端清理任务状态保存失败"}
	}
	return WorkerResult{}
}

func (s *MediaArtifactService) generateArtifacts(ctx context.Context, runtime JobRuntime, claim ClaimedJob, permit CatalogPhysicalWritePermit, run models.MediaArtifactRun, policy mediaArtifactPolicy) WorkerResult {
	root, err := (storagefs.LocalDriver{}).CanonicalizeRoot(policy.ProjectionRoot)
	if err != nil {
		_ = s.failRun(run.ID, "artifact_projection_unavailable")
		return WorkerResult{ErrorCode: "artifact_projection_unavailable", ErrorMessage: "媒体产物目录不可用"}
	}
	var existingArtifacts []models.MediaArtifact
	if err := func() error {
		if policy.ScopeVersion == 1 {
			return nil
		}
		return s.db.Where("library_id = ? AND target_kind = ?", run.LibraryID, policy.TargetKind).Find(&existingArtifacts).Error
	}(); err != nil {
		_ = s.failRun(run.ID, "artifact_manifest_unavailable")
		return WorkerResult{ErrorCode: "artifact_manifest_unavailable", ErrorMessage: "媒体产物清单不可用"}
	}
	manifest := newArtifactManifestIndex(len(existingArtifacts))
	manifest.lazy = policy.ScopeVersion == 1
	manifest.physical = &artifactPhysicalExecution{ctx: ctx, permit: permit, policy: policy}
	manifest.reserve = func(artifact *models.MediaArtifact) error {
		return s.catalogArtifactWriteTx(ctx, func(tx *gorm.DB) error {
			if _, err := s.queue.verifyLease(tx, claim.Job.ID, claim.LeaseToken); err != nil {
				return err
			}
			return tx.Create(artifact).Error
		})
	}
	manifest.beforeWrite = func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, identity, err := canonicalProjectionRoot(policy.ProjectionRoot)
		if err != nil || identity != policy.ProjectionRootIdentity {
			return ErrCatalogFence
		}
		return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if s.queue == nil {
				return ErrCatalogInvalid
			}
			if _, err := s.queue.verifyLease(tx, claim.Job.ID, claim.LeaseToken); err != nil {
				return err
			}
			var current models.MediaArtifactRun
			if err := tx.First(&current, "id = ?", run.ID).Error; err != nil {
				return err
			}
			if current.JobID == nil || *current.JobID != claim.Job.ID || current.PolicyJSON != run.PolicyJSON || current.Status != models.MediaArtifactStatusRunning {
				return ErrCatalogFence
			}
			return nil
		})
	}
	for index := range existingArtifacts {
		artifact := existingArtifacts[index]
		manifest.rows[artifactManifestKey(artifact.TargetKind, artifact.RelativePath)] = artifact
	}
	var entries []models.MediaLibraryEntry
	entryQuery := s.db.Where("library_id = ?", run.LibraryID).Where(catalogVisibleEntryPredicate)
	if policy.ScopeVersion == 1 {
		entryQuery = entryQuery.Where("id IN ?", policy.EntryIDs)
	}
	if err := entryQuery.Order("relative_path").Find(&entries).Error; err != nil {
		_ = s.failRun(run.ID, "artifact_source_unavailable")
		return WorkerResult{ErrorCode: "artifact_source_unavailable", ErrorMessage: "媒体源清单不可用"}
	}
	activeVerifier := signedArtifactVerifier{}
	if policy.STRMEnabled && len(entries) > 0 {
		if s.signedProxy == nil {
			_ = s.failRun(run.ID, "artifact_proxy_unavailable")
			return WorkerResult{ErrorCode: "artifact_proxy_unavailable", ErrorMessage: "302 签名服务不可用"}
		}
		var profileErr error
		activeVerifier, profileErr = s.signedProxy.activeSigningVerifier()
		if profileErr != nil {
			_ = s.failRun(run.ID, "artifact_proxy_unavailable")
			return WorkerResult{ErrorCode: "artifact_proxy_unavailable", ErrorMessage: "302 签名服务不可用"}
		}
	}
	entriesByRecognition := make(map[uint][]models.MediaLibraryEntry)
	for _, entry := range entries {
		if entry.RecognitionID != nil {
			entriesByRecognition[*entry.RecognitionID] = append(entriesByRecognition[*entry.RecognitionID], entry)
		}
	}
	var recognitions []models.MediaLibraryRecognition
	if policy.Metadata {
		recognitionQuery := s.db.Where("library_id = ? AND status = ?", run.LibraryID, mediaRecognitionStatusMatched).
			Where("EXISTS (SELECT 1 FROM media_library_entries WHERE media_library_entries.recognition_id=media_library_recognitions.id AND " + catalogVisibleEntryPredicate + ")")
		if policy.ScopeVersion == 1 {
			recognitionQuery = recognitionQuery.Where("id IN ?", policy.RecognitionIDs)
		} else if !policy.ScanPartial {
			recognitionQuery = recognitionQuery.Where("last_generation = ?", run.Generation)
		}
		if err := recognitionQuery.Order("id").Find(&recognitions).Error; err != nil {
			_ = s.failRun(run.ID, "artifact_metadata_unavailable")
			return WorkerResult{ErrorCode: "artifact_metadata_unavailable", ErrorMessage: "媒体元数据快照不可用"}
		}
	}
	var sourceAssets []models.MediaLibrarySourceAsset
	if policy.STRMEnabled && policy.StorageType != models.StorageTypeLocal {
		assetQuery := s.db.Where("library_id = ? AND active = ?", run.LibraryID, true)
		if policy.ScopeVersion == 1 {
			assetQuery = assetQuery.Where("id IN ?", policy.SourceAssetIDs)
		} else if !policy.ScanPartial {
			assetQuery = assetQuery.Where("generation = ?", run.Generation)
		}
		if err := assetQuery.Order("relative_path").Find(&sourceAssets).Error; err != nil {
			_ = s.failRun(run.ID, "artifact_source_assets_unavailable")
			return WorkerResult{ErrorCode: "artifact_source_assets_unavailable", ErrorMessage: "源伴随文件清单不可用"}
		}
	}
	if artifactFullAudit(policy) {
		entries, recognitions, sourceAssets = s.filterLegacyFullArtifactWork(root, run, policy, entries, recognitions, sourceAssets, entriesByRecognition, manifest, activeVerifier)
	}
	videoCount := 0
	if policy.STRMEnabled {
		videoCount = len(entries)
	}
	run.ExpectedCount = videoCount + len(sourceAssets) + len(recognitions)
	for index, entry := range entries {
		if !policy.STRMEnabled {
			break
		}
		if err := ctx.Err(); err != nil {
			_ = s.failRun(run.ID, "artifact_context_canceled")
			return WorkerResult{ErrorCode: "artifact_context_canceled", ErrorMessage: "媒体产物任务已取消"}
		}
		if superseded, checkErr := s.artifactPolicySuperseded(policy); checkErr != nil || superseded {
			now := time.Now().UTC()
			_ = s.db.Model(&models.MediaArtifactRun{}).Where("id = ?", run.ID).Updates(map[string]any{"status": models.MediaArtifactStatusSuperseded, "cleanup_status": models.MediaArtifactCleanupSkipped, "cleanup_at": now, "finished_at": now, "updated_at": now}).Error
			return WorkerResult{}
		}
		outcome, writeErr := s.writeSTRM(ctx, root, run, entry, manifest, activeVerifier)
		switch outcome {
		case "written":
			run.WrittenCount++
		case "updated":
			run.UpdatedCount++
		case "skipped":
			run.SkippedCount++
		}
		if writeErr != nil {
			run.FailedCount++
		}
		processed, total := int64(index+1), int64(run.ExpectedCount)
		progress := float64(processed) / float64(max(1, run.ExpectedCount))
		if err := runtime.Heartbeat(&progress, &processed, &total, nil, nil); err != nil {
			_ = s.failRun(run.ID, CodeQueueLeaseInvalid)
			return WorkerResult{ErrorCode: CodeQueueLeaseInvalid, ErrorMessage: "媒体产物任务租约已失效"}
		}
	}
	for index, asset := range sourceAssets {
		if err := ctx.Err(); err != nil {
			_ = s.failRun(run.ID, "artifact_context_canceled")
			return WorkerResult{ErrorCode: "artifact_context_canceled", ErrorMessage: "媒体产物任务已取消"}
		}
		if superseded, checkErr := s.artifactPolicySuperseded(policy); checkErr != nil || superseded {
			now := time.Now().UTC()
			_ = s.db.Model(&models.MediaArtifactRun{}).Where("id = ?", run.ID).Updates(map[string]any{"status": models.MediaArtifactStatusSuperseded, "cleanup_status": models.MediaArtifactCleanupSkipped, "cleanup_at": now, "finished_at": now, "updated_at": now}).Error
			return WorkerResult{}
		}
		outcome, writeErr := s.writeSourceAsset(ctx, root, run, policy, asset, manifest)
		switch outcome {
		case "written":
			run.WrittenCount++
		case "updated":
			run.UpdatedCount++
		case "skipped":
			run.SkippedCount++
		}
		if writeErr != nil {
			run.FailedCount++
		}
		processed, total := int64(videoCount+index+1), int64(run.ExpectedCount)
		progress := float64(processed) / float64(max(1, run.ExpectedCount))
		if err := runtime.Heartbeat(&progress, &processed, &total, nil, nil); err != nil {
			_ = s.failRun(run.ID, CodeQueueLeaseInvalid)
			return WorkerResult{ErrorCode: CodeQueueLeaseInvalid, ErrorMessage: "媒体产物任务租约已失效"}
		}
	}
	for index, recognition := range recognitions {
		if err := ctx.Err(); err != nil {
			_ = s.failRun(run.ID, "artifact_context_canceled")
			return WorkerResult{ErrorCode: "artifact_context_canceled", ErrorMessage: "媒体产物任务已取消"}
		}
		if superseded, checkErr := s.artifactPolicySuperseded(policy); checkErr != nil || superseded {
			now := time.Now().UTC()
			_ = s.db.Model(&models.MediaArtifactRun{}).Where("id = ?", run.ID).Updates(map[string]any{"status": models.MediaArtifactStatusSuperseded, "cleanup_status": models.MediaArtifactCleanupSkipped, "cleanup_at": now, "finished_at": now, "updated_at": now}).Error
			return WorkerResult{}
		}
		outcome, writeErr := s.writeNFO(ctx, root, run, policy.TargetKind, recognition, entriesByRecognition[recognition.ID], manifest)
		switch outcome {
		case "written":
			run.WrittenCount++
		case "updated":
			run.UpdatedCount++
		case "skipped":
			run.SkippedCount++
		}
		if writeErr != nil {
			run.FailedCount++
		}
		processed, total := int64(videoCount+len(sourceAssets)+index+1), int64(run.ExpectedCount)
		progress := float64(processed) / float64(max(1, run.ExpectedCount))
		if err := runtime.Heartbeat(&progress, &processed, &total, nil, nil); err != nil {
			_ = s.failRun(run.ID, CodeQueueLeaseInvalid)
			return WorkerResult{ErrorCode: CodeQueueLeaseInvalid, ErrorMessage: "媒体产物任务租约已失效"}
		}
	}
	if err := s.persistArtifactManifest(manifest); err != nil {
		_ = s.failRun(run.ID, "artifact_manifest_persist_failed")
		return WorkerResult{ErrorCode: "artifact_manifest_persist_failed", ErrorMessage: "媒体产物清单保存失败"}
	}
	finished := time.Now().UTC()
	status, code := models.MediaArtifactStatusCompleted, ""
	if run.FailedCount > 0 {
		status, code = models.MediaArtifactStatusFailed, "artifact_write_failed"
	}
	superseded := false
	requeued := false
	if err := s.db.Transaction(func(tx *gorm.DB) error {
		if status == models.MediaArtifactStatusCompleted {
			var currentRun models.MediaArtifactRun
			if err := tx.First(&currentRun, "id = ?", run.ID).Error; err != nil {
				return err
			}
			if currentRun.PolicyJSON != run.PolicyJSON {
				requeued = true
				if err := tx.Model(&models.MediaArtifactRun{}).Where("id = ?", run.ID).Updates(map[string]any{
					"status": models.MediaArtifactStatusQueued, "expected_count": 0, "written_count": 0, "updated_count": 0,
					"removed_count": 0, "skipped_count": 0, "failed_count": 0, "error_code": "",
					"cleanup_status": models.MediaArtifactCleanupPending, "cleanup_error_code": "", "cleanup_at": nil,
					"started_at": nil, "finished_at": nil, "updated_at": finished,
				}).Error; err != nil {
					return err
				}
				return tx.Model(&models.MediaLibrary{}).Where("id = ? AND artifact_generation = ?", run.LibraryID, run.Generation).
					Updates(map[string]any{"artifact_status": models.MediaArtifactStatusQueued, "artifact_error": "", "artifact_updated_at": finished}).Error
			}
			var current models.MediaLibrary
			if err := tx.First(&current, run.LibraryID).Error; err != nil {
				return err
			}
			if !artifactPolicyMatchesLibrary(policy, current) {
				superseded = true
				return tx.Model(&models.MediaArtifactRun{}).Where("id = ?", run.ID).Updates(map[string]any{"status": models.MediaArtifactStatusSuperseded, "expected_count": run.ExpectedCount, "written_count": run.WrittenCount, "updated_count": run.UpdatedCount, "skipped_count": run.SkippedCount, "failed_count": run.FailedCount, "error_code": "", "cleanup_status": models.MediaArtifactCleanupSkipped, "cleanup_at": finished, "finished_at": finished, "updated_at": finished}).Error
			}
			if policy.ScopeVersion != 1 {
				if err := tx.Model(&models.MediaArtifact{}).Where("library_id = ? AND target_kind = ? AND run_id <> ? AND active = ?", run.LibraryID, policy.TargetKind, run.ID, true).Updates(map[string]any{"active": false, "updated_at": finished}).Error; err != nil {
					return err
				}
			} else if len(policy.DeletedManifestIDs) > 0 {
				if err := tx.Model(&models.MediaArtifact{}).Where("library_id = ? AND id IN ? AND managed = ? AND target_kind = ? AND run_id <> ?", run.LibraryID, policy.DeletedManifestIDs, true, policy.TargetKind, run.ID).Updates(map[string]any{"active": false, "updated_at": finished}).Error; err != nil {
					return err
				}
			}
			if err := tx.Model(&models.MediaArtifactRun{}).Where("id = ?", run.ID).Updates(map[string]any{"status": status, "expected_count": run.ExpectedCount, "written_count": run.WrittenCount, "updated_count": run.UpdatedCount, "skipped_count": run.SkippedCount, "failed_count": run.FailedCount, "error_code": code, "cleanup_status": models.MediaArtifactCleanupPending, "cleanup_error_code": "", "cleanup_at": nil, "finished_at": finished, "updated_at": finished}).Error; err != nil {
				return err
			}
			result := tx.Model(&models.MediaLibrary{}).Where("id = ? AND artifact_generation = ?", run.LibraryID, run.Generation).Updates(map[string]any{"artifact_status": status, "artifact_error": code, "artifact_updated_at": finished, "artifact_applied_generation": run.Generation})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 && policy.ScopeVersion != 1 {
				return errors.New("artifact generation changed during completion")
			}
			return nil
		}
		if err := tx.Model(&models.MediaArtifactRun{}).Where("id = ?", run.ID).Updates(map[string]any{"status": status, "expected_count": run.ExpectedCount, "written_count": run.WrittenCount, "updated_count": run.UpdatedCount, "skipped_count": run.SkippedCount, "failed_count": run.FailedCount, "error_code": code, "cleanup_status": models.MediaArtifactCleanupSkipped, "cleanup_at": finished, "finished_at": finished, "updated_at": finished}).Error; err != nil {
			return err
		}
		return tx.Model(&models.MediaLibrary{}).Where("id = ? AND artifact_generation = ?", run.LibraryID, run.Generation).Updates(map[string]any{"artifact_status": status, "artifact_error": code, "artifact_updated_at": finished}).Error
	}); err != nil {
		return WorkerResult{ErrorCode: "artifact_state_persist_failed", ErrorMessage: "媒体产物结果保存失败"}
	}
	if superseded {
		return WorkerResult{}
	}
	if requeued {
		withArtifactScanContext(serverlog.OperationMediaArtifact.Event(s.log.Info()).Uint("library_id", run.LibraryID).Str("task_id", run.ID).Uint64("generation", run.Generation), policy, "artifact_refresh_queued").
			Msg(serverlog.OperationMediaArtifact.Message("识别结果已更新，将继续生成元数据产物"))
		return WorkerResult{}
	}
	if run.FailedCount > 0 {
		next := time.Now().UTC().Add(time.Minute)
		return WorkerResult{RetryAt: &next, ErrorCode: code, ErrorMessage: "部分媒体产物生成失败，将自动重试"}
	}
	return WorkerResult{}
}

// The physical permit spans generation AND automatic cleanup. Only their
// verified durable completion may settle it, before announcing readiness.
func (s *MediaArtifactService) finishArtifactGeneration(ctx context.Context, claim ClaimedJob, permit CatalogPhysicalWritePermit, run models.MediaArtifactRun, policy mediaArtifactPolicy) WorkerResult {
	if policy.CloudEmptyCleanupEnabled {
		if err := s.cleanupCloudEmptyDirectories(ctx, claim, permit, run, policy); err != nil {
			next := time.Now().UTC().Add(time.Minute * time.Duration(1<<min(max(claim.Job.AttemptCount-1, 0), 4)))
			code, message := "cloud_empty_cleanup_failed", "云端空目录清理尚未完成，已保留进度"
			if providerCode, _ := cloudpkg.ErrorInfo(err); providerCode != "" {
				code = providerCode
				if code == cloudpkg.CodeAuthExpired || code == cloudpkg.CodeCookieInvalid {
					next = time.Now().UTC()
					message = "云盘登录凭据已失效，已保存进度并等待更新凭据"
				}
			}
			return WorkerResult{RetryAt: &next, ErrorCode: code, ErrorMessage: message}
		}
	}
	if s.cleanup != nil && policy.TargetKind != artifactTargetCloudCleanup {
		cleanup := s.cleanup.AutoCleanup(withArtifactCleanupPermit(ctx, permit), run.ID)
		if cleanup.ErrorCode != "" {
			return WorkerResult{ErrorCode: cleanup.ErrorCode, ErrorMessage: "媒体产物清理失败，媒体变更尚未发布"}
		}
	}
	if err := s.catalogArtifactWriteTx(ctx, func(tx *gorm.DB) error {
		if s.cleanup == nil || policy.TargetKind == artifactTargetCloudCleanup {
			now := time.Now().UTC()
			if err := tx.Model(&models.MediaArtifactRun{}).Where("id = ? AND status = ?", run.ID, models.MediaArtifactStatusCompleted).Updates(map[string]any{"cleanup_status": models.MediaArtifactCleanupSkipped, "cleanup_at": now, "updated_at": now}).Error; err != nil {
				return err
			}
		}
		return SettleCatalogPhysicalWriteTx(tx, permit, &claim)
	}); err != nil {
		return WorkerResult{ErrorCode: "artifact_write_settlement_failed", ErrorMessage: "媒体产物文件操作完成确认失败，保留恢复记录"}
	}
	if policy.ScanPartial && policy.ScopeVersion != 1 {
		return WorkerResult{}
	}
	return s.publishGenerationReady(run.LibraryID, run.Generation, policy)
}

func mediaLibraryRequiresArtifacts(storageType string, library models.MediaLibrary, available bool) bool {
	if !available {
		return false
	}
	if storageType == models.StorageTypeLocal {
		return library.MetadataArtifactsEnabled
	}
	return library.STRMEnabled && library.SignedProxyEnabled
}

func mediaLibraryRequiresCloudCleanup(library models.MediaLibrary, run models.MediaLibraryScanRun, available bool) bool {
	if !available || !library.CloudEmptyCleanupEnabled {
		return false
	}
	var checkpoint struct {
		Directories []cloudCleanupDirectory `json:"cloud_cleanup_directories"`
	}
	return json.Unmarshal([]byte(run.CheckpointJSON), &checkpoint) == nil && len(checkpoint.Directories) > 0
}

func (s *MediaArtifactService) publishGenerationReady(libraryID uint, generation uint64, policy mediaArtifactPolicy) WorkerResult {
	if s.changes == nil {
		return WorkerResult{}
	}
	if policy.ScanPartial && policy.ScopeVersion != 1 {
		return WorkerResult{}
	}
	var readied []models.MediaLibraryChange
	if err := s.catalogArtifactWriteTx(context.Background(), func(tx *gorm.DB) error {
		var library models.MediaLibrary
		if err := tx.First(&library, libraryID).Error; err != nil {
			return err
		}
		if policy.ScopeVersion == 1 {
			var completed models.MediaArtifactRun
			if err := legacyArtifactRunScopeQuery(tx.Where("library_id = ? AND generation = ? AND status = ?", libraryID, generation, models.MediaArtifactStatusCompleted), policy).First(&completed).Error; err != nil {
				return err
			}
			if !artifactPolicyMatchesLibrary(policy, library) {
				return ErrCatalogFence
			}
		} else if library.ArtifactGeneration != generation || library.ArtifactAppliedGeneration != generation || library.ArtifactStatus != models.MediaArtifactStatusCompleted {
			return ErrCatalogFence
		}
		if policy.CatalogBindingID != "" {
			if err := s.validateArtifactBindingTx(tx, policy, nil, nil, false); err != nil {
				return err
			}
			var binding models.CatalogArtifactBinding
			if err := tx.First(&binding, "id = ?", policy.CatalogBindingID).Error; err != nil {
				return err
			}
			if binding.State != "completed" {
				return ErrCatalogFence
			}
		}
		var err error
		if policy.ScopeVersion == 1 {
			readied, err = s.markBatchChangesReadyTx(tx, policy)
		} else {
			readied, err = s.changes.MarkGenerationReadyTx(tx, libraryID, generation)
		}
		return err
	}); err != nil {
		if errors.Is(err, ErrCatalogFence) {
			return WorkerResult{}
		}
		return WorkerResult{ErrorCode: "media_change_ready_failed", ErrorMessage: "媒体变更发布失败"}
	}
	if len(readied) > 0 {
		s.changes.NotifyCommitted(libraryID, readied[len(readied)-1].Revision)
	}
	return WorkerResult{}
}

func artifactPolicyMatchesLibrary(policy mediaArtifactPolicy, library models.MediaLibrary) bool {
	if policy.CloudEmptyCleanupEnabled && !library.CloudEmptyCleanupEnabled {
		return false
	}
	if library.StorageID != policy.StorageID || (policy.ScopeVersion != 1 && library.ArtifactGeneration != policy.Generation) {
		return false
	}
	switch policy.TargetKind {
	case artifactTargetCloudCleanup:
		return library.Enabled && library.CloudEmptyCleanupEnabled && policy.CloudEmptyCleanupEnabled && len(policy.CloudCleanupDirectories) > 0
	case models.MediaArtifactTargetLocalProjection:
		if !library.Enabled || !library.STRMEnabled || !library.SignedProxyEnabled {
			return false
		}
		if policy.ProjectionRootIdentity == "" {
			return storagefs.NormalizeForComparison(library.STRMLocalRoot) == storagefs.NormalizeForComparison(policy.ProjectionRoot)
		}
		_, identity, err := canonicalProjectionRoot(library.STRMLocalRoot)
		return err == nil && identity == policy.ProjectionRootIdentity
	case models.MediaArtifactTargetLocalAdjacent:
		return library.Enabled && library.MetadataArtifactsEnabled
	default:
		return false
	}
}

func (s *MediaArtifactService) artifactPolicySuperseded(policy mediaArtifactPolicy) (bool, error) {
	if s.catalogStore != nil {
		err := s.catalogStore.Read(context.Background(), []uint{policy.LibraryID}, func(reader *CatalogReader) error {
			head, _ := reader.Head(policy.LibraryID)
			if head.Mode == "versioned" && policy.CatalogBindingID == "" {
				return ErrCatalogFence
			}
			if policy.CatalogBindingID != "" {
				return s.validateArtifactBindingTx(reader.tx, policy, nil, nil, false)
			}
			return nil
		})
		if errors.Is(err, ErrCatalogFence) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
	}
	var current models.MediaLibrary
	if err := s.db.First(&current, policy.LibraryID).Error; err != nil {
		return false, err
	}
	if policy.ScopeVersion == 1 {
		fingerprint, err := artifactBatchSourceFingerprint(s.db, current)
		if err != nil {
			return false, err
		}
		if fingerprint != policy.BatchSourceFingerprint {
			return true, nil
		}
	}
	return !artifactPolicyMatchesLibrary(policy, current), nil
}

func automaticCleanupScanKind(kind string) bool {
	switch kind {
	case "initial", "catch_up", "manual", "event", "incremental", "full", "strm_incremental_manual", "strm_full_manual", "reorganization":
		return true
	default:
		return false
	}
}

func canonicalProjectionRoot(root string) (string, string, error) {
	canonical, err := (storagefs.LocalDriver{}).CanonicalizeRoot(root)
	if err != nil {
		return "", "", err
	}
	resolved, err := filepath.EvalSymlinks(canonical)
	if err != nil {
		return "", "", err
	}
	resolved, err = (storagefs.LocalDriver{}).CanonicalizeRoot(resolved)
	if err != nil {
		return "", "", err
	}
	return canonical, storagefs.NormalizeForComparison(resolved), nil
}

func (s *MediaArtifactService) writeSourceAsset(ctx context.Context, root string, run models.MediaArtifactRun, policy mediaArtifactPolicy, asset models.MediaLibrarySourceAsset, manifest *artifactManifestIndex) (string, error) {
	extension := strings.ToLower(strings.TrimSpace(asset.Extension))
	bareExtension := strings.TrimPrefix(extension, ".")
	if len(policy.AssetExtensions) == 0 {
		policy.AssetExtensions = effectiveSourceAssetExtensions(nil)
	}
	allowed := false
	for _, candidate := range policy.AssetExtensions {
		if candidate == bareExtension {
			allowed = true
			break
		}
	}
	if !allowed {
		return "skipped", nil
	}
	limit := int64(10 << 20)
	kind := models.MediaArtifactKindSourceAsset
	switch extension {
	case ".srt", ".ssa", ".ass":
		kind = models.MediaArtifactKindSubtitle
	case ".jpg":
		limit, kind = 20<<20, models.MediaArtifactKindImage
	}
	if asset.Size <= 0 || asset.Size > limit || strings.TrimSpace(asset.ProviderID) == "" || policy.ConnectionID == 0 || s.connections == nil {
		return "", errors.New("source asset facts are invalid")
	}
	_, driver, err := s.connections.driver(policy.ConnectionID)
	if err != nil {
		return "", err
	}
	item, err := driver.Stat(ctx, asset.ProviderID)
	if err != nil {
		return "", err
	}
	if item.ID != asset.ProviderID || item.IsDir || item.Size <= 0 || item.Size > limit || (asset.ParentProviderID != "" && item.ParentID != asset.ParentProviderID) {
		return "", errors.New("source asset identity changed")
	}
	const userAgent = "OhMyCine-Artifact/1.0"
	temporary, err := driver.DirectURL(ctx, cloudpkg.DirectURLRequest{FileID: item.ID, PickCode: item.PickCode, UserAgent: userAgent})
	if err != nil {
		return "", err
	}
	if len(temporary.Headers) != 0 {
		return "", errors.New("source asset requires unsupported upstream headers")
	}
	parsed, err := url.Parse(temporary.URL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil {
		return "", errors.New("source asset URL is invalid")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return "", errors.New("source asset request is invalid")
	}
	request.Header.Set("Accept", "*/*")
	request.Header.Set("User-Agent", userAgent)
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return "", errors.New("source asset download failed")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK || response.ContentLength > limit {
		return "", errors.New("source asset response is invalid")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil || len(body) == 0 || int64(len(body)) > limit {
		return "", errors.New("source asset body is invalid")
	}
	if extension == ".jpg" {
		contentType := strings.ToLower(strings.TrimSpace(strings.Split(response.Header.Get("Content-Type"), ";")[0]))
		if contentType != "image/jpeg" || len(body) < 3 || body[0] != 0xff || body[1] != 0xd8 || body[2] != 0xff {
			return "", errors.New("source asset JPEG is invalid")
		}
	} else if kind == models.MediaArtifactKindSubtitle && bytes.IndexByte(body, 0) >= 0 {
		return "", errors.New("source asset contains binary data")
	}
	relative := filepath.ToSlash(filepath.Clean(filepath.FromSlash(strings.TrimPrefix(asset.RelativePath, "/"))))
	if relative == "." || relative == ".." || strings.HasPrefix(relative, "../") || !strings.EqualFold(filepath.Ext(relative), extension) {
		return "", errors.New("source asset path is invalid")
	}
	return s.writeLocalArtifact(root, run, localArtifactSpec{SourceFingerprint: artifactAssetSourceFingerprint(asset), SourceIdentity: fmt.Sprintf("asset:%d", asset.ID), ProviderItemID: asset.ProviderID, Kind: kind, TargetKind: models.MediaArtifactTargetLocalProjection, RelativePath: "/" + relative, Manifest: manifest, Content: func(models.MediaArtifact) ([]byte, error) { return body, nil }})
}

func (s *MediaArtifactService) writeNFO(ctx context.Context, root string, run models.MediaArtifactRun, targetKind string, recognition models.MediaLibraryRecognition, entries []models.MediaLibraryEntry, manifest *artifactManifestIndex) (string, error) {
	if len(entries) == 0 {
		return "skipped", nil
	}
	_, snapshot, err := decodeRecognitionMetadata(recognition.MetadataJSON)
	if err != nil {
		return "", err
	}
	content, err := nfo.Render(snapshot)
	if errors.Is(err, nfo.ErrSnapshotIncomplete) {
		return "skipped", nil
	}
	if err != nil {
		return "", err
	}
	relative, err := nfoRelativePath(snapshot.MediaType, entries)
	if err != nil {
		return "", err
	}
	outcome, err := s.writeLocalArtifact(root, run, localArtifactSpec{SourceIdentity: fmt.Sprintf("recognition:%d", recognition.ID), Kind: models.MediaArtifactKindNFO, TargetKind: targetKind, RelativePath: relative, Manifest: manifest, Content: func(models.MediaArtifact) ([]byte, error) { return content, nil }})
	if err != nil {
		return outcome, err
	}
	images := nfo.Images(snapshot)
	if len(images) == 0 {
		return outcome, nil
	}
	if s.metadata == nil {
		return outcome, errors.New("metadata image client is unavailable")
	}
	client, _, _, err := s.metadata.clientWithCredentialInfo()
	if err != nil {
		return outcome, err
	}
	combined := outcome
	for _, image := range images {
		imageRelative, imageKind, err := nfoImageRelativePath(relative, image)
		if err != nil {
			return combined, err
		}
		limit := int64(10 << 20)
		size := "w780"
		if image.Kind == "fanart" {
			limit, size = 15<<20, "w1280"
		}
		body, err := client.DownloadJPEG(ctx, image.TMDBPath, size, limit)
		if err != nil {
			return combined, err
		}
		season := ""
		if image.SeasonNumber != nil {
			season = fmt.Sprintf(":%d", *image.SeasonNumber)
		}
		imageOutcome, err := s.writeLocalArtifact(root, run, localArtifactSpec{SourceIdentity: fmt.Sprintf("recognition:%d:%s%s", recognition.ID, image.Kind, season), Kind: imageKind, TargetKind: targetKind, RelativePath: imageRelative, Manifest: manifest, Content: func(models.MediaArtifact) ([]byte, error) { return body, nil }})
		combined = combineArtifactOutcome(combined, imageOutcome)
		if err != nil {
			return combined, err
		}
	}
	return combined, nil
}

func nfoImageRelativePath(nfoRelative string, image nfo.ImageIdentity) (string, string, error) {
	directory := filepath.ToSlash(filepath.Dir(nfoRelative))
	base := strings.TrimSuffix(filepath.Base(nfoRelative), filepath.Ext(nfoRelative))
	switch image.Kind {
	case "poster":
		if base == "tvshow" {
			return filepath.ToSlash(filepath.Join(directory, "poster.jpg")), models.MediaArtifactKindPoster, nil
		}
		return filepath.ToSlash(filepath.Join(directory, base+"-poster.jpg")), models.MediaArtifactKindPoster, nil
	case "fanart":
		if base == "tvshow" {
			return filepath.ToSlash(filepath.Join(directory, "fanart.jpg")), models.MediaArtifactKindFanart, nil
		}
		return filepath.ToSlash(filepath.Join(directory, base+"-fanart.jpg")), models.MediaArtifactKindFanart, nil
	case "season_poster":
		if image.SeasonNumber == nil || *image.SeasonNumber < 0 || *image.SeasonNumber > 10000 {
			return "", "", errors.New("season image identity is invalid")
		}
		return filepath.ToSlash(filepath.Join(directory, fmt.Sprintf("season%02d-poster.jpg", *image.SeasonNumber))), models.MediaArtifactKindPoster, nil
	default:
		return "", "", errors.New("metadata image kind is invalid")
	}
}

func combineArtifactOutcome(left, right string) string {
	if left == "written" || right == "written" {
		return "written"
	}
	if left == "updated" || right == "updated" {
		return "updated"
	}
	if left == "skipped" || right == "skipped" {
		return "skipped"
	}
	return ""
}

func nfoRelativePath(mediaType string, entries []models.MediaLibraryEntry) (string, error) {
	if len(entries) == 0 {
		return "", errors.New("NFO source entries are missing")
	}
	first := filepath.ToSlash(filepath.Clean(filepath.FromSlash(strings.TrimPrefix(entries[0].RelativePath, "/"))))
	if first == "." || first == ".." || strings.HasPrefix(first, "../") {
		return "", errors.New("NFO source path is invalid")
	}
	if mediaType == "movie" {
		extension := filepath.Ext(first)
		if extension == "" {
			return "", errors.New("movie source extension is missing")
		}
		return "/" + strings.TrimSuffix(first, extension) + ".nfo", nil
	}
	if mediaType != "tv" {
		return "", errors.New("NFO media type is invalid")
	}
	directory := filepath.ToSlash(filepath.Dir(first))
	base := strings.ToLower(filepath.Base(directory))
	if strings.HasPrefix(base, "season ") || strings.HasPrefix(base, "season.") || strings.HasPrefix(base, "season_") || strings.HasPrefix(base, "season-") {
		directory = filepath.ToSlash(filepath.Dir(directory))
	}
	if directory == "." {
		directory = ""
	}
	return "/" + strings.Trim(directory+"/tvshow.nfo", "/"), nil
}

func (s *MediaArtifactService) writeSTRM(_ context.Context, root string, run models.MediaArtifactRun, entry models.MediaLibraryEntry, manifest *artifactManifestIndex, verifier signedArtifactVerifier) (string, error) {
	relative, err := strmRelativePath(entry.RelativePath)
	if err != nil {
		return "", err
	}
	return s.writeLocalArtifact(root, run, localArtifactSpec{SourceIdentity: fmt.Sprintf("entry:%d", entry.ID), ProviderItemID: entry.ProviderID, Kind: models.MediaArtifactKindSTRM, TargetKind: models.MediaArtifactTargetLocalProjection, RelativePath: relative, Manifest: manifest, Render: func(artifact models.MediaArtifact, target string) (localArtifactRender, error) {
		if artifact.ID != 0 {
			if content, inspection, ok := s.reusableSTRM(target, artifact, verifier); ok {
				return localArtifactRender{Content: content, ContentExpiresAt: &inspection.ExpiresAt, ContentFormatVersion: inspection.FormatVersion, PreserveExisting: true}, nil
			}
		}
		lease, err := s.signedProxy.signArtifactLease(artifact.OpaqueID, run.LibraryID, proxyDefaultTTL)
		if err != nil {
			return localArtifactRender{}, err
		}
		return localArtifactRender{Content: []byte(lease.URL + "\n"), ContentExpiresAt: &lease.ExpiresAt, ContentFormatVersion: lease.FormatVersion}, nil
	}})
}

const maximumPersistedSTRMBytes = 4096

func (s *MediaArtifactService) reusableSTRM(target string, artifact models.MediaArtifact, verifier signedArtifactVerifier) ([]byte, signedArtifactInspection, bool) {
	info, err := os.Lstat(target)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maximumPersistedSTRMBytes {
		return nil, signedArtifactInspection{}, false
	}
	content, err := os.ReadFile(target)
	if err != nil || len(content) == 0 || len(content) > maximumPersistedSTRMBytes || bytes.IndexByte(content, 0) >= 0 {
		return nil, signedArtifactInspection{}, false
	}
	raw := strings.TrimSpace(string(content))
	if raw == "" || strings.ContainsAny(raw, "\r\n\t ") {
		return nil, signedArtifactInspection{}, false
	}
	inspection, err := s.signedProxy.inspectArtifactLeaseURL(raw, artifact, verifier)
	if err != nil {
		return nil, signedArtifactInspection{}, false
	}
	if !inspection.ExpiresAt.After(s.signedProxy.now().Add(proxyRenewalWindow)) {
		return nil, signedArtifactInspection{}, false
	}
	return content, inspection, true
}

type artifactManifestIndex struct {
	rows             map[string]models.MediaArtifact
	dirty            map[string]struct{}
	beforeWrite      func() error
	reserve          func(*models.MediaArtifact) error
	catalogBindingID string
	lazy             bool
	physical         *artifactPhysicalExecution
}

func newArtifactManifestIndex(capacity int) *artifactManifestIndex {
	return &artifactManifestIndex{rows: make(map[string]models.MediaArtifact, capacity), dirty: make(map[string]struct{}, capacity)}
}

func artifactManifestKey(targetKind, relativePath string) string {
	return targetKind + "\x00" + relativePath
}

type localArtifactRender struct {
	Content              []byte
	ContentExpiresAt     *time.Time
	ContentFormatVersion string
	PreserveExisting     bool
}

type localArtifactSpec struct {
	SourceFingerprint string
	SourceIdentity    string
	ProviderItemID    string
	Kind              string
	TargetKind        string
	RelativePath      string
	Manifest          *artifactManifestIndex
	Content           func(models.MediaArtifact) ([]byte, error)
	Render            func(models.MediaArtifact, string) (localArtifactRender, error)
}

func (s *MediaArtifactService) writeLocalArtifact(root string, run models.MediaArtifactRun, spec localArtifactSpec) (string, error) {
	relative := spec.RelativePath
	var artifact models.MediaArtifact
	key := artifactManifestKey(spec.TargetKind, relative)
	artifact, exists := models.MediaArtifact{}, false
	if spec.Manifest != nil {
		artifact, exists = spec.Manifest.rows[key]
	}
	if spec.Manifest == nil || (!exists && spec.Manifest.lazy) {
		readDB := s.db
		if s.catalogStore != nil && spec.Manifest != nil && spec.Manifest.catalogBindingID != "" {
			readDB = s.catalogStore.readDB
		}
		findErr := readDB.Where("library_id = ? AND target_kind = ? AND relative_path = ?", run.LibraryID, spec.TargetKind, relative).First(&artifact).Error
		exists = findErr == nil
		if findErr != nil && !errors.Is(findErr, gorm.ErrRecordNotFound) {
			return "", findErr
		}
	}
	target, err := storagefs.Constrain(root, filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(relative, "/"))))
	if err != nil {
		return "", err
	}
	if !exists {
		if _, err := os.Lstat(target); err == nil {
			return "skipped", nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		opaque, err := NewArtifactOpaqueID()
		if err != nil {
			return "", err
		}
		artifact = models.MediaArtifact{OpaqueID: opaque, RunID: run.ID, LibraryID: run.LibraryID, SourceIdentity: spec.SourceIdentity, ProviderItemID: spec.ProviderItemID, Kind: spec.Kind, TargetKind: spec.TargetKind, RelativePath: relative, Managed: true, Active: true, Status: models.MediaArtifactStatusQueued, CreatedAt: time.Now().UTC()}
	} else if !artifact.Managed {
		return "skipped", nil
	} else if artifact.Status == models.MediaArtifactStatusCleanup && !artifact.Active {
		return "", errors.New("managed artifact cleanup is in progress")
	}
	beforeArtifact := artifact
	artifact.SourceFingerprint = spec.SourceFingerprint
	artifact.RunID, artifact.SourceIdentity, artifact.ProviderItemID, artifact.Kind, artifact.Active = run.ID, spec.SourceIdentity, spec.ProviderItemID, spec.Kind, true
	if spec.Manifest != nil {
		artifact.CatalogBindingID = spec.Manifest.catalogBindingID
	}
	if !exists && spec.Manifest != nil && spec.Manifest.reserve != nil {
		// Persist managed ownership before physical I/O. A crash after atomic
		// rename must not turn our own generated file into an unmanaged collision.
		if err := spec.Manifest.reserve(&artifact); err != nil {
			return "", err
		}
		beforeArtifact = artifact
	}
	render := localArtifactRender{}
	if spec.Render != nil {
		render, err = spec.Render(artifact, target)
	} else if spec.Content != nil {
		render.Content, err = spec.Content(artifact)
	} else {
		err = errors.New("artifact content renderer is unavailable")
	}
	if err != nil {
		return "", err
	}
	fingerprint := sha256.Sum256(render.Content)
	if spec.Manifest != nil && spec.Manifest.beforeWrite != nil {
		if err := spec.Manifest.beforeWrite(); err != nil {
			return "", err
		}
	}
	fingerprintHex := hex.EncodeToString(fingerprint[:])
	artifact.ContentExpiresAt, artifact.ContentFormatVersion = render.ContentExpiresAt, render.ContentFormatVersion
	// A manifest fingerprint alone is not proof that the managed file is still
	// healthy. Full reconciliation deliberately reaches this writer after it
	// detects damaged bytes; short-circuiting on the database value would leave
	// the corruption in place forever. PreserveExisting is already backed by the
	// renderer's format/signature inspection (STRM), while ordinary content must
	// still match the on-disk digest.
	contentAlreadyPresent := render.PreserveExisting || (artifact.ContentFingerprint == fingerprintHex && artifactFileMatches(root, artifact))
	if exists && contentAlreadyPresent {
		if info, err := os.Stat(target); err == nil && !info.IsDir() {
			artifact.ContentFingerprint = fingerprintHex
			artifact.Status, artifact.ErrorCode, artifact.UpdatedAt = models.MediaArtifactStatusCompleted, "", time.Now().UTC()
			if spec.Manifest != nil && spec.Manifest.physical != nil {
				prepared, err := s.prepareArtifactPhysicalWrite(root, beforeArtifact, artifact, render.Content, *spec.Manifest.physical)
				if err != nil {
					return "", err
				}
				if err := s.reconcileArtifactPhysicalWrite(root, prepared, *spec.Manifest.physical); err != nil {
					return "", err
				}
			}
			if spec.Manifest != nil {
				spec.Manifest.rows[key] = artifact
				if spec.Manifest.physical == nil {
					spec.Manifest.dirty[key] = struct{}{}
				}
			} else if err := s.db.Save(&artifact).Error; err != nil {
				return "", err
			}
			return "skipped", nil
		}
	}
	var receipt *models.CatalogArtifactWriteReceipt
	afterArtifact := artifact
	afterArtifact.ContentFingerprint, afterArtifact.Status, afterArtifact.ErrorCode, afterArtifact.UpdatedAt = fingerprintHex, models.MediaArtifactStatusCompleted, "", time.Now().UTC()
	if spec.Manifest != nil && spec.Manifest.physical != nil {
		prepared, err := s.prepareArtifactPhysicalWrite(root, beforeArtifact, afterArtifact, render.Content, *spec.Manifest.physical)
		if err != nil {
			return "", err
		}
		receipt = &prepared
	}
	if err := atomicWriteArtifact(root, target, render.Content); err != nil {
		if receipt != nil {
			// The durable prepared before/after intent, not an in-memory failed
			// manifest overwrite, owns recovery after physical mutation starts.
			return "", err
		}
		artifact.Status, artifact.ErrorCode, artifact.UpdatedAt = models.MediaArtifactStatusFailed, "artifact_write_failed", time.Now().UTC()
		if spec.Manifest != nil {
			spec.Manifest.rows[key] = artifact
			spec.Manifest.dirty[key] = struct{}{}
		} else {
			_ = s.db.Save(&artifact).Error
		}
		return "", err
	}
	if receipt != nil {
		if err := s.reconcileArtifactPhysicalWrite(root, *receipt, *spec.Manifest.physical); err != nil {
			return "", err
		}
	}
	artifact = afterArtifact
	if spec.Manifest != nil {
		spec.Manifest.rows[key] = artifact
		if spec.Manifest.physical == nil {
			spec.Manifest.dirty[key] = struct{}{}
		}
	} else if err := s.db.Save(&artifact).Error; err != nil {
		return "", err
	}
	if exists {
		return "updated", nil
	}
	return "written", nil
}

func (s *MediaArtifactService) persistArtifactManifest(manifest *artifactManifestIndex) error {
	if manifest == nil || len(manifest.dirty) == 0 {
		return nil
	}
	keys := make([]string, 0, len(manifest.dirty))
	for key := range manifest.dirty {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	existing := make([]models.MediaArtifact, 0, len(keys))
	created := make([]models.MediaArtifact, 0, len(keys))
	for _, key := range keys {
		artifact := manifest.rows[key]
		if artifact.ID == 0 {
			created = append(created, artifact)
		} else {
			existing = append(existing, artifact)
		}
	}
	return s.db.Transaction(func(tx *gorm.DB) error {
		if len(existing) > 0 {
			if err := tx.Save(&existing).Error; err != nil {
				return err
			}
		}
		if len(created) > 0 {
			if err := tx.CreateInBatches(&created, 100).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

func strmRelativePath(source string) (string, error) {
	source = filepath.ToSlash(filepath.Clean(filepath.FromSlash(strings.TrimPrefix(strings.TrimSpace(source), "/"))))
	if source == "." || source == ".." || strings.HasPrefix(source, "../") {
		return "", errors.New("STRM source path is invalid")
	}
	extension := strings.ToLower(filepath.Ext(source))
	if extension == ".iso" {
		return "/" + source + ".strm", nil
	}
	if extension == "" {
		return "", errors.New("STRM source extension is missing")
	}
	return "/" + strings.TrimSuffix(source, filepath.Ext(source)) + ".strm", nil
}

func atomicWriteArtifact(root, target string, content []byte) error {
	parent := filepath.Dir(target)
	if err := ensureSafeProjectionDirectory(root, parent); err != nil {
		return err
	}
	if _, err := (storagefs.LocalDriver{}).CanonicalizeRoot(parent); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(parent, ".omc-artifact-*.tmp")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer func() { _ = os.Remove(temporaryName) }()
	if _, err := bytes.NewReader(content).WriteTo(temporary); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if _, err := (storagefs.LocalDriver{}).CanonicalizeRoot(parent); err != nil {
		return err
	}
	if info, err := os.Lstat(target); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return errors.New("managed artifact target became a symlink")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return replaceArtifactFile(temporaryName, target)
}

func ensureSafeProjectionDirectory(root, parent string) error {
	relative, err := filepath.Rel(root, parent)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return errors.New("artifact target escapes projection root")
	}
	current := root
	if relative == "." {
		return nil
	}
	for _, part := range strings.Split(relative, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		if err := os.Mkdir(current, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		if _, err := (storagefs.LocalDriver{}).CanonicalizeRoot(current); err != nil {
			return err
		}
	}
	return nil
}
