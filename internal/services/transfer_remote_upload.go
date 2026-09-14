package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	pathpkg "path"
	"strconv"
	"strings"
	"time"

	serverlog "github.com/yuanjing-hash/OhMyCine-Server/internal/logging"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/nodeclient"
	cloudpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	remoteUploadLeaseDuration = 5 * time.Minute
	remoteUploadPollInterval  = time.Second
)

type remoteUploadNodeClient interface {
	PutOperation(context.Context, nodeprotocol.PutOperationRequest) (nodeprotocol.OperationResponse, error)
	RenewOperationLease(context.Context, string, nodeprotocol.LeaseRequest) (nodeprotocol.OperationResponse, error)
	PutCredentialGrant(context.Context, nodeprotocol.CredentialGrantEnvelope) error
	StorageAction(context.Context, nodeprotocol.StorageActionRequest) (nodeprotocol.StorageActionResponse, error)
	StorageActionResult(context.Context, string, string, string) (nodeprotocol.StorageActionResponse, error)
}

type remoteUploadBatch struct {
	index int
	rows  []models.RemoteUploadFile
	plan  nodeprotocol.StorageUploadPlan
}

// runRemoteNodeUpload keeps all media bytes on the selected Node. The Server
// freezes naming and conflict decisions once, then dispatches deterministic,
// bounded batches while retaining one ordinary TransferTask for the user.
func (w *TransferWorker) runRemoteNodeUpload(ctx context.Context, runtime JobRuntime, job ClaimedJob, task models.TransferTask, download models.DownloadTask, manifest downloadpkg.Manifest, started time.Time) WorkerResult {
	if w.service == nil || w.service.downloader == nil || w.service.downloader.transferNodes == nil || normalizeExecutionLocation(download.ExecutionLocation) != models.NodeLocationRemote || download.NodeID == nil || task.NodeID == nil || *download.NodeID != *task.NodeID {
		return w.cloudFailure(task, cloudTransferError("node_transfer_snapshot_invalid", false, nil))
	}
	if (download.ProviderType != models.DownloaderTypeQBittorrent && download.ProviderType != models.DownloaderTypePan115Offline) || download.TargetStorageType != models.StorageTypePan115 || download.TargetStorageID == nil || download.TargetConnectionID == nil || strings.TrimSpace(download.TargetProviderRootID) == "" {
		return w.cloudFailure(task, cloudTransferError(CodeTransferRouteUnsupported, false, nil))
	}
	if download.TransferMode != models.MediaLibraryTransferCopy && download.TransferMode != models.MediaLibraryTransferMove {
		return w.cloudFailure(task, cloudTransferError("cloud_transfer_mode_invalid", false, nil))
	}
	if manifest.RemoteExportOperationKey == "" || !nodeprotocol.ValidDigest(manifest.RemoteExportDigest) || manifest.RemoteExportExpiresAt == nil {
		return w.cloudFailure(task, cloudTransferError("node_file_export_invalid", false, nil))
	}

	client, node, err := w.service.downloader.transferNodes.nodeClient(*download.NodeID)
	if err != nil {
		return w.cloudFailure(task, remoteUploadClientError(err))
	}
	var capabilities nodeprotocol.Capabilities
	if json.Unmarshal([]byte(node.CapabilitiesJSON), &capabilities) != nil || !capabilities.Has(nodeprotocol.CapabilityPan115Upload) || !capabilities.Has(nodeprotocol.CapabilityRangeExport) {
		return w.cloudFailure(task, cloudTransferError(nodeprotocol.ErrorCapabilityMissing, false, nil))
	}
	var storage models.Storage
	if err := w.service.db.WithContext(ctx).First(&storage, *download.TargetStorageID).Error; err != nil || storage.Type != models.StorageTypePan115 || storage.ConnectionID == nil || *storage.ConnectionID != *download.TargetConnectionID {
		return w.cloudFailure(task, cloudTransferError("cloud_transfer_boundary_invalid", false, err))
	}

	rows, exists, err := w.loadRemoteUploadFiles(ctx, task, download, manifest)
	if err != nil {
		return w.cloudFailure(task, cloudTransferError("node_operation_plan_conflict", false, err))
	}
	if !exists {
		policy := download.ConflictPolicy
		if response := transferActionResponse(job.Job.CheckpointJSON); response != "" {
			policy = response
		}
		planned, conflicts, planErr := w.planRemoteNodeUpload(ctx, download, manifest, policy)
		if planErr != nil {
			return w.cloudFailure(task, planErr)
		}
		if conflicts > 0 && policy == models.MediaLibraryConflictAsk {
			return WorkerResult{Wait: &WaitForAction{ActionType: "transfer_conflict", Prompt: "目标媒体库存在同名文件，请选择处理方式", Options: []string{models.MediaLibraryConflictOverwrite, models.MediaLibraryConflictSkip, models.MediaLibraryConflictRename}, Preview: map[string]string{"媒体库": task.LibraryName, "冲突文件": strconv.Itoa(conflicts)}, Checkpoint: map[string]any{"conflict_count": conflicts}}}
		}
		rows, err = w.createRemoteUploadFiles(ctx, task, planned)
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			rows, exists, err = w.loadRemoteUploadFiles(ctx, task, download, manifest)
			if err == nil && !exists {
				err = errors.New("remote upload plan disappeared after concurrent creation")
			}
		}
		if err != nil {
			return w.cloudFailure(task, cloudTransferError("transfer_state_persist_failed", true, err))
		}
	}
	if len(rows) == 0 {
		return w.cloudFailure(task, cloudTransferError("transfer_plan_invalid", false, nil))
	}
	if err := w.persistRemoteUploadProgress(ctx, &task, rows, models.TransferTaskStatusPlanning); err != nil {
		return w.cloudFailure(task, cloudTransferError("transfer_state_persist_failed", true, err))
	}

	permit, err := enterCatalogPhysicalWrite(ctx, w.service.db, CatalogPhysicalWriteInput{LibraryID: task.LibraryID, OwnerKind: CatalogPhysicalTransfer, OwnerID: task.ID, Job: &job})
	if err != nil {
		return w.cloudFailure(task, cloudTransferError("transfer_write_admission_failed", true, err))
	}
	defer quiesceCatalogPhysicalWrite(w.service.db, permit, w.service.log)

	batches, err := buildRemoteUploadBatches(rows, storage, download, manifest)
	if err != nil {
		return w.cloudFailure(task, cloudTransferError("transfer_plan_invalid", false, err))
	}
	for _, batch := range batches {
		if ctx.Err() != nil {
			return WorkerResult{}
		}
		if remoteUploadRowsTerminal(batch.rows) {
			continue
		}
		if err := w.refreshRemoteUploadExport(ctx, &task, download, &manifest); err != nil {
			return w.cloudFailure(task, err)
		}
		batch.plan.SourceExportOperationKey = manifest.RemoteExportOperationKey
		batch.plan.SourceManifestDigest = manifest.RemoteExportDigest
		if err := w.executeRemoteUploadBatch(ctx, runtime, client, node, storage, &task, download, rows, batch); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return WorkerResult{}
			}
			return w.cloudFailure(task, err)
		}
	}
	if !remoteUploadRowsTerminal(rows) {
		return w.cloudFailure(task, cloudTransferError(nodeprotocol.ErrorReconciliationNeeded, true, nil))
	}

	now := time.Now().UTC()
	err = w.service.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := ensureDownloadPipelineActive(tx, task.DownloadTaskID); err != nil {
			return err
		}
		if err := captureRemoteUploadManagedItems(tx, task, download, rows); err != nil {
			return err
		}
		if err := tx.Model(&models.MediaLibrary{}).Where("id = ?", task.LibraryID).UpdateColumn("dirty_generation", gorm.Expr("dirty_generation + 1")).Error; err != nil {
			return err
		}
		summary, err := remoteUploadPlanSummary(rows)
		if err != nil {
			return err
		}
		encoded, err := encodeTransferPlanSummary(summary)
		if err != nil {
			return err
		}
		if result := tx.Model(&models.TransferTask{}).Where("id = ? AND download_task_id = ?", task.ID, download.ID).Updates(map[string]any{"phase": models.TransferTaskStatusCompleted, "processed_files": len(rows), "total_files": len(rows), "plan_summary_json": encoded, "last_error_code": "", "finished_at": now, "updated_at": now}); result.Error != nil || result.RowsAffected != 1 {
			return firstNonNil(result.Error, gorm.ErrRecordNotFound)
		}
		if err := SettleCatalogPhysicalWriteTx(tx, permit, &job); err != nil {
			return err
		}
		return w.service.audit.Record(tx, &task.OwnerID, "transfer.complete", "transfer_task", task.ID, "success", map[string]any{"download_task_id": task.DownloadTaskID, "media_library_id": task.LibraryID, "mode": "remote_node_upload", "files": len(rows), "provider": cloudpkg.ProviderPan115, "node_id": node.ID}, RequestContext{})
	})
	if errors.Is(err, context.Canceled) {
		return WorkerResult{}
	}
	if err != nil {
		return w.cloudFailure(task, cloudTransferError("transfer_state_persist_failed", true, err))
	}
	serverlog.OperationPan115CloudTransfer.Event(w.service.log.Info()).Str("task_id", task.ID).Uint("library_id", task.LibraryID).Str("node_id", node.ID).Int("files", len(rows)).Int64("duration_ms", time.Since(started).Milliseconds()).Msg(serverlog.OperationPan115CloudTransfer.Message("传输节点直传完成"))
	task.Phase = models.TransferTaskStatusCompleted
	return w.finishCompletedTransfer(ctx, task)
}

func buildRemoteUploadBatches(rows []models.RemoteUploadFile, storage models.Storage, download models.DownloadTask, manifest downloadpkg.Manifest) ([]remoteUploadBatch, error) {
	if len(rows) == 0 || storage.ID == 0 || download.ID == "" || manifest.RemoteExportOperationKey == "" || !nodeprotocol.ValidDigest(manifest.RemoteExportDigest) {
		return nil, errors.New("remote upload batch input is incomplete")
	}
	base := nodeprotocol.StorageUploadPlan{StorageID: strconv.FormatUint(uint64(storage.ID), 10), ProviderType: nodeprotocol.StorageProviderPan115, SourceExportOperationKey: manifest.RemoteExportOperationKey, SourceManifestDigest: manifest.RemoteExportDigest, TargetRootID: download.TargetProviderRootID}
	batches := make([]remoteUploadBatch, 0, (len(rows)+nodeprotocol.MaxStorageUploadFiles-1)/nodeprotocol.MaxStorageUploadFiles)
	for offset := 0; offset < len(rows); {
		plan := base
		plan.Files = make([]nodeprotocol.StorageUploadFile, 0, min(nodeprotocol.MaxStorageUploadFiles, len(rows)-offset))
		end := offset
		for end < len(rows) && len(plan.Files) < nodeprotocol.MaxStorageUploadFiles {
			row := rows[end]
			candidate := nodeprotocol.StorageUploadFile{SourceFileToken: row.SourceFileToken, TargetRelativePath: row.TargetRelative, Size: row.Size, SHA256: row.SHA256, ConflictAction: row.ConflictAction}
			plan.Files = append(plan.Files, candidate)
			encoded, err := json.Marshal(plan)
			if err != nil {
				return nil, err
			}
			if len(encoded) > nodeprotocol.MaxWireBodyBytes {
				plan.Files = plan.Files[:len(plan.Files)-1]
				if len(plan.Files) == 0 {
					return nil, errors.New("one remote upload file exceeds the wire plan limit")
				}
				break
			}
			end++
		}
		if len(plan.Files) == 0 || plan.Validate() != nil {
			return nil, errors.New("remote upload batch is invalid")
		}
		batchRows := rows[offset:end]
		batches = append(batches, remoteUploadBatch{index: len(batches), rows: batchRows, plan: plan})
		offset = end
	}
	return batches, nil
}

func (w *TransferWorker) executeRemoteUploadBatch(ctx context.Context, runtime JobRuntime, client remoteUploadNodeClient, node models.TransferNode, storage models.Storage, task *models.TransferTask, download models.DownloadTask, allRows []models.RemoteUploadFile, batch remoteUploadBatch) error {
	operationKey := remoteUploadOperationKey(task.ID, batch.index)
	plan, record, completed, err := w.prepareRemoteUploadOperation(ctx, node.ID, download.ID, operationKey, batch.plan)
	if err != nil {
		return err
	}
	requestID := operationKey
	request := nodeprotocol.StorageActionRequest{RequestID: requestID, TaskID: download.ID, OperationKey: operationKey, PlanDigest: record.PlanDigest, StorageID: batch.plan.StorageID, Action: nodeprotocol.StorageActionUpload}
	var response nodeprotocol.StorageActionResponse
	if completed {
		response, err = client.StorageActionResult(ctx, requestID, download.ID, operationKey)
	} else {
		remote, putErr := client.PutOperation(ctx, nodeprotocol.PutOperationRequest{Plan: plan, PlanDigest: record.PlanDigest})
		if putErr != nil {
			return remoteUploadClientError(putErr)
		}
		if err := validateRemoteOperationResponse(remote, record); err != nil {
			return err
		}
		if remote.Status == nodeprotocol.OperationFailed || remote.Status == nodeprotocol.OperationCancelled {
			return cloudTransferError(firstNonEmpty(remote.ErrorCode, nodeprotocol.ErrorPlanConflict), false, nil)
		}
		nodeCompleted := remote.Status == nodeprotocol.OperationCompleted
		if !nodeCompleted && (remote.LeaseEpoch != plan.LeaseEpoch || remote.LeaseExpiresAt.Before(plan.LeaseExpiresAt)) {
			remote, putErr = client.RenewOperationLease(ctx, operationKey, nodeprotocol.LeaseRequest{PlanDigest: record.PlanDigest, LeaseEpoch: plan.LeaseEpoch, LeaseExpiresAt: plan.LeaseExpiresAt})
			if putErr != nil {
				return remoteUploadClientError(putErr)
			}
			if err := validateRemoteOperationResponse(remote, record); err != nil {
				return err
			}
		}
		if err := w.persistRemoteOperationResponse(ctx, record, remote); err != nil {
			return err
		}
		if nodeCompleted {
			response, err = client.StorageActionResult(ctx, requestID, download.ID, operationKey)
		} else {
			request, err = w.authorizeRemoteUpload(ctx, client, node, storage, request, batch.rows)
			if err != nil {
				return err
			}
			response, err = client.StorageAction(ctx, request)
		}
	}
	if err != nil {
		return remoteUploadClientError(err)
	}

	credentialRefreshes := 0
	leaseExpiresAt := plan.LeaseExpiresAt
	for {
		if err := validateStorageActionResponse(response, request, len(batch.rows)); err != nil {
			return err
		}
		if err := w.applyRemoteUploadResponse(ctx, task, allRows, batch.rows, response); err != nil {
			return err
		}
		phase := nodeprotocol.PhaseUploadingTarget
		switch response.Status {
		case nodeprotocol.StorageActionWaitingCredentials:
			phase = nodeprotocol.PhaseWaitingCredentials
		case nodeprotocol.StorageActionReconciliation:
			phase = nodeprotocol.PhaseVerifyingTarget
		case nodeprotocol.StorageActionCompleted:
			phase = nodeprotocol.PhaseCompleted
		}
		status := nodeprotocol.OperationRunning
		if response.Status == nodeprotocol.StorageActionCompleted {
			status = nodeprotocol.OperationCompleted
		}
		progress := float64(response.CompletedFiles) / float64(len(batch.rows))
		if err := w.persistRemoteOperationState(ctx, record, status, phase, response.ErrorCode, &progress); err != nil {
			return err
		}
		if response.Status == nodeprotocol.StorageActionCompleted {
			if !remoteUploadRowsTerminal(batch.rows) {
				return cloudTransferError(nodeprotocol.ErrorReconciliationNeeded, true, errors.New("node completed without all file receipts"))
			}
			return nil
		}
		switch response.ErrorCode {
		case nodeprotocol.ErrorCredentialExpired:
			if credentialRefreshes > 0 {
				return cloudTransferError(nodeprotocol.ErrorCredentialExpired, true, nil)
			}
			credentialRefreshes++
			var renewErr error
			leaseExpiresAt, renewErr = w.renewRemoteUploadOperation(ctx, client, &record)
			if renewErr != nil {
				return renewErr
			}
			request, renewErr = w.authorizeRemoteUpload(ctx, client, node, storage, request, batch.rows)
			if renewErr != nil {
				return renewErr
			}
			response, err = client.StorageAction(ctx, request)
			if err != nil {
				return remoteUploadClientError(err)
			}
			continue
		case nodeprotocol.ErrorTargetRateLimited:
			return cloudTransferError(cloudpkg.CodeRateLimited, true, nil)
		case nodeprotocol.ErrorReconciliationNeeded:
			return cloudTransferError(nodeprotocol.ErrorReconciliationNeeded, true, nil)
		case nodeprotocol.ErrorLeaseExpired:
			var renewErr error
			leaseExpiresAt, renewErr = w.renewRemoteUploadOperation(ctx, client, &record)
			if renewErr != nil {
				return renewErr
			}
			response, err = client.StorageAction(ctx, request)
			if err != nil {
				return remoteUploadClientError(err)
			}
			continue
		case nodeprotocol.ErrorPlanConflict, nodeprotocol.ErrorChecksumMismatch, nodeprotocol.ErrorCapabilityMissing:
			return cloudTransferError(response.ErrorCode, false, nil)
		case "":
		default:
			return cloudTransferError(response.ErrorCode, true, nil)
		}

		processed, total := int64(remoteUploadTerminalCount(allRows)), int64(len(allRows))
		progress = float64(processed) * 100 / float64(total)
		if err := runtime.Heartbeat(&progress, &processed, &total, nil, nil); err != nil {
			return cloudTransferError(CodeQueueLeaseInvalid, true, err)
		}
		if time.Until(leaseExpiresAt) < 2*time.Minute {
			leaseExpiresAt, err = w.renewRemoteUploadOperation(ctx, client, &record)
			if err != nil {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(remoteUploadPollInterval):
		}
		response, err = client.StorageActionResult(ctx, requestID, download.ID, operationKey)
		if err != nil {
			return remoteUploadClientError(err)
		}
	}
}

func (w *TransferWorker) authorizeRemoteUpload(ctx context.Context, client remoteUploadNodeClient, node models.TransferNode, storage models.Storage, request nodeprotocol.StorageActionRequest, rows []models.RemoteUploadFile) (nodeprotocol.StorageActionRequest, error) {
	actions := []string{nodeprotocol.StorageActionUpload}
	for _, row := range rows {
		if row.ConflictAction == nodeprotocol.StorageConflictReplace {
			actions = append(actions, nodeprotocol.StorageActionReplace)
			break
		}
	}
	envelope, err := w.service.downloader.transferNodes.sealStorageGrant(ctx, node, storage, request.TaskID, request.OperationKey, actions)
	if err != nil {
		return request, err
	}
	if err := client.PutCredentialGrant(ctx, envelope); err != nil {
		return request, remoteUploadClientError(err)
	}
	request.GrantID = envelope.GrantID
	return request, nil
}

func (w *TransferWorker) prepareRemoteUploadOperation(ctx context.Context, nodeID, taskID, operationKey string, payload nodeprotocol.StorageUploadPlan) (nodeprotocol.OperationPlan, models.RemoteOperation, bool, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nodeprotocol.OperationPlan{}, models.RemoteOperation{}, false, err
	}
	now := time.Now().UTC()
	plan := nodeprotocol.OperationPlan{ProtocolVersion: nodeprotocol.VersionV1, OperationKey: operationKey, TaskID: taskID, NodeID: nodeID, Kind: nodeprotocol.OperationKindStorageUpload, PlanRevision: 1, LeaseEpoch: 1, LeaseExpiresAt: now.Add(remoteUploadLeaseDuration), Payload: raw}
	if err := plan.Validate(now); err != nil {
		return nodeprotocol.OperationPlan{}, models.RemoteOperation{}, false, cloudTransferError("transfer_plan_invalid", false, err)
	}
	digest, err := plan.Digest()
	if err != nil {
		return nodeprotocol.OperationPlan{}, models.RemoteOperation{}, false, err
	}
	var record models.RemoteOperation
	completed := false
	err = w.service.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("operation_key = ?", operationKey).First(&record).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			record = models.RemoteOperation{OperationKey: operationKey, TaskID: taskID, NodeID: nodeID, PlanDigest: digest, PlanRevision: plan.PlanRevision, LeaseEpoch: plan.LeaseEpoch, Phase: nodeprotocol.PhaseAccepted, Status: nodeprotocol.OperationPending, CreatedAt: now, UpdatedAt: now}
			return tx.Create(&record).Error
		}
		if err != nil {
			return err
		}
		if record.TaskID != taskID || record.NodeID != nodeID || record.PlanDigest != digest || record.PlanRevision != plan.PlanRevision {
			return cloudTransferError(nodeprotocol.ErrorPlanConflict, false, nil)
		}
		if record.Status == nodeprotocol.OperationCompleted {
			completed = true
			plan.LeaseEpoch = record.LeaseEpoch
			return nil
		}
		if record.Status == nodeprotocol.OperationFailed || record.Status == nodeprotocol.OperationCancelled {
			return cloudTransferError(nodeprotocol.ErrorPlanConflict, false, nil)
		}
		plan.LeaseEpoch = record.LeaseEpoch + 1
		plan.LeaseExpiresAt = now.Add(remoteUploadLeaseDuration)
		record.LeaseEpoch = plan.LeaseEpoch
		record.UpdatedAt = now
		return tx.Model(&models.RemoteOperation{}).Where("id = ? AND plan_digest = ?", record.ID, digest).Updates(map[string]any{"lease_epoch": record.LeaseEpoch, "updated_at": now}).Error
	})
	return plan, record, completed, err
}

func (w *TransferWorker) renewRemoteUploadOperation(ctx context.Context, client remoteUploadNodeClient, record *models.RemoteOperation) (time.Time, error) {
	if record == nil {
		return time.Time{}, cloudTransferError(nodeprotocol.ErrorPlanConflict, false, nil)
	}
	now := time.Now().UTC()
	epoch := record.LeaseEpoch + 1
	expiresAt := now.Add(remoteUploadLeaseDuration)
	remote, err := client.RenewOperationLease(ctx, record.OperationKey, nodeprotocol.LeaseRequest{PlanDigest: record.PlanDigest, LeaseEpoch: epoch, LeaseExpiresAt: expiresAt})
	if err != nil {
		return time.Time{}, remoteUploadClientError(err)
	}
	expected := *record
	expected.LeaseEpoch = epoch
	if err := validateRemoteOperationResponse(remote, expected); err != nil {
		return time.Time{}, err
	}
	result := w.service.db.WithContext(ctx).Model(&models.RemoteOperation{}).Where("id = ? AND plan_digest = ? AND lease_epoch = ? AND status NOT IN ?", record.ID, record.PlanDigest, record.LeaseEpoch, []string{nodeprotocol.OperationCompleted, nodeprotocol.OperationFailed, nodeprotocol.OperationCancelled}).Updates(map[string]any{"lease_epoch": epoch, "updated_at": now})
	if result.Error != nil || result.RowsAffected != 1 {
		return time.Time{}, cloudTransferError(nodeprotocol.ErrorPlanConflict, false, firstNonNil(result.Error, gorm.ErrRecordNotFound))
	}
	record.LeaseEpoch = epoch
	if err := w.persistRemoteOperationResponse(ctx, *record, remote); err != nil {
		return time.Time{}, err
	}
	return expiresAt, nil
}

func validateRemoteOperationResponse(response nodeprotocol.OperationResponse, record models.RemoteOperation) error {
	if response.OperationKey != record.OperationKey || response.PlanDigest != record.PlanDigest || response.LeaseEpoch == 0 || response.Revision == 0 || response.Status != nodeprotocol.OperationCompleted && response.LeaseEpoch < record.LeaseEpoch {
		return cloudTransferError(nodeprotocol.ErrorPlanConflict, false, nil)
	}
	return nil
}

func (w *TransferWorker) persistRemoteOperationResponse(ctx context.Context, record models.RemoteOperation, response nodeprotocol.OperationResponse) error {
	return w.persistRemoteOperationState(ctx, record, response.Status, response.Phase, response.ErrorCode, response.Progress)
}

func (w *TransferWorker) persistRemoteOperationState(ctx context.Context, record models.RemoteOperation, status, phase, errorCode string, progress *float64) error {
	updates := map[string]any{"status": status, "phase": phase, "progress": progress, "error_code": safeLabel(errorCode, 96), "updated_at": time.Now().UTC()}
	result := w.service.db.WithContext(ctx).Model(&models.RemoteOperation{}).Where("id = ? AND operation_key = ? AND plan_digest = ?", record.ID, record.OperationKey, record.PlanDigest).Updates(updates)
	if result.Error != nil || result.RowsAffected != 1 {
		return cloudTransferError("transfer_state_persist_failed", true, firstNonNil(result.Error, gorm.ErrRecordNotFound))
	}
	return nil
}

func validateStorageActionResponse(response nodeprotocol.StorageActionResponse, request nodeprotocol.StorageActionRequest, total int) error {
	if response.RequestID != request.RequestID || response.OperationKey != request.OperationKey || response.PlanDigest != request.PlanDigest || response.TotalFiles != total || response.CompletedFiles < 0 || response.CompletedFiles > total || len(response.Files) > total {
		return cloudTransferError(nodeprotocol.ErrorPlanConflict, false, nil)
	}
	switch response.Status {
	case nodeprotocol.StorageActionAccepted, nodeprotocol.StorageActionRunning, nodeprotocol.StorageActionWaitingCredentials, nodeprotocol.StorageActionReconciliation, nodeprotocol.StorageActionCompleted:
		return nil
	default:
		return cloudTransferError(nodeprotocol.ErrorPlanConflict, false, nil)
	}
}

func (w *TransferWorker) applyRemoteUploadResponse(ctx context.Context, task *models.TransferTask, allRows, batchRows []models.RemoteUploadFile, response nodeprotocol.StorageActionResponse) error {
	byToken := make(map[string]int, len(batchRows))
	for index := range batchRows {
		byToken[batchRows[index].SourceFileToken] = index
	}
	seen := make(map[string]struct{}, len(response.Files))
	for _, file := range response.Files {
		index, exists := byToken[file.SourceFileToken]
		if !exists || file.Size != batchRows[index].Size || file.TargetName != pathpkg.Base(batchRows[index].TargetRelative) {
			return cloudTransferError(nodeprotocol.ErrorPlanConflict, false, nil)
		}
		if _, duplicate := seen[file.SourceFileToken]; duplicate {
			return cloudTransferError(nodeprotocol.ErrorPlanConflict, false, nil)
		}
		seen[file.SourceFileToken] = struct{}{}
		if !remoteUploadResultTerminal(file.Status) {
			continue
		}
		if file.Status != nodeprotocol.StorageFileSkipped && (strings.TrimSpace(file.TargetParentID) == "" || strings.TrimSpace(file.TargetItemID) == "" || !validRemoteSHA1(file.SHA1)) {
			return cloudTransferError(nodeprotocol.ErrorReconciliationNeeded, true, nil)
		}
		if file.Status == nodeprotocol.StorageFileSkipped && (file.TargetItemID != "" || file.SHA1 != "") {
			return cloudTransferError(nodeprotocol.ErrorPlanConflict, false, nil)
		}
		row := &batchRows[index]
		if remoteUploadResultTerminal(row.Status) {
			if row.Status != file.Status || row.TargetParentID != file.TargetParentID || row.TargetItemID != file.TargetItemID || !strings.EqualFold(row.TargetSHA1, file.SHA1) {
				return cloudTransferError(nodeprotocol.ErrorPlanConflict, false, nil)
			}
			continue
		}
		now := time.Now().UTC()
		result := w.service.db.WithContext(ctx).Model(&models.RemoteUploadFile{}).Where("id = ? AND transfer_task_id = ? AND source_file_token = ? AND status NOT IN ?", row.ID, task.ID, row.SourceFileToken, []string{nodeprotocol.StorageFileCompleted, nodeprotocol.StorageFileReused, nodeprotocol.StorageFileSkipped}).Updates(map[string]any{"status": file.Status, "target_parent_id": file.TargetParentID, "target_item_id": file.TargetItemID, "target_sha1": strings.ToUpper(file.SHA1), "updated_at": now})
		if result.Error != nil || result.RowsAffected != 1 {
			return cloudTransferError("transfer_state_persist_failed", true, firstNonNil(result.Error, gorm.ErrRecordNotFound))
		}
		row.Status, row.TargetParentID, row.TargetItemID, row.TargetSHA1, row.UpdatedAt = file.Status, file.TargetParentID, file.TargetItemID, strings.ToUpper(file.SHA1), now
		for allIndex := range allRows {
			if allRows[allIndex].ID == row.ID {
				allRows[allIndex] = *row
				break
			}
		}
	}
	return w.persistRemoteUploadProgress(ctx, task, allRows, models.TransferTaskStatusTransferring)
}

func (w *TransferWorker) persistRemoteUploadProgress(ctx context.Context, task *models.TransferTask, rows []models.RemoteUploadFile, phase string) error {
	summary, err := remoteUploadPlanSummary(rows)
	if err != nil {
		return err
	}
	encoded, err := encodeTransferPlanSummary(summary)
	if err != nil {
		return err
	}
	processed := remoteUploadTerminalCount(rows)
	result := w.service.db.WithContext(ctx).Model(&models.TransferTask{}).Where("id = ?", task.ID).Updates(map[string]any{"phase": phase, "processed_files": processed, "total_files": len(rows), "plan_summary_json": encoded, "updated_at": time.Now().UTC()})
	if result.Error != nil || result.RowsAffected != 1 {
		return firstNonNil(result.Error, gorm.ErrRecordNotFound)
	}
	task.Phase, task.ProcessedFiles, task.TotalFiles, task.PlanSummaryJSON = phase, processed, len(rows), encoded
	return nil
}

func remoteUploadPlanSummary(rows []models.RemoteUploadFile) (TransferPlanSummary, error) {
	plan := make([]transferPlanItem, 0, min(len(rows), maxTransferPlanSummaryItems))
	for index := range rows {
		if len(plan) == maxTransferPlanSummaryItems {
			break
		}
		plan = append(plan, transferPlanItem{Relative: rows[index].TargetRelative, Size: rows[index].Size})
	}
	summary, err := newTransferPlanSummary(plan)
	if err != nil {
		return TransferPlanSummary{}, err
	}
	summary.TotalFiles = len(rows)
	summary.Truncated = len(rows) > len(summary.Items)
	for index := range summary.Items {
		switch rows[index].Status {
		case nodeprotocol.StorageFileCompleted, nodeprotocol.StorageFileReused:
			summary.Items[index].Result = "completed"
		case nodeprotocol.StorageFileSkipped:
			summary.Items[index].Result = "skipped"
		}
	}
	for _, row := range rows {
		switch row.Status {
		case nodeprotocol.StorageFileCompleted, nodeprotocol.StorageFileReused:
			summary.CompletedFiles++
		case nodeprotocol.StorageFileSkipped:
			summary.SkippedFiles++
		}
	}
	return summary, nil
}

func captureRemoteUploadManagedItems(tx *gorm.DB, task models.TransferTask, download models.DownloadTask, rows []models.RemoteUploadFile) error {
	for _, row := range rows {
		if row.Status == nodeprotocol.StorageFileSkipped {
			continue
		}
		if (row.Status != nodeprotocol.StorageFileCompleted && row.Status != nodeprotocol.StorageFileReused) || strings.TrimSpace(row.TargetItemID) == "" || strings.TrimSpace(row.TargetParentID) == "" || !validRemoteSHA1(row.TargetSHA1) {
			return errors.New("remote upload result is incomplete")
		}
		kind := models.MediaManagedItemKindSidecar
		if isVideoFile(row.TargetRelative) {
			kind = models.MediaManagedItemKindVideo
		}
		if err := upsertManagedItem(tx, task, download, row.TargetRelative, kind, row.Size, row.TargetItemID, row.TargetParentID); err != nil {
			return err
		}
	}
	return nil
}

func remoteUploadRowsTerminal(rows []models.RemoteUploadFile) bool {
	return len(rows) > 0 && remoteUploadTerminalCount(rows) == len(rows)
}

func remoteUploadTerminalCount(rows []models.RemoteUploadFile) int {
	count := 0
	for _, row := range rows {
		if remoteUploadResultTerminal(row.Status) {
			count++
		}
	}
	return count
}

func remoteUploadResultTerminal(status string) bool {
	return status == nodeprotocol.StorageFileCompleted || status == nodeprotocol.StorageFileReused || status == nodeprotocol.StorageFileSkipped
}

func remoteUploadOperationKey(transferTaskID string, batch int) string {
	digest := sha256.Sum256([]byte("remote-storage-upload-v1\x00" + transferTaskID + "\x00" + strconv.Itoa(batch)))
	return "storage:upload:" + hex.EncodeToString(digest[:20])
}

func validRemoteSHA1(value string) bool {
	if len(value) != 40 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func (w *TransferWorker) refreshRemoteUploadExport(ctx context.Context, task *models.TransferTask, download models.DownloadTask, manifest *downloadpkg.Manifest) error {
	if manifest.RemoteExportExpiresAt != nil && manifest.RemoteExportExpiresAt.After(time.Now().UTC().Add(2*time.Minute)) {
		return nil
	}
	refreshed, err := w.service.downloader.refreshRemoteFileExport(ctx, download)
	if err != nil {
		return err
	}
	if refreshed.RemoteExportOperationKey != manifest.RemoteExportOperationKey || refreshed.RemoteExportDigest != manifest.RemoteExportDigest || refreshed.RemoteExportExpiresAt == nil || !refreshed.RemoteExportExpiresAt.After(time.Now().UTC()) {
		return cloudTransferError("node_file_export_changed", false, nil)
	}
	manifest.RemoteExportExpiresAt = refreshed.RemoteExportExpiresAt
	encoded, err := json.Marshal(manifest)
	if err != nil || len(encoded) > nodeprotocol.MaxWireBodyBytes {
		return cloudTransferError("node_file_export_invalid", false, err)
	}
	if result := w.service.db.WithContext(ctx).Model(&models.TransferTask{}).Where("id = ?", task.ID).Updates(map[string]any{"manifest_json": string(encoded), "updated_at": time.Now().UTC()}); result.Error != nil || result.RowsAffected != 1 {
		return cloudTransferError("transfer_state_persist_failed", true, firstNonNil(result.Error, gorm.ErrRecordNotFound))
	}
	task.ManifestJSON = string(encoded)
	return nil
}

func remoteUploadClientError(err error) error {
	if err == nil {
		return nil
	}
	var remote *nodeclient.RemoteError
	if errors.As(err, &remote) {
		switch remote.Code {
		case nodeprotocol.ErrorPlanConflict, nodeprotocol.ErrorChecksumMismatch, nodeprotocol.ErrorCapabilityMissing, nodeprotocol.ErrorProtocolIncompatible:
			return cloudTransferError(remote.Code, false, err)
		case nodeprotocol.ErrorTargetRateLimited:
			return cloudTransferError(cloudpkg.CodeRateLimited, true, err)
		default:
			return cloudTransferError(remote.Code, true, err)
		}
	}
	return cloudTransferError(nodeprotocol.ErrorNodeOffline, true, err)
}
