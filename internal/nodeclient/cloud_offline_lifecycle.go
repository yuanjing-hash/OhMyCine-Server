package nodeclient

import (
	"context"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
	"strings"
	"time"
)

func (c *StorageSourceClient) OfflineOperation(source downloader.Source) downloader.OfflineOperation {
	if c.sourceKind != nodeprotocol.StorageSourceKindPan115OfflineMagnet || !c.matchesSource(source) {
		return downloader.OfflineOperation{}
	}
	return downloader.OfflineOperation{Kind: downloader.OfflineOperationCloud, AccountNamespace: c.basePlan.OfflineAccountNamespace, KeepOutputStop: c.basePlan.CloudOfflineLifecycleVersion == 1, RequiredFileCompletion: c.basePlan.CloudOfflineLifecycleVersion == 1}
}
func (c *StorageSourceClient) StopOfflineKeepOutput(ctx context.Context, id string) error {
	return c.cloudOfflineControl(ctx, id, true)
}
func (c *StorageSourceClient) cloudOfflineControl(ctx context.Context, id string, keepMaterialization bool) error {
	operation, err := c.ensureOperation(ctx)
	if err != nil {
		return err
	}
	if strings.TrimSpace(id) != operation.Plan.OperationKey {
		return downloader.Error(nodeprotocol.ErrorPlanConflict, false, nil)
	}
	if c.basePlan.CloudOfflineLifecycleVersion != 1 || c.sourceKind != nodeprotocol.StorageSourceKindPan115OfflineMagnet {
		return downloader.Error(nodeprotocol.ErrorCapabilityMissing, false, nil)
	}
	operation, err = c.controlOperationLease(ctx, operation)
	if err != nil {
		return err
	}
	health, err := c.node.Health(ctx)
	if err != nil {
		return remoteDownloaderError(err, nodeprotocol.ErrorNodeOffline)
	}
	if !health.Capabilities.Has(nodeprotocol.CapabilityCloudOfflineLifecycleV1) {
		return downloader.Error(nodeprotocol.ErrorCapabilityMissing, false, nil)
	}
	actions := []string{nodeprotocol.StorageSourceGrantRead, nodeprotocol.StorageSourceGrantOfflineStatus, nodeprotocol.StorageSourceGrantOfflineStop}
	envelope, err := c.grant(ctx, operation.Plan.OperationKey, actions, c.sourceURI)
	if err != nil {
		return err
	}
	if err := c.node.PutCredentialGrant(ctx, envelope); err != nil {
		return remoteDownloaderError(err, nodeprotocol.ErrorNodeOffline)
	}
	input := nodeprotocol.StorageSourceActionRequest{RequestID: operation.Plan.OperationKey + ":offline:stop", TaskID: c.taskID, OperationKey: operation.Plan.OperationKey, PlanDigest: operation.PlanDigest, StorageID: c.storageID, Action: nodeprotocol.StorageSourceActionOfflineStop, GrantID: envelope.GrantID, KeepMaterialization: keepMaterialization}
	response, err := c.node.StorageSourceAction(ctx, input)
	if err != nil {
		return remoteDownloaderError(err, "downloader_control_failed")
	}
	if response.RequestID != input.RequestID || response.OperationKey != input.OperationKey || response.PlanDigest != input.PlanDigest || response.Offline == nil || response.Offline.Version != 1 || response.Offline.Kind != downloader.OfflineOperationCloud || response.Offline.AccountNamespace != c.basePlan.OfflineAccountNamespace {
		return downloader.Error(nodeprotocol.ErrorPlanConflict, false, nil)
	}
	remote, err := c.node.Operation(ctx, operation.Plan.OperationKey)
	if err != nil {
		return remoteDownloaderError(err, nodeprotocol.ErrorNodeOffline)
	}
	if err := c.observe(ctx, remote, &response); err != nil {
		return err
	}
	if !response.Offline.UpstreamStopped || response.Offline.StopPending {
		return downloader.Error(nodeprotocol.ErrorReconciliationNeeded, true, nil)
	}
	return nil
}

var _ downloader.OfflineLifecycleClient = (*StorageSourceClient)(nil)
var _ downloader.OfflineStopper = (*StorageSourceClient)(nil)

func (c *StorageSourceClient) refreshCloudOfflineStatus(ctx context.Context, operation StorageSourceOperation) (nodeprotocol.StorageSourceActionResponse, error) {
	actions := []string{nodeprotocol.StorageSourceGrantRead, nodeprotocol.StorageSourceGrantOfflineStatus}
	envelope, err := c.grant(ctx, operation.Plan.OperationKey, actions, c.sourceURI)
	if err != nil {
		return nodeprotocol.StorageSourceActionResponse{}, err
	}
	if err := c.node.PutCredentialGrant(ctx, envelope); err != nil {
		return nodeprotocol.StorageSourceActionResponse{}, remoteDownloaderError(err, nodeprotocol.ErrorNodeOffline)
	}
	input := nodeprotocol.StorageSourceActionRequest{RequestID: operation.Plan.OperationKey + ":offline:status", TaskID: c.taskID, OperationKey: operation.Plan.OperationKey, PlanDigest: operation.PlanDigest, StorageID: c.storageID, Action: nodeprotocol.StorageSourceActionOfflineStatus, GrantID: envelope.GrantID}
	response, err := c.node.StorageSourceAction(ctx, input)
	if err != nil {
		return response, remoteDownloaderError(err, nodeprotocol.ErrorNodeOffline)
	}
	if response.RequestID != input.RequestID || response.OperationKey != input.OperationKey || response.PlanDigest != input.PlanDigest {
		return response, downloader.Error(nodeprotocol.ErrorPlanConflict, false, nil)
	}
	// Return the main materialization receipt identity to the ordinary observer.
	response.RequestID = operation.Plan.OperationKey
	return response, nil
}

func (c *StorageSourceClient) ObserveOffline(ctx context.Context, id string) (downloader.Task, error) {
	operation, err := c.ensureOperation(ctx)
	if err != nil {
		return downloader.Task{}, err
	}
	if id != operation.Plan.OperationKey || c.basePlan.CloudOfflineLifecycleVersion != 1 {
		return downloader.Task{}, downloader.Error(nodeprotocol.ErrorCapabilityMissing, false, nil)
	}
	operation, err = c.controlOperationLease(ctx, operation)
	if err != nil {
		return downloader.Task{}, err
	}
	response, err := c.refreshCloudOfflineStatus(ctx, operation)
	if err != nil {
		return downloader.Task{}, err
	}
	if err := c.validateResponse(operation, response); err != nil {
		return downloader.Task{}, err
	}
	task := c.storageSourceTask(response)
	if response.Offline != nil && response.Offline.TerminalFailure {
		return task, downloader.WithOfflineTerminalFailure(downloader.Error("node_source_offline_failed", false, nil))
	}
	if response.Offline != nil && response.Offline.DefiniteNonAcceptance {
		return task, downloader.WithDefiniteSubmissionNonAcceptance(downloader.Error("downloader_submission_not_accepted", false, nil))
	}
	return task, nil
}

// A control can renew the original operation lease without PutOperation,
// Submit or materialization. Export completion may precede native stop settlement.
func (c *StorageSourceClient) controlOperationLease(ctx context.Context, operation StorageSourceOperation) (StorageSourceOperation, error) {
	remote, err := c.node.Operation(ctx, operation.Plan.OperationKey)
	if err != nil {
		return operation, remoteDownloaderError(err, nodeprotocol.ErrorNodeOffline)
	}
	if remote.OperationKey != operation.Plan.OperationKey || remote.PlanDigest != operation.PlanDigest {
		return operation, downloader.Error(nodeprotocol.ErrorPlanConflict, false, nil)
	}
	if remote.Status == nodeprotocol.OperationCompleted || remote.Status == nodeprotocol.OperationCancelled || remote.Status == nodeprotocol.OperationFailed {
		return operation, nil
	}
	if remote.LeaseEpoch < operation.Plan.LeaseEpoch || remote.LeaseExpiresAt.Before(operation.Plan.LeaseExpiresAt) || !remote.LeaseExpiresAt.After(time.Now().UTC().Add(2*time.Minute)) {
		next := operation
		if next.Plan.LeaseEpoch <= remote.LeaseEpoch {
			next.Plan.LeaseEpoch = remote.LeaseEpoch + 1
		}
		next.Plan.LeaseExpiresAt = time.Now().UTC().Add(5 * time.Minute)
		renewed, err := c.node.RenewOperationLease(ctx, next.Plan.OperationKey, nodeprotocol.LeaseRequest{PlanDigest: next.PlanDigest, LeaseEpoch: next.Plan.LeaseEpoch, LeaseExpiresAt: next.Plan.LeaseExpiresAt})
		if err != nil {
			return operation, remoteDownloaderError(err, nodeprotocol.ErrorNodeOffline)
		}
		if renewed.OperationKey != next.Plan.OperationKey || renewed.PlanDigest != next.PlanDigest || renewed.LeaseEpoch != next.Plan.LeaseEpoch || renewed.LeaseExpiresAt.Before(next.Plan.LeaseExpiresAt) {
			return operation, downloader.Error(nodeprotocol.ErrorPlanConflict, false, nil)
		}
		if err := c.observe(ctx, renewed, nil); err != nil {
			return operation, err
		}
		c.mu.Lock()
		c.operation = &next
		c.mu.Unlock()
		return next, nil
	}
	return operation, nil
}

func (c *StorageSourceClient) ReconcileSubmission(ctx context.Context, request downloader.SubmitRequest) (downloader.Task, error) {
	if !c.matchesSource(request.Source) {
		return downloader.Task{}, downloader.Error("downloader_source_unsupported", false, nil)
	}
	operation, err := c.ensureOperation(ctx)
	if err != nil {
		return downloader.Task{}, err
	}
	if c.basePlan.CloudOfflineLifecycleVersion == 1 {
		return c.ObserveOffline(ctx, operation.Plan.OperationKey)
	}
	// Legacy accepted identity is read only. No Put, credential refresh, action or
	// materialization is issued merely to recover a lost initial acknowledgement.
	remote, err := c.node.Operation(ctx, operation.Plan.OperationKey)
	if err != nil {
		return downloader.Task{}, remoteDownloaderError(err, nodeprotocol.ErrorReconciliationNeeded)
	}
	if remote.PlanDigest != operation.PlanDigest {
		return downloader.Task{}, downloader.Error(nodeprotocol.ErrorPlanConflict, false, nil)
	}
	response, err := c.node.StorageSourceActionResult(ctx, operation.Plan.OperationKey, c.taskID, operation.Plan.OperationKey)
	if err != nil {
		return downloader.Task{}, remoteDownloaderError(err, nodeprotocol.ErrorReconciliationNeeded)
	}
	if err := c.validateResponse(operation, response); err != nil {
		return downloader.Task{}, err
	}
	if err := storageSourceResponseError(response); err != nil {
		return downloader.Task{}, err
	}
	return c.storageSourceTask(response), nil
}

var _ downloader.SubmissionReconciler = (*StorageSourceClient)(nil)
