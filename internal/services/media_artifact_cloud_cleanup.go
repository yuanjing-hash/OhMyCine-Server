package services

import (
	"context"
	"encoding/json"
	"errors"
	"path"
	"sort"
	"strings"
	"time"

	serverlog "github.com/yuanjing-hash/OhMyCine-Server/internal/logging"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	cloudpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"gorm.io/gorm"
)

type cloudCleanupDirectory struct {
	ProviderID       string `json:"provider_id"`
	ParentProviderID string `json:"parent_provider_id"`
	RelativePath     string `json:"relative_path"`
	ScanRunID        uint   `json:"scan_run_id"`
}

// Freeze only published, source-bound ancestry of the explicitly removed IDs.
// Neither guessed names nor a provider lookup of a deleted file grant authority.
func freezeCloudCleanupDirectoriesTx(tx *gorm.DB, run *models.MediaLibraryScanRun, deletedIDs []string) error {
	if len(deletedIDs) == 0 {
		return nil
	}
	var library models.MediaLibrary
	if err := tx.First(&library, run.LibraryID).Error; err != nil {
		return err
	}
	if !library.CloudEmptyCleanupEnabled {
		return nil
	}
	var storage models.Storage
	if err := tx.First(&storage, library.StorageID).Error; err != nil {
		return err
	}
	if storage.Type != models.StorageTypePan115 || library.ProviderRootID == "" {
		return ErrCatalogFence
	}
	fingerprint := catalogSourceFingerprint(library, storage)
	dirs := map[string]cloudCleanupDirectory{}
	for _, id := range deletedIDs {
		var item models.MediaLibraryProviderPath
		err := publishedProviderPaths(tx, library.ID, fingerprint).Where("paths.provider_id=?", id).Take(&item).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		// A directory-deletion notification may already have recycled that exact
		// directory; its saved chain still lets us consider its empty ancestors.
		chain := []cloudCleanupDirectory{}
		parent, expected := item.ParentProviderID, path.Dir(item.RelativePath)
		// File staging records its full canonical relative path, not ParentID.
		// Resolve that exact persisted path to a unique published directory ID.
		if parent == "" && expected != "/" {
			var parents []models.MediaLibraryProviderPath
			if err := publishedProviderPaths(tx, library.ID, fingerprint).Where("paths.is_dir=1 AND paths.relative_path=? AND paths.scan_run_id<=?", expected, item.ScanRunID).Limit(2).Find(&parents).Error; err != nil {
				return err
			}
			if len(parents) != 1 {
				continue
			}
			parent = parents[0].ProviderID
		}
		seen := map[string]bool{id: true}
		valid := true
		for parent != library.ProviderRootID {
			if parent == "" || seen[parent] || !safeCloudRelativePath(expected) {
				valid = false
				break
			}
			seen[parent] = true
			var row models.MediaLibraryProviderPath
			err = publishedProviderPaths(tx, library.ID, fingerprint).Where("paths.provider_id=?", parent).Take(&row).Error
			if errors.Is(err, gorm.ErrRecordNotFound) {
				valid = false
				break
			}
			if err != nil {
				return err
			}
			if !row.IsDir || row.RelativePath != expected {
				valid = false
				break
			}
			chain = append(chain, cloudCleanupDirectory{row.ProviderID, row.ParentProviderID, row.RelativePath, row.ScanRunID})
			parent, expected = row.ParentProviderID, path.Dir(row.RelativePath)
		}
		if expected != "/" {
			valid = false
		}
		if valid {
			for _, d := range chain {
				dirs[d.ProviderID] = d
			}
		}
	}
	if len(dirs) == 0 {
		return nil
	}
	list := make([]cloudCleanupDirectory, 0, len(dirs))
	for _, d := range dirs {
		list = append(list, d)
	}
	sortCloudCleanupDirectories(list)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(run.CheckpointJSON), &fields); err != nil {
		return err
	}
	if fields == nil {
		return ErrCatalogInvalid
	}
	raw, err := json.Marshal(list)
	if err != nil {
		return err
	}
	fields["cloud_cleanup_directories"] = raw
	raw, err = json.Marshal(fields)
	if err != nil {
		return err
	}
	run.CheckpointJSON = string(raw)
	return nil
}

func safeCloudRelativePath(value string) bool {
	return value != "/" && strings.HasPrefix(value, "/") && !strings.HasPrefix(value, "//") && !strings.ContainsAny(value, "\\\x00") && path.Clean(value) == value
}

func sortCloudCleanupDirectories(dirs []cloudCleanupDirectory) {
	sort.Slice(dirs, func(i, j int) bool {
		a, b := strings.Count(dirs[i].RelativePath, "/"), strings.Count(dirs[j].RelativePath, "/")
		if a != b {
			return a > b
		}
		return dirs[i].RelativePath < dirs[j].RelativePath
	})
}

func cloudCleanupFailure(causes ...error) error {
	if len(causes) > 0 && causes[0] != nil {
		code, _ := cloudpkg.ErrorInfo(causes[0])
		return appError(code, connectionTestMessage(cloudpkg.ProviderPan115, code), causes[0])
	}
	return appError("cloud_empty_cleanup_failed", "云端空目录检查或回收失败，保留当前任务恢复记录", nil)
}

// Revalidate the actual physical permit and queued writer at every checkpoint.
// No transaction remains open while provider IO is in progress.
func (s *MediaArtifactService) cloudCleanupGuardTx(tx *gorm.DB, claim ClaimedJob, permit CatalogPhysicalWritePermit, run models.MediaArtifactRun, policy mediaArtifactPolicy) (models.MediaLibrary, error) {
	var library models.MediaLibrary
	proof, current, err := catalogArtifactPermitTx(tx, permit)
	if err != nil {
		return library, err
	}
	if proof.State != "entered" || current.ID != run.ID || claim.Job.ID != proof.JobID {
		return library, ErrCatalogFence
	}
	if _, err = s.queue.verifyLease(tx, claim.Job.ID, claim.LeaseToken); err != nil {
		return library, err
	}
	if err = tx.First(&library, policy.LibraryID).Error; err != nil {
		return library, err
	}
	var storage models.Storage
	if err = tx.First(&storage, library.StorageID).Error; err != nil {
		return library, err
	}
	if !library.Enabled || !library.CloudEmptyCleanupEnabled || !policy.CloudEmptyCleanupEnabled || storage.Type != models.StorageTypePan115 || storage.ConnectionID == nil || *storage.ConnectionID != policy.ConnectionID || library.ProviderRootID == "" || catalogSourceFingerprint(library, storage) != policy.SourceBoundaryFingerprint {
		return library, ErrCatalogFence
	}
	return library, nil
}

func cloudCleanupBusyTx(tx *gorm.DB, libraryID uint) (bool, error) {
	// Import/repair writers use the same physical admission. Downloads may be
	// fetching into their staging destination before acquiring an import permit.
	var active int
	if err := tx.Raw(`SELECT 1 FROM download_tasks d JOIN jobs j ON j.id=d.job_id WHERE d.target_library_id=? AND (j.status IN ? OR j.lease_token_hash<>'') LIMIT 1`, libraryID, activeJobStatuses()).Scan(&active).Error; err != nil {
		return false, err
	}
	if active != 0 {
		return true, nil
	}
	if err := tx.Raw(`SELECT 1 FROM transfer_tasks t JOIN jobs j ON j.id=t.job_id WHERE t.library_id=? AND (j.status IN ? OR j.lease_token_hash<>'') LIMIT 1`, libraryID, activeJobStatuses()).Scan(&active).Error; err != nil {
		return false, err
	}
	if active != 0 {
		return true, nil
	}
	return false, nil
}

func (s *MediaArtifactService) cleanupCloudEmptyDirectories(ctx context.Context, claim ClaimedJob, permit CatalogPhysicalWritePermit, run models.MediaArtifactRun, policy mediaArtifactPolicy) error {
	if !policy.CloudEmptyCleanupEnabled || len(policy.CloudCleanupDirectories) == 0 {
		return nil
	}
	var library models.MediaLibrary
	busy := false
	guard := func() error {
		return s.catalogArtifactWriteTx(ctx, func(tx *gorm.DB) error {
			var err error
			library, err = s.cloudCleanupGuardTx(tx, claim, permit, run, policy)
			if err == nil {
				busy, err = cloudCleanupBusyTx(tx, library.ID)
			}
			return err
		})
	}
	if err := guard(); err != nil {
		return err
	}
	if s.connections == nil {
		return cloudCleanupFailure()
	}
	connection, driver, err := s.connections.driver(policy.ConnectionID)
	if err != nil {
		return cloudCleanupFailure(err)
	}
	providerFailure := func(err error) error {
		code, _ := cloudpkg.ErrorInfo(err)
		if code == cloudpkg.CodeAuthExpired || code == cloudpkg.CodeCookieInvalid {
			s.connections.recordConnectionCredentialFailure(connection.ID, connection.Revision, cloudpkg.Error(cloudpkg.CodeAuthExpired, false, nil))
		}
		return cloudCleanupFailure(err)
	}
	mutator, ok := driver.(cloudpkg.MutationDriver)
	if !ok || !driver.Capabilities().Recycle || !driver.Capabilities().DirectoryList {
		return cloudCleanupFailure()
	}
	ctx = cloudpkg.WithReadClass(ctx, cloudpkg.ReadClassPipeline)
	dirs := append([]cloudCleanupDirectory(nil), policy.CloudCleanupDirectories...)
	sortCloudCleanupDirectories(dirs)
	byID := map[string]cloudCleanupDirectory{}
	for _, d := range dirs {
		if d.ProviderID == "" || d.ProviderID == library.ProviderRootID || !safeCloudRelativePath(d.RelativePath) || d.ScanRunID == 0 {
			return ErrCatalogFence
		}
		if _, dup := byID[d.ProviderID]; dup {
			return ErrCatalogFence
		}
		byID[d.ProviderID] = d
	}
	for _, d := range dirs {
		if err = guard(); err != nil {
			return err
		}
		var receipt models.CatalogCloudCleanupClaim
		err = s.db.Where("run_id=? AND provider_id=?", run.ID, d.ProviderID).Take(&receipt).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		encoded, _ := json.Marshal(d)
		if receipt.ID != 0 {
			if receipt.PhysicalWriteID != permit.evidence.ID || receipt.SourceFingerprint != policy.SourceBoundaryFingerprint || receipt.DirectoryJSON != string(encoded) {
				return ErrCatalogFence
			}
			if receipt.Status == "completed" || receipt.Status == "kept" || receipt.Status == "abandoned" {
				continue
			}
		}
		// Optional empty-folder cleanup must never retain the physical owner
		// merely because a download will later need it to import. Existing
		// prepared claims still receive read-only outcome reconciliation below.
		if busy && receipt.ID == 0 {
			serverlog.OperationMediaArtifact.Event(s.log.Info()).Uint("library_id", library.ID).Str("task_id", run.ID).Msg("【云端空目录】有下载或入库任务，本次保留目录")
			continue
		}
		// Check the frozen ancestry against both the latest published fact and
		// live stable IDs. A moved/renamed parent can never redirect a cleanup.
		current := d
		seen := map[string]bool{}
		missing := false
		for {
			if seen[current.ProviderID] {
				return ErrCatalogFence
			}
			seen[current.ProviderID] = true
			var saved models.MediaLibraryProviderPath
			if err = publishedProviderPaths(s.db, library.ID, policy.SourceBoundaryFingerprint).Where("paths.provider_id=?", current.ProviderID).Take(&saved).Error; err != nil {
				return ErrCatalogFence
			}
			if !saved.IsDir || saved.ScanRunID != current.ScanRunID || saved.RelativePath != current.RelativePath || saved.ParentProviderID != current.ParentProviderID {
				return ErrCatalogFence
			}
			observed, statErr := driver.Stat(ctx, current.ProviderID)
			if statErr != nil {
				code, _ := cloudpkg.ErrorInfo(statErr)
				if code == cloudpkg.CodeNotFound && current.ProviderID == d.ProviderID {
					missing = true
				} else {
					return providerFailure(statErr)
				}
			} else if observed.ID != current.ProviderID || !observed.IsDir || observed.ParentID != current.ParentProviderID || observed.Name != path.Base(current.RelativePath) {
				return ErrCatalogFence
			}
			if current.ParentProviderID == library.ProviderRootID {
				if path.Dir(current.RelativePath) != "/" {
					return ErrCatalogFence
				}
				break
			}
			parent, exists := byID[current.ParentProviderID]
			if !exists || parent.RelativePath != path.Dir(current.RelativePath) {
				return ErrCatalogFence
			}
			current = parent
		}
		status := "completed"
		if !missing && busy {
			status = "kept"
		}
		if !missing && !busy {
			page, listErr := driver.List(ctx, d.ProviderID, cloudpkg.PageRequest{Offset: 0, Limit: 1})
			if listErr != nil {
				return providerFailure(listErr)
			}
			if page.Offset != 0 || (len(page.Items) == 0 && page.HasMore) {
				return cloudCleanupFailure()
			}
			if len(page.Items) > 0 {
				status = "kept"
			} else {
				// Durable intent before external mutation, also on a first attempt.
				if err = s.catalogArtifactWriteTx(ctx, func(tx *gorm.DB) error {
					if _, e := s.cloudCleanupGuardTx(tx, claim, permit, run, policy); e != nil {
						return e
					}
					if receipt.ID != 0 {
						return nil
					}
					now := time.Now().UTC()
					receipt = models.CatalogCloudCleanupClaim{LibraryID: library.ID, RunID: run.ID, PhysicalWriteID: permit.evidence.ID, ProviderID: d.ProviderID, SourceFingerprint: policy.SourceBoundaryFingerprint, DirectoryJSON: string(encoded), Status: "prepared", CreatedAt: now, UpdatedAt: now}
					return tx.Create(&receipt).Error
				}); err != nil {
					return err
				}
				if err = guard(); err != nil {
					return err
				}
				// Recheck after recording intent. Recycle has no permanent fallback.
				page, listErr = driver.List(ctx, d.ProviderID, cloudpkg.PageRequest{Offset: 0, Limit: 1})
				if listErr != nil {
					return providerFailure(listErr)
				}
				if page.Offset != 0 || (len(page.Items) == 0 && page.HasMore) {
					return cloudCleanupFailure()
				}
				if err = guard(); err != nil {
					return err
				}
				if len(page.Items) > 0 || busy {
					status = "kept"
				} else if err = mutator.Recycle(ctx, d.ProviderID); err != nil {
					return providerFailure(err)
				}
			}
		}
		if err = s.catalogArtifactWriteTx(ctx, func(tx *gorm.DB) error {
			if _, e := s.cloudCleanupGuardTx(tx, claim, permit, run, policy); e != nil {
				return e
			}
			now := time.Now().UTC()
			if receipt.ID == 0 {
				receipt = models.CatalogCloudCleanupClaim{LibraryID: library.ID, RunID: run.ID, PhysicalWriteID: permit.evidence.ID, ProviderID: d.ProviderID, SourceFingerprint: policy.SourceBoundaryFingerprint, DirectoryJSON: string(encoded), Status: status, CreatedAt: now, UpdatedAt: now}
				return tx.Create(&receipt).Error
			}
			return tx.Model(&models.CatalogCloudCleanupClaim{}).Where("id=? AND status='prepared'", receipt.ID).Updates(map[string]any{"status": status, "updated_at": now}).Error
		}); err != nil {
			return err
		}
		message := "【云端空目录】空目录已回收或确认不存在"
		if status == "kept" {
			message = "【云端空目录】目录仍有文件或正在入库，已保留"
		}
		serverlog.OperationMediaArtifact.Event(s.log.Info()).Uint("library_id", library.ID).Str("task_id", run.ID).Str("directory_name", safeLabel(path.Base(d.RelativePath), 128)).Msg(message)
	}
	return nil
}
