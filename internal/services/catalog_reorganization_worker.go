package services

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	pathpkg "path"
	"path/filepath"
	"strings"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/medialibrary"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	storagefs "github.com/yuanjing-hash/OhMyCine-Server/internal/storage"
	cloudpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"gorm.io/gorm"
)

func equalReorganizationUintPtr(a, b *uint) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}
func reorganizationParentPath(relative string) string {
	return pathpkg.Dir(strings.TrimPrefix(relative, "/"))
}

func (w *MediaReorganizationWorker) validateCatalogExecution(ctx context.Context, task models.MediaReorganizationTask, plan reorganizationPlan, state reorganizationState, claim ClaimedJob) error {
	return w.validateCatalogItemExecution(ctx, task, plan, state, claim, 0)
}

func (w *MediaReorganizationWorker) validateCatalogItemExecution(ctx context.Context, task models.MediaReorganizationTask, plan reorganizationPlan, state reorganizationState, claim ClaimedJob, itemID uint) error {
	return w.service.catalogRead(ctx, task.LibraryID, func(tx *gorm.DB, _ *CatalogReader) error {
		return w.validateCatalogExecutionGuardTx(tx, task, plan, state, claim, &itemID)
	})
}

func (w *MediaReorganizationWorker) runVersioned(ctx context.Context, runtime JobRuntime, claim ClaimedJob, task models.MediaReorganizationTask, download models.DownloadTask, library models.MediaLibrary, storage models.Storage, plan reorganizationPlan, state reorganizationState) WorkerResult {
	fail := func(err error) WorkerResult {
		if result, waiting := w.quarkReorganizationWait(ctx, task, plan, state, claim, err); waiting {
			return result
		}
		code, message := reorganizationWriteError(err)
		if storage.Type == models.StorageTypeQuark {
			if safeCode, safeMessage, ok := quarkReorganizationFailure(err); ok {
				code, message = safeCode, safeMessage
			}
			if state.Catalog != nil && state.Catalog.ObservationHoldVersion == 1 && (errors.Is(err, ErrCatalogFence) || errors.Is(err, ErrCatalogInvalid)) {
				message = "媒体、来源或任务已变化；已保留原操作与观察暂停状态，请恢复原任务条件后核对，不要重复提交"
			}
		}
		// A lost lease cannot write failure over a successor's state.
		if w.service.queue != nil {
			var admission *CatalogWriteAdmission
			if w.service.libraries != nil && w.service.libraries.catalogStore != nil {
				admission = w.service.libraries.catalogStore.Admission()
			}
			_ = withBackgroundTransaction(ctx, w.service.db, admission, func(tx *gorm.DB) error {
				if _, e := w.service.queue.verifyLease(tx, claim.Job.ID, claim.LeaseToken); e != nil {
					return e
				}
				return tx.Model(&models.MediaReorganizationTask{}).Where("id=? AND job_id=? AND phase<>?", task.ID, claim.Job.ID, models.MediaReorganizationPhaseCompleted).Updates(map[string]any{"phase": models.MediaReorganizationPhaseFailed, "last_error_code": code, "updated_at": time.Now().UTC()}).Error
			})
		}
		return WorkerResult{ErrorCode: code, ErrorMessage: message}
	}
	if w.service.libraries == nil || w.service.libraries.catalogStore == nil || state.Catalog == nil || plan.CatalogFence == nil {
		return fail(ErrCatalogFence)
	}
	if state.Catalog.Stage == "reconciling" {
		permit, err := enterCatalogPhysicalWrite(ctx, w.service.db, CatalogPhysicalWriteInput{LibraryID: library.ID, OwnerKind: CatalogPhysicalReorganization, OwnerID: task.ID, Job: &claim})
		if err != nil {
			return fail(err)
		}
		defer quiesceCatalogPhysicalWrite(w.service.db, permit, w.service.log)
		if err := w.reconcileCatalogReorganization(ctx, task, library, storage, plan, state, claim, permit); err != nil {
			return fail(err)
		}
		return WorkerResult{}
	}
	p, err := w.prepareReorganization(ctx, task, plan, state, claim)
	defer func() { w.abandonReorganization(&p) }()
	if err != nil {
		return fail(err)
	}
	permit, err := enterCatalogPhysicalWrite(ctx, w.service.db, CatalogPhysicalWriteInput{LibraryID: library.ID, OwnerKind: CatalogPhysicalReorganization, OwnerID: task.ID, Job: &claim})
	if err != nil {
		return fail(err)
	}
	defer quiesceCatalogPhysicalWrite(w.service.db, permit, w.service.log)
	// Durable before even mkdir: absence is meaningful only with the explicit
	// proof version. A crash between a physical operation and its checkpoint
	// must never permit history cleanup to discard the only recovery evidence.
	state.Catalog.ExecutionProofVersion = 1
	state.Catalog.ExecutionStarted = true
	if err := w.persistCatalogReorganizationState(ctx, task, plan, state, claim); err != nil {
		return fail(err)
	}
	// Provider parents may require mkdir, but only after the complete tentative
	// candidate (including collection projection) fits all publication budgets.
	if storage.Type == models.StorageTypePan115 || storage.Type == models.StorageTypeQuark {
		if err := w.prepareReorganizationDirectories(ctx, task, library, storage, plan, &state, claim); err != nil {
			return fail(err)
		}
		w.abandonReorganization(&p)
		p, err = w.prepareReorganization(ctx, task, plan, state, claim)
		if err != nil {
			return fail(err)
		}
	}
	if err := w.persistCatalogReorganizationState(ctx, task, plan, state, claim); err != nil {
		return fail(err)
	}
	if err := w.executeCatalogReorganization(ctx, runtime, task, library, storage, plan, &state, claim, p.Facts); err != nil {
		return fail(err)
	}
	if err := w.verifyQuarkReorganizationDirectories(ctx, task, library, storage, plan, &state, claim); err != nil {
		return fail(err)
	}
	// Rebuild only for physical compaction/expired candidate lease. Bound facts
	// stay exact; any logical content or source change still refuses publication.
	w.abandonReorganization(&p)
	p, err = w.prepareReorganization(ctx, task, plan, state, claim)
	if err != nil {
		return fail(err)
	}
	if err := w.finalizeCatalogReorganization(ctx, task, download, library, storage, plan, state, claim, &p, permit); err != nil {
		return fail(err)
	}
	return WorkerResult{}
}

func (w *MediaReorganizationWorker) prepareReorganizationDirectories(ctx context.Context, task models.MediaReorganizationTask, library models.MediaLibrary, storage models.Storage, plan reorganizationPlan, state *reorganizationState, claim ClaimedJob) error {
	if storage.ConnectionID == nil || w.service.connections == nil {
		return ErrCatalogFence
	}
	driver, mutation, err := w.service.reorganizationCloudDriver(ctx, library, storage, task.ID)
	if err != nil {
		return err
	}
	root, err := providerItemWithinRoot(ctx, driver, library.ProviderRootID, storage.RootPath)
	if err != nil {
		return reorganizationSourceReadError(storage, err)
	}
	if !root.IsDir {
		return ErrCatalogFence
	}
	if state.Catalog.Directories == nil {
		state.Catalog.Directories = map[string]string{}
	}
	state.Catalog.Directories["."] = root.ID
	if storage.Type == models.StorageTypeQuark {
		rememberQuarkReorganizationFact(state, root)
	}
	for _, item := range plan.Items {
		parent, walked := root.ID, "."
		for _, segment := range strings.Split(reorganizationParentPath(item.NewRelativePath), "/") {
			if segment == "." || segment == "" {
				continue
			}
			if segment == ".." {
				return ErrCatalogInvalid
			}
			walked = pathpkg.Join(walked, segment)
			if err := w.validateCatalogExecution(ctx, task, plan, *state, claim); err != nil {
				return err
			}
			if known := state.Catalog.Directories[walked]; known != "" {
				current, err := driver.Stat(ctx, known)
				if err != nil {
					return reorganizationSourceReadError(storage, err)
				}
				if !current.IsDir || current.ParentID != parent || current.Name != segment {
					return ErrCatalogFence
				}
				if storage.Type == models.StorageTypeQuark {
					rememberQuarkReorganizationFact(state, current)
				}
				parent = known
				continue
			}
			children, err := listCloudDirectory(ctx, driver, parent)
			if err != nil {
				return err
			}
			matches := namedCloudItems(children, segment)
			pending := state.Catalog.QuarkMutation
			resumeCreate := storage.Type == models.StorageTypeQuark && pending != nil && pending.Kind == "mkdir" && pending.DirectoryPath == walked
			if !resumeCreate && (len(matches) > 1 || len(matches) == 1 && !matches[0].IsDir) {
				return ErrCatalogFence
			}
			var child cloudpkg.Item
			if len(matches) == 1 && !resumeCreate {
				child = matches[0]
			} else {
				if err := w.beginQuarkReorganizationMutation(ctx, task, plan, state, claim, quarkReorganizationMutation{Kind: "mkdir", ParentID: parent, Name: segment, DirectoryPath: walked}); err != nil {
					return err
				}
				child, err = mutation.CreateDirectory(ctx, parent, segment)
				if err != nil {
					return err
				}
			}
			if child.ID == "" || !child.IsDir || child.ParentID != parent || child.Name != segment {
				return ErrCatalogFence
			}
			if _, err := providerItemWithinRoot(ctx, driver, child.ID, root.ID); err != nil {
				return err
			}
			state.Catalog.Directories[walked] = child.ID
			if storage.Type == models.StorageTypeQuark {
				rememberQuarkReorganizationFact(state, child)
				if state.Catalog.QuarkMutation != nil && state.Catalog.QuarkMutation.Kind == "mkdir" && state.Catalog.QuarkMutation.DirectoryPath == walked {
					state.Catalog.QuarkMutation = nil
				}
			}
			if err := w.persistCatalogReorganizationState(ctx, task, plan, *state, claim); err != nil {
				return err
			}
			parent = child.ID
		}
	}
	return nil
}

func (w *MediaReorganizationWorker) executeCatalogReorganization(ctx context.Context, runtime JobRuntime, task models.MediaReorganizationTask, library models.MediaLibrary, storage models.Storage, plan reorganizationPlan, state *reorganizationState, claim ClaimedJob, facts CatalogFactBatch) (err error) {
	var active *reorganizationPlanItem
	activeOutcome := "failed"
	defer func() {
		if err != nil && active != nil {
			recordReorganizationFile(ctx, w.service.log, *active, activeOutcome, "本文件操作或后续核验未全部完成；已受理和已确认阶段以此前记录为准，不重复推断成功")
		}
	}()
	modified := map[string]time.Time{}
	for _, f := range facts.Entries {
		modified[strings.TrimPrefix(f.RelativePath, "/")] = f.ModifiedAt
	}
	for _, f := range facts.SourceAssets {
		modified[strings.TrimPrefix(f.RelativePath, "/")] = f.ModifiedAt
	}
	var root string
	var driver cloudpkg.Driver
	var mutation cloudpkg.MutationDriver
	switch storage.Type {
	case models.StorageTypeLocal:
		var err error
		root, err = medialibrary.ResolveRoot(storage.RootPath, library.RelativeRoot)
		if err != nil {
			return err
		}
		root, err = (storagefs.LocalDriver{}).CanonicalizeRoot(root)
		if err != nil {
			return err
		}
	case models.StorageTypePan115, models.StorageTypeQuark:
		if storage.ConnectionID == nil || w.service.connections == nil {
			return ErrCatalogFence
		}
		d, m, err := w.service.reorganizationCloudDriver(ctx, library, storage, task.ID)
		if err != nil {
			return err
		}
		mutation = m
		driver = d
		root = library.ProviderRootID
	default:
		return ErrCatalogInvalid
	}
	for _, item := range plan.Items {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := w.validateCatalogItemExecution(ctx, task, plan, *state, claim, item.ManagedItemID); err != nil {
			return err
		}
		if item.OldRelativePath != item.NewRelativePath {
			active = &item
			activeOutcome = "failed"
		} else {
			active = nil
		}
		if storage.Type == models.StorageTypeLocal {
			oldPath, newPath := filepath.Join(root, filepath.FromSlash(item.OldRelativePath)), filepath.Join(root, filepath.FromSlash(item.NewRelativePath))
			if ensureWithin(root, oldPath) != nil || ensureWithin(root, newPath) != nil || ensureSafeDirectoryPath(root, filepath.Dir(oldPath), false) != nil || ensureSafeDirectoryPath(root, filepath.Dir(newPath), true) != nil {
				return ErrCatalogFence
			}
			oldInfo, oldErr := os.Lstat(oldPath)
			newInfo, newErr := os.Lstat(newPath)
			valid := func(info os.FileInfo) bool {
				return info != nil && info.Mode().IsRegular() && info.Size() == item.Size && (modified[item.NewRelativePath].IsZero() || modified[item.NewRelativePath].Equal(info.ModTime()))
			}
			if item.OldRelativePath == item.NewRelativePath {
				if oldErr != nil || !valid(oldInfo) {
					return ErrCatalogFence
				}
			} else if errors.Is(oldErr, os.ErrNotExist) && newErr == nil && valid(newInfo) {
				// Exact known size+mtime is required, never size-only adoption.
				if modified[item.NewRelativePath].IsZero() {
					return ErrCatalogFence
				}
			} else {
				if state.Completed[item.ManagedItemID] || oldErr != nil || !valid(oldInfo) {
					return ErrCatalogFence
				}
				if newErr == nil {
					return appError(CodeReorganizationConflict, "目标已存在，请重新预览", nil)
				}
				if !errors.Is(newErr, os.ErrNotExist) {
					return newErr
				}
				if err := os.Rename(oldPath, newPath); err != nil {
					return appError(CodeReorganizationUnavailable, "移动托管文件失败", nil)
				}
				activeOutcome = "partial"
				recordReorganizationFile(ctx, w.service.log, item, "confirmed", "本地移动与目标文件名已确认；后续只提交既有目录结果")
			}
		} else {
			current, err := providerItemWithinRoot(ctx, driver, item.ProviderItemID, root)
			if err != nil {
				return reorganizationSourceReadError(storage, err)
			}
			if current.ID != item.ProviderItemID || cloudReorganizationSourceChanged(item, current) {
				return ErrCatalogFence
			}
			parent := state.Catalog.Directories[reorganizationParentPath(item.NewRelativePath)]
			if parent == "" {
				return ErrCatalogFence
			}
			parentFact, err := providerItemWithinRoot(ctx, driver, parent, root)
			if err != nil {
				return reorganizationSourceReadError(storage, err)
			}
			if !parentFact.IsDir {
				return ErrCatalogFence
			}
			name := pathpkg.Base(item.NewRelativePath)
			if item.NewRelativePath == item.OldRelativePath {
				if current.ParentID != item.ProviderParentID || current.Name != name {
					return ErrCatalogFence
				}
			} else {
				if current.ParentID != item.ProviderParentID && current.ParentID != parent {
					return ErrCatalogFence
				}
				if current.Name != pathpkg.Base(item.OldRelativePath) && current.Name != name {
					return ErrCatalogFence
				}
				if err := ensureReorganizationCloudTargetAvailable(ctx, driver, parent, name, current.ID); err != nil {
					return err
				}
				pending := state.Catalog.QuarkMutation
				resumeMove := storage.Type == models.StorageTypeQuark && pending != nil && pending.Kind == "move" && pending.ItemID == current.ID
				if current.ParentID != parent || resumeMove {
					if err := w.beginQuarkReorganizationMutation(ctx, task, plan, state, claim, quarkReorganizationMutation{Kind: "move", ItemID: current.ID, ParentID: parent}); err != nil {
						return err
					}
					activeOutcome = "uncertain"
					if err := mutation.Move(ctx, current.ID, parent); err != nil {
						if storage.Type == models.StorageTypeQuark {
							return err
						}
						return appError(CodeReorganizationUnavailable, "115 移动失败", nil)
					}
					if err := w.finishQuarkReorganizationMutation(ctx, task, plan, state, claim); err != nil {
						return err
					}
					activeOutcome = "partial"
					recordReorganizationFile(ctx, w.service.log, item, "accepted", "云端移动请求已受理；此时尚未读取最终位置，不能提前记为完整成功")
				}
				if err := w.validateCatalogItemExecution(ctx, task, plan, *state, claim, item.ManagedItemID); err != nil {
					return err
				}
				pending = state.Catalog.QuarkMutation
				resumeRename := storage.Type == models.StorageTypeQuark && pending != nil && pending.Kind == "rename" && pending.ItemID == current.ID
				if current.Name != name || resumeRename {
					if err := w.beginQuarkReorganizationMutation(ctx, task, plan, state, claim, quarkReorganizationMutation{Kind: "rename", ItemID: current.ID, Name: name}); err != nil {
						return err
					}
					if activeOutcome != "partial" {
						activeOutcome = "uncertain"
					}
					if err := mutation.Rename(ctx, current.ID, name); err != nil {
						if storage.Type == models.StorageTypeQuark {
							return err
						}
						return appError(CodeReorganizationUnavailable, "115 重命名失败", nil)
					}
					if err := w.finishQuarkReorganizationMutation(ctx, task, plan, state, claim); err != nil {
						return err
					}
					recordReorganizationFile(ctx, w.service.log, item, "accepted", "云端重命名请求已受理，继续既有最终状态核验")
				}
				current, err = driver.Stat(ctx, current.ID)
				if err != nil {
					return reorganizationSourceReadError(storage, err)
				}
				if current.ID != item.ProviderItemID || current.ParentID != parent || current.Name != name || cloudReorganizationSourceChanged(item, current) {
					return ErrCatalogFence
				}
				recordReorganizationFile(ctx, w.service.log, item, "confirmed", "现有状态核验已确认云端目标目录、文件名、大小与内容身份")
			}
			if storage.Type == models.StorageTypeQuark {
				rememberQuarkReorganizationFact(state, current)
			}
		}
		state.Completed[item.ManagedItemID] = true
		if err := w.persistCatalogReorganizationState(ctx, task, plan, *state, claim); err != nil {
			return err
		}
		if err := heartbeatReorganization(runtime, len(state.Completed), len(plan.Items)); err != nil {
			return err
		}
	}
	return nil
}

func (w *MediaReorganizationWorker) finalizeCatalogReorganization(ctx context.Context, task models.MediaReorganizationTask, download models.DownloadTask, library models.MediaLibrary, storage models.Storage, plan reorganizationPlan, state reorganizationState, claim ClaimedJob, p *reorganizationPrepared, permit CatalogPhysicalWritePermit) error {
	if len(state.Completed) != len(plan.Items) {
		return ErrCatalogFence
	}
	libraries := w.service.libraries
	now := time.Now().UTC()
	var target MediaIdentitySnapshot
	if decodeStrictJSON(task.TargetIdentityJSON, &target) != nil {
		return ErrCatalogInvalid
	}
	generation := max(max(library.DirtyGeneration, library.ArtifactGeneration), library.BaselineGeneration) + 1
	err := libraries.catalogStore.Admission().WithForeground(ctx, func() error {
		return w.service.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			stateOnly := uint(0)
			if storage.Type == models.StorageTypeQuark && state.Catalog.ObservationHoldVersion == 0 {
				// A metadata-only plan may have made no cloud mutation. It needs no
				// durable hold during reads; establish and release it atomically with
				// publication, so a failed preflight never freezes the observer.
				if err := w.validateCatalogExecutionGuardTx(tx, task, plan, state, claim, &stateOnly); err != nil {
					return err
				}
				publicationState := *state.Catalog
				publicationState.ObservationHoldVersion = 1
				state.Catalog = &publicationState
				raw, err := json.Marshal(state)
				if err != nil {
					return err
				}
				if len(raw) > CatalogBatchBytes {
					return ErrCatalogBudget
				}
				if err := tx.Model(&models.MediaReorganizationTask{}).Where("id=?", task.ID).Update("state_json", string(raw)).Error; err != nil {
					return err
				}
			}
			if _, err := libraries.catalogStore.PublishTx(tx, p.Candidate.ID, p.Token, p.Head.Revision, func(tx *gorm.DB) error {
				return w.validateCatalogExecutionGuardTx(tx, task, plan, state, claim, &stateOnly)
			}); err != nil {
				return err
			}
			result := tx.Model(&models.DownloadTask{}).Where("id=? AND identity_revision=?", download.ID, task.SourceIdentityRevision).Updates(map[string]any{"identity_source": mediaIdentitySourceManual, "identity_status": mediaIdentityStatusVerified, "identity_locked": true, "identity_revision": target.Revision, "identity_snapshot_json": task.TargetIdentityJSON, "recognition_override_tmdb_id": target.TMDBID, "recognition_override_media_type": target.MediaType, "scrape_tmdb_id": target.TMDBID, "scrape_media_type": target.MediaType, "scrape_title": target.Title, "scrape_year": target.Year, "updated_at": now})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return ErrCatalogFence
			}
			if err := tx.Model(&models.MediaLibrary{}).Where("id=?", library.ID).Updates(map[string]any{"dirty_generation": generation, "updated_at": now}).Error; err != nil {
				return err
			}
			if mediaLibraryRequiresArtifacts(storage.Type, library, libraries.artifacts != nil) {
				if _, err := libraries.artifacts.BindCatalogGenerationChangesTx(tx, library.ID, generation, p.ArtifactChanges); err != nil {
					return err
				}
			}
			if libraries.changes != nil {
				var err error
				_, err = libraries.changes.RecordTx(tx, library.ID, generation, models.MediaLibraryChangeMetadata, false)
				if err != nil {
					return err
				}
			} else {
				if err := tx.Model(&models.MediaLibrary{}).Where("id=?", library.ID).Update("content_revision", gorm.Expr("content_revision + 1")).Error; err != nil {
					return err
				}
			}
			var published models.MediaLibrary
			if err := tx.First(&published, library.ID).Error; err != nil {
				return err
			}
			if storage.Type == models.StorageTypeQuark {
				if state.Catalog.QuarkMutation != nil {
					return ErrCatalogFence
				}
				if err := ConfirmQuarkReorganizationObservationTx(tx, task, plan, state, quarkReorganizationFacts(state)); err != nil {
					return err
				}
			}
			nextCatalog := *state.Catalog
			nextCatalog.ObservationHoldVersion = 0
			nextCatalog.Stage = "reconciling"
			nextCatalog.ManagedRevision = plan.ManagedRevision
			nextCatalog.PublishedContentRevision = published.ContentRevision
			nextCatalog.ArtifactGeneration = generation
			state.Catalog = &nextCatalog
			raw, err := json.Marshal(state)
			if err != nil {
				return err
			}
			if err := tx.Model(&models.MediaReorganizationTask{}).Where("id=?", task.ID).Updates(map[string]any{"state_json": string(raw), "phase": models.MediaReorganizationPhaseReconciling, "processed_items": len(plan.Items), "last_error_code": "", "updated_at": now}).Error; err != nil {
				return err
			}
			return nil
		})
	})
	if err != nil {
		return err
	}
	p.Token = ""
	w.reorganizationEvent(ctx, task, "reorganization.catalog.published", "publish", "confirmed", "受控文件调整已提交到媒体目录；元数据产物及媒体刷新仍按原有后续阶段处理", map[string]any{"published_count": len(plan.Items), "generation": generation})
	return w.reconcileCatalogReorganization(ctx, task, library, storage, plan, state, claim, permit)
}

func publishReorganizationManagedManifestTx(tx *gorm.DB, task models.MediaReorganizationTask, plan reorganizationPlan, state reorganizationState, revision uint64, now time.Time) error {
	if len(plan.Items) == 0 || len(plan.Items) > CatalogBatchRows {
		return ErrCatalogBudget
	}
	for start := 0; start < len(plan.Items); start += CatalogBatchRows {
		part := plan.Items[start:min(start+CatalogBatchRows, len(plan.Items))]
		ids := make([]uint, 0, len(part))
		paths, parents := "CASE id", "CASE id"
		pathArgs, parentArgs := []any{}, []any{}
		for _, item := range part {
			ids = append(ids, item.ManagedItemID)
			paths += " WHEN ? THEN ?"
			pathArgs = append(pathArgs, item.ManagedItemID, item.NewRelativePath)
			parent := item.ProviderParentID
			if item.ProviderItemID != "" {
				parent = state.Catalog.Directories[reorganizationParentPath(item.NewRelativePath)]
			}
			parents += " WHEN ? THEN ?"
			parentArgs = append(parentArgs, item.ManagedItemID, parent)
		}
		result := tx.Model(&models.MediaManagedItem{}).Where("id IN ? AND transfer_task_id=? AND library_id=? AND managed=? AND active=?", ids, task.TransferTaskID, task.LibraryID, true, true).Updates(map[string]any{"relative_path": gorm.Expr(paths+" ELSE relative_path END", pathArgs...), "provider_parent_id": gorm.Expr(parents+" ELSE provider_parent_id END", parentArgs...), "identity_revision": revision, "updated_at": now})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != int64(len(part)) {
			return ErrCatalogFence
		}
	}
	return nil
}
