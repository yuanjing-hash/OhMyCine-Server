package nodeagent

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
)

type storageUploadProgressKey struct{}

func storageUploadProgressCallback(ctx context.Context) func(cloud.UploadProgress) {
	callback, _ := ctx.Value(storageUploadProgressKey{}).(func(cloud.UploadProgress))
	return callback
}

func reportStorageUploadPhase(ctx context.Context, phase string) {
	if callback := storageUploadProgressCallback(ctx); callback != nil {
		callback(cloud.UploadProgress{Phase: phase})
	}
}

func (a *Agent) storageUploadObserver(ctx context.Context, server string, input nodeprotocol.StorageActionRequest, plan nodeprotocol.StorageUploadPlan, response *nodeprotocol.StorageActionResponse) func(cloud.UploadProgress) {
	var mu sync.Mutex
	var total, delivered, lastNetwork int64
	for _, file := range plan.Files {
		total += file.Size
	}
	for _, file := range response.Files {
		if file.Status == nodeprotocol.StorageFileCompleted || file.Status == nodeprotocol.StorageFileReused {
			delivered += file.Size
		}
	}
	var lastWrite, lastSample time.Time
	return func(sample cloud.UploadProgress) {
		mu.Lock()
		defer mu.Unlock()
		now := a.now()
		phaseChanged := sample.Phase != "" && response.Phase != sample.Phase
		if sample.Phase != "" {
			response.Phase = sample.Phase
		}
		if phaseChanged {
			log.Info().Str("operation", "node_storage_upload").Str("task_id", input.TaskID).Str("phase", response.Phase).Msg("节点上传进入新阶段")
		}
		if sample.TotalBytes > 0 && sample.TransferredBytes >= 0 && sample.TransferredBytes <= sample.TotalBytes && sample.NetworkBytes >= lastNetwork && sample.SampledAt.After(lastSample) && !sample.SampledAt.After(now.Add(time.Second)) {
			progress := &nodeprotocol.StorageUploadProgress{Version: 1, TransferredBytes: delivered + sample.TransferredBytes, TotalBytes: total, SampledAt: sample.SampledAt}
			if !lastSample.IsZero() && sample.NetworkBytes > lastNetwork {
				speed := int64(float64(sample.NetworkBytes-lastNetwork) / sample.SampledAt.Sub(lastSample).Seconds())
				progress.UploadSpeedBPS = &speed
			}
			lastNetwork, lastSample = sample.NetworkBytes, sample.SampledAt
			response.UploadProgress = progress
		}
		if response.Phase != nodeprotocol.PhaseUploadingTarget && response.UploadProgress != nil {
			response.UploadProgress.UploadSpeedBPS = nil
		}
		if !phaseChanged && !lastWrite.IsZero() && now.Sub(lastWrite) < time.Second {
			return
		}
		lastWrite = now
		_ = a.store.UpdateActionReceipt(ctx, server, input.RequestID, *response, now)
		_ = a.store.UpdateOperationExecution(ctx, server, input.OperationKey, input.PlanDigest, nodeprotocol.OperationRunning, response.Phase, "", nil, now)
	}
}

// Renewals extend authority only for the same sealed account and target. The
// running driver retains its credential, while its liveness deadline advances.
func (a *Agent) storageUploadGuard(server string, input nodeprotocol.StorageActionRequest, initial nodeprotocol.CredentialGrant, credential nodeprotocol.StorageCredential) func(context.Context) error {
	var mu sync.Mutex
	grant := initial
	return func(ctx context.Context) error {
		mu.Lock()
		defer mu.Unlock()
		if err := ctx.Err(); err != nil {
			return err
		}
		now := a.now()
		if _, err := a.store.ActiveOperationPlan(ctx, server, input.OperationKey, input.PlanDigest, now); err != nil {
			return err
		}
		if grant.ExpiresAt.Before(now.Add(2 * time.Minute)) {
			var raw string
			err := a.store.db.QueryRowContext(ctx, `SELECT envelope_json FROM credential_grants WHERE paired_server_id=? AND operation_key=? AND task_id=? AND resource_kind=? AND resource_id=? AND expires_at>? ORDER BY expires_at DESC LIMIT 1`, server, input.OperationKey, input.TaskID, grant.ResourceKind, grant.ResourceID, grant.ExpiresAt).Scan(&raw)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if err == nil {
				var envelope nodeprotocol.CredentialGrantEnvelope
				if json.Unmarshal([]byte(raw), &envelope) != nil {
					return ErrPlanConflict
				}
				next, err := nodeprotocol.OpenCredentialGrant(a.sealingPrivateKey, envelope, now)
				if err != nil {
					return err
				}
				if next.NodeID != grant.NodeID || next.TaskID != grant.TaskID || next.OperationKey != grant.OperationKey || next.ResourceKind != grant.ResourceKind || next.ResourceID != grant.ResourceID || next.CredentialRevision < grant.CredentialRevision || !bytes.Equal(next.Credential, grant.Credential) || !sameStrings(next.AllowedActions, grant.AllowedActions) {
					return ErrPlanConflict
				}
				grant = next
			}
		}
		if !grant.ExpiresAt.After(now) {
			return errors.New(nodeprotocol.ErrorCredentialExpired)
		}
		return nil
	}
}

func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func (a *Agent) storageExecutionResponse(response nodeprotocol.StorageActionResponse) nodeprotocol.StorageActionResponse {
	a.storageMu.Lock()
	_, active := a.storageRuns[response.OperationKey]
	a.storageMu.Unlock()
	if response.Status == nodeprotocol.StorageActionCompleted {
		active = false
	}
	response.ExecutionActive = &active
	return response
}

func (a *Agent) finishStorageExecution(server string, input nodeprotocol.StorageActionRequest) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	receipt, err := a.store.ProviderReceipt(ctx, server, input.RequestID)
	if err != nil || receipt.Status == "completed" {
		return
	}
	var response nodeprotocol.StorageActionResponse
	if json.Unmarshal([]byte(receipt.ResponseJSON), &response) != nil || response.PlanDigest != input.PlanDigest {
		return
	}
	active := false
	response.ExecutionActive = &active
	if response.UploadProgress != nil {
		response.UploadProgress.UploadSpeedBPS = nil
	}
	if response.ErrorCode == "" {
		response.ErrorCode = nodeprotocol.ErrorReconciliationNeeded
		response.Status = nodeprotocol.StorageActionReconciliation
		response.Phase = nodeprotocol.PhaseVerifyingTarget
	}
	if err := a.store.UpdateActionReceipt(ctx, server, input.RequestID, response, a.now()); err != nil {
		log.Warn().Str("operation", "node_storage_upload").Str("task_id", input.TaskID).Str("error_code", "node_persistence_failed").Msg("节点执行已退出，退出回执暂未保存")
	}
}
