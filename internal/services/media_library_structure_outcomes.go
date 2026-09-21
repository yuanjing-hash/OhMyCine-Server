package services

import (
	"context"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

// Reconcile failed calls by observation, never by repeating a mutation. A
// provider may have committed a move before returning an error. Exact target
// identity is a successful outcome; an unchanged source is a settled failure.
func (s *MediaLibraryStructureService) reconcileStructureFailureOutcomes(ctx context.Context, repair models.MediaLibraryStructureRepair, plan StructurePlan, boundary StructureBoundary, backend MediaLibraryStructureBackend, claim *ClaimedJob, execution *structureRepairExecution) error {
	var rows []models.MediaLibraryStructureRepairItem
	if err := s.db.WithContext(ctx).Where("repair_id=? AND status<>?", repair.ID, structureRepairItemSucceeded).Order("ordinal").Find(&rows).Error; err != nil {
		return err
	}
	observe, err := structureFileObserver(ctx, boundary, backend)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if row.Status != structureRepairItemFailed && row.Status != structureRepairItemBlocked {
			return ErrCatalogFence
		}
		var source, target, providerID string
		var size, modified int64
		recycle := row.Ordinal < len(plan.RecycleItems)
		if row.Ordinal < 0 {
			return ErrCatalogFence
		}
		if recycle {
			item := plan.RecycleItems[row.Ordinal]
			source, target, providerID, size, modified = item.SourceRelative, item.RecycleRelative, item.ProviderID, item.Size, item.ModifiedAtUnixNano
		} else {
			index := row.Ordinal - len(plan.RecycleItems)
			if index >= len(plan.Items) {
				return ErrCatalogFence
			}
			item := plan.Items[index]
			source, target, providerID, size, modified = item.SourceRelative, item.TargetRelative, item.ProviderID, item.Size, item.ModifiedAtUnixNano
		}
		if row.SourceRelative != source || row.TargetRelative != target {
			return ErrCatalogFence
		}
		atSource, missingSource, err := observe(source, providerID, size, modified)
		if err != nil {
			return err
		}
		if atSource {
			continue
		}
		// A cloud recycle leaves this root and absence cannot prove its outcome.
		// Local files require source absence, since size/time are not stable IDs.
		if (recycle && boundary.Storage.Type != models.StorageTypeLocal) || (!missingSource && boundary.Storage.Type != models.StorageTypePan115) {
			return ErrCatalogFence
		}
		atTarget, _, err := observe(target, providerID, size, modified)
		if err != nil {
			return err
		}
		if !atTarget {
			return ErrCatalogFence
		}
		if err := validateStructureMutation(boundary); err != nil {
			return err
		}
		if err := s.transitionStructureRepairItem(ctx, repair, claim, row.Ordinal, row.Status, structureRepairItemSucceeded, map[string]any{"error_code": "", "error_message": "", "finished_at": time.Now().UTC()}); err != nil {
			return err
		}
		execution.Succeeded++
		if row.Status == structureRepairItemFailed {
			execution.Failed--
		} else {
			execution.Blocked--
		}
		if recycle {
			execution.Plan.RecycleItems = append(execution.Plan.RecycleItems, plan.RecycleItems[row.Ordinal])
		} else {
			execution.Plan.Items = append(execution.Plan.Items, plan.Items[row.Ordinal-len(plan.RecycleItems)])
		}
	}
	return nil
}
