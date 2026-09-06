package services

import (
	"context"
	"encoding/json"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

// Head + locked Download identity are already published. Every subsequent
// transaction touches <=250 managed rows and checkpoints its own parent revision
// and cursor; retry never returns to filesystem/provider execution.
func (w *MediaReorganizationWorker) reconcileCatalogReorganization(ctx context.Context, task models.MediaReorganizationTask, library models.MediaLibrary, storage models.Storage, plan reorganizationPlan, state reorganizationState, claim ClaimedJob, permit CatalogPhysicalWritePermit) error {
	if state.Catalog == nil || state.Catalog.Stage != "reconciling" || state.Catalog.AfterManaged < 0 || state.Catalog.AfterManaged > len(plan.Items) {
		return ErrCatalogFence
	}
	libraries := w.service.libraries
	for {
		next := state
		catalog := *state.Catalog
		next.Catalog = &catalog
		done, requiresArtifacts := false, false
		var readied []models.MediaLibraryChange
		err := withBackgroundTransaction(ctx, w.service.db, libraries.catalogStore.Admission(), func(tx *gorm.DB) error {
			stateOnly := uint(0)
			if err := w.validateCatalogExecutionGuardTx(tx, task, plan, state, claim, &stateOnly); err != nil {
				return err
			}
			now := time.Now().UTC()
			if next.Catalog.AfterManaged < len(plan.Items) {
				end := min(next.Catalog.AfterManaged+CatalogBatchRows, len(plan.Items))
				batch := plan
				batch.Items = plan.Items[next.Catalog.AfterManaged:end]
				if err := publishReorganizationManagedManifestTx(tx, task, batch, state, task.TargetIdentityRevision, now); err != nil {
					return err
				}
				var transfer models.TransferTask
				if err := tx.Select("id", "managed_revision").First(&transfer, "id=?", task.TransferTaskID).Error; err != nil {
					return err
				}
				next.Catalog.AfterManaged, next.Catalog.ManagedRevision = end, transfer.ManagedRevision
				return persistReorganizationReconcileStateTx(tx, task.ID, next, map[string]any{"phase": models.MediaReorganizationPhaseReconciling, "last_error_code": ""})
			}
			var current models.MediaLibrary
			if err := tx.First(&current, library.ID).Error; err != nil {
				return err
			}
			// A later logical writer owns the next artifact generation. Finishing
			// this task must not bind a newer head to our old generation.
			if current.ContentRevision == next.Catalog.PublishedContentRevision {
				requiresArtifacts = mediaLibraryRequiresArtifacts(storage.Type, current, libraries.artifacts != nil)
				if requiresArtifacts {
					if _, err := libraries.artifacts.BindCatalogGenerationTx(tx, library.ID, next.Catalog.ArtifactGeneration); err != nil {
						return err
					}
				} else if libraries.changes != nil {
					var err error
					readied, err = libraries.changes.MarkGenerationReadyTx(tx, library.ID, next.Catalog.ArtifactGeneration)
					if err != nil {
						return err
					}
				}
			}
			if err := ReleaseCatalogBindingTx(tx, next.Catalog.binding(*plan.CatalogFence), "reorganization", task.ID); err != nil {
				return err
			}
			next.Catalog.Stage = "completed"
			if err := persistReorganizationReconcileStateTx(tx, task.ID, next, map[string]any{"phase": models.MediaReorganizationPhaseCompleted, "processed_items": len(plan.Items), "finished_at": now, "last_error_code": ""}); err != nil {
				return err
			}
			if err := SettleCatalogPhysicalWriteTx(tx, permit, &claim); err != nil {
				return err
			}
			if err := w.service.audit.Record(tx, &task.OwnerID, "media.reorganization.complete", "media_reorganization_task", task.ID, "success", map[string]any{"library_id": library.ID, "items": len(plan.Items)}, RequestContext{}); err != nil {
				return err
			}
			done = true
			return nil
		})
		if err != nil {
			return err
		}
		state = next
		if !done {
			continue
		}
		if len(readied) > 0 {
			libraries.changes.NotifyCommitted(library.ID, readied[len(readied)-1].Revision)
		}
		if requiresArtifacts {
			if err := libraries.artifacts.ScheduleGeneration(library.ID, state.Catalog.ArtifactGeneration); err != nil {
				w.service.log.Warn().Uint("library_id", library.ID).Str("error_code", "artifact_schedule_pending").Msg("重新整理已完成，媒体产物等待恢复调度")
			}
		}
		return nil
	}
}

func persistReorganizationReconcileStateTx(tx *gorm.DB, id string, state reorganizationState, updates map[string]any) error {
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if len(raw) > CatalogBatchBytes {
		return ErrCatalogBudget
	}
	updates["state_json"], updates["updated_at"] = string(raw), time.Now().UTC()
	return tx.Model(&models.MediaReorganizationTask{}).Where("id=?", id).Updates(updates).Error
}
