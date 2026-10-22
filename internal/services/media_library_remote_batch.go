package services

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
	"gorm.io/gorm"
)

// remoteUploadBatchEvidenceTx validates the immutable Node plan and confirmed
// file receipts. It performs no network calls and never manufactures the Server
// executor's cloud checkpoint. Current provider facts are checked afterwards by
// inspectTransferCatalogBatch before the existing single catalog publication.
func remoteUploadBatchEvidenceTx(tx *gorm.DB, task models.TransferTask, download models.DownloadTask, storage models.Storage, items []models.MediaManagedItem) (int, error) {
	invalid := errors.New("remote upload completion evidence is invalid")
	if task.DownloadTaskID != download.ID || task.Phase != models.TransferTaskStatusCompleted ||
		normalizeExecutionLocation(task.ExecutionLocation) != models.NodeLocationRemote || normalizeExecutionLocation(download.ExecutionLocation) != models.NodeLocationRemote ||
		task.NodeID == nil || download.NodeID == nil || strings.TrimSpace(*task.NodeID) == "" || *task.NodeID != *download.NodeID ||
		!models.IsNativeCloudStorage(storage.Type) || storage.Type != download.TargetStorageType || storage.ConnectionID == nil || download.TargetConnectionID == nil || *storage.ConnectionID != *download.TargetConnectionID ||
		download.TargetStorageID == nil || *download.TargetStorageID != storage.ID || download.TargetLibraryID == nil || *download.TargetLibraryID != task.LibraryID ||
		task.RouteVersion != download.TransferRouteVersion || task.RouteKind != download.TransferRouteKind || task.SourceDataSourceJSON != download.SourceDataSourceJSON || task.TargetDataSourceJSON != download.TargetDataSourceJSON ||
		validateTransferRouteSnapshot(download) != nil {
		return 0, invalid
	}
	var manifest downloadpkg.Manifest
	if json.Unmarshal([]byte(task.ManifestJSON), &manifest) != nil || !manifest.Complete || len(manifest.Files) == 0 {
		return 0, invalid
	}
	rows, found, err := loadRemoteUploadFilesTx(tx, task, download, manifest)
	if err != nil || !found || len(rows) != task.TotalFiles || task.ProcessedFiles != task.TotalFiles {
		return 0, invalid
	}
	batches, err := buildRemoteUploadBatches(rows, storage, download, manifest)
	if err != nil {
		return 0, invalid
	}
	for _, batch := range batches {
		record, err := remoteUploadBatchRecordTx(tx, task, download, batch)
		if err != nil || record.Status != nodeprotocol.OperationCompleted || record.Phase != nodeprotocol.PhaseCompleted || record.ErrorCode != "" {
			return 0, invalid
		}
	}
	managed := make(map[string]models.MediaManagedItem, len(items))
	for _, item := range items {
		if _, duplicate := managed[item.RelativePath]; duplicate || !item.Active || !item.Managed || item.TransferTaskID != task.ID || item.DownloadTaskID != download.ID || item.LibraryID != task.LibraryID {
			return 0, invalid
		}
		managed[item.RelativePath] = item
	}
	seenPaths, seenIDs := map[string]bool{}, map[string]bool{}
	completed := 0
	for _, row := range rows {
		if seenPaths[row.TargetRelative] {
			return 0, invalid
		}
		seenPaths[row.TargetRelative] = true
		if row.Status == nodeprotocol.StorageFileSkipped {
			if row.TargetItemID != "" || row.TargetSHA1 != "" || row.TargetDigestJSON != "" {
				return 0, invalid
			}
			if _, exists := managed[row.TargetRelative]; exists {
				return 0, invalid
			}
			continue
		}
		if row.Status != nodeprotocol.StorageFileCompleted && row.Status != nodeprotocol.StorageFileReused {
			return 0, invalid
		}
		validDigest := validRemoteSHA1(row.TargetSHA1)
		if storage.Type == models.StorageTypeQuark {
			validDigest = row.TargetSHA1 == "" && validRemoteQuarkDigest(row.TargetDigestJSON, row.SHA256)
		}
		if !validDigest || strings.TrimSpace(row.TargetItemID) == "" || strings.TrimSpace(row.TargetParentID) == "" || seenIDs[row.TargetItemID] {
			return 0, invalid
		}
		seenIDs[row.TargetItemID] = true
		item, exists := managed[row.TargetRelative]
		kind := models.MediaManagedItemKindSidecar
		if isVideoFile(row.TargetRelative) {
			kind = models.MediaManagedItemKindVideo
		}
		if !exists || item.ProviderItemID != row.TargetItemID || item.ProviderParentID != row.TargetParentID || item.Size != row.Size || item.Kind != kind || item.IdentityRevision != download.IdentityRevision {
			return 0, invalid
		}
		completed++
	}
	if completed != len(managed) {
		return 0, invalid
	}
	return completed, nil
}
