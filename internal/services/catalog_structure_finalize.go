package services

import (
	"context"
	"errors"
	pathpkg "path"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

// Metadata bookkeeping follows the published catalog in bounded transactions.
// Every cursor moves in the same transaction as its rows; a restart resumes
// here, never repeats successful physical media operations.
func (s *MediaLibraryStructureService) finalizeCatalogStructureRepair(ctx context.Context, repair models.MediaLibraryStructureRepair, plan StructurePlan, state *structureCatalogRepairState, claim *ClaimedJob, permit CatalogPhysicalWritePermit) error {
	for {
		done := false
		requiresArtifacts := false
		var readied []models.MediaLibraryChange
		next := *state
		next.Stage = "reconciling"
		err := s.structureCatalogWriteTx(ctx, func(tx *gorm.DB) error {
			if err := s.validateCatalogStructureExecutionTx(tx, repair, next, claim, true); err != nil {
				return err
			}
			if next.AfterManaged < len(plan.RecycleItems)+len(plan.Items) {
				for checked := 0; checked < CatalogBatchRows && next.AfterManaged < len(plan.RecycleItems)+len(plan.Items); checked++ {
					complete, err := s.finalizeCatalogStructureManagedTx(tx, repair, plan, next.AfterManaged)
					if err != nil {
						return err
					}
					if !complete { // one bounded mutation batch exhausts this writer
						break
					}
					next.AfterManaged++
				}
				return persistCatalogStructureStateTx(tx, repair.ID, next, map[string]any{"phase": "reconciling"})
			}
			if next.AfterResolved < len(plan.ResolvedIssues) {
				complete, err := deleteCatalogStructureIssueBatchTx(tx, repair.LibraryID, next.DiagnosisJobID, next.DiagnosisGeneration, plan.ResolvedIssues[next.AfterResolved], nil)
				if err != nil {
					return err
				}
				if complete {
					next.AfterResolved++
				}
				return persistCatalogStructureStateTx(tx, repair.ID, next, nil)
			}
			if !plan.SelectionBound && next.AfterMoves < len(plan.Items) {
				for checked := 0; checked < CatalogBatchRows && next.AfterMoves < len(plan.Items); checked++ {
					item := plan.Items[next.AfterMoves]
					complete, err := deleteCatalogStructureIssueBatchTx(tx, repair.LibraryID, next.DiagnosisJobID, next.DiagnosisGeneration, "", &item)
					if err != nil {
						return err
					}
					if !complete {
						break
					}
					next.AfterMoves++
				}
				return persistCatalogStructureStateTx(tx, repair.ID, next, nil)
			}
			if next.AfterSkipped < len(plan.SkippedIssues) {
				end := min(next.AfterSkipped+CatalogBatchRows, len(plan.SkippedIssues))
				if err := tx.Model(&models.MediaLibraryStructureIssue{}).Where("library_id = ? AND diagnosis_job_id = ? AND generation = ? AND token IN ?", repair.LibraryID, next.DiagnosisJobID, next.DiagnosisGeneration, plan.SkippedIssues[next.AfterSkipped:end]).Updates(map[string]any{"state": "skipped", "updated_at": time.Now().UTC()}).Error; err != nil {
					return err
				}
				next.AfterSkipped = end
				return persistCatalogStructureStateTx(tx, repair.ID, next, nil)
			}
			var directories []string
			if err := tx.Model(&models.CatalogStructureDirectoryReceipt{}).Where("repair_id = ?", repair.ID).Order("target_relative").Limit(CatalogBatchRows).Pluck("target_relative", &directories).Error; err != nil {
				return err
			}
			if len(directories) > 0 {
				return tx.Where("repair_id = ? AND target_relative IN ?", repair.ID, directories).Delete(&models.CatalogStructureDirectoryReceipt{}).Error
			}
			var library models.MediaLibrary
			var storage models.Storage
			if err := tx.First(&library, repair.LibraryID).Error; err != nil {
				return err
			}
			if err := tx.First(&storage, library.StorageID).Error; err != nil {
				return err
			}
			finished := time.Now().UTC()
			if err := refreshStructureSummaryTx(tx, repair.LibraryID, finished); err != nil {
				return err
			}
			// A later logical writer owns its own generation and artifact binding.
			// Finish only this receipt's bookkeeping without binding the newer head
			// to the old generation or publishing the newer writer's ready event.
			if next.ArtifactGeneration > 0 && library.ContentRevision == next.PublishedContentRevision {
				requiresArtifacts = mediaLibraryRequiresArtifacts(storage.Type, library, true)
				if requiresArtifacts {
					if s.artifacts == nil {
						return ErrCatalogInvalid
					}
					if _, err := s.artifacts.BindCatalogGenerationTx(tx, library.ID, next.ArtifactGeneration); err != nil {
						return err
					}
				} else {
					var err error
					readied, err = s.changes.MarkGenerationReadyTx(tx, library.ID, next.ArtifactGeneration)
					if err != nil {
						return err
					}
				}
			}
			if err := ReleaseCatalogBindingTx(tx, next.binding(), "repair", repair.ID); err != nil {
				return err
			}
			next.Stage = "completed"
			if err := persistCatalogStructureStateTx(tx, repair.ID, next, map[string]any{"phase": "completed", "processed_items": len(plan.Items) + len(plan.RecycleItems), "finished_at": finished, "last_error_code": ""}); err != nil {
				return err
			}
			if err := SettleCatalogPhysicalWriteTx(tx, permit, claim); err != nil {
				return err
			}
			if err := s.audit.Record(tx, &repair.OwnerID, "media_library.structure_repair.complete", "media_library", uintID(repair.LibraryID), "success", map[string]any{"scope": repair.Scope, "move_count": len(plan.Items), "recycle_count": len(plan.RecycleItems)}, RequestContext{}); err != nil {
				return err
			}
			done = true
			return nil
		})
		if err != nil {
			return err
		}
		*state = next
		if !done {
			continue
		}
		if len(readied) > 0 {
			s.changes.NotifyCommitted(repair.LibraryID, readied[len(readied)-1].Revision)
		}
		if requiresArtifacts {
			if err := s.artifacts.ScheduleGeneration(repair.LibraryID, state.ArtifactGeneration); err != nil {
				s.log.Warn().Uint("library_id", repair.LibraryID).Str("error_code", "structure_artifact_schedule_pending").Msg("目录修复已完成，媒体产物任务等待恢复调度")
			}
		}
		return nil
	}
}

func (s *MediaLibraryStructureService) finalizeCatalogStructureManagedTx(tx *gorm.DB, repair models.MediaLibraryStructureRepair, plan StructurePlan, index int) (bool, error) {
	recycle := index < len(plan.RecycleItems)
	provider, source, target, parent := "", "", "", ""
	if recycle {
		item := plan.RecycleItems[index]
		provider, source = item.ProviderID, item.SourceRelative
	} else {
		item := plan.Items[index-len(plan.RecycleItems)]
		provider, source, target = item.ProviderID, item.SourceRelative, item.TargetRelative
		if provider != "" {
			directory := pathpkg.Dir(target)
			if directory == "." || directory == "" {
				var library models.MediaLibrary
				var storage models.Storage
				if err := tx.First(&library, repair.LibraryID).Error; err != nil {
					return false, err
				}
				if err := tx.First(&storage, library.StorageID).Error; err != nil {
					return false, err
				}
				parent = library.ProviderRootID
				if parent == "" {
					parent = storage.RootPath
				}
			} else {
				var receipt models.CatalogStructureDirectoryReceipt
				err := tx.First(&receipt, "repair_id = ? AND target_relative = ?", repair.ID, directory).Error
				if err == nil {
					parent = receipt.ParentProviderID
				} else if !errors.Is(err, gorm.ErrRecordNotFound) {
					return false, err
				}
			}
		}
	}
	query := tx.Model(&models.MediaManagedItem{}).Where("library_id = ? AND active = ? AND managed = ?", repair.LibraryID, true, true)
	if provider != "" {
		query = query.Where("provider_item_id = ?", provider)
	} else {
		query = query.Where("relative_path IN ?", []string{source, "/" + source})
	}
	updates := map[string]any{"updated_at": time.Now().UTC()}
	if recycle {
		updates["active"] = false
	} else {
		updates["relative_path"] = target
		if parent != "" {
			updates["provider_parent_id"] = parent
			query = query.Where("relative_path <> ? OR provider_parent_id <> ?", target, parent)
		} else {
			query = query.Where("relative_path <> ?", target)
		}
	}
	var ids []uint
	if err := query.Order("id").Limit(CatalogBatchRows).Pluck("id", &ids).Error; err != nil {
		return false, err
	}
	if len(ids) == 0 {
		return true, nil
	}
	return false, tx.Model(&models.MediaManagedItem{}).Where("id IN ?", ids).Updates(updates).Error
}

func deleteCatalogStructureIssueBatchTx(tx *gorm.DB, libraryID uint, diagnosisID string, generation uint64, token string, move *StructurePlanItem) (bool, error) {
	if diagnosisID == "" {
		return true, nil
	}
	query := tx.Where("library_id = ? AND diagnosis_job_id = ? AND generation = ?", libraryID, diagnosisID, generation)
	if token != "" {
		query = query.Where("token = ?", token)
	} else if move != nil {
		query = query.Where("kind = ? AND current_path = ? AND expected_path = ? AND repairable = ? AND conflict_source_count <= ?", move.Kind, safeStructurePath(move.SourceRelative), safeStructurePath(move.TargetRelative), true, 1)
	} else {
		return true, nil
	}
	var issue models.MediaLibraryStructureIssue
	if err := query.Order("id").First(&issue).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return true, nil
	} else if err != nil {
		return false, err
	}
	var ids []uint
	if err := tx.Model(&models.MediaLibraryStructureIssueMember{}).Where("issue_id = ?", issue.ID).Order("id").Limit(CatalogBatchRows).Pluck("id", &ids).Error; err != nil {
		return false, err
	}
	if len(ids) > 0 {
		return false, tx.Where("id IN ? AND issue_id = ?", ids, issue.ID).Delete(&models.MediaLibraryStructureIssueMember{}).Error
	}
	return false, tx.Delete(&issue).Error
}
