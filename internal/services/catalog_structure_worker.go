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

func (s *MediaLibraryStructureService) runCatalogStructureRepair(ctx context.Context, runtime JobRuntime, repair models.MediaLibraryStructureRepair, plan StructurePlan, claim *ClaimedJob) WorkerResult {
	var state structureCatalogRepairState
	if s.catalogStore == nil || s.changes == nil || len(repair.StateJSON) > 16*1024 || json.Unmarshal([]byte(repair.StateJSON), &state) != nil || state.Version != 1 || state.Fence.LibraryID != repair.LibraryID {
		return WorkerResult{ErrorCode: CodeMediaLibraryStructureBoundaryChanged, ErrorMessage: "目录修复缺少当前索引确认，请重新预览"}
	}
	plan.catalogFence = &state.Fence
	physicalDone := state.Stage == "physical_completed"
	fail := func(err error) WorkerResult {
		code := CodeMediaLibraryStructureApplyFailed
		if errors.Is(err, ErrCatalogFence) {
			code = CodeMediaLibraryStructureBoundaryChanged
		}
		if errors.Is(err, ErrCatalogBudget) {
			code = CodeMediaLibraryStructureUnavailable
		}
		if errors.Is(err, errStructureConflict) {
			code = CodeMediaLibraryStructureConflict
		}
		s.log.Warn().Uint("library_id", repair.LibraryID).Str("repair_id", repair.ID).Str("stage", state.Stage).Str("error_code", code).Msg("目录修复暂停，原计划与已完成进度已保留")
		published := state.Stage == "catalog_published" || state.Stage == "reconciling"
		_ = s.structureCatalogWriteTx(ctx, func(tx *gorm.DB) error {
			if claim != nil {
				if _, e := s.queue.verifyLease(tx, claim.Job.ID, claim.LeaseToken); e != nil {
					return e
				}
			}
			phase := "failed"
			if published {
				phase = "reconciling"
			}
			return tx.Model(&models.MediaLibraryStructureRepair{}).Where("id = ? AND plan_json = ? AND phase <> ?", repair.ID, repair.PlanJSON, "completed").Updates(map[string]any{"phase": phase, "last_error_code": code, "updated_at": time.Now().UTC()}).Error
		})
		message := "目录修复已停止，请核对后重试；已完成的文件操作会按原计划恢复"
		if state.Stage == "directories_prepared" || state.Stage == "directories_preparing" {
			message = "可能已准备部分目标目录，媒体文件尚未移动；请核对后重试"
		}
		if published {
			message = "文件处理和目录索引已完成，结果收尾等待恢复，不会重跑文件操作"
			next := time.Now().UTC().Add(time.Minute)
			return WorkerResult{RetryAt: &next, ErrorCode: code, ErrorMessage: message}
		}
		return WorkerResult{ErrorCode: code, ErrorMessage: message}
	}
	if state.Stage == "catalog_published" || state.Stage == "reconciling" {
		permit, err := enterCatalogPhysicalWrite(ctx, s.db, CatalogPhysicalWriteInput{LibraryID: repair.LibraryID, OwnerKind: CatalogPhysicalRepair, OwnerID: repair.ID, Job: claim, ActorID: repair.OwnerID})
		if err != nil {
			return fail(err)
		}
		defer quiesceCatalogPhysicalWrite(s.db, permit, s.log)
		if err := s.finalizeCatalogStructureRepair(ctx, repair, plan, &state, claim, permit); err != nil {
			return fail(err)
		}
		return WorkerResult{}
	}
	var library models.MediaLibrary
	var storage models.Storage
	if err := s.withCatalogRead(ctx, repair.LibraryID, func(tx *gorm.DB, _ *CatalogReader) error {
		if err := s.validateCatalogStructureExecutionTx(tx, repair, state, claim, true); err != nil {
			return err
		}
		if err := tx.First(&library, repair.LibraryID).Error; err != nil {
			return err
		}
		return tx.First(&storage, library.StorageID).Error
	}); err != nil {
		return fail(err)
	}
	if mediaLibraryRequiresArtifacts(storage.Type, library, true) && s.artifacts == nil {
		return fail(ErrCatalogInvalid)
	}
	backend, err := s.backends.Get(storage.Type)
	if err != nil {
		return fail(err)
	}
	if err := s.validateStructureSelectionSafety(ctx, plan); err != nil {
		return fail(err)
	}
	if err := s.structureCatalogWriteTx(ctx, func(tx *gorm.DB) error {
		if err := s.validateCatalogStructureExecutionTx(tx, repair, state, claim, true); err != nil {
			return err
		}
		return persistCatalogStructureStateTx(tx, repair.ID, state, map[string]any{"phase": "executing", "last_error_code": ""})
	}); err != nil {
		return fail(err)
	}
	prepared := catalogStructurePrepared{}
	defer func() { s.abandonCatalogStructureCandidate(&prepared) }()
	lastRenew := time.Now()
	check := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.withCatalogRead(ctx, repair.LibraryID, func(tx *gorm.DB, _ *CatalogReader) error {
			return s.validateCatalogStructureExecutionTx(tx, repair, state, claim, true)
		}); err != nil {
			return err
		}
		if prepared.Token != "" && time.Since(lastRenew) > 30*time.Second {
			if err := s.catalogStore.RenewCandidate(ctx, prepared.Candidate.ID, prepared.Token, 2*time.Minute); err != nil {
				return err
			}
			lastRenew = time.Now()
		}
		return nil
	}
	boundary := StructureBoundary{Library: library, Storage: storage, beforeMutation: check}
	facts, err := s.catalogStructureMutationFacts(ctx, repair, state, plan, nil)
	if err != nil {
		return fail(err)
	}
	// Budget the largest provider parent identity before preparing any directory.
	if storage.Type != models.StorageTypeLocal {
		for i := range facts.SourceAssets {
			if !facts.SourceAssets[i].Tombstone {
				facts.SourceAssets[i].ParentProviderID = strings.Repeat("x", 128)
			}
		}
	}
	prepared, err = s.prepareCatalogStructureCandidate(ctx, repair, state, claim, facts)
	if err != nil {
		return fail(err)
	}
	permit, err := enterCatalogPhysicalWrite(ctx, s.db, CatalogPhysicalWriteInput{LibraryID: repair.LibraryID, OwnerKind: CatalogPhysicalRepair, OwnerID: repair.ID, Job: claim, ActorID: repair.OwnerID})
	if err != nil {
		return fail(err)
	}
	defer quiesceCatalogPhysicalWrite(s.db, permit, s.log)
	if storage.Type != models.StorageTypeLocal && !physicalDone && state.Stage != "physical_running" && len(plan.Items) > 0 {
		state.Stage = "directories_preparing"
		if err := s.structureCatalogWriteTx(ctx, func(tx *gorm.DB) error {
			if err := s.validateCatalogStructureExecutionTx(tx, repair, state, claim, true); err != nil {
				return err
			}
			return persistCatalogStructureStateTx(tx, repair.ID, state, nil)
		}); err != nil {
			return fail(err)
		}
	}
	parents, err := s.prepareCatalogStructureDirectories(ctx, repair, state, plan, boundary, backend, claim)
	if err != nil {
		return fail(err)
	}
	boundary.preparedParents = parents
	if storage.Type != models.StorageTypeLocal && len(plan.Items) > 0 {
		if !physicalDone && state.Stage != "physical_running" {
			state.Stage = "directories_prepared"
		}
		if err := s.structureCatalogWriteTx(ctx, func(tx *gorm.DB) error {
			if err := s.validateCatalogStructureExecutionTx(tx, repair, state, claim, true); err != nil {
				return err
			}
			return persistCatalogStructureStateTx(tx, repair.ID, state, nil)
		}); err != nil {
			return fail(err)
		}
		facts, err = s.catalogStructureMutationFacts(ctx, repair, state, plan, parents)
		if err != nil {
			return fail(err)
		}
		s.abandonCatalogStructureCandidate(&prepared)
		prepared, err = s.prepareCatalogStructureCandidate(ctx, repair, state, claim, facts)
		if err != nil {
			return fail(err)
		}
	}
	if !physicalDone {
		state.Stage = "physical_running"
		if err := s.structureCatalogWriteTx(ctx, func(tx *gorm.DB) error {
			if err := s.validateCatalogStructureExecutionTx(tx, repair, state, claim, true); err != nil {
				return err
			}
			return persistCatalogStructureStateTx(tx, repair.ID, state, nil)
		}); err != nil {
			return fail(err)
		}
		execution := s.executeStructureRepairItems(ctx, runtime, repair, plan, boundary, backend, claim)
		if execution.GlobalErr != nil {
			if execution.RetryAt != nil {
				_ = s.structureRepairCheckpointTx(ctx, repair, claim, func(tx *gorm.DB) error {
					return tx.Model(&models.MediaLibraryStructureRepair{}).Where("id = ?", repair.ID).Updates(map[string]any{"phase": "queued", "last_error_code": execution.GlobalCode, "updated_at": time.Now().UTC()}).Error
				})
				return WorkerResult{RetryAt: execution.RetryAt, ErrorCode: execution.GlobalCode, ErrorMessage: "云盘正在风控恢复，已保存成功项进度"}
			}
			return fail(execution.GlobalErr)
		}
		originalTotal := len(plan.Items) + len(plan.RecycleItems)
		state.FailedItems, state.BlockedItems, state.OriginalTotalItems = execution.Failed, execution.Blocked, originalTotal
		if execution.Failed+execution.Blocked > 0 {
			// Keep the old bound Catalog visible and retain its binding while the
			// physical filesystem is only partially converged. A retry reuses the
			// exact frozen plan, skips succeeded checkpoints, and publishes once
			// every operation has succeeded. This never publishes invented paths.
			s.abandonCatalogStructureCandidate(&prepared)
			state.Stage = "physical_partial"
			if err := s.structureCatalogWriteTx(ctx, func(tx *gorm.DB) error {
				if err := s.validateCatalogStructureExecutionTx(tx, repair, state, claim, true); err != nil {
					return err
				}
				return persistCatalogStructureStateTx(tx, repair.ID, state, map[string]any{"phase": "failed", "last_error_code": CodeMediaLibraryStructureApplyFailed})
			}); err != nil {
				return fail(err)
			}
			return WorkerResult{ErrorCode: CodeMediaLibraryStructureApplyFailed, ErrorMessage: "目录整理已部分完成；失败项已隔离，重试只会处理剩余项"}
		}
		plan = execution.Plan
		// Candidates prepared before physical work budget the full authorized
		// plan only. Rebuild from checked successes before publication.
		s.abandonCatalogStructureCandidate(&prepared)
		facts, err = s.catalogStructureMutationFacts(ctx, repair, state, plan, parents)
		if err != nil {
			return fail(err)
		}
		prepared, err = s.prepareCatalogStructureCandidate(ctx, repair, state, claim, facts)
		if err != nil {
			return fail(err)
		}
		state.Stage = "physical_completed"
		if err := s.structureCatalogWriteTx(ctx, func(tx *gorm.DB) error {
			if err := s.validateCatalogStructureExecutionTx(tx, repair, state, claim, true); err != nil {
				return err
			}
			return persistCatalogStructureStateTx(tx, repair.ID, state, nil)
		}); err != nil {
			return fail(err)
		}
	}
	if err := s.publishCatalogStructureRepair(ctx, repair, plan, &state, claim, &prepared); err != nil {
		return fail(err)
	}
	if err := s.finalizeCatalogStructureRepair(ctx, repair, plan, &state, claim, permit); err != nil {
		return fail(err)
	}
	return WorkerResult{}
}

func (s *MediaLibraryStructureService) publishCatalogStructureRepair(ctx context.Context, repair models.MediaLibraryStructureRepair, plan StructurePlan, state *structureCatalogRepairState, claim *ClaimedJob, prepared *catalogStructurePrepared) error {
	for attempt := 0; attempt < 3; attempt++ {
		nextState := *state
		err := s.structureCatalogWriteTx(ctx, func(tx *gorm.DB) error {
			guard := func(tx *gorm.DB) error { return s.validateCatalogStructureExecutionTx(tx, repair, *state, claim, true) }
			if prepared.Token != "" {
				if _, err := s.catalogStore.PublishTx(tx, prepared.Candidate.ID, prepared.Token, prepared.Head.Revision, guard); err != nil {
					return err
				}
			} else if err := guard(tx); err != nil {
				return err
			}
			var library models.MediaLibrary
			if err := tx.First(&library, repair.LibraryID).Error; err != nil {
				return err
			}
			if len(plan.Items)+len(plan.RecycleItems) > 0 {
				generation := max(max(library.DirtyGeneration, library.ArtifactGeneration), library.BaselineGeneration) + 1
				if err := tx.Model(&library).Update("dirty_generation", generation).Error; err != nil {
					return err
				}
				kind := models.MediaLibraryChangeCatalog
				if len(plan.Items) == 0 {
					kind = models.MediaLibraryChangeRemoval
				}
				change, err := s.changes.RecordTx(tx, library.ID, generation, kind, false)
				if err != nil {
					return err
				}
				nextState.PublishedContentRevision, nextState.ArtifactGeneration = change.Revision, generation
			} else {
				nextState.PublishedContentRevision = library.ContentRevision
			}
			nextState.Stage = "catalog_published"
			return persistCatalogStructureStateTx(tx, repair.ID, nextState, map[string]any{"phase": "reconciling"})
		})
		if err == nil {
			*state = nextState
			prepared.Token = ""
			return nil
		}
		// An unrelated logical edit remains a hard refusal. A storage-only
		// compaction can rebase the already-approved exact facts without replay.
		if !errors.Is(err, ErrCatalogFence) {
			return err
		}
		if err := s.withCatalogRead(ctx, repair.LibraryID, func(tx *gorm.DB, _ *CatalogReader) error {
			return s.validateCatalogStructureExecutionTx(tx, repair, *state, claim, true)
		}); err != nil {
			return err
		}
		facts := prepared.Facts
		s.abandonCatalogStructureCandidate(prepared)
		var prepareErr error
		*prepared, prepareErr = s.prepareCatalogStructureCandidate(ctx, repair, *state, claim, facts)
		if prepareErr != nil {
			return prepareErr
		}
	}
	return ErrCatalogFence
}
