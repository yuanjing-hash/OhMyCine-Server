package services

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
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
			return WorkerResult{ErrorCode: "library_retirement_needs_attention", ErrorMessage: "媒体库索引移除暂停；文件和历史均保留，请检查任务后重试"}
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
			timer := time.NewTimer(100 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return WorkerResult{}
			case <-timer.C:
			}
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
		if row.Phase != "finalizing" {
			var head models.CatalogHead
			if err := tx.First(&head, "library_id=?", row.LibraryID).Error; err != nil {
				return err
			}
			if head.SourceEpoch != row.SourceEpoch || head.SourceFingerprint != row.SourceFingerprint || head.ConfigFingerprint != row.ConfigFingerprint {
				return ErrCatalogFence
			}
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
			waiting = !complete && interrupt != ""
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
				row.Phase = "finalizing"
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
			if err := s.audit.Record(tx, nil, "media_library.retirement.complete", "media_library", uintID(row.LibraryID), "success", map[string]any{"job_id": row.JobID, "files_preserved": true, "history_preserved": true}, RequestContext{}); err != nil {
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
	if err == nil && interrupt != "" && s.queue.interrupt != nil {
		s.queue.interrupt(interrupt, "cancel")
	}
	return
}

// Only library-owned maintenance jobs are cancelled. Download / Transfer /
// seeding histories and their providers are never controlled by retirement.
func retirementLibraryJobs(tx *gorm.DB, libraryID uint) *gorm.DB {
	return tx.Model(&models.Job{}).Where("job_type<>?", JobTypeMediaLibraryRetirement).Where(`resource_key IN ? OR id IN (SELECT job_id FROM media_artifact_runs WHERE library_id=?) OR id IN (SELECT job_id FROM catalog_snapshots WHERE library_id=?) OR id IN (SELECT job_id FROM media_library_structure_diagnoses WHERE library_id=?) OR id IN (SELECT job_id FROM media_library_structure_repairs WHERE library_id=?) OR id IN (SELECT job_id FROM media_reorganization_tasks WHERE library_id=?) OR id IN (SELECT job_id FROM media_server_refresh_runs WHERE target_id IN (SELECT id FROM media_server_refresh_targets WHERE library_id=?)) OR id IN (SELECT job_id FROM schedule_runs WHERE schedule_id IN (SELECT id FROM schedule_definitions WHERE target_type='media_library' AND target_id=CAST(? AS TEXT)))`, []string{mediaArtifactResourceKey(libraryID), strmReconcileResourceKey(libraryID), "structure-diagnosis-library:" + uintID(libraryID)}, libraryID, libraryID, libraryID, libraryID, libraryID, libraryID, libraryID)
}
func drainLibraryRetirementJobTx(tx *gorm.DB, row models.MediaLibraryRetirement) (bool, string, error) {
	var job models.Job
	err := retirementLibraryJobs(tx, row.LibraryID).Where("status IN ? OR lease_token_hash<>'' OR interrupt_status<>''", activeJobStatuses()).Order("id").First(&job).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return true, "", nil
	}
	if err != nil {
		return false, "", err
	}
	switch job.JobType {
	case JobTypeMediaLibraryRecognition, JobTypeMediaLibraryStructureDiagnosis, JobTypeMediaArtifact, JobTypeMediaReorganization, JobTypeMediaLibraryRepair, "strm_reconcile", "catalog_conversion", "catalog_compaction", JobTypeMediaServerRefresh, "unified_schedule":
	default:
		return false, "", appError(CodeConflict, "仍有需要恢复的媒体写入任务，请先处理任务", nil)
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
	if job.LeaseTokenHash != "" || job.InterruptStatus != "" {
		return false, "", ErrCatalogFence
	}
	if err := tx.Model(&job).Updates(map[string]any{"status": models.JobStatusCancelled, "cancellation_asked": true, "finished_at": now, "revision": job.Revision + 1, "updated_at": now}).Error; err != nil {
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

// One exact, verified reference per short transaction. Unknown owner types or
// missing receipts fail closed; age alone never licenses reference deletion.
func releaseLibraryRetirementOwnerTx(tx *gorm.DB, row models.MediaLibraryRetirement) (bool, error) {
	var ref models.CatalogSnapshotReference
	err := tx.Table("catalog_snapshot_references r").Select("r.*").Joins("JOIN catalog_snapshots s ON s.id=r.snapshot_id").Where("s.library_id=?", row.LibraryID).Order("r.snapshot_id,r.owner_kind,r.owner_id").Take(&ref).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	switch ref.OwnerKind {
	case "artifact":
		var binding models.CatalogArtifactBinding
		if err := tx.First(&binding, "id=? AND library_id=?", ref.OwnerID, row.LibraryID).Error; err != nil {
			return false, err
		}
		if err := tx.Model(&binding).Update("state", "superseded").Error; err != nil {
			return false, err
		}
	case "reorganization":
		var task models.MediaReorganizationTask
		if err := tx.First(&task, "id=? AND library_id=?", ref.OwnerID, row.LibraryID).Error; err != nil {
			return false, err
		}
		if err := requireRetiredJobTx(tx, task.JobID); err != nil {
			return false, err
		}
		if task.Phase != "completed" {
			if err := requireUnenteredRetirementOwnerTx(tx, row.LibraryID, CatalogPhysicalReorganization, task.ID); err != nil {
				return false, err
			}
		}
	case "repair":
		var task models.MediaLibraryStructureRepair
		if err := tx.First(&task, "id=? AND library_id=?", ref.OwnerID, row.LibraryID).Error; err != nil {
			return false, err
		}
		if task.JobID != nil {
			if err := requireRetiredJobTx(tx, *task.JobID); err != nil {
				return false, err
			}
		}
		if task.Phase != "completed" {
			if err := requireUnenteredRetirementOwnerTx(tx, row.LibraryID, CatalogPhysicalRepair, task.ID); err != nil {
				return false, err
			}
		}
	case "compaction":
		id, _, ok := strings.Cut(ref.OwnerID, ":")
		if !ok {
			return false, ErrCatalogInvalid
		}
		var candidate models.CatalogSnapshot
		if err := tx.First(&candidate, "id=? AND library_id=?", id, row.LibraryID).Error; err != nil {
			return false, err
		}
		if candidate.JobID != nil {
			if err := requireRetiredJobTx(tx, *candidate.JobID); err != nil {
				return false, err
			}
		}
		if candidate.State != "published" && candidate.State != "abandoned" {
			if err := tx.Model(&candidate).Update("state", "abandoned").Error; err != nil {
				return false, err
			}
		}
	case "diagnosis":
		// Current production diagnoses do not retain refs. If an older build did,
		// require its explicit same-library job receipt; never guess an owner.
		var owner models.MediaLibraryStructureDiagnosis
		if err := tx.First(&owner, "job_id=? AND library_id=?", ref.OwnerID, row.LibraryID).Error; err != nil {
			return false, err
		}
		if err := requireRetiredJobTx(tx, owner.JobID); err != nil {
			return false, err
		}
	default:
		return false, ErrCatalogFence
	}
	return false, ReleaseCatalogReferenceTx(tx, ref.SnapshotID, ref.OwnerKind, ref.OwnerID)
}

func requireUnenteredRetirementOwnerTx(tx *gorm.DB, libraryID uint, kind, id string) error {
	var proof models.CatalogPhysicalWrite
	if err := tx.Where("library_id=? AND owner_kind=? AND owner_id=? AND state='admitted'", libraryID, kind, id).First(&proof).Error; err != nil {
		return ErrCatalogFence
	}
	return nil
}
