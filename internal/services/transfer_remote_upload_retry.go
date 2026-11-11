package services

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
	"gorm.io/gorm"
)

type uploadRetryIntent struct {
	Batch        int    `json:"batch"`
	OperationKey string `json:"operation_key"`
	PlanDigest   string `json:"plan_digest"`
}

type uploadRetryCheckpoint struct {
	Attempts map[string]nodeprotocol.StorageUploadRetry `json:"upload_attempts"`
	Pending  *uploadRetryIntent                         `json:"upload_retry"`
	Response struct {
		Version    uint64 `json:"version"`
		ActionType string `json:"action_type"`
		Value      string `json:"value"`
	} `json:"action_response"`
}

func (w *TransferWorker) pendingRemoteUploadRetry(ctx context.Context, task models.TransferTask, batch int) bool {
	var job models.Job
	var checkpoint uploadRetryCheckpoint
	return w.service.db.WithContext(ctx).Select("checkpoint_json").First(&job, "id=?", task.JobID).Error == nil && json.Unmarshal([]byte(job.CheckpointJSON), &checkpoint) == nil && checkpoint.Pending != nil && checkpoint.Pending.Batch == batch && checkpoint.Response.ActionType != "upload_retry"
}

// Every reader (execution, stop/join and catalog publication) resolves the same
// durable action generation. A lost response cannot silently select the old key.
func remoteUploadBatchIdentityTx(tx *gorm.DB, task models.TransferTask, batch remoteUploadBatch) (string, nodeprotocol.StorageUploadPlan, error) {
	key, plan := remoteUploadOperationKey(task.ID, batch.index), batch.plan
	var job models.Job
	if err := tx.Select("id,checkpoint_json").First(&job, "id=?", task.JobID).Error; err != nil {
		return "", plan, err
	}
	var checkpoint uploadRetryCheckpoint
	if json.Unmarshal([]byte(job.CheckpointJSON), &checkpoint) != nil {
		return "", plan, ErrCatalogFence
	}
	if previous, found := checkpoint.Attempts[strconv.Itoa(batch.index)]; found {
		plan.RetryOf = &previous
		if plan.Validate() != nil {
			return "", plan, ErrCatalogFence
		}
		var count int64
		if err := tx.Model(&models.JobActionRequest{}).Where("job_id=? AND version=? AND action_type=? AND response=? AND responded_by IS NOT NULL AND responded_at IS NOT NULL", task.JobID, previous.ActionVersion, "upload_retry", "retry_upload").Count(&count).Error; err != nil || count != 1 {
			return "", plan, firstNonNil(err, ErrCatalogFence)
		}
		key = nodeprotocol.StorageUploadRetryKey(previous.OperationKey, previous.ActionVersion)
	}
	return key, plan, nil
}

func requireUploadRetryCapability(ctx context.Context, raw remoteUploadNodeClient, nodeID string) error {
	client, ok := raw.(interface {
		Health(context.Context) (nodeprotocol.HealthResponse, error)
	})
	if !ok {
		return cloudTransferError(nodeprotocol.ErrorCapabilityMissing, false, nil)
	}
	health, err := client.Health(ctx) // authenticated and fresh, never enrollment cache
	if err != nil {
		return remoteUploadClientError(err)
	}
	if health.NodeID != nodeID || health.ProtocolMin > nodeprotocol.VersionV1 || health.ProtocolMax < nodeprotocol.VersionV1 || !health.Capabilities.Has(nodeprotocol.CapabilityStorageUploadRetryV1) {
		return cloudTransferError(nodeprotocol.ErrorCapabilityMissing, false, nil)
	}
	return nil
}

// Consume only a response actually persisted by Queue.Respond. Existing claim
// and epoch fences protect this local CAS before any child operation is sent.
func (w *TransferWorker) activateRemoteUploadRetry(ctx context.Context, client remoteUploadNodeClient, task models.TransferTask, download models.DownloadTask, batches []remoteUploadBatch) error {
	var current models.Job
	if err := w.service.db.WithContext(ctx).First(&current, "id=?", task.JobID).Error; err != nil {
		return err
	}
	var state uploadRetryCheckpoint
	if json.Unmarshal([]byte(current.CheckpointJSON), &state) != nil {
		return ErrCatalogFence
	}
	if state.Response.ActionType != "upload_retry" || state.Response.Value != "retry_upload" {
		return nil
	}
	if task.NodeID == nil {
		return ErrCatalogFence
	}
	if err := requireUploadRetryCapability(ctx, client, *task.NodeID); err != nil {
		return err
	}
	return w.service.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := assertRemoteUploadClaimTx(tx, ctx); err != nil {
			return err
		}
		if err := tx.First(&current, "id=?", task.JobID).Error; err != nil {
			return err
		}
		if json.Unmarshal([]byte(current.CheckpointJSON), &state) != nil || state.Pending == nil || state.Response.Version == 0 || state.Pending.Batch < 0 || state.Pending.Batch >= len(batches) {
			return ErrCatalogFence
		}
		var action models.JobActionRequest
		if err := tx.First(&action, "job_id=? AND version=? AND action_type=? AND response=? AND responded_by IS NOT NULL AND responded_at IS NOT NULL", task.JobID, state.Response.Version, "upload_retry", "retry_upload").Error; err != nil {
			return ErrCatalogFence
		}
		record, err := remoteUploadBatchRecordTx(tx, task, download, batches[state.Pending.Batch])
		if err != nil || record.OperationKey != state.Pending.OperationKey || record.PlanDigest != state.Pending.PlanDigest || record.Status == nodeprotocol.OperationCompleted {
			return firstNonNil(err, ErrCatalogFence)
		}
		if state.Attempts == nil {
			state.Attempts = map[string]nodeprotocol.StorageUploadRetry{}
		}
		state.Attempts[strconv.Itoa(state.Pending.Batch)] = nodeprotocol.StorageUploadRetry{OperationKey: record.OperationKey, PlanDigest: record.PlanDigest, ActionVersion: action.Version}
		var checkpoint map[string]any
		if json.Unmarshal([]byte(current.CheckpointJSON), &checkpoint) != nil || checkpoint == nil {
			return ErrCatalogFence
		}
		checkpoint["upload_attempts"] = state.Attempts
		delete(checkpoint, "upload_retry")
		delete(checkpoint, "action_response")
		raw, err := json.Marshal(checkpoint)
		if err != nil || len(raw) > 64*1024 {
			return firstNonNil(err, ErrCatalogFence)
		}
		result := fenceRemoteUploadClaim(tx.Model(&models.Job{}).Where("id=? AND checkpoint_json=?", task.JobID, current.CheckpointJSON), ctx).Update("checkpoint_json", string(raw))
		if result.Error != nil || result.RowsAffected != 1 {
			return firstNonNil(result.Error, ErrCatalogFence)
		}
		return nil
	})
}

// Unknown output is retained. Only a full, unambiguous read in the retained
// parent scope permits asking the user for a new attempt; it is never automatic.
func (w *TransferWorker) waitForRemoteUploadRetry(ctx context.Context, client remoteUploadNodeClient, task models.TransferTask, download models.DownloadTask, batches []remoteUploadBatch, batch remoteUploadBatch, proof remoteUploadStopProof) (*WaitForAction, error) {
	var observed struct {
		TargetCheck *nodeprotocol.StorageTargetCheck `json:"target_check"`
	}
	if json.Unmarshal([]byte(task.UploadProgressJSON), &observed) != nil || observed.TargetCheck == nil || !observed.TargetCheck.Valid() || !observed.TargetCheck.Complete {
		return nil, nil
	}
	check := observed.TargetCheck
	if check.Reason != "target_missing" && check.Reason != "digest_missing" && check.Reason != "digest_mismatch" || check.NameMatches == nil || *check.NameMatches > 1 {
		return nil, nil
	}
	if task.NodeID == nil {
		return nil, ErrCatalogFence
	}
	if err := requireUploadRetryCapability(ctx, client, *task.NodeID); err != nil {
		return nil, err
	}
	stopCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := w.stopRemoteUploadExecution(stopCtx, client, task, download, batches, proof); err != nil {
		return nil, err
	}
	var checkpoint map[string]any
	var record models.RemoteOperation
	err := w.service.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := assertRemoteUploadClaimTx(tx, ctx); err != nil {
			return err
		}
		var err error
		record, err = remoteUploadBatchRecordTx(tx, task, download, batch)
		if err != nil {
			return err
		}
		var job models.Job
		if err := tx.First(&job, "id=?", task.JobID).Error; err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(job.CheckpointJSON), &checkpoint); err != nil {
			return err
		}
		if checkpoint == nil {
			checkpoint = map[string]any{}
		}
		checkpoint["upload_retry"] = uploadRetryIntent{Batch: batch.index, OperationKey: record.OperationKey, PlanDigest: record.PlanDigest}
		delete(checkpoint, "action_response")
		encoded, err := json.Marshal(checkpoint)
		if err != nil {
			return err
		}
		result := fenceRemoteUploadClaim(tx.Model(&models.Job{}).Where("id=? AND checkpoint_json=?", task.JobID, job.CheckpointJSON), ctx).Update("checkpoint_json", string(encoded))
		if result.Error != nil || result.RowsAffected != 1 {
			return firstNonNil(result.Error, ErrCatalogFence)
		}
		return assertRemoteUploadStopProofTx(tx, proof.physical)
	})
	if err != nil {
		return nil, err
	}
	request := nodeprotocol.StorageActionRequest{RequestID: record.OperationKey, OperationKey: record.OperationKey, PlanDigest: record.PlanDigest}
	response, err := client.StorageActionResult(stopCtx, record.OperationKey, download.ID, record.OperationKey)
	if err != nil {
		return nil, remoteUploadClientError(err)
	}
	if err := validateStorageActionResponse(response, request, len(batch.rows)); err != nil {
		return nil, err
	}
	// A final positive can legitimately predate an issued-but-lost renewal.
	// Read it without sending that higher epoch back as a new control intent.
	if response.Status != nodeprotocol.StorageActionCompleted {
		control := client.(remoteUploadControlClient) // stopRemoteUploadExecution already checked it
		sealed, err := control.StopOperation(stopCtx, record.OperationKey, nodeprotocol.OperationStopRequest{PlanDigest: record.PlanDigest, LeaseEpoch: record.LeaseEpoch, SealUploadRetry: true})
		if err != nil {
			return nil, remoteUploadClientError(err)
		}
		if sealed.OperationKey != record.OperationKey || sealed.PlanDigest != record.PlanDigest || sealed.LeaseEpoch != record.LeaseEpoch {
			return nil, ErrCatalogFence
		}
		for {
			response, err = client.StorageActionResult(stopCtx, record.OperationKey, download.ID, record.OperationKey)
			if err != nil {
				return nil, remoteUploadClientError(err)
			}
			if err := validateStorageActionResponse(response, request, len(batch.rows)); err != nil {
				return nil, err
			}
			if response.Status == nodeprotocol.StorageActionCompleted || response.ExecutionActive != nil && !*response.ExecutionActive {
				break
			}
			if response.ExecutionActive == nil {
				return nil, cloudTransferError("node_execution_exit_unknown", true, nil)
			}
			select {
			case <-stopCtx.Done():
				return nil, cloudTransferError("node_execution_exit_unknown", true, stopCtx.Err())
			case <-time.After(200 * time.Millisecond):
			}
		}
	}
	if response.Status == nodeprotocol.StorageActionCompleted {
		delete(checkpoint, "upload_retry")
		raw, _ := json.Marshal(checkpoint)
		result := fenceRemoteUploadClaim(w.service.db.WithContext(ctx).Model(&models.Job{}).Where("id=?", task.JobID), ctx).Update("checkpoint_json", string(raw))
		if result.Error != nil || result.RowsAffected != 1 {
			return nil, firstNonNil(result.Error, ErrCatalogFence)
		}
		return nil, nil
	} // next same-key read publishes the late positive
	if response.ExecutionActive == nil || *response.ExecutionActive {
		return nil, cloudTransferError("node_execution_exit_unknown", true, errors.New("node_execution_exit_unknown"))
	}
	return &WaitForAction{ActionType: "upload_retry", Prompt: "原上传已停止，但旧结果仍未能确认。重新尝试会保留旧记录，先核对同一目标，再按原冲突策略上传。", Options: []string{"retry_upload"}, Preview: map[string]string{"媒体库": task.LibraryName}, Checkpoint: checkpoint}, nil
}
