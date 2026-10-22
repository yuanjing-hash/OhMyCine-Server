package services

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
	"gorm.io/gorm"
)

// Only current access credentials and this exact proven storage scope cross
// the sealed task boundary. Refresh tokens, Web sessions, other roots and
// unrelated accounts remain on Server.
func restrictedQuarkNodeCredentials(bundle quark.Credentials, root string) (json.RawMessage, error) {
	if bundle.TV == nil || bundle.Agent == nil {
		return nil, appError(CodeConnectionUnavailable, "夸克播放或文件管理尚未授权", nil)
	}
	tv, agent := *bundle.TV, *bundle.Agent
	tv.RefreshToken = ""
	agent.RefreshToken = ""
	out := quark.Credentials{Version: 1, Revision: bundle.Revision, TV: &tv, Agent: &agent}
	for _, p := range bundle.Proofs {
		if p.TVRoot == root && bundle.HasProof(p.TVRoot, p.AgentRoot) {
			out.Proofs = []quark.SpaceProof{p}
			break
		}
	}
	if len(out.Proofs) != 1 {
		return nil, appError(CodeConnectionUnavailable, "夸克目标范围尚未验证", nil)
	}
	return json.Marshal(out)
}
func (s *TransferNodeService) sealQuarkStorageGrant(ctx context.Context, node models.TransferNode, storage models.Storage, taskID, operationKey string, actions []string) (nodeprotocol.CredentialGrantEnvelope, error) {
	var empty nodeprotocol.CredentialGrantEnvelope
	var capabilities nodeprotocol.Capabilities
	if json.Unmarshal([]byte(node.CapabilitiesJSON), &capabilities) != nil || !nodeprotocol.QuarkUploadSupported(capabilities) {
		return empty, appError(CodeTransferRouteUnsupported, "请升级传输节点：需要夸克上传和完整文件校验能力", nil)
	}
	if storage.Type != models.StorageTypeQuark || storage.ConnectionID == nil || storage.ID == 0 || taskID == "" || operationKey == "" || len(actions) != 1 || actions[0] != nodeprotocol.StorageActionUpload {
		return empty, appError(CodeTransferRouteUnsupported, "夸克节点仅支持已验证的上传动作", nil)
	}
	registry := cloud.NewRegistry()
	if e := registry.Register(cloud.ProviderQuark, quark.New); e != nil {
		return empty, e
	}
	connections := NewConnectionService(s.db, s.audit, s.credentials, registry, zerolog.Nop())
	connection, driver, e := connections.driver(*storage.ConnectionID)
	if e != nil {
		return empty, e
	}
	q, ok := driver.(*quark.Client)
	if !ok || connection.Provider != cloud.ProviderQuark {
		return empty, appError(CodeConnectionUnavailable, "夸克连接不可用", nil)
	}
	// Refresh/validate identities on Server; Node can only ask for another short
	// grant and never rotates a long-lived account authorization itself.
	if _, e = q.ResolveManagementItem(ctx, storage.RootPath); e != nil {
		return empty, e
	}
	bundle := q.Credentials()
	raw, e := restrictedQuarkNodeCredentials(bundle, storage.RootPath)
	if e != nil {
		return empty, e
	}
	credential, e := json.Marshal(nodeprotocol.StorageCredential{ProviderType: cloud.ProviderQuark, Quark: raw, RootID: storage.RootPath})
	if e != nil {
		return empty, e
	}
	now := s.now()
	grant := nodeprotocol.CredentialGrant{GrantID: uuid.NewString(), NodeID: node.ID, TaskID: taskID, OperationKey: operationKey, ResourceKind: nodeprotocol.ResourceKindStorage, ResourceID: strconv.FormatUint(uint64(storage.ID), 10), CredentialRevision: bundle.Revision, AllowedActions: append([]string(nil), actions...), ExpiresAt: now.Add(10 * time.Minute), Credential: credential}
	envelope, e := nodeprotocol.SealCredentialGrant(node.EncryptionPublicKey, grant, now)
	if e != nil {
		return empty, e
	}
	e = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if e := s.fenceQuarkGrant(tx, connection, storage, bundle); e != nil {
			return e
		}
		record := models.NodeCredentialGrant{ID: grant.GrantID, NodeID: node.ID, TaskID: taskID, OperationKey: operationKey, StorageID: &storage.ID, CredentialRevision: bundle.Revision, ExpiresAt: grant.ExpiresAt, CreatedAt: now}
		if e := tx.Create(&record).Error; e != nil {
			return e
		}
		return s.audit.Record(tx, nil, "transfer_node.credential_grant", "transfer_node", node.ID, "success", map[string]any{"resource_kind": nodeprotocol.ResourceKindStorage, "actions": actions, "provider": cloud.ProviderQuark}, RequestContext{})
	})
	return envelope, e
}
func validRemoteQuarkDigest(raw, expected string) bool {
	var d nodeprotocol.ContentDigest
	return strings.TrimSpace(raw) != "" && json.Unmarshal([]byte(raw), &d) == nil && d.ValidReadbackSHA256(expected)
}

func (s *TransferNodeService) sealQuarkSourceGrant(ctx context.Context, node models.TransferNode, storage models.Storage, taskID, operationKey string, actions []string, sourceURI string) (nodeprotocol.CredentialGrantEnvelope, error) {
	var empty nodeprotocol.CredentialGrantEnvelope
	var caps nodeprotocol.Capabilities
	if storage.Type != models.StorageTypeQuark || storage.ConnectionID == nil || storage.ID == 0 || taskID == "" || operationKey == "" || json.Unmarshal([]byte(node.CapabilitiesJSON), &caps) != nil || !nodeprotocol.QuarkShareSupported(caps) {
		return empty, appError(CodeTransferRouteUnsupported, "请升级节点后使用夸克分享接收与读取", nil)
	}
	seen := map[string]bool{}
	for _, action := range actions {
		switch action {
		case nodeprotocol.StorageSourceGrantRead, nodeprotocol.StorageSourceGrantShareInspect, nodeprotocol.StorageSourceGrantShareReceive:
		default:
			return empty, appError(CodeInvalidRequest, "夸克来源授权动作无效", nil)
		}
		seen[action] = true
	}
	if !seen[nodeprotocol.StorageSourceGrantRead] {
		return empty, appError(CodeInvalidRequest, "来源授权缺少读取动作", nil)
	}
	registry := cloud.NewRegistry()
	if e := registry.Register(cloud.ProviderQuark, quark.New); e != nil {
		return empty, e
	}
	connections := NewConnectionService(s.db, s.audit, s.credentials, registry, zerolog.Nop())
	connection, driver, e := connections.driver(*storage.ConnectionID)
	if e != nil {
		return empty, e
	}
	q, ok := driver.(*quark.Client)
	if !ok {
		return empty, appError(CodeConnectionUnavailable, "夸克连接不可用", nil)
	}
	if _, e = q.ResolveManagementItem(ctx, storage.RootPath); e != nil {
		return empty, e
	}
	bundle := q.Credentials()
	raw, e := restrictedQuarkNodeCredentials(bundle, storage.RootPath)
	if e != nil {
		return empty, e
	}
	credential := nodeprotocol.StorageSourceCredential{ProviderType: cloud.ProviderQuark, Quark: raw, RootID: storage.RootPath, SourceURI: sourceURI}
	if e = nodeprotocol.ValidateStorageSourceCredential(credential, seen[nodeprotocol.StorageSourceGrantShareReceive]); e != nil {
		return empty, e
	}
	payload, e := json.Marshal(credential)
	if e != nil {
		return empty, e
	}
	now := s.now()
	grant := nodeprotocol.CredentialGrant{GrantID: uuid.NewString(), NodeID: node.ID, TaskID: taskID, OperationKey: operationKey, ResourceKind: nodeprotocol.ResourceKindStorage, ResourceID: strconv.FormatUint(uint64(storage.ID), 10), CredentialRevision: bundle.Revision, AllowedActions: append([]string(nil), actions...), ExpiresAt: now.Add(10 * time.Minute), Credential: payload}
	envelope, e := nodeprotocol.SealCredentialGrant(node.EncryptionPublicKey, grant, now)
	if e != nil {
		return empty, e
	}
	e = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if e := s.fenceQuarkGrant(tx, connection, storage, bundle); e != nil {
			return e
		}
		if e := tx.Create(&models.NodeCredentialGrant{ID: grant.GrantID, NodeID: node.ID, TaskID: taskID, OperationKey: operationKey, StorageID: &storage.ID, CredentialRevision: bundle.Revision, ExpiresAt: grant.ExpiresAt, CreatedAt: now}).Error; e != nil {
			return e
		}
		return s.audit.Record(tx, nil, "transfer_node.credential_grant", "transfer_node", node.ID, "success", map[string]any{"resource_kind": nodeprotocol.ResourceKindStorage, "actions": actions, "provider": cloud.ProviderQuark}, RequestContext{})
	})
	return envelope, e
}

func (s *TransferNodeService) fenceQuarkGrant(tx *gorm.DB, connection models.Connection, storage models.Storage, bundle quark.Credentials) error {
	var current models.Connection
	if e := tx.First(&current, connection.ID).Error; e != nil {
		return e
	}
	if current.Provider != cloud.ProviderQuark || !current.Enabled || current.Revision != connection.Revision {
		return appError(CodeConnectionUnavailable, "夸克连接已变化", nil)
	}
	raw, e := s.credentials.Decrypt(connectionPurpose(current.ID, current.Provider), current.CredentialCiphertext)
	if e != nil {
		return appError(CodeConnectionUnavailable, "夸克授权不可用", nil)
	}
	currentBundle, e := quark.ParseCredentials(json.RawMessage(raw))
	if e != nil || currentBundle.Revision != bundle.Revision {
		return appError(CodeConnectionUnavailable, "夸克授权已更新，请重试当前任务", nil)
	}
	var currentStorage models.Storage
	if e = tx.First(&currentStorage, storage.ID).Error; e != nil {
		return e
	}
	if !currentStorage.Enabled || currentStorage.Type != storage.Type || currentStorage.ConnectionID == nil || storage.ConnectionID == nil || *currentStorage.ConnectionID != *storage.ConnectionID || currentStorage.RootPath != storage.RootPath {
		return appError(CodeDownloaderStorageUnavailable, "夸克数据源目录已变化", nil)
	}
	return nil
}
func remoteUploadCapabilities(capabilities nodeprotocol.Capabilities, storageType string) bool {
	if storageType == models.StorageTypeQuark {
		return nodeprotocol.QuarkUploadSupported(capabilities)
	}
	return storageType == models.StorageTypePan115 && capabilities.Has(nodeprotocol.CapabilityPan115Upload) && capabilities.Has(nodeprotocol.CapabilityRangeExport)
}
