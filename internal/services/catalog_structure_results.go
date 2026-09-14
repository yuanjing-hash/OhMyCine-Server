package services

import (
	"context"
	"encoding/json"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

func (s *MediaLibraryStructureService) finishCancelledCatalogStructureRepair(permit CatalogPhysicalWritePermit, repair models.MediaLibraryStructureRepair, plan StructurePlan, boundary StructureBoundary, backend MediaLibraryStructureBackend) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var job models.Job
	if err := s.db.WithContext(ctx).First(&job, "id=?", permit.evidence.JobID).Error; err != nil || !artifactJobCancelled(job) {
		return
	}
	quiesceCatalogPhysicalWrite(s.db, permit, s.log)
	if err := s.finalizeCancelledCatalogStructureResult(ctx, permit, repair, plan, boundary, backend); err != nil {
		s.log.Warn().Uint("library_id", repair.LibraryID).Str("error_code", "structure_cancelled_outcome_pending").Msg("已取消整理的文件结果尚未确认，保留原任务明细")
	}
}

// Cancellation authorizes observations and result bookkeeping only. No backend
// mutation is reachable from here, and every writer rechecks the original permit.
func (s *MediaLibraryStructureService) finalizeCancelledCatalogStructureResult(ctx context.Context, permit CatalogPhysicalWritePermit, repair models.MediaLibraryStructureRepair, plan StructurePlan, boundary StructureBoundary, backend MediaLibraryStructureBackend) error {
	if s.scanBoundary != nil {
		lock := s.scanBoundary(repair.LibraryID)
		if !lock.TryLock() {
			return ErrCatalogFence
		}
		defer lock.Unlock()
	}
	var state structureCatalogRepairState
	if err := s.structureCatalogWriteTx(ctx, func(tx *gorm.DB) error {
		if err := validateCancelledStructurePermitTx(tx, permit, repair); err != nil {
			return err
		}
		var current models.MediaLibraryStructureRepair
		if err := tx.First(&current, "id=?", repair.ID).Error; err != nil {
			return err
		}
		if json.Unmarshal([]byte(current.StateJSON), &state) != nil || state.Version != 1 {
			return ErrCatalogFence
		}
		state.Cancelled, state.cancelPermit = true, &permit
		return s.validateCatalogStructureExecutionTx(tx, repair, state, nil, true)
	}); err != nil {
		return err
	}
	if state.Stage != "catalog_published" && state.Stage != "reconciling" {
		observe, err := structureFileObserver(ctx, boundary, backend)
		if err != nil {
			return err
		}
		total := len(plan.Items) + len(plan.RecycleItems)
		var itemCount int64
		if err := s.db.WithContext(ctx).Model(&models.MediaLibraryStructureRepairItem{}).Where("repair_id=?", repair.ID).Count(&itemCount).Error; err != nil {
			return err
		}
		if itemCount == 0 && total > 0 {
			if _, err := s.ensureStructureRepairItems(ctx, repair, plan, boundary.Storage.Type, nil); err != nil {
				return err
			}
		}
		for offset := 0; offset < total; {
			var rows []models.MediaLibraryStructureRepairItem
			if err := s.db.WithContext(ctx).Where("repair_id=? AND ordinal>=?", repair.ID, offset).Order("ordinal").Limit(CatalogBatchRows).Find(&rows).Error; err != nil {
				return err
			}
			if len(rows) == 0 {
				return ErrCatalogFence
			}
			for i := range rows {
				row := &rows[i]
				if row.Ordinal != offset || offset >= total {
					return ErrCatalogFence
				}
				var source, target, provider, kind, action string
				var size, modified int64
				recycle := offset < len(plan.RecycleItems)
				if recycle {
					item := plan.RecycleItems[offset]
					source, target, provider, size, modified, kind, action = item.SourceRelative, item.RecycleRelative, item.ProviderID, item.Size, item.ModifiedAtUnixNano, item.Kind, "recycle"
				} else {
					item := plan.Items[offset-len(plan.RecycleItems)]
					source, target, provider, size, modified, kind, action = item.SourceRelative, item.TargetRelative, item.ProviderID, item.Size, item.ModifiedAtUnixNano, item.Kind, "move"
				}
				if row.SourceRelative != safeStructurePath(source) || row.TargetRelative != safeStructurePath(target) || row.Action != action || row.Kind != kind {
					return ErrCatalogFence
				}
				if row.Status == structureRepairItemSucceeded && !recycle {
					exact, _, err := observe(target, provider, size, modified)
					if err != nil {
						return err
					}
					if !exact {
						return ErrCatalogFence
					}
				}
				if row.Status != structureRepairItemSucceeded {
					row.FinishedAt = nil
					atSource, missing, err := observe(source, provider, size, modified)
					if err != nil {
						return err
					}
					if atSource {
						row.Status = structureRepairItemFailed
					} else {
						if (!missing && boundary.Storage.Type != models.StorageTypePan115) || (recycle && boundary.Storage.Type != models.StorageTypeLocal) {
							return ErrCatalogFence
						}
						atTarget, _, err := observe(target, provider, size, modified)
						if err != nil || !atTarget {
							return ErrCatalogFence
						}
						row.Status = structureRepairItemSucceeded
					}
				}
				offset++
			}
			if err := s.structureCatalogWriteTx(ctx, func(tx *gorm.DB) error {
				if err := s.validateCatalogStructureExecutionTx(tx, repair, state, nil, true); err != nil {
					return err
				}
				for _, row := range rows {
					code := "structure_cancelled"
					if row.Status == structureRepairItemSucceeded {
						code = ""
					}
					finished := row.FinishedAt
					if finished == nil || row.Status != structureRepairItemSucceeded {
						now := time.Now().UTC()
						finished = &now
					}
					if err := tx.Model(&models.MediaLibraryStructureRepairItem{}).Where("id=? AND repair_id=?", row.ID, repair.ID).Updates(map[string]any{"status": row.Status, "error_code": code, "finished_at": finished}).Error; err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				return err
			}
		}
		applied, err := s.catalogStructureSucceededPlan(ctx, repair, plan, state.RetryCheckpointBefore)
		if err != nil {
			return err
		}
		state.OriginalTotalItems = total
		var successful int64
		if err := s.db.WithContext(ctx).Model(&models.MediaLibraryStructureRepairItem{}).Where("repair_id=? AND status=?", repair.ID, structureRepairItemSucceeded).Count(&successful).Error; err != nil {
			return err
		}
		state.FailedItems = total - int(successful)
		state.BlockedItems = 0
		parents, err := s.repairedManagedProviderParents(ctx, boundary.Library, boundary.Storage, applied.Items)
		if err != nil {
			return err
		}
		facts, err := s.catalogStructureMutationFacts(ctx, repair, state, applied, parents)
		if err != nil {
			return err
		}
		prepared, err := s.prepareCatalogStructureCandidate(ctx, repair, state, nil, facts)
		defer s.abandonCatalogStructureCandidate(&prepared)
		if err != nil {
			return err
		}
		if err := s.publishCatalogStructureRepair(ctx, repair, applied, &state, nil, &prepared); err != nil {
			return err
		}
	}
	applied, err := s.catalogStructureSucceededPlan(ctx, repair, plan, state.RetryCheckpointBefore)
	if err != nil {
		return err
	}
	return s.finalizeCatalogStructureRepair(ctx, repair, applied, &state, nil, permit)
}

// Reconstruct the published subset from durable ordinal checkpoints, rather
// than copying a second unbounded file manifest into the result receipt.
func (s *MediaLibraryStructureService) catalogStructureSucceededPlan(ctx context.Context, repair models.MediaLibraryStructureRepair, plan StructurePlan, before ...time.Time) (StructurePlan, error) {
	result := plan
	result.Items, result.RecycleItems, result.ResolvedIssues = nil, nil, nil
	total := len(plan.Items) + len(plan.RecycleItems)
	for offset := 0; offset < total; {
		var rows []models.MediaLibraryStructureRepairItem
		if err := s.db.WithContext(ctx).Where("repair_id = ? AND ordinal >= ?", repair.ID, offset).Order("ordinal").Limit(CatalogBatchRows).Find(&rows).Error; err != nil {
			return result, err
		}
		if len(rows) == 0 {
			return result, ErrCatalogFence
		}
		for _, row := range rows {
			if row.Ordinal != offset || offset >= total {
				return result, ErrCatalogFence
			}
			var source, target, kind, action string
			succeeded := row.Status == structureRepairItemSucceeded
			if len(before) > 0 && !before[0].IsZero() && row.FinishedAt != nil && !row.FinishedAt.After(before[0]) {
				succeeded = false
			}
			if offset < len(plan.RecycleItems) {
				item := plan.RecycleItems[offset]
				source, target, kind, action = item.SourceRelative, item.RecycleRelative, item.Kind, "recycle"
				if succeeded {
					result.RecycleItems = append(result.RecycleItems, item)
				}
			} else {
				item := plan.Items[offset-len(plan.RecycleItems)]
				source, target, kind, action = item.SourceRelative, item.TargetRelative, item.Kind, "move"
				if succeeded {
					result.Items = append(result.Items, item)
				}
			}
			if row.SourceRelative != safeStructurePath(source) || row.TargetRelative != safeStructurePath(target) || row.Kind != kind || row.Action != action {
				return result, ErrCatalogFence
			}
			if row.Status != structureRepairItemSucceeded && row.Status != structureRepairItemFailed && row.Status != structureRepairItemBlocked {
				return result, ErrCatalogFence
			}
			offset++
		}
	}
	return result, nil
}
