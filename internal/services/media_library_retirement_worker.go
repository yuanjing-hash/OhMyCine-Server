package services

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

type MediaLibraryRetirementWorker struct{ libraries *MediaLibraryService }

func NewMediaLibraryRetirementWorker(s *MediaLibraryService) *MediaLibraryRetirementWorker {
	return &MediaLibraryRetirementWorker{libraries: s}
}

func (w *MediaLibraryRetirementWorker) Run(ctx context.Context, runtime JobRuntime, claim ClaimedJob) WorkerResult {
	var payload struct {
		RetirementID string `json:"retirement_id"`
	}
	if json.Unmarshal([]byte(claim.Job.PayloadJSON), &payload) != nil || payload.RetirementID == "" {
		return WorkerResult{ErrorCode: "library_retirement_invalid", ErrorMessage: "媒体库移除任务无效"}
	}
	s := w.libraries
	var row models.MediaLibraryRetirement
	if err := s.db.WithContext(ctx).First(&row, "id=? AND job_id=?", payload.RetirementID, claim.Job.ID).Error; err != nil {
		return WorkerResult{ErrorCode: "library_retirement_missing", ErrorMessage: "媒体库移除凭据不可用"}
	}
	if row.Phase == "completed" {
		return WorkerResult{}
	}
	// No DB writer is held while a provider/local enumerator acknowledges stop.
	s.stopSupervisor(row.LibraryID)
	for ctx.Err() == nil {
		waiting, done, err := w.step(ctx, claim, &row)
		if err != nil {
			if ctx.Err() != nil {
				return WorkerResult{}
			}
			_ = s.db.Transaction(func(tx *gorm.DB) error {
				if _, err := s.queue.verifyLease(tx, claim.Job.ID, claim.LeaseToken); err != nil {
					return err
				}
				return tx.Model(&models.MediaLibraryRetirement{}).Where("id=? AND phase<>?", row.ID, "completed").Updates(map[string]any{"last_error_code": "library_retirement_needs_attention", "updated_at": time.Now().UTC()}).Error
			})
			return WorkerResult{ErrorCode: "library_retirement_needs_attention", ErrorMessage: "媒体库移除出现内部错误；文件未改动，当前进度和安全证据已保留"}
		}
		if done {
			return WorkerResult{}
		}
		if runtime != nil {
			if err := runtime.Checkpoint(map[string]any{"phase": row.Phase, "processed_rows": row.ProcessedRows}); err != nil {
				return WorkerResult{}
			}
		}
		if waiting {
			// Waiting for another worker to leave its real external-I/O boundary is
			// normal retirement progress. Yield the queue lease instead of occupying
			// a worker or spending the bounded failure budget.
			next := time.Now().UTC().Add(2 * time.Second)
			return WorkerResult{RetryAt: &next}
		}
	}
	return WorkerResult{}
}

func (w *MediaLibraryRetirementWorker) step(ctx context.Context, claim ClaimedJob, row *models.MediaLibraryRetirement) (waiting, done bool, err error) {
	s := w.libraries
	var interrupt string
	write := func(tx *gorm.DB) error {
		if _, err := s.queue.verifyLease(tx, claim.Job.ID, claim.LeaseToken); err != nil {
			return err
		}
		if err := tx.First(row, "id=? AND job_id=?", row.ID, claim.Job.ID).Error; err != nil {
			return err
		}
		if row.Phase == "completed" {
			done = true
			return nil
		}
		var library models.MediaLibrary
		if err := tx.First(&library, row.LibraryID).Error; err != nil {
			return err
		}
		if library.Enabled {
			return ErrCatalogFence
		}
		if err := verifyLibraryRetirementHeadTx(tx, *row); err != nil {
			return err
		}
		switch row.Phase {
		case "queued":
			row.Phase = "draining"
		case "draining":
			// Disable producers in bounded writers before draining their jobs.
			for _, producer := range []libraryRetirementCleanupStep{
				{"schedule_definitions", "target_type='media_library' AND target_id=CAST(? AS TEXT) AND enabled=1", "enabled=0,next_run_at=NULL,revision=revision+1", false},
				{"media_server_refresh_targets", "library_id=? AND enabled=1", "enabled=0,revision=revision+1", false},
			} {
				changed, err := producer.run(tx, row.LibraryID)
				if err != nil {
					return err
				}
				if changed != 0 {
					row.ProcessedRows += changed
					row.Revision++
					row.UpdatedAt = time.Now().UTC()
					return tx.Save(row).Error
				}
			}
			var complete bool
			var err error
			complete, interrupt, err = drainLibraryRetirementJobTx(tx, *row)
			if err != nil {
				return err
			}
			waiting = !complete
			if complete {
				row.Phase = "owners"
			}
		case "owners":
			complete, err := releaseLibraryRetirementOwnerTx(tx, *row)
			if err != nil {
				return err
			}
			if complete {
				if err := tx.Where("library_id=?", row.LibraryID).Delete(&models.CatalogHeadLayer{}).Error; err != nil {
					return err
				}
				row.Phase = "snapshots"
			}
		case "snapshots":
			var snapshot models.CatalogSnapshot
			err := tx.Where("library_id=?", row.LibraryID).Order("id").First(&snapshot).Error
			if errors.Is(err, gorm.ErrRecordNotFound) {
				row.Phase = "derived"
				break
			}
			if err != nil {
				return err
			}
			if snapshot.State != "gc" {
				if snapshot.State != "published" && snapshot.State != "abandoned" {
					// The durable retirement gate revokes all new candidate writes.
					// A real queue owner still must have acknowledged cancellation.
					if snapshot.JobID != nil {
						if err := requireRetiredJobTx(tx, *snapshot.JobID); err != nil {
							return err
						}
					}
					if err := tx.Model(&snapshot).Update("state", "abandoned").Error; err != nil {
						return err
					}
				}
				if err := MarkCatalogGCTx(tx, snapshot.ID); err != nil {
					return err
				}
			}
			if _, err := collectCatalogBatchTx(tx, snapshot.ID); err != nil {
				return err
			}
		case "derived", "anchors":
			if row.Cursor >= len(libraryRetirementCleanup) {
				changed, err := cleanupLibraryRetirementJobsTx(tx, *row)
				if err != nil {
					return err
				}
				row.ProcessedRows += changed
				if changed == 0 {
					row.Phase = "finalizing"
				}
				break
			}
			item := libraryRetirementCleanup[row.Cursor]
			if item.anchor {
				row.Phase = "anchors"
			}
			changed, err := item.run(tx, row.LibraryID)
			if err != nil {
				return err
			}
			row.ProcessedRows += changed
			if changed == 0 {
				row.Cursor++
			}
		case "finalizing":
			if err := assertLibraryRetirementEmptyTx(tx, row.LibraryID); err != nil {
				return err
			}
			if err := tx.Where("library_id=?", row.LibraryID).Delete(&models.CatalogHead{}).Error; err != nil {
				return err
			}
			if err := tx.Delete(&library).Error; err != nil {
				return err
			}
			now := time.Now().UTC()
			row.Phase = "completed"
			row.CompletedAt = &now
			done = true
			if err := s.audit.Record(tx, nil, "media_library.retirement.complete", "media_library", uintID(row.LibraryID), "success", map[string]any{"job_id": row.JobID, "files_preserved": true, "library_history_removed": true}, RequestContext{}); err != nil {
				return err
			}
		default:
			return ErrCatalogInvalid
		}
		row.Revision++
		row.UpdatedAt = time.Now().UTC()
		row.LastErrorCode = ""
		return tx.Save(row).Error
	}
	if s.catalogStore != nil {
		err = s.catalogStore.writeCatalogBatch(ctx, write)
	} else {
		err = s.db.WithContext(ctx).Transaction(write)
	}
	if err == nil && interrupt != "" {
		s.queue.interruptLocally([]string{interrupt})
	}
	return
}

// Zero source fences identify a retirement accepted before the library has
// published its first baseline. It must stay headless for the entire cleanup; a newly appearing head
// is a producer race and is never silently removed. Snapshot-backed retirements
// keep their original exact head fence through finalization.
func verifyLibraryRetirementHeadTx(tx *gorm.DB, row models.MediaLibraryRetirement) error {
	var head models.CatalogHead
	err := tx.First(&head, "library_id=?", row.LibraryID).Error
	headless := row.SourceEpoch == 0 && row.SourceFingerprint == "" && row.ConfigFingerprint == ""
	if headless {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		return ErrCatalogFence
	}
	if err != nil {
		return err
	}
	if head.SourceEpoch != row.SourceEpoch || head.SourceFingerprint != row.SourceFingerprint || head.ConfigFingerprint != row.ConfigFingerprint {
		return ErrCatalogFence
	}
	return nil
}

// Freeze only work with exact selected-library ownership. A Transfer is owned
// by its one immutable target library and must be stopped and removed with that
// library. Download and seeding work remain separate provider/source facts.
func retirementLibraryJobs(tx *gorm.DB, libraryID uint) *gorm.DB {
	return tx.Model(&models.Job{}).Where("job_type<>?", JobTypeMediaLibraryRetirement).Where(`resource_key IN ? OR id IN (SELECT job_id FROM catalog_physical_writes WHERE library_id=? AND job_id<>'') OR id IN (SELECT job_id FROM transfer_tasks WHERE library_id=?) OR id IN (SELECT job_id FROM media_artifact_runs WHERE library_id=?) OR id IN (SELECT job_id FROM catalog_snapshots WHERE library_id=?) OR id IN (SELECT job_id FROM media_library_structure_diagnoses WHERE library_id=?) OR id IN (SELECT job_id FROM media_library_structure_repairs WHERE library_id=?) OR id IN (SELECT job_id FROM media_reorganization_tasks WHERE library_id=?) OR id IN (SELECT job_id FROM media_server_refresh_runs WHERE target_id IN (SELECT id FROM media_server_refresh_targets WHERE library_id=?)) OR id IN (SELECT job_id FROM schedule_runs WHERE schedule_id IN (SELECT id FROM schedule_definitions WHERE target_type='media_library' AND target_id=CAST(? AS TEXT)))`, []string{mediaArtifactResourceKey(libraryID), strmReconcileResourceKey(libraryID), "structure-diagnosis-library:" + uintID(libraryID)}, libraryID, libraryID, libraryID, libraryID, libraryID, libraryID, libraryID, libraryID, libraryID)
}
func drainLibraryRetirementJobTx(tx *gorm.DB, row models.MediaLibraryRetirement) (bool, string, error) {
	var job models.Job
	err := tx.Table("jobs j").Select("j.*").
		Joins("JOIN media_library_retirement_jobs r ON r.job_id=j.id").
		Where("r.retirement_id=?", row.ID).
		Where("j.status NOT IN ? OR j.finished_at IS NULL OR j.lease_token_hash<>'' OR j.lease_expires_at IS NOT NULL OR j.interrupt_status<>''", historyTerminalStatuses()).
		Order("j.id").First(&job).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		var entered int
		if err := tx.Raw("SELECT 1 FROM catalog_physical_writes WHERE library_id=? AND state='entered' LIMIT 1", row.LibraryID).Scan(&entered).Error; err != nil {
			return false, "", err
		}
		return entered == 0, "", nil
	}
	if err != nil {
		return false, "", err
	}
	now := time.Now().UTC()
	if job.Status == models.JobStatusRunning {
		if job.InterruptStatus == models.JobStatusCancelled && job.CancellationAsked {
			return false, job.ID, nil
		}
		if err := tx.Model(&job).Updates(map[string]any{"cancellation_asked": true, "interrupt_status": models.JobStatusCancelled, "revision": job.Revision + 1, "updated_at": now}).Error; err != nil {
			return false, "", err
		}
		return false, job.ID, nil
	}
	if job.LeaseTokenHash != "" {
		return false, job.ID, nil
	}
	if err := tx.Model(&job).Updates(map[string]any{
		"status": models.JobStatusCancelled, "cancellation_asked": true,
		"interrupt_status": "", "next_attempt_at": nil, "lease_expires_at": nil,
		"heartbeat_at": nil, "finished_at": now, "revision": job.Revision + 1,
		"updated_at": now,
	}).Error; err != nil {
		return false, "", err
	}
	return false, "", recordJobEvent(tx, job.ID, "library.retirement", job.Status, models.JobStatusCancelled, nil, "", now)
}

func requireRetiredJobTx(tx *gorm.DB, jobID string) error {
	var job models.Job
	if err := tx.First(&job, "id=?", jobID).Error; err != nil {
		return err
	}
	if !isTerminalPipelineJobStatus(job.Status) || job.LeaseTokenHash != "" || job.InterruptStatus != "" {
		return ErrCatalogFence
	}
	return nil
}

// One reference per short transaction. Draining has already proved that every
// frozen library-owned Job is terminal and that no external call remains in
// entered state. Retirement intentionally discards this library's recovery
// graph, so references do not need an owner-kind-specific recovery path.
func releaseLibraryRetirementOwnerTx(tx *gorm.DB, row models.MediaLibraryRetirement) (bool, error) {
	var ref models.CatalogSnapshotReference
	err := tx.Table("catalog_snapshot_references r").Select("r.*").Joins("JOIN catalog_snapshots s ON s.id=r.snapshot_id").Where("s.library_id=?", row.LibraryID).Order("r.snapshot_id,r.owner_kind,r.owner_id").Take(&ref).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return false, ReleaseCatalogReferenceTx(tx, ref.SnapshotID, ref.OwnerKind, ref.OwnerID)
}
