package services

import (
	"context"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

// A completed partial attempt owns no old Catalog binding. Explicit retry can
// bind the current head only when its own publication is still the latest
// logical content; otherwise the user needs a fresh diagnosis/confirmation.
func (s *MediaLibraryStructureService) rebindPartialStructureRetry(ctx context.Context, repair models.MediaLibraryStructureRepair, state *structureCatalogRepairState, claim *ClaimedJob) error {
	next := *state
	err := s.structureCatalogWriteTx(ctx, func(tx *gorm.DB) error {
		var current models.MediaLibraryStructureRepair
		if err := tx.First(&current, "id=?", repair.ID).Error; err != nil {
			return err
		}
		if current.PlanJSON != repair.PlanJSON || current.StateJSON != repair.StateJSON || current.Phase != "failed" {
			return ErrCatalogFence
		}
		if claim != nil {
			if _, err := s.queue.verifyLease(tx, claim.Job.ID, claim.LeaseToken); err != nil {
				return err
			}
		} else if repair.JobID != nil {
			return ErrCatalogFence
		}
		var proof models.CatalogPhysicalWrite
		if err := tx.Where("owner_kind=? AND owner_id=? AND library_id=? AND state='settled'", CatalogPhysicalRepair, repair.ID, repair.LibraryID).First(&proof).Error; err != nil {
			return err
		}
		reader, err := PinCatalogTx(tx, []uint{repair.LibraryID})
		if err != nil {
			return err
		}
		fence := state.Fence
		fence.ContentRevision = state.PublishedContentRevision
		if err := validateStructureLogicalFenceTx(tx, reader, fence); err != nil {
			return err
		}
		binding, err := CaptureCatalogBindingTx(tx, repair.LibraryID, "repair", repair.ID)
		if err != nil {
			return err
		}
		next = structureCatalogRepairState{Version: 1, Fence: fence, BoundRevision: binding.Head.Revision, Layers: binding.Layers, Stage: "queued", SourceRevision: state.SourceRevision, SelectionBound: state.SelectionBound, DiagnosisJobID: state.DiagnosisJobID, DiagnosisGeneration: state.DiagnosisGeneration, RetryCheckpointBefore: time.Now().UTC()}
		return persistCatalogStructureStateTx(tx, repair.ID, next, map[string]any{"phase": "queued", "last_error_code": "", "finished_at": nil})
	})
	if err == nil {
		*state = next
	}
	return err
}

func (s *MediaLibraryStructureService) catalogStructureRemainingPlan(ctx context.Context, repair models.MediaLibraryStructureRepair, plan StructurePlan, before time.Time) (StructurePlan, error) {
	result := plan
	result.Items = nil
	result.RecycleItems = nil
	var completed []models.MediaLibraryStructureRepairItem
	if err := s.db.WithContext(ctx).Where("repair_id=? AND status=? AND finished_at<=?", repair.ID, structureRepairItemSucceeded, before).Find(&completed).Error; err != nil {
		return result, err
	}
	done := make(map[int]bool, len(completed))
	for _, row := range completed {
		done[row.Ordinal] = true
	}
	for i, item := range plan.RecycleItems {
		if !done[i] {
			result.RecycleItems = append(result.RecycleItems, item)
		}
	}
	for i, item := range plan.Items {
		if !done[i+len(plan.RecycleItems)] {
			result.Items = append(result.Items, item)
		}
	}
	return result, nil
}
