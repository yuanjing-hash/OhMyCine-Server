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
		code, message := reorganizationWriteError(err)
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
	if storage.Type == models.StorageTypePan115 {
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
	connection, driver, err := w.service.connections.driver(*storage.ConnectionID)
	if err != nil {
		return err
	}
	mutation, ok := driver.(cloudpkg.MutationDriver)
	if !ok || connection.Provider != cloudpkg.ProviderPan115 {
		return ErrCatalogInvalid
	}
	root, err := providerItemWithinRoot(ctx, driver, library.ProviderRootID, storage.RootPath)
	if err != nil || !root.IsDir {
		return ErrCatalogFence
	}
	if state.Catalog.Directories == nil {
		state.Catalog.Directories = map[string]string{}
	}
	state.Catalog.Directories["."] = root.ID
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
				if err != nil || !current.IsDir || current.ParentID != parent || current.Name != segment {
					return ErrCatalogFence
				}
				parent = known
				continue
			}
			children, err := listCloudDirectory(ctx, driver, parent)
			if err != nil {
				return err
			}
			matches := namedCloudItems(children, segment)
			if len(matches) > 1 || len(matches) == 1 && !matches[0].IsDir {
				return ErrCatalogFence
			}
			var child cloudpkg.Item
			if len(matches) == 1 {
				child = matches[0]
			} else {
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
			if err := w.persistCatalogReorganizationState(ctx, task, plan, *state, claim); err != nil {
				return err
			}
			parent = child.ID
		}
	}
	return nil
}

func (w *MediaReorganizationWorker) executeCatalogReorganization(ctx context.Context, runtime JobRuntime, task models.MediaReorganizationTask, library models.MediaLibrary, storage models.Storage, plan reorganizationPlan, state *reorganizationState, claim ClaimedJob, facts CatalogFactBatch) error {
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
	case models.StorageTypePan115:
		if storage.ConnectionID == nil || w.service.connections == nil {
			return ErrCatalogFence
		}
		connection, d, err := w.service.connections.driver(*storage.ConnectionID)
		if err != nil {
			return err
		}
		var ok bool
		mutation, ok = d.(cloudpkg.MutationDriver)
		if !ok || connection.Provider != cloudpkg.ProviderPan115 {
			return ErrCatalogInvalid
		}
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
			}
		} else {
			current, err := providerItemWithinRoot(ctx, driver, item.ProviderItemID, root)
			if err != nil || current.ID != item.ProviderItemID || cloudReorganizationSourceChanged(item, current) {
				return ErrCatalogFence
			}
			parent := state.Catalog.Directories[reorganizationParentPath(item.NewRelativePath)]
			if parent == "" {
				return ErrCatalogFence
			}
			parentFact, err := providerItemWithinRoot(ctx, driver, parent, root)
			if err != nil || !parentFact.IsDir {
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
				if current.ParentID != parent {
					if err := mutation.Move(ctx, current.ID, parent); err != nil {
						return appError(CodeReorganizationUnavailable, "115 移动失败", nil)
					}
				}
				if err := w.validateCatalogItemExecution(ctx, task, plan, *state, claim, item.ManagedItemID); err != nil {
					return err
				}
				if current.Name != name {
					if err := mutation.Rename(ctx, current.ID, name); err != nil {
						return appError(CodeReorganizationUnavailable, "115 重命名失败", nil)
					}
				}
				current, err = driver.Stat(ctx, current.ID)
				if err != nil || current.ParentID != parent || current.Name != name || cloudReorganizationSourceChanged(item, current) {
					return ErrCatalogFence
				}
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
			nextCatalog := *state.Catalog
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
