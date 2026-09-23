package nodeclient

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
)

type StorageSourceOperation struct {
	Plan       nodeprotocol.OperationPlan
	PlanDigest string
	Completed  bool
}

type StorageSourceOperationFactory func(context.Context, nodeprotocol.StorageSourcePlan) (StorageSourceOperation, error)
type StorageSourceGrantFactory func(context.Context, string, []string, string) (nodeprotocol.CredentialGrantEnvelope, error)
type StorageSourceObserver func(context.Context, nodeprotocol.OperationResponse, *nodeprotocol.StorageSourceActionResponse) error

// StorageSourceClient adapts one frozen Node-side cloud materialization to the
// ordinary downloader contract. It never decides names, target paths or
// conflicts and never persists provider credentials.
type StorageSourceClient struct {
	node       *Client
	taskID     string
	storageID  string
	sourceKind string
	sourceURI  string
	basePlan   nodeprotocol.StorageSourcePlan
	prepare    StorageSourceOperationFactory
	grant      StorageSourceGrantFactory
	observe    StorageSourceObserver

	mu        sync.Mutex
	operation *StorageSourceOperation
}

func NewStorageSourceClient(node *Client, taskID, storageID, sourceKind, sourceURI string, basePlan nodeprotocol.StorageSourcePlan, prepare StorageSourceOperationFactory, grant StorageSourceGrantFactory, observe StorageSourceObserver) (*StorageSourceClient, error) {
	if node == nil || strings.TrimSpace(taskID) == "" || strings.TrimSpace(storageID) == "" || !nodeprotocol.StorageSourceKindSupported(sourceKind) || strings.TrimSpace(sourceURI) == "" || prepare == nil || grant == nil || observe == nil {
		return nil, errors.New("node_storage_source_config_invalid")
	}
	if basePlan.StorageID != storageID || basePlan.SourceKind != sourceKind || basePlan.Validate() != nil {
		return nil, errors.New("node_storage_source_plan_invalid")
	}
	return &StorageSourceClient{node: node, taskID: taskID, storageID: storageID, sourceKind: sourceKind, sourceURI: sourceURI, basePlan: basePlan, prepare: prepare, grant: grant, observe: observe}, nil
}

func (c *StorageSourceClient) Test(ctx context.Context) (downloadpkg.Health, error) {
	health, err := c.node.Health(ctx)
	if err != nil {
		return downloadpkg.Health{}, remoteDownloaderError(err, nodeprotocol.ErrorNodeOffline)
	}
	if !health.Capabilities.Has(nodeprotocol.CapabilityPan115Offline) || !health.Capabilities.Has(nodeprotocol.CapabilityPan115Read) || !health.Capabilities.Has(nodeprotocol.CapabilityRangeExport) || nodeprotocol.StorageSourceIsShare(c.sourceKind) && !health.Capabilities.Has(nodeprotocol.CapabilityPan115ShareReceive) {
		return downloadpkg.Health{}, downloadpkg.Error(nodeprotocol.ErrorCapabilityMissing, false, nil)
	}
	return downloadpkg.Health{Version: health.AgentVersion}, nil
}

func (c *StorageSourceClient) Submit(ctx context.Context, request downloadpkg.SubmitRequest) (downloadpkg.Task, error) {
	if request.MetadataOnly || !c.matchesSource(request.Source) {
		return downloadpkg.Task{}, downloadpkg.Error("downloader_source_unsupported", false, nil)
	}
	response, err := c.execute(ctx, true)
	if err != nil {
		return downloadpkg.Task{}, err
	}
	return c.storageSourceTask(response), nil
}

func (c *StorageSourceClient) Get(ctx context.Context, id string) (downloadpkg.Task, error) {
	operation, err := c.ensureOperation(ctx)
	if err != nil {
		return downloadpkg.Task{}, err
	}
	if strings.TrimSpace(id) != operation.Plan.OperationKey {
		return downloadpkg.Task{}, downloadpkg.Error(nodeprotocol.ErrorPlanConflict, false, nil)
	}
	response, err := c.execute(ctx, false)
	if err != nil {
		return downloadpkg.Task{}, err
	}
	return c.storageSourceTask(response), nil
}

func (c *StorageSourceClient) Cancel(ctx context.Context, id string, _ bool) error {
	operation, err := c.ensureOperation(ctx)
	if err != nil {
		return err
	}
	if strings.TrimSpace(id) != operation.Plan.OperationKey {
		return downloadpkg.Error(nodeprotocol.ErrorPlanConflict, false, nil)
	}
	remote, err := c.node.CancelOperation(ctx, operation.Plan.OperationKey)
	if err != nil {
		return remoteDownloaderError(err, "downloader_control_failed")
	}
	if err := c.observe(ctx, remote, nil); err != nil {
		return err
	}
	return c.cleanupManagedSource(ctx, operation)
}

func (c *StorageSourceClient) CleanupManagedSource(ctx context.Context) error {
	operation, err := c.ensureOperation(ctx)
	if err != nil {
		return err
	}
	return c.cleanupManagedSource(ctx, operation)
}

func (c *StorageSourceClient) cleanupManagedSource(ctx context.Context, operation StorageSourceOperation) error {
	request := nodeprotocol.StorageSourceCleanupRequest{RequestID: "source:cleanup:" + uuid.NewString(), OperationKey: operation.Plan.OperationKey, TaskID: c.taskID, PlanDigest: operation.PlanDigest}
	cleaned, err := c.node.CleanupStorageSource(ctx, request)
	if err != nil {
		return remoteDownloaderError(err, "downloader_control_failed")
	}
	if cleaned.RequestID != request.RequestID || cleaned.OperationKey != request.OperationKey || cleaned.TaskID != request.TaskID || cleaned.Status != "completed" || !cleaned.Cleaned {
		return downloadpkg.Error(nodeprotocol.ErrorReconciliationNeeded, true, nil)
	}
	return nil
}

func (c *StorageSourceClient) Manifest(ctx context.Context, id string) (downloadpkg.Manifest, error) {
	operation, err := c.ensureOperation(ctx)
	if err != nil {
		return downloadpkg.Manifest{}, err
	}
	if strings.TrimSpace(id) != operation.Plan.OperationKey {
		return downloadpkg.Manifest{}, downloadpkg.Error(nodeprotocol.ErrorPlanConflict, false, nil)
	}
	response, err := c.execute(ctx, false)
	if err != nil {
		return downloadpkg.Manifest{}, err
	}
	if response.Status != nodeprotocol.StorageSourceCompleted || response.FileExport == nil {
		return downloadpkg.Manifest{}, downloadpkg.Error("downloader_manifest_unavailable", true, nil)
	}
	return readFileExportManifest(ctx, c.node, *response.FileExport, operation.Plan.OperationKey, c.taskID, "storage:source:")
}

func (c *StorageSourceClient) RemoteFileChunks(ctx context.Context, manifest downloadpkg.Manifest, file downloadpkg.File) ([]downloadpkg.RemoteFileChunk, error) {
	return readRemoteFileChunks(ctx, c.node, c.taskID, manifest, file)
}

func (c *StorageSourceClient) ReadRemoteFileChunk(ctx context.Context, manifest downloadpkg.Manifest, file downloadpkg.File, chunk downloadpkg.RemoteFileChunk) ([]byte, error) {
	return readRemoteFileChunk(ctx, c.node, c.taskID, manifest, file, chunk)
}

func (c *StorageSourceClient) execute(ctx context.Context, forceAction bool) (nodeprotocol.StorageSourceActionResponse, error) {
	operation, err := c.ensureOperation(ctx)
	if err != nil {
		return nodeprotocol.StorageSourceActionResponse{}, err
	}
	if !operation.Completed && operation.Plan.LeaseExpiresAt.Before(time.Now().UTC().Add(2*time.Minute)) {
		operation, err = c.renewOperation(ctx, operation)
		if err != nil {
			return nodeprotocol.StorageSourceActionResponse{}, err
		}
	}
	remote, err := c.node.PutOperation(ctx, nodeprotocol.PutOperationRequest{Plan: operation.Plan, PlanDigest: operation.PlanDigest})
	if err != nil {
		return nodeprotocol.StorageSourceActionResponse{}, remoteDownloaderError(err, nodeprotocol.ErrorNodeOffline)
	}
	if remote.OperationKey != operation.Plan.OperationKey || remote.PlanDigest != operation.PlanDigest {
		return nodeprotocol.StorageSourceActionResponse{}, downloadpkg.Error(nodeprotocol.ErrorPlanConflict, false, nil)
	}
	if remote.Status != nodeprotocol.OperationCompleted && (remote.LeaseEpoch < operation.Plan.LeaseEpoch || remote.LeaseExpiresAt.Before(operation.Plan.LeaseExpiresAt)) {
		remote, err = c.node.RenewOperationLease(ctx, operation.Plan.OperationKey, nodeprotocol.LeaseRequest{PlanDigest: operation.PlanDigest, LeaseEpoch: operation.Plan.LeaseEpoch, LeaseExpiresAt: operation.Plan.LeaseExpiresAt})
		if err != nil {
			return nodeprotocol.StorageSourceActionResponse{}, remoteDownloaderError(err, nodeprotocol.ErrorNodeOffline)
		}
	}
	if err := c.observe(ctx, remote, nil); err != nil {
		return nodeprotocol.StorageSourceActionResponse{}, err
	}
	if remote.Status == nodeprotocol.OperationFailed || remote.Status == nodeprotocol.OperationCancelled {
		return nodeprotocol.StorageSourceActionResponse{}, downloadpkg.Error(firstStorageSourceError(remote.ErrorCode), false, nil)
	}
	requestID := operation.Plan.OperationKey
	response, resultErr := c.node.StorageSourceActionResult(ctx, requestID, c.taskID, operation.Plan.OperationKey)
	staleExport := response.Status == nodeprotocol.StorageSourceCompleted && (response.FileExport == nil || !response.FileExport.ExpiresAt.After(time.Now().UTC().Add(2*time.Minute)))
	if resultErr == nil && !forceAction && response.Status != nodeprotocol.StorageSourceWaitingCredentials && !staleExport && strings.TrimSpace(response.ErrorCode) == "" {
		if err := c.validateResponse(operation, response); err != nil {
			return nodeprotocol.StorageSourceActionResponse{}, err
		}
		_ = c.observe(ctx, remote, &response)
		c.rememberCompletion(response)
		if err := storageSourceResponseError(response); err != nil {
			return nodeprotocol.StorageSourceActionResponse{}, err
		}
		return response, nil
	}
	actions := []string{nodeprotocol.StorageSourceGrantRead}
	if c.sourceKind == nodeprotocol.StorageSourceKindPan115OfflineMagnet {
		actions = append(actions, nodeprotocol.StorageSourceGrantOfflineSubmit, nodeprotocol.StorageSourceGrantOfflineStatus)
	} else {
		actions = append(actions, nodeprotocol.StorageSourceGrantShareInspect, nodeprotocol.StorageSourceGrantShareReceive)
	}
	envelope, err := c.grant(ctx, operation.Plan.OperationKey, actions, c.sourceURI)
	if err != nil {
		return nodeprotocol.StorageSourceActionResponse{}, err
	}
	if err := c.node.PutCredentialGrant(ctx, envelope); err != nil {
		return nodeprotocol.StorageSourceActionResponse{}, remoteDownloaderError(err, nodeprotocol.ErrorNodeOffline)
	}
	request := nodeprotocol.StorageSourceActionRequest{RequestID: requestID, TaskID: c.taskID, OperationKey: operation.Plan.OperationKey, PlanDigest: operation.PlanDigest, StorageID: c.storageID, Action: nodeprotocol.StorageSourceActionMaterialize, GrantID: envelope.GrantID}
	response, err = c.node.StorageSourceAction(ctx, request)
	if err != nil {
		return nodeprotocol.StorageSourceActionResponse{}, remoteDownloaderError(err, nodeprotocol.ErrorNodeOffline)
	}
	if err := c.validateResponse(operation, response); err != nil {
		return nodeprotocol.StorageSourceActionResponse{}, err
	}
	if err := c.observe(ctx, remote, &response); err != nil {
		return nodeprotocol.StorageSourceActionResponse{}, err
	}
	c.rememberCompletion(response)
	if err := storageSourceResponseError(response); err != nil {
		return nodeprotocol.StorageSourceActionResponse{}, err
	}
	return response, nil
}

func (c *StorageSourceClient) rememberCompletion(response nodeprotocol.StorageSourceActionResponse) {
	if response.Status != nodeprotocol.StorageSourceCompleted {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.operation != nil {
		c.operation.Completed = true
	}
}

func (c *StorageSourceClient) renewOperation(ctx context.Context, operation StorageSourceOperation) (StorageSourceOperation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.operation != nil && c.operation.Plan.LeaseExpiresAt.After(time.Now().UTC().Add(2*time.Minute)) {
		return *c.operation, nil
	}
	next := operation
	next.Plan.LeaseEpoch++
	next.Plan.LeaseExpiresAt = time.Now().UTC().Add(5 * time.Minute)
	remote, err := c.node.RenewOperationLease(ctx, next.Plan.OperationKey, nodeprotocol.LeaseRequest{PlanDigest: next.PlanDigest, LeaseEpoch: next.Plan.LeaseEpoch, LeaseExpiresAt: next.Plan.LeaseExpiresAt})
	if err != nil {
		return StorageSourceOperation{}, remoteDownloaderError(err, nodeprotocol.ErrorNodeOffline)
	}
	if remote.OperationKey != next.Plan.OperationKey || remote.PlanDigest != next.PlanDigest || remote.LeaseEpoch != next.Plan.LeaseEpoch || remote.LeaseExpiresAt.Before(next.Plan.LeaseExpiresAt) {
		return StorageSourceOperation{}, downloadpkg.Error(nodeprotocol.ErrorPlanConflict, false, nil)
	}
	if err := c.observe(ctx, remote, nil); err != nil {
		return StorageSourceOperation{}, err
	}
	c.operation = &next
	return next, nil
}

func (c *StorageSourceClient) ensureOperation(ctx context.Context) (StorageSourceOperation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.operation != nil {
		return *c.operation, nil
	}
	operation, err := c.prepare(ctx, c.basePlan)
	if err != nil {
		return StorageSourceOperation{}, err
	}
	if operation.Plan.OperationKey == "" || operation.Plan.TaskID != c.taskID || operation.Plan.Kind != nodeprotocol.OperationKindStorageSourceMaterialize || !nodeprotocol.ValidDigest(operation.PlanDigest) {
		return StorageSourceOperation{}, downloadpkg.Error(nodeprotocol.ErrorPlanConflict, false, nil)
	}
	c.operation = &operation
	return operation, nil
}

func (c *StorageSourceClient) validateResponse(operation StorageSourceOperation, response nodeprotocol.StorageSourceActionResponse) error {
	if response.RequestID != operation.Plan.OperationKey || response.OperationKey != operation.Plan.OperationKey || response.PlanDigest != operation.PlanDigest || response.TotalFiles < 0 || response.DownloadedFiles < 0 || response.DownloadedFiles > response.TotalFiles || response.TotalBytes < 0 || response.DownloadedBytes < 0 || response.DownloadedBytes > response.TotalBytes {
		return downloadpkg.Error(nodeprotocol.ErrorPlanConflict, false, nil)
	}
	switch response.Status {
	case nodeprotocol.StorageSourceAccepted, nodeprotocol.StorageSourceSubmitting, nodeprotocol.StorageSourceWaiting, nodeprotocol.StorageSourceEnumerating, nodeprotocol.StorageSourceDownloading, nodeprotocol.StorageSourceWaitingCredentials, nodeprotocol.StorageSourceCompleted, nodeprotocol.StorageSourceFailed:
		return nil
	default:
		return downloadpkg.Error(nodeprotocol.ErrorPlanConflict, false, nil)
	}
}

func (c *StorageSourceClient) matchesSource(source downloadpkg.Source) bool {
	if c.sourceKind == nodeprotocol.StorageSourceKindPan115OfflineMagnet {
		return source.Kind == downloadpkg.SourceURL && strings.TrimSpace(source.URL) == c.sourceURI
	}
	if c.sourceKind == nodeprotocol.StorageSourceKindPan115ShareSelected {
		if source.Kind != downloadpkg.SourcePan115Share || source.ShareSelection == nil {
			return false
		}
		raw, err := cloud.EncodeSelectedShareSource(source.URL, *source.ShareSelection)
		if err != nil {
			return false
		}
		a, err := nodeprotocol.StorageSourceContentDigest(c.sourceKind, raw)
		b, otherErr := nodeprotocol.StorageSourceContentDigest(c.sourceKind, c.sourceURI)
		return err == nil && otherErr == nil && a == b
	}
	return source.ShareSelection == nil && source.Kind == downloadpkg.SourcePan115Share && strings.TrimSpace(source.URL) == c.sourceURI
}

func (c *StorageSourceClient) storageSourceTask(response nodeprotocol.StorageSourceActionResponse) downloadpkg.Task {
	progress := response.Progress
	if progress == nil && response.TotalBytes > 0 {
		value := float64(response.DownloadedBytes) / float64(response.TotalBytes)
		progress = &value
	}
	task := downloadpkg.Task{ID: response.OperationKey, Name: response.OperationKey, Status: response.ProviderStatus, Progress: progress, BytesCompleted: &response.DownloadedBytes, BytesTotal: &response.TotalBytes, ErrorCode: response.ErrorCode, OutputItemID: c.basePlan.OfflineDestinationID}
	if response.Status == nodeprotocol.StorageSourceCompleted {
		task.Completed = true
		task.Status = "completed"
	}
	if response.Status == nodeprotocol.StorageSourceFailed {
		task.Failed = true
		task.Status = "failed"
	}
	return task
}

func firstStorageSourceError(code string) string {
	if strings.TrimSpace(code) != "" {
		return code
	}
	return nodeprotocol.ErrorPlanConflict
}

func storageSourceResponseError(response nodeprotocol.StorageSourceActionResponse) error {
	code := strings.TrimSpace(response.ErrorCode)
	if code == "" || code == nodeprotocol.ErrorCredentialExpired && response.Status == nodeprotocol.StorageSourceWaitingCredentials {
		return nil
	}
	switch code {
	case nodeprotocol.ErrorSourceRateLimited, nodeprotocol.ErrorLeaseExpired, "node_source_unavailable":
		return downloadpkg.Error(code, true, nil)
	case nodeprotocol.ErrorPlanConflict, nodeprotocol.ErrorChecksumMismatch, nodeprotocol.ErrorCapabilityMissing, nodeprotocol.ErrorReconciliationNeeded, "node_source_offline_invalid", "node_source_offline_quota_exhausted", "node_source_offline_failed":
		return downloadpkg.Error(code, false, nil)
	default:
		return downloadpkg.Error(code, true, nil)
	}
}

func StorageSourceOperationKey(taskID string) string {
	digest := sha256.Sum256([]byte("remote-storage-source-v1\x00" + taskID))
	return "storage:source:" + hex.EncodeToString(digest[:20])
}

func StorageSourceLeaseExpiry() time.Time { return time.Now().UTC().Add(5 * time.Minute) }
