package services

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/nodeclient"
	cloudpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type nodeStorageProbe struct{ node *nodeclient.Client }

func (client nodeStorageProbe) Test(ctx context.Context) (downloadpkg.Health, error) {
	health, err := client.node.Health(ctx)
	if err != nil {
		return downloadpkg.Health{}, downloadpkg.Error(nodeprotocol.ErrorNodeOffline, true, err)
	}
	for _, capability := range []string{nodeprotocol.CapabilityPan115Offline, nodeprotocol.CapabilityPan115Read, nodeprotocol.CapabilityPan115ShareReceive, nodeprotocol.CapabilityRangeExport} {
		if !health.Capabilities.Has(capability) {
			return downloadpkg.Health{}, downloadpkg.Error(nodeprotocol.ErrorCapabilityMissing, false, nil)
		}
	}
	return downloadpkg.Health{Version: health.AgentVersion}, nil
}
func (nodeStorageProbe) Submit(context.Context, downloadpkg.SubmitRequest) (downloadpkg.Task, error) {
	return downloadpkg.Task{}, downloadpkg.Error("downloader_task_context_required", false, nil)
}
func (nodeStorageProbe) Get(context.Context, string) (downloadpkg.Task, error) {
	return downloadpkg.Task{}, downloadpkg.Error("downloader_task_context_required", false, nil)
}
func (nodeStorageProbe) Cancel(context.Context, string, bool) error {
	return downloadpkg.Error("downloader_task_context_required", false, nil)
}

func (s *DownloaderService) nodeStorageProbeClient(record models.Downloader) (downloadpkg.Client, error) {
	if record.NodeID == nil || s.transferNodes == nil {
		return nil, appError(CodeDownloaderUnavailable, "节点 115 下载器绑定不完整", nil)
	}
	client, _, err := s.transferNodes.nodeClient(*record.NodeID)
	if err != nil {
		return nil, err
	}
	return nodeStorageProbe{node: client}, nil
}

func (s *DownloaderService) nodeStorageSourceClient(ctx context.Context, record models.Downloader, task models.DownloadTask) (*nodeclient.StorageSourceClient, error) {
	if s == nil || s.transferNodes == nil || s.connections == nil || record.NodeID == nil || task.NodeID == nil || *record.NodeID != *task.NodeID || record.StorageID == nil || task.StagingStorageID == nil || *record.StorageID != *task.StagingStorageID {
		return nil, appError(CodeDownloaderUnavailable, "节点 115 下载任务的来源快照不完整", nil)
	}
	node, nodeRecord, err := s.transferNodes.nodeClient(*record.NodeID)
	if err != nil {
		return nil, err
	}
	var sourceStorage models.Storage
	if err := s.db.WithContext(ctx).First(&sourceStorage, *record.StorageID).Error; err != nil || sourceStorage.Type != models.StorageTypePan115 || sourceStorage.ConnectionID == nil {
		return nil, appError(CodeDownloaderStorageUnavailable, "节点 115 下载器的来源数据源不可用", err)
	}
	if task.TargetStorageID == nil || task.TargetLibraryID == nil {
		return nil, appError(CodeTransferRouteUnsupported, "节点 115 下载必须选择一个最终媒体库", nil)
	}
	plaintext, err := s.credentials.Decrypt(downloadSourcePurpose(task.ID), task.SourceCiphertext)
	if err != nil {
		return nil, appError(CodeConnectionUnavailable, "下载来源凭据不可用", err)
	}
	var source downloadSourceEnvelope
	if err := json.Unmarshal([]byte(plaintext), &source); err != nil {
		return nil, appError(CodeDownloadSourceInvalid, "下载来源快照无效", err)
	}
	sourceKind, sourceURI, err := nodeStorageSourceInput(source)
	if err != nil {
		return nil, err
	}
	destination, needsCreate, err := nodeStorageSourceDirectorySnapshot(record, task)
	if err != nil {
		return nil, err
	}
	if needsCreate {
		parentID := destination.ParentID
		destination, err = s.ensureNodeStorageSourceDirectory(ctx, sourceStorage, parentID, task.ProviderTag)
		if err != nil {
			return nil, err
		}
	}
	plan, err := buildNodeStorageSourcePlan(sourceStorage, task, sourceKind, sourceURI, destination.ID)
	if err != nil {
		return nil, err
	}
	return nodeclient.NewStorageSourceClient(node, task.ID, plan.StorageID, sourceKind, sourceURI, plan,
		func(callCtx context.Context, payload nodeprotocol.StorageSourcePlan) (nodeclient.StorageSourceOperation, error) {
			return s.prepareNodeStorageSourceOperation(callCtx, nodeRecord.ID, task.ID, payload)
		},
		func(callCtx context.Context, operationKey string, actions []string, uri string) (nodeprotocol.CredentialGrantEnvelope, error) {
			return s.transferNodes.sealStorageSourceGrant(callCtx, nodeRecord, sourceStorage, task.ID, operationKey, actions, uri)
		},
		func(callCtx context.Context, remote nodeprotocol.OperationResponse, action *nodeprotocol.StorageSourceActionResponse) error {
			return s.observeNodeStorageSource(callCtx, task.ID, remote, action)
		})
}

func nodeStorageSourceDirectorySnapshot(record models.Downloader, task models.DownloadTask) (cloudpkg.Item, bool, error) {
	parentID := strings.TrimSpace(task.StagingProviderDirectoryID)
	if parentID == "" {
		parentID = strings.TrimSpace(record.ProviderDirectoryID)
	}
	name := strings.TrimSpace(task.ProviderTag)
	if parentID == "" || !strings.HasPrefix(name, "omc-") || len(name) > 64 {
		return cloudpkg.Item{}, false, appError(CodeDownloaderStorageUnavailable, "节点 115 下载目录快照无效", nil)
	}
	destination := cloudpkg.Item{ID: strings.TrimSpace(task.ProviderOutputID), ParentID: parentID, Name: name, IsDir: true}
	return destination, destination.ID == "", nil
}

func nodeStorageSourceInput(source downloadSourceEnvelope) (string, string, error) {
	uri := strings.TrimSpace(source.URL)
	switch {
	case source.Kind == downloadpkg.SourceURL && strings.HasPrefix(strings.ToLower(uri), "magnet:"):
		return nodeprotocol.StorageSourceKindPan115OfflineMagnet, uri, nil
	case source.Kind == downloadpkg.SourceURL:
		return "", "", appError(CodeTransferRouteUnsupported, "传输节点首版只支持可稳定恢复的磁力离线任务，不支持普通 HTTP(S) 离线 URL", nil)
	case source.Kind == downloadpkg.SourcePan115Share:
		if source.ShareSelection != nil {
			raw, err := cloudpkg.EncodeSelectedShareSource(uri, *source.ShareSelection)
			return nodeprotocol.StorageSourceKindPan115ShareSelected, raw, err
		}
		return nodeprotocol.StorageSourceKindPan115Share, uri, nil
	default:
		return "", "", appError(CodeTransferRouteUnsupported, "当前下载来源不能交给传输节点的 115 下载器", nil)
	}
}

func buildNodeStorageSourcePlan(sourceStorage models.Storage, task models.DownloadTask, sourceKind, sourceURI, destinationID string) (nodeprotocol.StorageSourcePlan, error) {
	if sourceStorage.ID == 0 || sourceStorage.Type != models.StorageTypePan115 || sourceStorage.ConnectionID == nil {
		return nodeprotocol.StorageSourcePlan{}, appError(CodeDownloaderStorageUnavailable, "节点 115 下载器的来源数据源不可用", nil)
	}
	contentDigest, err := nodeprotocol.StorageSourceContentDigest(sourceKind, sourceURI)
	if err != nil {
		return nodeprotocol.StorageSourcePlan{}, appError(CodeDownloadSourceInvalid, "下载来源缺少稳定身份，不能安全重试", err)
	}
	sourceIdentity, err := nodeprotocol.StorageIdentityDigest(nodeprotocol.StorageProviderPan115, strconv.FormatUint(uint64(*sourceStorage.ConnectionID), 10))
	if err != nil {
		return nodeprotocol.StorageSourcePlan{}, appError(CodeTransferRouteUnsupported, "来源 115 身份无法冻结", err)
	}
	targetKind, targetIdentity := nodeprotocol.StorageSourceTargetLocal, ""
	if task.TargetStorageType == models.StorageTypePan115 {
		if task.TargetConnectionID == nil {
			return nodeprotocol.StorageSourcePlan{}, appError(CodeTransferRouteUnsupported, "目标 115 身份无法冻结", nil)
		}
		targetKind = nodeprotocol.StorageSourceTargetPan115
		targetIdentity, err = nodeprotocol.StorageIdentityDigest(nodeprotocol.StorageProviderPan115, strconv.FormatUint(uint64(*task.TargetConnectionID), 10))
		if err != nil || targetIdentity == sourceIdentity {
			return nodeprotocol.StorageSourcePlan{}, appError(CodeTransferRouteUnsupported, "同一 115 账号不经过传输节点中转", err)
		}
	} else if task.TargetStorageType != models.StorageTypeLocal {
		return nodeprotocol.StorageSourcePlan{}, appError(CodeTransferRouteUnsupported, "目标媒体库不支持节点中转", nil)
	}
	plan := nodeprotocol.StorageSourcePlan{StorageID: strconv.FormatUint(uint64(sourceStorage.ID), 10), ProviderType: nodeprotocol.StorageProviderPan115, SourceKind: sourceKind, TargetKind: targetKind, SourceIdentityDigest: sourceIdentity, TargetIdentityDigest: targetIdentity, SourceContentDigest: contentDigest, OfflineDestinationID: strings.TrimSpace(destinationID)}
	if err := plan.Validate(); err != nil {
		return nodeprotocol.StorageSourcePlan{}, appError(CodeTransferRouteUnsupported, "节点 115 来源计划无效", err)
	}
	return plan, nil
}

func (s *DownloaderService) ensureNodeStorageSourceDirectory(ctx context.Context, storage models.Storage, parentID, name string) (cloudpkg.Item, error) {
	if storage.ConnectionID == nil || strings.TrimSpace(parentID) == "" || !strings.HasPrefix(name, "omc-") || len(name) > 64 {
		return cloudpkg.Item{}, appError(CodeDownloaderStorageUnavailable, "节点 115 下载目录快照无效", nil)
	}
	_, driver, err := s.connections.driver(*storage.ConnectionID)
	if err != nil {
		return cloudpkg.Item{}, appError(CodeConnectionUnavailable, "来源 115 连接不可用", err)
	}
	mutations, ok := driver.(cloudpkg.MutationDriver)
	if !ok || !driver.Capabilities().CreateDirectory {
		return cloudpkg.Item{}, appError(CodeDownloaderStorageUnavailable, "来源 115 不支持创建任务专属目录", nil)
	}
	find := func() (cloudpkg.Item, bool, error) {
		for offset := int64(0); ; {
			page, listErr := driver.List(cloudpkg.WithReadClass(ctx, cloudpkg.ReadClassPipeline), parentID, cloudpkg.PageRequest{Offset: offset, Limit: 250})
			if listErr != nil {
				return cloudpkg.Item{}, false, listErr
			}
			if page.Offset != offset || page.HasMore && len(page.Items) == 0 {
				return cloudpkg.Item{}, false, errors.New("provider directory pagination made no progress")
			}
			for _, item := range page.Items {
				if item.Name == name {
					if item.ParentID != parentID || !item.IsDir || strings.TrimSpace(item.ID) == "" {
						return cloudpkg.Item{}, false, errors.New("task directory identity conflicts")
					}
					return item, true, nil
				}
			}
			if !page.HasMore {
				return cloudpkg.Item{}, false, nil
			}
			offset += int64(len(page.Items))
		}
	}
	if item, exists, findErr := find(); findErr != nil {
		return cloudpkg.Item{}, appError(CodeDownloaderStorageUnavailable, "无法检查节点 115 任务目录", findErr)
	} else if exists {
		return item, nil
	}
	created, err := mutations.CreateDirectory(ctx, parentID, name)
	if err == nil && created.ParentID == parentID && created.Name == name && created.IsDir && strings.TrimSpace(created.ID) != "" {
		return created, nil
	}
	if recovered, exists, findErr := find(); findErr == nil && exists {
		return recovered, nil
	}
	return cloudpkg.Item{}, appError(CodeDownloaderStorageUnavailable, "无法创建节点 115 任务专属目录", err)
}

func (s *DownloaderService) prepareNodeStorageSourceOperation(ctx context.Context, nodeID, taskID string, payload nodeprotocol.StorageSourcePlan) (nodeclient.StorageSourceOperation, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nodeclient.StorageSourceOperation{}, err
	}
	now := time.Now().UTC()
	operationKey := nodeclient.StorageSourceOperationKey(taskID)
	plan := nodeprotocol.OperationPlan{ProtocolVersion: nodeprotocol.VersionV1, OperationKey: operationKey, TaskID: taskID, NodeID: nodeID, Kind: nodeprotocol.OperationKindStorageSourceMaterialize, PlanRevision: 1, LeaseEpoch: 1, LeaseExpiresAt: now.Add(remoteUploadLeaseDuration), Payload: raw}
	if err := plan.Validate(now); err != nil {
		return nodeclient.StorageSourceOperation{}, downloadpkg.Error(nodeprotocol.ErrorPlanConflict, false, err)
	}
	digest, err := plan.Digest()
	if err != nil {
		return nodeclient.StorageSourceOperation{}, err
	}
	var record models.RemoteOperation
	completed := false
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("operation_key = ?", operationKey).First(&record).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			record = models.RemoteOperation{OperationKey: operationKey, TaskID: taskID, NodeID: nodeID, PlanDigest: digest, PlanRevision: plan.PlanRevision, LeaseEpoch: plan.LeaseEpoch, Phase: nodeprotocol.PhaseAccepted, Status: nodeprotocol.OperationPending, CreatedAt: now, UpdatedAt: now}
			return tx.Create(&record).Error
		}
		if err != nil {
			return err
		}
		if record.TaskID != taskID || record.NodeID != nodeID || record.PlanDigest != digest || record.PlanRevision != plan.PlanRevision {
			return downloadpkg.Error(nodeprotocol.ErrorPlanConflict, false, nil)
		}
		if record.Status == nodeprotocol.OperationCompleted {
			completed = true
			plan.LeaseEpoch = record.LeaseEpoch
			return nil
		}
		if record.Status == nodeprotocol.OperationFailed || record.Status == nodeprotocol.OperationCancelled {
			return downloadpkg.Error(firstNonEmpty(record.ErrorCode, nodeprotocol.ErrorPlanConflict), false, nil)
		}
		plan.LeaseEpoch = record.LeaseEpoch + 1
		plan.LeaseExpiresAt = now.Add(remoteUploadLeaseDuration)
		record.LeaseEpoch = plan.LeaseEpoch
		return tx.Model(&models.RemoteOperation{}).Where("id = ? AND plan_digest = ?", record.ID, digest).Updates(map[string]any{"lease_epoch": record.LeaseEpoch, "updated_at": now}).Error
	})
	if err != nil {
		return nodeclient.StorageSourceOperation{}, err
	}
	return nodeclient.StorageSourceOperation{Plan: plan, PlanDigest: digest, Completed: completed}, nil
}

func (s *DownloaderService) observeNodeStorageSource(ctx context.Context, taskID string, remote nodeprotocol.OperationResponse, action *nodeprotocol.StorageSourceActionResponse) error {
	operationKey := nodeclient.StorageSourceOperationKey(taskID)
	var record models.RemoteOperation
	if err := s.db.WithContext(ctx).Where("operation_key = ? AND task_id = ?", operationKey, taskID).First(&record).Error; err != nil {
		return err
	}
	status, phase, code, progress := remote.Status, remote.Phase, remote.ErrorCode, remote.Progress
	if action != nil {
		code, progress = action.ErrorCode, action.Progress
		switch action.Status {
		case nodeprotocol.StorageSourceSubmitting:
			status, phase = nodeprotocol.OperationRunning, nodeprotocol.PhaseSubmittingDownload
		case nodeprotocol.StorageSourceWaiting:
			status, phase = nodeprotocol.OperationRunning, nodeprotocol.PhaseWaitingDownload
		case nodeprotocol.StorageSourceEnumerating:
			status, phase = nodeprotocol.OperationRunning, nodeprotocol.PhaseVerifyingRemoteFiles
		case nodeprotocol.StorageSourceDownloading:
			status, phase = nodeprotocol.OperationRunning, nodeprotocol.PhasePullingSource
		case nodeprotocol.StorageSourceWaitingCredentials:
			status, phase = nodeprotocol.OperationRunning, nodeprotocol.PhaseWaitingCredentials
		case nodeprotocol.StorageSourceCompleted:
			status, phase = nodeprotocol.OperationCompleted, nodeprotocol.PhaseWaitingServerPlan
		case nodeprotocol.StorageSourceFailed:
			status, phase = nodeprotocol.OperationFailed, nodeprotocol.PhasePullingSource
		}
	}
	updates := map[string]any{"status": status, "phase": phase, "progress": progress, "error_code": safeLabel(code, 96), "updated_at": time.Now().UTC()}
	if remote.LeaseEpoch > 0 {
		updates["lease_epoch"] = remote.LeaseEpoch
	}
	result := s.db.WithContext(ctx).Model(&models.RemoteOperation{}).Where("id = ? AND operation_key = ? AND plan_digest = ?", record.ID, operationKey, record.PlanDigest).Updates(updates)
	if result.Error != nil || result.RowsAffected != 1 {
		return firstNonNil(result.Error, gorm.ErrRecordNotFound)
	}
	return nil
}

func (s *TransferNodeService) sealStorageSourceGrant(ctx context.Context, node models.TransferNode, storage models.Storage, taskID, operationKey string, actions []string, sourceURI string) (nodeprotocol.CredentialGrantEnvelope, error) {
	if storage.Type != models.StorageTypePan115 || storage.ConnectionID == nil || storage.ID == 0 {
		return nodeprotocol.CredentialGrantEnvelope{}, appError(CodeTransferRouteUnsupported, "来源存储不支持节点读取", nil)
	}
	allowed := map[string]struct{}{nodeprotocol.StorageSourceGrantOfflineSubmit: {}, nodeprotocol.StorageSourceGrantOfflineStatus: {}, nodeprotocol.StorageSourceGrantShareInspect: {}, nodeprotocol.StorageSourceGrantShareReceive: {}, nodeprotocol.StorageSourceGrantRead: {}}
	seen := map[string]struct{}{}
	normalized := make([]string, 0, len(actions))
	for _, action := range actions {
		if _, ok := allowed[action]; !ok {
			return nodeprotocol.CredentialGrantEnvelope{}, appError(CodeInvalidRequest, "节点来源授权动作无效", nil)
		}
		if _, duplicate := seen[action]; !duplicate {
			seen[action] = struct{}{}
			normalized = append(normalized, action)
		}
	}
	if _, ok := seen[nodeprotocol.StorageSourceGrantRead]; !ok {
		return nodeprotocol.CredentialGrantEnvelope{}, appError(CodeInvalidRequest, "节点来源授权必须包含读取动作", nil)
	}
	var connection models.Connection
	if err := s.db.WithContext(ctx).First(&connection, *storage.ConnectionID).Error; err != nil || !connection.Enabled || connection.Provider != cloudpkg.ProviderPan115 {
		return nodeprotocol.CredentialGrantEnvelope{}, appError(CodeConnectionUnavailable, "来源 115 连接不可用", err)
	}
	cookie, err := s.credentials.Decrypt(connectionPurpose(connection.ID, connection.Provider), connection.CredentialCiphertext)
	if err != nil {
		return nodeprotocol.CredentialGrantEnvelope{}, appError(CodeConnectionUnavailable, "来源 115 凭据不可用", err)
	}
	raw, err := json.Marshal(nodeprotocol.StorageSourceCredential{ProviderType: nodeprotocol.StorageProviderPan115, Cookie: cookie, SourceURI: sourceURI})
	if err != nil {
		return nodeprotocol.CredentialGrantEnvelope{}, err
	}
	now := s.now()
	grantID := uuid.NewString()
	resourceID := strconv.FormatUint(uint64(storage.ID), 10)
	grant := nodeprotocol.CredentialGrant{GrantID: grantID, NodeID: node.ID, TaskID: taskID, OperationKey: operationKey, ResourceKind: nodeprotocol.ResourceKindStorage, ResourceID: resourceID, CredentialRevision: connection.Revision, AllowedActions: normalized, ExpiresAt: now.Add(10 * time.Minute), Credential: raw}
	envelope, err := nodeprotocol.SealCredentialGrant(node.EncryptionPublicKey, grant, now)
	if err != nil {
		return nodeprotocol.CredentialGrantEnvelope{}, err
	}
	storageID := storage.ID
	record := models.NodeCredentialGrant{ID: grantID, NodeID: node.ID, TaskID: taskID, OperationKey: operationKey, StorageID: &storageID, CredentialRevision: connection.Revision, ExpiresAt: grant.ExpiresAt, CreatedAt: now}
	if err := s.db.WithContext(ctx).Create(&record).Error; err != nil {
		return nodeprotocol.CredentialGrantEnvelope{}, err
	}
	return envelope, nil
}
