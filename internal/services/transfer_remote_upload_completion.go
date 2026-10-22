package services

import (
	"context"
	"encoding/json"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
	"gorm.io/gorm"
)

// Older executions could mark the transfer complete after terminal rows while
// the original operation still lacked its completion ACK. Repair that boundary
// before publication too; already-confirmed batches require no Node connection.
func (w *TransferWorker) recoverCompletedRemoteUploadOperations(ctx context.Context, task *models.TransferTask, download models.DownloadTask) error {
	if !models.IsNativeCloudStorage(download.TargetStorageType) || normalizeExecutionLocation(download.ExecutionLocation) != models.NodeLocationRemote {
		return nil
	}
	conflict := cloudTransferError(nodeprotocol.ErrorPlanConflict, false, nil)
	if task == nil || task.Phase != models.TransferTaskStatusCompleted || normalizeExecutionLocation(task.ExecutionLocation) != models.NodeLocationRemote || task.NodeID == nil || download.NodeID == nil || *task.NodeID != *download.NodeID || download.TargetStorageID == nil {
		return conflict
	}
	// Published batches are recovered by the catalog's own durable ACK path;
	// they no longer depend on retention of the remote result.
	if task.CatalogPublishedAt != nil {
		return nil
	}
	var published int64
	if err := w.service.db.WithContext(ctx).Model(&models.MediaLibraryScanRun{}).Where("library_id = ? AND kind = ? AND status = ? AND CASE WHEN json_valid(checkpoint_json) THEN json_extract(checkpoint_json, '$.transfer_batch_id') ELSE '' END = ?", task.LibraryID, "transfer_batch", "success", task.ID).Limit(1).Count(&published).Error; err != nil {
		return err
	}
	if published > 0 {
		return nil
	}
	var manifest downloadpkg.Manifest
	var storage models.Storage
	if json.Unmarshal([]byte(task.ManifestJSON), &manifest) != nil || !manifest.Complete || w.service.db.WithContext(ctx).First(&storage, *download.TargetStorageID).Error != nil || storage.Type != download.TargetStorageType || storage.ConnectionID == nil || download.TargetConnectionID == nil || *storage.ConnectionID != *download.TargetConnectionID || validateTransferRouteSnapshot(download) != nil || task.RouteVersion != download.TransferRouteVersion || task.RouteKind != download.TransferRouteKind || task.SourceDataSourceJSON != download.SourceDataSourceJSON || task.TargetDataSourceJSON != download.TargetDataSourceJSON {
		return conflict
	}
	rows, found, err := w.loadRemoteUploadFiles(ctx, *task, download, manifest)
	if err != nil || !found || !remoteUploadRowsTerminal(rows) {
		return conflict
	}
	batches, err := buildRemoteUploadBatches(rows, storage, download, manifest)
	if err != nil {
		return conflict
	}
	var client remoteUploadNodeClient
	for _, batch := range batches {
		record, err := remoteUploadBatchRecordTx(w.service.db.WithContext(ctx), *task, download, batch)
		if err != nil {
			return err
		}
		if record.Status == nodeprotocol.OperationCompleted && record.Phase == nodeprotocol.PhaseCompleted && record.ErrorCode == "" {
			continue
		}
		if record.Status == nodeprotocol.OperationFailed || record.Status == nodeprotocol.OperationCancelled {
			return conflict
		}
		if client == nil {
			if w.service.downloader == nil || w.service.downloader.transferNodes == nil {
				return cloudTransferError(nodeprotocol.ErrorNodeOffline, true, nil)
			}
			client, _, err = w.service.downloader.transferNodes.nodeClient(*download.NodeID)
			if err != nil {
				return remoteUploadClientError(err)
			}
		}
		if err = w.recoverTerminalRemoteUploadBatch(ctx, client, task, download, rows, batch); err != nil {
			return err
		}
	}
	return nil
}

// Read the original immutable operation, never create or renew it. The same
// binding is required by recovery and by the terminal catalog publisher.
func remoteUploadBatchRecordTx(tx *gorm.DB, task models.TransferTask, download models.DownloadTask, batch remoteUploadBatch) (models.RemoteOperation, error) {
	var record models.RemoteOperation
	conflict := cloudTransferError(nodeprotocol.ErrorPlanConflict, false, nil)
	if task.NodeID == nil || download.NodeID == nil || *task.NodeID == "" || *task.NodeID != *download.NodeID || task.DownloadTaskID != download.ID {
		return record, conflict
	}
	key := remoteUploadOperationKey(task.ID, batch.index)
	if err := tx.First(&record, "operation_key = ?", key).Error; err != nil {
		return record, conflict
	}
	raw, err := json.Marshal(batch.plan)
	if err != nil {
		return record, conflict
	}
	plan := nodeprotocol.OperationPlan{ProtocolVersion: nodeprotocol.VersionV1, OperationKey: key, TaskID: download.ID, NodeID: *task.NodeID, Kind: nodeprotocol.OperationKindStorageUpload, PlanRevision: 1, Payload: raw}
	digest, err := plan.Digest()
	if err != nil || record.TaskID != download.ID || record.NodeID != *task.NodeID || record.PlanRevision != 1 || record.PlanDigest != digest {
		return record, conflict
	}
	return record, nil
}

// File receipts are written before the operation completion checkpoint. A
// crash between them must query the original result, never submit another
// upload or require a fresh source export/credential grant merely to finalize.
func (w *TransferWorker) recoverTerminalRemoteUploadBatch(ctx context.Context, client remoteUploadNodeClient, task *models.TransferTask, download models.DownloadTask, allRows []models.RemoteUploadFile, batch remoteUploadBatch) error {
	if task == nil || !remoteUploadRowsTerminal(batch.rows) {
		return cloudTransferError(nodeprotocol.ErrorPlanConflict, false, nil)
	}
	record, err := remoteUploadBatchRecordTx(w.service.db.WithContext(ctx), *task, download, batch)
	if err != nil {
		return err
	}
	if record.Status == nodeprotocol.OperationCompleted && record.Phase == nodeprotocol.PhaseCompleted && record.ErrorCode == "" {
		return nil
	}
	if record.Status == nodeprotocol.OperationFailed || record.Status == nodeprotocol.OperationCancelled {
		return cloudTransferError(nodeprotocol.ErrorPlanConflict, false, nil)
	}
	response, err := client.StorageActionResult(ctx, record.OperationKey, download.ID, record.OperationKey)
	if err != nil {
		return remoteUploadClientError(err)
	}
	request := nodeprotocol.StorageActionRequest{RequestID: record.OperationKey, TaskID: download.ID, OperationKey: record.OperationKey, PlanDigest: record.PlanDigest, StorageID: batch.plan.StorageID, Action: nodeprotocol.StorageActionUpload}
	if err := validateStorageActionResponse(response, request, len(batch.rows)); err != nil {
		return err
	}
	if response.Status != nodeprotocol.StorageActionCompleted || response.ErrorCode != "" || response.CompletedFiles != len(batch.rows) || len(response.Files) != len(batch.rows) {
		return cloudTransferError(nodeprotocol.ErrorReconciliationNeeded, true, nil)
	}
	for _, file := range response.Files {
		if !remoteUploadResultTerminal(file.Status) {
			return cloudTransferError(nodeprotocol.ErrorReconciliationNeeded, true, nil)
		}
	}
	return w.service.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := ensureDownloadPipelineActive(tx, task.DownloadTaskID); err != nil {
			return err
		}
		var persistedTask models.TransferTask
		if err := tx.First(&persistedTask, "id = ?", task.ID).Error; err != nil || persistedTask.DownloadTaskID != task.DownloadTaskID || persistedTask.Phase != task.Phase || persistedTask.ManifestJSON != task.ManifestJSON || persistedTask.RouteVersion != task.RouteVersion || persistedTask.RouteKind != task.RouteKind || persistedTask.SourceDataSourceJSON != task.SourceDataSourceJSON || persistedTask.TargetDataSourceJSON != task.TargetDataSourceJSON || persistedTask.NodeID == nil || task.NodeID == nil || *persistedTask.NodeID != *task.NodeID {
			return cloudTransferError(nodeprotocol.ErrorPlanConflict, false, nil)
		}
		current, err := remoteUploadBatchRecordTx(tx, *task, download, batch)
		if err != nil || current.ID != record.ID || current.LeaseEpoch != record.LeaseEpoch || current.Status == nodeprotocol.OperationFailed || current.Status == nodeprotocol.OperationCancelled {
			return cloudTransferError(nodeprotocol.ErrorPlanConflict, false, nil)
		}
		if current.Status == nodeprotocol.OperationCompleted && current.Phase == nodeprotocol.PhaseCompleted && current.ErrorCode == "" {
			return nil
		}
		service := *w.service
		service.db = tx
		worker := NewTransferWorker(&service)
		originalPhase := task.Phase
		if err := worker.applyRemoteUploadResponse(ctx, task, allRows, batch.rows, response, batch.plan.ProviderType); err != nil {
			return err
		}
		if originalPhase == models.TransferTaskStatusCompleted {
			if err := tx.Model(&models.TransferTask{}).Where("id = ?", task.ID).Update("phase", originalPhase).Error; err != nil {
				return err
			}
			task.Phase = originalPhase
		}
		progress := 1.0
		return worker.persistRemoteOperationState(ctx, record, nodeprotocol.OperationCompleted, nodeprotocol.PhaseCompleted, "", &progress)
	})
}
