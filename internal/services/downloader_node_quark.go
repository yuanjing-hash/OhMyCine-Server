package services

import (
	"context"
	"encoding/json"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/nodeclient"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
	"strings"
)

func (s *DownloaderService) nodeQuarkStorageSourceClient(ctx context.Context, record models.Downloader, task models.DownloadTask) (*nodeclient.StorageSourceClient, error) {
	if s.transferNodes == nil || s.connections == nil || record.NodeID == nil || task.NodeID == nil || *record.NodeID != *task.NodeID || record.StorageID == nil || task.StagingStorageID == nil || *record.StorageID != *task.StagingStorageID || task.TargetStorageID == nil || task.TargetLibraryID == nil {
		return nil, appError(CodeDownloaderUnavailable, "夸克节点任务的来源和目标快照不完整", nil)
	}
	node, nodeRecord, e := s.transferNodes.nodeClient(*record.NodeID)
	if e != nil {
		return nil, e
	}
	var capabilities nodeprotocol.Capabilities
	if json.Unmarshal([]byte(nodeRecord.CapabilitiesJSON), &capabilities) != nil || !nodeprotocol.QuarkShareSupported(capabilities) {
		return nil, appError(CodeTransferRouteUnsupported, "请升级传输节点：需要夸克分享接收、原文件读取和完整文件校验能力", nil)
	}
	var storage models.Storage
	if e = s.db.WithContext(ctx).First(&storage, *record.StorageID).Error; e != nil || !storage.Enabled || storage.Type != models.StorageTypeQuark || storage.ConnectionID == nil {
		return nil, appError(CodeDownloaderStorageUnavailable, "夸克来源数据源不可用", e)
	}
	plaintext, e := s.credentials.Decrypt(downloadSourcePurpose(task.ID), task.SourceCiphertext)
	if e != nil {
		return nil, appError(CodeConnectionUnavailable, "下载来源凭据不可用", e)
	}
	var source downloadSourceEnvelope
	if e = json.Unmarshal([]byte(plaintext), &source); e != nil {
		return nil, appError(CodeDownloadSourceInvalid, "下载来源快照无效", e)
	}
	kind, uri, e := nodeStorageSourceInput(source)
	if e != nil {
		return nil, e
	}
	parent := strings.TrimSpace(task.StagingProviderDirectoryID)
	if parent == "" {
		parent = strings.TrimSpace(record.ProviderDirectoryID)
	}
	if parent == "" {
		parent = storage.RootPath
	}
	plan, e := buildNodeStorageSourcePlan(storage, task, kind, uri, parent)
	if e != nil {
		return nil, e
	}
	return nodeclient.NewStorageSourceClient(node, task.ID, plan.StorageID, kind, uri, plan,
		func(ctx context.Context, p nodeprotocol.StorageSourcePlan) (nodeclient.StorageSourceOperation, error) {
			return s.prepareNodeStorageSourceOperation(ctx, nodeRecord.ID, task.ID, p)
		},
		func(ctx context.Context, op string, actions []string, uri string) (nodeprotocol.CredentialGrantEnvelope, error) {
			return s.transferNodes.sealQuarkSourceGrant(ctx, nodeRecord, storage, task.ID, op, actions, uri)
		},
		func(ctx context.Context, r nodeprotocol.OperationResponse, a *nodeprotocol.StorageSourceActionResponse) error {
			return s.observeNodeStorageSource(ctx, task.ID, r, a)
		})
}
