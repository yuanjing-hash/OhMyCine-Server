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
	cloudpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"gorm.io/gorm"
)

var errStructureCancelProgress = errors.New("structure cancellation verification continues")

// This runs only on the original synchronous execution stack after all file
// calls returned. It never resumes the cancelled plan; it only accounts for
// observed outcomes. Pause/shutdown remain resumable and take no such path.
func (s *MediaLibraryStructureService) finishCancelledStructureRepair(permit CatalogPhysicalWritePermit, repair models.MediaLibraryStructureRepair, plan StructurePlan, boundary StructureBoundary, backend MediaLibraryStructureBackend) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var job models.Job
	if err := s.db.WithContext(ctx).First(&job, "id=?", permit.evidence.JobID).Error; err != nil || !artifactJobCancelled(job) {
		return
	}
	quiesceCatalogPhysicalWrite(s.db, permit, s.log)
	if err := s.reconcileCancelledStructureRepair(ctx, permit, repair, plan, boundary, backend); err != nil && !errors.Is(err, errStructureCancelProgress) {
		s.log.Warn().Uint("library_id", repair.LibraryID).Str("error_code", "structure_cancelled_outcome_pending").Msg("已取消整理的部分文件结果尚未确认，保留原任务明细")
	}
}

func (s *MediaLibraryStructureService) reconcileCancelledStructureRepair(ctx context.Context, permit CatalogPhysicalWritePermit, repair models.MediaLibraryStructureRepair, plan StructurePlan, boundary StructureBoundary, backend MediaLibraryStructureBackend) error {
	if s.scanBoundary != nil {
		lock := s.scanBoundary(repair.LibraryID)
		if !lock.TryLock() {
			return ErrCatalogFence
		}
		defer lock.Unlock()
	}
	if err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := validateCancelledStructurePermitTx(tx, permit, repair); err != nil {
			return err
		}
		var current models.MediaLibraryStructureRepair
		if err := tx.First(&current, "id=?", repair.ID).Error; err != nil {
			return err
		}
		repair.StateJSON = current.StateJSON
		return nil
	}); err != nil {
		return err
	}
	// Preserve versioned bindings: their publication owner must account for its
	// exact candidate before it may settle. This adapter is the row Catalog path.
	var state structureCatalogRepairState
	_ = json.Unmarshal([]byte(repair.StateJSON), &state)
	if state.Version != 0 {
		return ErrCatalogFence
	}
	// Leave time to commit an observed prefix even if a provider read times out.
	observeCtx, stopObserve := context.WithTimeout(ctx, 20*time.Second)
	defer stopObserve()
	observe, err := structureFileObserver(observeCtx, boundary, backend)
	if err != nil {
		return err
	}
	applied := plan
	applied.Items, applied.RecycleItems = nil, nil
	var rows []models.MediaLibraryStructureRepairItem
	if err := s.db.WithContext(ctx).Where("repair_id=?", repair.ID).Order("ordinal").Find(&rows).Error; err != nil {
		return err
	}
	initializeRows := len(rows) == 0
	if initializeRows {
		// Cancellation may interrupt checkpoint initialization before the first
		// backend call. These are observations to perform, not no-I/O claims.
		now := time.Now().UTC()
		for index, item := range plan.RecycleItems {
			rows = append(rows, models.MediaLibraryStructureRepairItem{RepairID: repair.ID, Ordinal: index, Action: "recycle", Kind: item.Kind, SourceRelative: safeStructurePath(item.SourceRelative), TargetRelative: safeStructurePath(item.RecycleRelative), Status: structureRepairItemPending, CreatedAt: now, UpdatedAt: now})
		}
		for index, item := range plan.Items {
			rows = append(rows, models.MediaLibraryStructureRepairItem{RepairID: repair.ID, Ordinal: index + len(plan.RecycleItems), Action: "move", Kind: item.Kind, SourceRelative: safeStructurePath(item.SourceRelative), TargetRelative: safeStructurePath(item.TargetRelative), Status: structureRepairItemPending, CreatedAt: now, UpdatedAt: now})
		}
	}
	if len(rows) != len(plan.Items)+len(plan.RecycleItems) {
		return ErrCatalogFence
	}
	var progress struct {
		CancelVerified int `json:"cancel_verified"`
	}
	if err := json.Unmarshal([]byte(repair.StateJSON), &progress); err != nil {
		return err
	}
	if progress.CancelVerified < 0 || progress.CancelVerified > len(rows) {
		return ErrCatalogFence
	}
	var succeeded []int
	next := progress.CancelVerified
	batchStarted := time.Now()
	var observationErr error
	for index := next; index < len(rows) && index < progress.CancelVerified+CatalogBatchRows; index++ {
		row := rows[index]
		success, err := func() (bool, error) {
			if row.Ordinal != index {
				return false, ErrCatalogFence
			}
			var source, target, providerID string
			var size, modified int64
			recycle := index < len(plan.RecycleItems)
			if recycle {
				item := plan.RecycleItems[index]
				source, target, providerID, size, modified = item.SourceRelative, item.RecycleRelative, item.ProviderID, item.Size, item.ModifiedAtUnixNano
			} else {
				item := plan.Items[index-len(plan.RecycleItems)]
				source, target, providerID, size, modified = item.SourceRelative, item.TargetRelative, item.ProviderID, item.Size, item.ModifiedAtUnixNano
			}
			if row.SourceRelative != safeStructurePath(source) {
				return false, ErrCatalogFence
			}
			if row.TargetRelative != safeStructurePath(target) || (recycle && row.Action != "recycle") || (!recycle && row.Action != "move") {
				return false, ErrCatalogFence
			}
			success := row.Status == structureRepairItemSucceeded
			if success && !recycle {
				atTarget, _, err := observe(target, providerID, size, modified)
				if err != nil {
					return false, err
				}
				if !atTarget {
					return false, ErrCatalogFence
				}
			}
			if !success {
				atSource, missingSource, err := observe(source, providerID, size, modified)
				if err != nil {
					return false, err
				}
				if !atSource {
					// Provider recycle exits the library boundary. Absence alone never
					// proves its recycle destination; retain an ambiguous receipt.
					if (!missingSource && boundary.Storage.Type != models.StorageTypePan115) || (recycle && boundary.Storage.Type != models.StorageTypeLocal) {
						return false, ErrCatalogFence
					}
					atTarget, _, err := observe(target, providerID, size, modified)
					if err != nil || !atTarget {
						return false, ErrCatalogFence
					}
					success = true
				}
			}
			return success, nil
		}()
		if err != nil {
			observationErr = err
			break
		}
		if success {
			succeeded = append(succeeded, index)
			if index < len(plan.RecycleItems) {
				applied.RecycleItems = append(applied.RecycleItems, plan.RecycleItems[index])
			} else {
				applied.Items = append(applied.Items, plan.Items[index-len(plan.RecycleItems)])
			}
		}
		next = index + 1
		if time.Since(batchStarted) >= 5*time.Second {
			break
		}
	}
	if next == progress.CancelVerified && observationErr != nil {
		return observationErr
	}
	complete := next == len(rows)
	parents, err := s.repairedManagedProviderParents(ctx, boundary.Library, boundary.Storage, applied.Items)
	if err != nil {
		return err
	}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := validateCancelledStructurePermitTx(tx, permit, repair); err != nil {
			return err
		}
		if err := validateStructureOutcomeCatalogTx(tx, repair.LibraryID, applied); err != nil {
			return err
		}
		if initializeRows && len(rows) > 0 {
			if err := tx.CreateInBatches(&rows, CatalogBatchRows).Error; err != nil {
				return err
			}
		}
		if err := removeStructureCatalogItems(tx, repair.LibraryID, applied.RecycleItems); err != nil {
			return err
		}
		if err := updateStructureCatalogPaths(tx, repair.LibraryID, applied.Items, parents); err != nil {
			return err
		}
		if _, err := resolveCompletedStructureMovesTx(tx, repair.LibraryID, applied.Items); err != nil {
			return err
		}
		now := time.Now().UTC()
		for start := 0; start < len(succeeded); start += CatalogBatchRows {
			if err := tx.Model(&models.MediaLibraryStructureRepairItem{}).Where("repair_id=? AND ordinal IN ?", repair.ID, succeeded[start:min(start+CatalogBatchRows, len(succeeded))]).Updates(map[string]any{"status": structureRepairItemSucceeded, "error_code": "", "error_message": "", "finished_at": now, "updated_at": now}).Error; err != nil {
				return err
			}
		}
		var totalSucceeded, failed, blocked int64
		if err := tx.Model(&models.MediaLibraryStructureRepairItem{}).Where("repair_id=? AND status=?", repair.ID, structureRepairItemSucceeded).Count(&totalSucceeded).Error; err != nil {
			return err
		}
		if err := tx.Model(&models.MediaLibraryStructureRepairItem{}).Where("repair_id=? AND status=?", repair.ID, structureRepairItemFailed).Count(&failed).Error; err != nil {
			return err
		}
		if err := tx.Model(&models.MediaLibraryStructureRepairItem{}).Where("repair_id=? AND status=?", repair.ID, structureRepairItemBlocked).Count(&blocked).Error; err != nil {
			return err
		}
		if err := tx.Model(&models.MediaLibraryStructureRepair{}).Where("id=?", repair.ID).Updates(map[string]any{"phase": "failed", "last_error_code": "structure_cancelled", "succeeded_items": totalSucceeded, "failed_items": failed, "blocked_items": blocked, "processed_items": totalSucceeded + failed + blocked, "current_action": "", "current_item": "", "current_batch_size": 0, "finished_at": now, "updated_at": now}).Error; err != nil {
			return err
		}
		if len(succeeded) > 0 {
			if err := tx.Model(&models.MediaLibrary{}).Where("id=?", repair.LibraryID).UpdateColumn("dirty_generation", gorm.Expr("dirty_generation + 1")).Error; err != nil {
				return err
			}
		}
		if err := refreshStructureSummaryTx(tx, repair.LibraryID, now); err != nil {
			return err
		}
		var durable map[string]any
		if err := json.Unmarshal([]byte(repair.StateJSON), &durable); err != nil {
			return err
		}
		if durable == nil {
			durable = map[string]any{}
		}
		durable["cancel_verified"] = next
		raw, err := json.Marshal(durable)
		if err != nil {
			return err
		}
		if err := tx.Model(&models.MediaLibraryStructureRepair{}).Where("id=?", repair.ID).Update("state_json", string(raw)).Error; err != nil {
			return err
		}
		if !complete {
			return nil
		}
		changed := tx.Model(&models.CatalogPhysicalWrite{}).Where("id=? AND revision=? AND state='quiescent'", permit.evidence.ID, permit.evidence.Revision).Updates(map[string]any{"state": "settled", "settled_at": now, "updated_at": now})
		if changed.Error != nil {
			return changed.Error
		}
		if changed.RowsAffected != 1 {
			return ErrCatalogFence
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !complete {
		return errStructureCancelProgress
	}
	return nil
}

// A paused attempt may be followed by a read-only scan. Never apply a frozen
// source-path update to a replacement file now occupying that path.
func validateStructureOutcomeCatalogTx(tx *gorm.DB, libraryID uint, plan StructurePlan) error {
	check := func(kind, source, target, providerID string, size, modified int64) error {
		table := "media_library_entries"
		if kind == "sidecar" {
			table = "media_library_source_assets"
		}
		paths := []string{source, "/" + source}
		if target != "" {
			paths = append(paths, target, "/"+target)
		}
		var rows []struct {
			ProviderID string
			Size       int64
			ModifiedAt time.Time
		}
		if err := tx.Table(table).Select("provider_id,size,modified_at").Where("library_id=? AND relative_path IN ?", libraryID, paths).Find(&rows).Error; err != nil {
			return err
		}
		for _, row := range rows {
			if row.ProviderID != providerID || (size > 0 && row.Size != size) || (modified != 0 && row.ModifiedAt.UTC().UnixNano() != modified) {
				return ErrCatalogFence
			}
		}
		return nil
	}
	for _, item := range plan.Items {
		if err := check(item.Kind, item.SourceRelative, item.TargetRelative, item.ProviderID, item.Size, item.ModifiedAtUnixNano); err != nil {
			return err
		}
	}
	for _, item := range plan.RecycleItems {
		if err := check(item.Kind, item.SourceRelative, "", item.ProviderID, item.Size, item.ModifiedAtUnixNano); err != nil {
			return err
		}
	}
	return nil
}

func validateCancelledStructurePermitTx(tx *gorm.DB, permit CatalogPhysicalWritePermit, repair models.MediaLibraryStructureRepair) error {
	proof := permit.evidence
	if err := requireCatalogTransaction(tx); err != nil {
		return err
	}
	if proof.OwnerKind != CatalogPhysicalRepair || proof.OwnerID != repair.ID || proof.LibraryID != repair.LibraryID || proof.JobID == "" {
		return ErrCatalogFence
	}
	if err := requireCatalogPhysicalPermitRuntimeTx(tx, proof); err != nil {
		return err
	}
	var current models.CatalogPhysicalWrite
	if err := tx.First(&current, proof.ID).Error; err != nil {
		return err
	}
	if current.State != "quiescent" || current.OwnerKind != proof.OwnerKind || current.OwnerID != proof.OwnerID || current.LibraryID != proof.LibraryID || current.ClaimDigest != proof.ClaimDigest || current.Revision != proof.Revision || current.RuntimeID != proof.RuntimeID || current.JobLeaseHash != proof.JobLeaseHash || current.OwnerDigest != proof.OwnerDigest || current.JobID != proof.JobID {
		return ErrCatalogFence
	}
	var job models.Job
	if err := tx.First(&job, "id=?", proof.JobID).Error; err != nil {
		return err
	}
	if !artifactJobCancelled(job) {
		return ErrCatalogFence
	}
	if job.LeaseTokenHash != "" && job.LeaseTokenHash != proof.JobLeaseHash {
		return ErrCatalogFence
	}
	var stored models.MediaLibraryStructureRepair
	if err := tx.First(&stored, "id=?", repair.ID).Error; err != nil {
		return err
	}
	if stored.JobID == nil || *stored.JobID != proof.JobID || stored.PlanJSON != repair.PlanJSON || stored.LibraryID != repair.LibraryID {
		return ErrCatalogFence
	}
	if catalogPhysicalDigest(catalogPhysicalDigest(stored.PlanJSON, stored.RuleFingerprint, stored.Scope, stored.WorkKey), uintID(stored.LibraryID), uintID(stored.OwnerID)) != proof.OwnerDigest {
		return ErrCatalogFence
	}
	var payload mediaLibraryRepairJobPayload
	if job.JobType != JobTypeMediaLibraryRepair || json.Unmarshal([]byte(job.PayloadJSON), &payload) != nil || payload.RepairID != repair.ID {
		return ErrCatalogFence
	}
	library, storage, profile, err := catalogConversionContextTx(tx, repair.LibraryID)
	if err != nil {
		return err
	}
	var epoch uint64
	if err := tx.Model(&models.CatalogHead{}).Where("library_id=?", repair.LibraryID).Select("source_epoch").Scan(&epoch).Error; err != nil {
		return err
	}
	if epoch != proof.SourceEpoch || proof.SourceFingerprint != catalogSourceFingerprint(library, storage) || proof.ConfigFingerprint != catalogConfigFingerprint(library, storage, profile) {
		return ErrCatalogFence
	}
	return nil
}

// Returns exact identity match and positive absence separately. Unknown access
// errors and replacement files never become absence or permission to settle.
func structureFileObserver(ctx context.Context, boundary StructureBoundary, backend MediaLibraryStructureBackend) (func(string, string, int64, int64) (bool, bool, error), error) {
	var index *providerStructureDirectoryIndex
	if boundary.Storage.Type == models.StorageTypePan115 {
		provider, ok := backend.(pan115MediaLibraryStructureBackend)
		if !ok || provider.driver == nil || boundary.Storage.ConnectionID == nil {
			return nil, ErrCatalogFence
		}
		driver, err := provider.driver(*boundary.Storage.ConnectionID)
		if err != nil {
			return nil, err
		}
		root := boundary.Library.ProviderRootID
		if root == "" {
			root = boundary.Storage.RootPath
		}
		index = newProviderStructureDirectoryIndex(cloudpkg.WithReadClass(ctx, cloudpkg.ReadClassPipeline), driver, root)
	}
	return func(relative, providerID string, size, modified int64) (bool, bool, error) {
		if err := ctx.Err(); err != nil {
			return false, false, err
		}
		if relative == "" || safeStructurePath(relative) != relative {
			return false, false, ErrCatalogFence
		}
		if index != nil {
			parent := index.rootID
			directory := pathpkg.Dir(relative)
			if directory != "." {
				for _, segment := range strings.Split(directory, "/") {
					listing, err := index.directory(parent, false)
					if err != nil {
						return false, false, err
					}
					matches := listing.byName[strings.ToLower(segment)]
					if len(matches) == 0 {
						return false, true, nil
					}
					if len(matches) != 1 || !matches[0].IsDir || matches[0].Name != segment {
						return false, false, ErrCatalogFence
					}
					parent = matches[0].ID
				}
			}
			listing, err := index.directory(parent, false)
			if err != nil {
				return false, false, err
			}
			item, ok := listing.byID[providerID]
			if !ok {
				return false, true, nil
			}
			return providerID != "" && !item.IsDir && item.ParentID == parent && item.Name == pathpkg.Base(relative) && (size <= 0 || item.Size == size), false, nil
		}
		if boundary.Storage.Type != models.StorageTypeLocal {
			return false, false, ErrCatalogFence
		}
		root, err := medialibrary.ResolveRoot(boundary.Storage.RootPath, boundary.Library.RelativeRoot)
		if err != nil {
			return false, false, err
		}
		name := filepath.Join(root, filepath.FromSlash(relative))
		if ensureWithin(root, name) != nil {
			return false, false, ErrCatalogFence
		}
		if err := ensureSafeDirectoryPath(root, filepath.Dir(name), false); err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, false, err
		}
		info, err := os.Lstat(name)
		if errors.Is(err, os.ErrNotExist) {
			return false, true, nil
		}
		if err != nil {
			return false, false, err
		}
		return info.Mode().IsRegular() && (size <= 0 || info.Size() == size) && (modified == 0 || info.ModTime().UTC().UnixNano() == modified), false, nil
	}, nil
}
