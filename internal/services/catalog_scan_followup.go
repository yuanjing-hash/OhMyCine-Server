package services

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// EnableCatalogScanFollowups installs the compact production hook. This does
// not convert a library or enable the new catalog storage format.
func (s *MediaLibraryService) EnableCatalogScanFollowups() {
	s.SetCatalogScanCommit(s.commitCatalogScanFollowupTx)
}

func (s *MediaLibraryService) commitCatalogScanFollowupTx(tx *gorm.DB, p CatalogScanPublication) error {
	if s.catalogStore == nil || s.changes == nil || p.Head.Mode != "versioned" || p.Run.LibraryID != p.Head.LibraryID {
		return ErrCatalogInvalid
	}
	var library models.MediaLibrary
	if err := tx.First(&library, p.Head.LibraryID).Error; err != nil {
		return err
	}
	var storage models.Storage
	if err := tx.First(&storage, library.StorageID).Error; err != nil {
		return err
	}
	sourceConfig, err := catalogFollowupSourceFingerprint(tx, library, storage)
	if err != nil {
		return err
	}
	cloudCleanup := mediaLibraryRequiresCloudCleanup(library, p.Run, s.artifacts != nil)
	requiresArtifacts := mediaLibraryRequiresArtifacts(storage.Type, library, s.artifacts != nil) || cloudCleanup
	// Initial and explicit STRM full scans establish policy. An unchanged
	// routine full scan is not a request to rebuild or audit every artifact.
	fullArtifactReconcile := !p.RecognitionOnly && (p.Run.Kind == "initial" || p.Run.Kind == "strm_full_manual" || (p.Run.Kind == "full" && !p.NoContentChange))
	generate := requiresArtifacts && (fullArtifactReconcile || mediaLibraryArtifactGenerationRequired(p.Run.Kind, p.Run, p.MetadataChanged) || len(p.ArtifactChanges.Manifests) > 0)
	if p.ArtifactChanges.Empty() && !fullArtifactReconcile {
		generate = false
	}
	if p.NoContentChange && !fullArtifactReconcile {
		generate = false
	}
	// A metadata worker's final no-op must not create another binding for a
	// generation it just finished. Earlier changed batches already bound it.
	if p.RecognitionOnly && p.NoContentChange {
		generate = false
	}
	if cloudCleanup && !p.NoContentChange && !p.RecognitionOnly {
		generate = true
	}
	artifactGeneration := max(p.Run.Generation, max(library.DirtyGeneration, library.ArtifactGeneration))
	if generate && p.Run.Kind == "strm_full_manual" && p.NoContentChange {
		artifactGeneration++
		if err := tx.Model(&models.MediaLibrary{}).Where("id = ?", library.ID).Update("dirty_generation", artifactGeneration).Error; err != nil {
			return err
		}
		library.DirtyGeneration = artifactGeneration
	}
	if generate {
		var bindErr error
		if fullArtifactReconcile {
			_, bindErr = s.artifacts.BindCatalogGenerationTx(tx, library.ID, artifactGeneration)
		} else {
			_, bindErr = s.artifacts.BindCatalogGenerationChangesTx(tx, library.ID, artifactGeneration, p.ArtifactChanges)
		}
		if bindErr != nil {
			return bindErr
		}
	}
	if !p.NoContentChange {
		kind := models.MediaLibraryChangeCatalog
		if p.RecognitionOnly {
			kind = models.MediaLibraryChangeMetadata
		}
		changeGeneration := p.Run.Generation
		if requiresArtifacts {
			changeGeneration = artifactGeneration
		}
		if _, err := s.changes.RecordTx(tx, library.ID, changeGeneration, kind, !requiresArtifacts); err != nil {
			return err
		}
	}
	var previous models.CatalogScanFollowup
	err = tx.First(&previous, "library_id = ?", library.ID).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	sameSource := err == nil && previous.SourceEpoch == p.Head.SourceEpoch && previous.SourceFingerprint == p.Head.SourceFingerprint && previous.ConfigFingerprint == p.Head.ConfigFingerprint && previous.SourceConfigFingerprint == sourceConfig
	complete := !p.Run.Partial || (sameSource && previous.CompleteObserved)
	row := models.CatalogScanFollowup{
		LibraryID: library.ID, ReceiptID: uuid.NewString(), ScanRunID: p.Run.ID, Generation: p.Run.Generation,
		ArtifactGeneration: artifactGeneration,
		SourceEpoch:        p.Head.SourceEpoch, SourceFingerprint: p.Head.SourceFingerprint, ConfigFingerprint: p.Head.ConfigFingerprint,
		SourceConfigFingerprint: sourceConfig,
		RecognitionPending:      !p.RecognitionOnly && p.Run.RecognitionCompleted < p.Run.RecognitionTotal,
		ArtifactPending:         generate || (sameSource && previous.ArtifactGeneration == artifactGeneration && previous.ArtifactPending),
		ArtworkPending:          s.libraryArtwork != nil && (!p.NoContentChange || (sameSource && previous.ArtworkPending)),
		DiagnosisPending:        s.structure != nil && complete,
		CompleteObserved:        complete, UpdatedAt: time.Now().UTC(),
	}
	// Full-scan evidence is kept in this single row even after delivery. Routine
	// partial publications cannot lose the once-per-source diagnosis intent.
	return tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "library_id"}}, UpdateAll: true}).Create(&row).Error
}

func (s *MediaLibraryService) validateCatalogFollowupTx(tx *gorm.DB, row models.CatalogScanFollowup) (models.MediaLibrary, models.MediaLibraryScanRun, error) {
	var library models.MediaLibrary
	var run models.MediaLibraryScanRun
	var current models.CatalogScanFollowup
	if err := tx.First(&current, "library_id = ?", row.LibraryID).Error; err != nil {
		return library, run, err
	}
	if current.ReceiptID != row.ReceiptID {
		return library, run, ErrCatalogFence
	}
	var head models.CatalogHead
	if err := tx.First(&head, "library_id = ?", row.LibraryID).Error; err != nil {
		return library, run, err
	}
	if head.Mode != "versioned" || head.SourceEpoch != row.SourceEpoch || head.SourceFingerprint != row.SourceFingerprint || head.ConfigFingerprint != row.ConfigFingerprint {
		return library, run, ErrCatalogFence
	}
	if err := tx.First(&library, row.LibraryID).Error; err != nil {
		return library, run, err
	}
	if !library.Enabled || library.BaselineGeneration != row.Generation {
		return library, run, ErrCatalogFence
	}
	var storage models.Storage
	if err := tx.First(&storage, library.StorageID).Error; err != nil {
		return library, run, err
	}
	fingerprint, err := catalogFollowupSourceFingerprint(tx, library, storage)
	if err != nil {
		return library, run, err
	}
	if !storage.Enabled || fingerprint != row.SourceConfigFingerprint {
		return library, run, ErrCatalogFence
	}
	if err := tx.First(&run, row.ScanRunID).Error; err != nil {
		return library, run, err
	}
	if run.LibraryID != row.LibraryID || run.Generation != row.Generation || run.CatalogPublishedAt == nil {
		return library, run, ErrCatalogFence
	}
	return library, run, nil
}

func catalogFollowupSourceFingerprint(tx *gorm.DB, library models.MediaLibrary, storage models.Storage) (string, error) {
	var profile models.MediaClassificationProfile
	if err := tx.First(&profile, library.ProfileID).Error; err != nil {
		return "", err
	}
	// Metadata edits and no-op scans advance dirty_generation without replacing
	// the source. BaselineGeneration and receipt CAS independently fence the run.
	library.DirtyGeneration = 0
	return mediaLibraryScanSourceFingerprint(library, storage, profile), nil
}

func acknowledgeCatalogFollowupTx(tx *gorm.DB, row models.CatalogScanFollowup, flag string) error {
	switch flag {
	case "recognition_pending", "artifact_pending", "artwork_pending", "diagnosis_pending":
	default:
		return ErrCatalogInvalid
	}
	result := tx.Model(&models.CatalogScanFollowup{}).Where("library_id = ? AND receipt_id = ?", row.LibraryID, row.ReceiptID).Update(flag, false)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrCatalogFence
	}
	return nil
}

func (s *MediaLibraryService) acknowledgeCatalogFollowup(ctx context.Context, row models.CatalogScanFollowup, flag string) error {
	return s.catalogStore.writeCatalogBatch(ctx, func(tx *gorm.DB) error {
		return acknowledgeCatalogFollowupTx(tx, row, flag)
	})
}

func (s *MediaLibraryService) dispatchCatalogRecognition(ctx context.Context, row models.CatalogScanFollowup) error {
	if s.queue == nil {
		return ErrCatalogInvalid
	}
	var library models.MediaLibrary
	var run models.MediaLibraryScanRun
	var existing models.Job
	key := "generation:" + strconv.FormatUint(row.Generation, 10)
	err := s.catalogStore.writeCatalogBatch(ctx, func(tx *gorm.DB) error {
		var err error
		library, run, err = s.validateCatalogFollowupTx(tx, row)
		if err != nil {
			return err
		}
		// Any existing occurrence owns its retry/pause/cancel policy, including
		// terminal failures. A recovery poll must never revive or bump that Job.
		err = tx.Where("job_type = ? AND resource_key = ? AND coalescing_key = ?", JobTypeMediaLibraryRecognition, mediaArtifactResourceKey(row.LibraryID), key).Order("created_at DESC").First(&existing).Error
		if err == nil {
			return acknowledgeCatalogFollowupTx(tx, row, "recognition_pending")
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		return nil
	})
	if err != nil || existing.ID != "" {
		return err
	}
	return s.catalogStore.Admission().WithBackground(ctx, func() error {
		_, err := s.queue.EnqueueLatestWith(EnqueueJobInput{System: true, JobType: JobTypeMediaLibraryRecognition, Priority: 20,
			DisplayName: "媒体库后台识别 · " + safeMediaDisplayName(library.Name), Provider: "media_library", ResourceKey: mediaArtifactResourceKey(library.ID), CoalescingKey: key,
			Payload: mediaLibraryRecognitionJobPayload{LibraryID: library.ID, ScanRunID: run.ID, Generation: run.Generation},
		}, func(tx *gorm.DB, job models.Job) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if _, _, err := s.validateCatalogFollowupTx(tx, row); err != nil {
				return err
			}
			if job.Generation != 1 {
				return ErrCatalogFence
			}
			return acknowledgeCatalogFollowupTx(tx, row, "recognition_pending")
		})
		return err
	})
}

// RecoverCatalogScanFollowups executes bounded publication receipts outside the
// scan writer. Artwork runs synchronously in this dedicated lifecycle worker,
// so a process loss cannot acknowledge an in-memory-only artwork queue.
func (s *MediaLibraryService) RecoverCatalogScanFollowups(ctx context.Context, limit int) error {
	if s.catalogStore == nil {
		return nil
	}
	if limit < 1 || limit > 100 {
		return ErrCatalogBudget
	}
	s.catalogFollowupMu.Lock()
	defer s.catalogFollowupMu.Unlock()
	if err := s.supersedeObsoleteCatalogJobs(ctx, limit); err != nil {
		return err
	}
	var rows []models.CatalogScanFollowup
	if err := s.catalogStore.readDB.WithContext(ctx).Where("recognition_pending = 1 OR artifact_pending = 1 OR artwork_pending = 1 OR diagnosis_pending = 1").Order("updated_at,library_id").Limit(limit).Find(&rows).Error; err != nil {
		return err
	}
	var failures []error
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := s.recoverCatalogScanFollowup(ctx, row)
		if errors.Is(err, ErrCatalogFence) || errors.Is(err, gorm.ErrRecordNotFound) {
			// Clear only this obsolete receipt. A newer publication remains intact.
			err = s.catalogStore.writeCatalogBatch(ctx, func(tx *gorm.DB) error {
				return tx.Model(&models.CatalogScanFollowup{}).Where("library_id = ? AND receipt_id = ?", row.LibraryID, row.ReceiptID).Updates(map[string]any{"recognition_pending": false, "artifact_pending": false, "artwork_pending": false, "diagnosis_pending": false}).Error
			})
		} else {
			// Rotate both failures and convergence waits so the next bounded pass
			// reaches siblings even while recognition is still running.
			rotateErr := s.catalogStore.writeCatalogBatch(ctx, func(tx *gorm.DB) error {
				return tx.Model(&models.CatalogScanFollowup{}).Where("library_id = ? AND receipt_id = ?", row.LibraryID, row.ReceiptID).Update("updated_at", time.Now().UTC()).Error
			})
			err = errors.Join(err, rotateErr)
		}
		if err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

const catalogGenerationSupersededCode = "catalog_generation_superseded"

// supersedeObsoleteCatalogJobs drains only queue rows with complete no-I/O
// evidence. Running/retry/paused rows and queued artifact owners that are not
// still admitted are intentionally left for their ordinary worker recovery.
func (s *MediaLibraryService) supersedeObsoleteCatalogJobs(ctx context.Context, limit int) error {
	if s.queue == nil || s.catalogStore == nil || limit < 1 {
		return nil
	}
	var cancelled []models.Job
	err := s.catalogStore.writeCatalogBatch(ctx, func(tx *gorm.DB) error {
		var jobs []models.Job
		if err := tx.Where("job_type IN ? AND status = ? AND lease_token_hash = '' AND lease_expires_at IS NULL AND started_generation = 0 AND interrupt_status = ''", []string{JobTypeMediaLibraryRecognition, JobTypeMediaArtifact}, models.JobStatusQueued).
			Order("created_at,id").Limit(limit).Find(&jobs).Error; err != nil {
			return err
		}
		now := time.Now().UTC()
		for _, job := range jobs {
			proven, err := s.supersedeObsoleteCatalogProjectionTx(tx, job, now)
			if err != nil {
				return err
			}
			if !proven {
				continue
			}
			updates := map[string]any{
				"status":             models.JobStatusCancelled,
				"revision":           job.Revision + 1,
				"finished_at":        now,
				"last_error_code":    catalogGenerationSupersededCode,
				"last_error_message": "已由当前媒体库 generation 接管",
				"cancellation_asked": false,
				"updated_at":         now,
			}
			releaseLease(updates)
			changed := tx.Model(&models.Job{}).Where("id = ? AND status = ? AND revision = ? AND lease_token_hash = '' AND lease_expires_at IS NULL AND started_generation = 0", job.ID, models.JobStatusQueued, job.Revision).Updates(updates)
			if changed.Error != nil {
				return changed.Error
			}
			if changed.RowsAffected != 1 {
				return ErrCatalogFence
			}
			if err := recordJobEvent(tx, job.ID, "catalog.generation_superseded", models.JobStatusQueued, models.JobStatusCancelled, nil, catalogGenerationSupersededCode, now); err != nil {
				return err
			}
			job.Status, job.LastErrorCode, job.LastErrorMessage = models.JobStatusCancelled, catalogGenerationSupersededCode, "已由当前媒体库 generation 接管"
			job.Revision++
			job.FinishedAt, job.UpdatedAt = &now, now
			cancelled = append(cancelled, job)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, job := range cancelled {
		s.queue.publish(job, "job.status_changed")
	}
	return nil
}

func (s *MediaLibraryService) supersedeObsoleteCatalogProjectionTx(tx *gorm.DB, job models.Job, now time.Time) (bool, error) {
	switch job.JobType {
	case JobTypeMediaLibraryRecognition:
		var payload mediaLibraryRecognitionJobPayload
		if json.Unmarshal([]byte(job.PayloadJSON), &payload) != nil || payload.LibraryID == 0 || payload.ScanRunID == 0 || payload.Generation == 0 {
			return false, nil
		}
		var library models.MediaLibrary
		if err := tx.Select("id", "baseline_generation").First(&library, payload.LibraryID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return false, nil
			}
			return false, err
		}
		if payload.Generation >= library.BaselineGeneration {
			return false, nil
		}
		var run models.MediaLibraryScanRun
		if err := tx.First(&run, "id = ? AND library_id = ? AND generation = ?", payload.ScanRunID, payload.LibraryID, payload.Generation).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return false, nil
			}
			return false, err
		}
		if run.Status != "catalog_ready" {
			return false, nil
		}
		changed := tx.Model(&models.MediaLibraryScanRun{}).Where("id = ? AND status = ? AND generation = ?", run.ID, "catalog_ready", run.Generation).
			Updates(map[string]any{"status": "superseded", "phase": "superseded", "error_code": catalogGenerationSupersededCode, "finished_at": now})
		return changed.RowsAffected == 1, changed.Error
	case JobTypeMediaArtifact:
		var payload mediaArtifactJobPayload
		if json.Unmarshal([]byte(job.PayloadJSON), &payload) != nil || payload.ArtifactRunID == "" {
			return false, nil
		}
		var run models.MediaArtifactRun
		if err := tx.First(&run, "id = ?", payload.ArtifactRunID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return false, nil
			}
			return false, err
		}
		if run.JobID == nil || *run.JobID != job.ID || run.Status != models.MediaArtifactStatusQueued {
			return false, nil
		}
		var library models.MediaLibrary
		if err := tx.Select("id", "artifact_generation").First(&library, run.LibraryID).Error; err != nil {
			return false, err
		}
		if library.ArtifactGeneration == 0 || run.Generation >= library.ArtifactGeneration {
			return false, nil
		}
		var owner models.CatalogPhysicalWrite
		if err := tx.Where("owner_kind = ? AND owner_id = ? AND library_id = ? AND job_id = ?", CatalogPhysicalArtifact, run.ID, run.LibraryID, job.ID).First(&owner).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return false, nil
			}
			return false, err
		}
		if owner.State != "admitted" {
			return false, nil
		}
		var policy mediaArtifactPolicy
		if err := json.Unmarshal([]byte(run.PolicyJSON), &policy); err != nil || policy.LibraryID != run.LibraryID || policy.Generation != run.Generation {
			return false, nil
		}
		changed := tx.Model(&models.MediaArtifactRun{}).Where("id = ? AND job_id = ? AND status = ? AND generation = ?", run.ID, job.ID, models.MediaArtifactStatusQueued, run.Generation).
			Updates(map[string]any{"status": models.MediaArtifactStatusSuperseded, "cleanup_status": models.MediaArtifactCleanupSkipped, "cleanup_at": now, "finished_at": now, "error_code": catalogGenerationSupersededCode, "updated_at": now})
		if changed.Error != nil || changed.RowsAffected != 1 {
			return false, changed.Error
		}
		if policy.CatalogBindingID != "" {
			var binding models.CatalogArtifactBinding
			if err := tx.First(&binding, "id = ? AND library_id = ? AND generation = ?", policy.CatalogBindingID, run.LibraryID, run.Generation).Error; err != nil {
				return false, err
			}
			if binding.State != "completed" && binding.State != "superseded" {
				snapshot, err := artifactCatalogSnapshot(binding)
				if err != nil {
					return false, err
				}
				if err := ReleaseCatalogBindingTx(tx, snapshot, "artifact", binding.ID); err != nil {
					return false, err
				}
				if err := tx.Model(&binding).Updates(map[string]any{"state": "superseded", "updated_at": now}).Error; err != nil {
					return false, err
				}
			}
		}
		if err := settleAdmittedCatalogPhysicalWriteTx(tx, owner); err != nil {
			return false, err
		}
		return true, nil
	default:
		return false, nil
	}
}

func (s *MediaLibraryService) recoverCatalogScanFollowup(ctx context.Context, row models.CatalogScanFollowup) error {
	var run models.MediaLibraryScanRun
	if err := s.catalogStore.readDB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		_, current, err := s.validateCatalogFollowupTx(tx, row)
		run = current
		return err
	}); err != nil {
		return err
	}
	var failures []error
	if row.RecognitionPending {
		if err := s.dispatchCatalogRecognition(ctx, row); err != nil {
			failures = append(failures, err)
		}
	}
	if row.ArtifactPending {
		err := s.dispatchCatalogArtifact(ctx, row)
		if err != nil {
			failures = append(failures, err)
		}
	}
	if row.DiagnosisPending && run.Status == "success" && run.RecognitionCompleted >= run.RecognitionTotal {
		if err := s.dispatchCatalogAutomaticDiagnosis(ctx, row, run); err != nil {
			failures = append(failures, err)
		}
	}
	if row.ArtworkPending {
		generator, ok := s.libraryArtwork.(interface {
			ReconcileMediaLibrary(context.Context, uint, bool) error
		})
		err := ErrCatalogInvalid
		if ok {
			err = generator.ReconcileMediaLibrary(ctx, row.LibraryID, !run.Partial)
		}
		if err == nil {
			err = s.acknowledgeCatalogFollowup(ctx, row, "artwork_pending")
		}
		if err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (s *MediaLibraryService) dispatchCatalogArtifact(ctx context.Context, row models.CatalogScanFollowup) error {
	if s.artifacts == nil {
		return ErrCatalogInvalid
	}
	var binding models.CatalogArtifactBinding
	var run models.MediaArtifactRun
	delivered := false
	err := s.catalogStore.readDB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if _, _, err := s.validateCatalogFollowupTx(tx, row); err != nil {
			return err
		}
		if err := tx.Where("library_id = ? AND generation = ?", row.LibraryID, row.ArtifactGeneration).Order("head_revision DESC").First(&binding).Error; err != nil {
			return err
		}
		if err := s.artifacts.validateArtifactBindingTx(tx, mediaArtifactPolicy{LibraryID: row.LibraryID, Generation: row.ArtifactGeneration, CatalogBindingID: binding.ID}, nil, nil, false); err != nil {
			return err
		}
		err := tx.Where("library_id = ? AND generation = ?", row.LibraryID, row.ArtifactGeneration).First(&run).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		var policy mediaArtifactPolicy
		if err := json.Unmarshal([]byte(run.PolicyJSON), &policy); err != nil {
			return err
		}
		delivered = policy.CatalogBindingID == binding.ID && run.JobID != nil
		return nil
	})
	if errors.Is(err, ErrCatalogFence) {
		// A newer manual binding owns artifact convergence, but the older scan
		// can still need recognition/diagnosis. Do not discard sibling flags.
		return s.acknowledgeCatalogFollowup(ctx, row, "artifact_pending")
	}
	if err != nil {
		return err
	}
	if !delivered {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.artifacts.ScheduleGeneration(row.LibraryID, row.ArtifactGeneration); err != nil {
			return err
		}
	}
	return s.acknowledgeCatalogFollowup(ctx, row, "artifact_pending")
}

func (s *MediaLibraryService) dispatchCatalogAutomaticDiagnosis(ctx context.Context, row models.CatalogScanFollowup, run models.MediaLibraryScanRun) error {
	if s.structure == nil || !row.CompleteObserved {
		return ErrCatalogInvalid
	}
	var state models.MediaLibraryStructureAutoState
	if err := s.catalogStore.writeCatalogBatch(ctx, func(tx *gorm.DB) error {
		if _, _, err := s.validateCatalogFollowupTx(tx, row); err != nil {
			return err
		}
		err := tx.First(&state, "library_id = ?", row.LibraryID).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			state = models.MediaLibraryStructureAutoState{LibraryID: row.LibraryID, SourceRevision: 1, Status: "pending", UpdatedAt: time.Now().UTC()}
			return tx.Create(&state).Error
		}
		return err
	}); err != nil {
		return err
	}
	if state.SourceRevision == 0 || state.DiagnosedRevision >= state.SourceRevision || state.Status == "queued" || state.Status == "running" {
		return s.acknowledgeCatalogFollowup(ctx, row, "diagnosis_pending")
	}
	// A later partial scan may carry a previously complete source observation.
	// Diagnose the current converged generation, never the stale full-scan rows.
	return s.catalogStore.Admission().WithBackground(ctx, func() error {
		return s.structure.enqueueDiagnosisGuarded(ctx, row.LibraryID, run.ID, run.Generation, run.Kind, true, state.SourceRevision, nil, func(tx *gorm.DB) error {
			if _, _, err := s.validateCatalogFollowupTx(tx, row); err != nil {
				return err
			}
			return acknowledgeCatalogFollowupTx(tx, row, "diagnosis_pending")
		})
	})
}

func (s *MediaLibraryService) RunCatalogScanFollowups(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		if err := s.RecoverCatalogScanFollowups(ctx, 10); err != nil && ctx.Err() == nil {
			s.log.Warn().Str("error_code", "catalog_followup_pending").Msg("目录已发布，后续任务调度暂未完成，将自动重试")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
