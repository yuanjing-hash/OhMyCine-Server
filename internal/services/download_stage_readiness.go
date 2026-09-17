package services

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	storagefs "github.com/yuanjing-hash/OhMyCine-Server/internal/storage"
	"gorm.io/gorm"
)

// Download writes only its frozen staging location; Transfer owns final writes.
// Grant this exception only with complete staging/route evidence, and only for
// artifact contention. Repair, retirement, credentials and unknown writers keep
// their normal admission semantics. This read never probes a provider or mutates.
func downloadIndependentOfLibraryArtifacts(db *gorm.DB, job models.Job, libraryID uint) (bool, error) {
	var task models.DownloadTask
	if err := db.Where("job_id=? AND target_library_id=?", job.ID, libraryID).First(&task).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, nil
		}
		return false, err
	}
	if task.Phase == models.DownloadTaskStatusCancelled || task.Phase == models.DownloadTaskStatusCompleted || task.DownloaderID == nil || task.TransferRouteVersion != models.TransferRouteVersionCurrent {
		return false, nil
	}
	var source models.DataSourceIdentity
	if json.Unmarshal([]byte(task.SourceDataSourceJSON), &source) != nil {
		return false, nil
	}
	var downloader models.Downloader
	if err := db.First(&downloader, "id=?", *task.DownloaderID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, nil
		}
		return false, err
	}
	if !downloader.Enabled || downloader.Type != task.ProviderType {
		return false, nil
	}
	staging := strings.TrimSpace(task.StagingAbsolutePath)
	var library models.MediaLibrary
	if err := db.First(&library, libraryID).Error; err != nil {
		return false, err
	}
	var storage models.Storage
	if err := db.First(&storage, library.StorageID).Error; err != nil {
		return false, err
	}
	if !library.Enabled || !storage.Enabled || library.BaselineGeneration == 0 || library.Status == models.MediaLibraryStatusInitializationFailed || task.TargetStorageID == nil || *task.TargetStorageID != storage.ID || task.TargetStorageType != storage.Type || task.TargetStorageRoot != storage.RootPath || task.TargetRelativeRoot != library.RelativeRoot {
		return false, nil
	}
	var checking int64
	if err := db.Model(&models.MediaLibraryStructureAutoState{}).Where("library_id=? AND diagnosed_revision<source_revision", libraryID).Count(&checking).Error; err != nil {
		return false, err
	}
	if checking != 0 {
		return false, nil
	}
	if retiring, err := mediaLibraryRetiringTx(db, libraryID); err != nil || retiring {
		return false, err
	}
	var target models.DataSourceIdentity
	expected, err := mediaLibraryDataSourceIdentity(storage)
	if err != nil || json.Unmarshal([]byte(task.TargetDataSourceJSON), &target) != nil || target != expected || task.TransferRouteKind != selectTransferRoute(source, target) {
		return false, nil
	}
	switch source.Kind {
	case models.DataSourceKindLocal:
		if source != localDataSourceIdentity() || !filepath.IsAbs(staging) || (task.ProviderType != models.DownloaderTypeQBittorrent && task.ProviderType != models.DownloaderTypePluginHTTP) {
			return false, nil
		}
	case models.DataSourceKindProvider:
		// Another account/local destination cannot be touched by source-side
		// offline download. Same-account directory IDs alone prove no ancestry.
		if task.ProviderType != models.DownloaderTypePan115Offline || task.StagingStorageID == nil || downloader.StorageID == nil || *task.StagingStorageID != *downloader.StorageID || task.TransferRouteKind != models.TransferRouteCrossSource {
			return false, nil
		}
		var sourceStorage models.Storage
		if err := db.First(&sourceStorage, *task.StagingStorageID).Error; err != nil {
			return false, err
		}
		sourceExpected, err := mediaLibraryDataSourceIdentity(sourceStorage)
		if err != nil || sourceExpected != source || !sourceStorage.Enabled || sourceStorage.ConnectionID == nil {
			return false, nil
		}
		var connection models.Connection
		if err := db.First(&connection, *sourceStorage.ConnectionID).Error; err != nil {
			return false, err
		}
		if !connection.Enabled || (connection.LastHealthStatus == "offline" && connection.LastHealthErrorCode == "pan115_auth_expired") {
			return false, nil
		}
		// Cross-source materialization still uses the immutable Server staging.
		if !filepath.IsAbs(staging) {
			return false, nil
		}
	default:
		return false, nil
	}
	// Include projection output: a cloud target can still write local STRM/art.
	if library.STRMLocalRoot != "" && stagingPathsOverlap(staging, library.STRMLocalRoot) {
		return false, nil
	}
	if storage.Type == models.StorageTypeLocal {
		rel := strings.Trim(strings.ReplaceAll(library.RelativeRoot, "\\", "/"), "/")
		root, err := storagefs.Constrain(storage.RootPath, filepath.Join(storage.RootPath, filepath.FromSlash(rel)))
		if err != nil || stagingPathsOverlap(staging, root) {
			return false, nil
		}
	}
	var other int64
	if err := db.Model(&models.CatalogPhysicalWrite{}).Where("library_id=? AND state IN ('entered','quiescent') AND owner_kind<>?", libraryID, CatalogPhysicalArtifact).Count(&other).Error; err != nil {
		return false, err
	}
	return other == 0, nil
}

func stagingPathsOverlap(left, right string) bool {
	// Existing configuration validates roots for symlinks/reparse points. The
	// worker revalidates staging before using it; admission compares snapshots.
	left = filepath.ToSlash(filepath.Clean(left))
	right = filepath.ToSlash(filepath.Clean(right))
	if filepath.Separator == '\\' {
		left, right = strings.ToLower(left), strings.ToLower(right)
	}
	return rootsOverlap(left, right)
}
