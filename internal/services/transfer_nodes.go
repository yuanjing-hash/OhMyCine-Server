package services

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/credential"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/nodeclient"
	cloudpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
	"gorm.io/gorm"
)

type TransferNodeService struct {
	db          *gorm.DB
	audit       *AuditService
	credentials *credential.Store
	now         func() time.Time
}

func NewTransferNodeService(db *gorm.DB, audit *AuditService, credentials *credential.Store) *TransferNodeService {
	return &TransferNodeService{db: db, audit: audit, credentials: credentials, now: func() time.Time { return time.Now().UTC() }}
}

func (s *TransferNodeService) nodeClient(id string) (*nodeclient.Client, models.TransferNode, error) {
	var node models.TransferNode
	if err := s.db.First(&node, "id = ?", strings.TrimSpace(id)).Error; err != nil {
		return nil, node, transferNodeNotFound(err)
	}
	if node.Status != models.NodeStatusOnline || !nodeprotocol.ValidDigest(node.PublicKeyFingerprint) || strings.TrimSpace(node.EncryptionPublicKey) == "" {
		return nil, node, appError(CodeDownloaderUnavailable, "所选传输节点当前不可用", nil)
	}
	identity, err := s.controllerIdentity()
	if err != nil {
		return nil, node, err
	}
	client, err := nodeclient.New(node.APIURL, node.PublicKeyFingerprint, identity)
	if err != nil {
		return nil, node, appError(CodeDownloaderUnavailable, "传输节点连接配置无效", err)
	}
	return client, node, nil
}

func (s *TransferNodeService) sealDownloaderGrant(ctx context.Context, node models.TransferNode, binding models.NodeDownloaderBinding, taskID, operationKey, action string, credential nodeprotocol.DownloaderCredential) (nodeprotocol.CredentialGrantEnvelope, error) {
	raw, err := json.Marshal(credential)
	if err != nil {
		return nodeprotocol.CredentialGrantEnvelope{}, err
	}
	now := s.now()
	grantID := uuid.NewString()
	grant := nodeprotocol.CredentialGrant{GrantID: grantID, NodeID: node.ID, TaskID: taskID, OperationKey: operationKey, ResourceKind: nodeprotocol.ResourceKindDownloader, ResourceID: binding.DownloaderID, CredentialRevision: binding.Revision, AllowedActions: []string{action}, ExpiresAt: now.Add(10 * time.Minute), Credential: raw}
	envelope, err := nodeprotocol.SealCredentialGrant(node.EncryptionPublicKey, grant, now)
	if err != nil {
		return nodeprotocol.CredentialGrantEnvelope{}, err
	}
	downloaderID := binding.DownloaderID
	record := models.NodeCredentialGrant{ID: grantID, NodeID: node.ID, TaskID: taskID, OperationKey: operationKey, DownloaderID: &downloaderID, CredentialRevision: binding.Revision, ExpiresAt: grant.ExpiresAt, CreatedAt: now}
	if err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&record).Error; err != nil {
			return err
		}
		return s.audit.Record(tx, nil, "transfer_node.credential_grant", "transfer_node", node.ID, "success", map[string]any{"resource_kind": nodeprotocol.ResourceKindDownloader, "action": action}, RequestContext{})
	}); err != nil {
		return nodeprotocol.CredentialGrantEnvelope{}, err
	}
	return envelope, nil
}

func (s *TransferNodeService) sealStorageGrant(ctx context.Context, node models.TransferNode, storage models.Storage, taskID, operationKey string, actions []string) (nodeprotocol.CredentialGrantEnvelope, error) {
	if storage.Type != models.StorageTypePan115 || storage.ConnectionID == nil || storage.ID == 0 || strings.TrimSpace(taskID) == "" || strings.TrimSpace(operationKey) == "" {
		return nodeprotocol.CredentialGrantEnvelope{}, appError(CodeTransferRouteUnsupported, "目标存储不支持节点直传", nil)
	}
	actions, err := normalizeStorageGrantActions(actions)
	if err != nil {
		return nodeprotocol.CredentialGrantEnvelope{}, err
	}
	var connection models.Connection
	if err := s.db.WithContext(ctx).First(&connection, *storage.ConnectionID).Error; err != nil || !connection.Enabled || connection.Provider != cloudpkg.ProviderPan115 {
		return nodeprotocol.CredentialGrantEnvelope{}, appError(CodeConnectionUnavailable, "目标 115 连接不可用", err)
	}
	cookie, err := s.credentials.Decrypt(connectionPurpose(connection.ID, connection.Provider), connection.CredentialCiphertext)
	if err != nil {
		return nodeprotocol.CredentialGrantEnvelope{}, appError(CodeConnectionUnavailable, "目标 115 凭据不可用", err)
	}
	credentialRaw, err := json.Marshal(nodeprotocol.StorageCredential{ProviderType: nodeprotocol.StorageProviderPan115, Cookie: cookie})
	if err != nil {
		return nodeprotocol.CredentialGrantEnvelope{}, err
	}
	now := s.now()
	resourceID := strconv.FormatUint(uint64(storage.ID), 10)
	grantID := uuid.NewString()
	var existing models.NodeCredentialGrant
	lookup := s.db.WithContext(ctx).Where("node_id = ? AND task_id = ? AND operation_key = ? AND storage_id = ? AND credential_revision = ? AND revoked_at IS NULL AND expires_at > ?", node.ID, taskID, operationKey, storage.ID, connection.Revision, now.Add(time.Minute)).Order("expires_at DESC").First(&existing).Error
	if lookup == nil {
		grantID = existing.ID
	} else if !errors.Is(lookup, gorm.ErrRecordNotFound) {
		return nodeprotocol.CredentialGrantEnvelope{}, lookup
	}
	grant := nodeprotocol.CredentialGrant{GrantID: grantID, NodeID: node.ID, TaskID: taskID, OperationKey: operationKey, ResourceKind: nodeprotocol.ResourceKindStorage, ResourceID: resourceID, CredentialRevision: connection.Revision, AllowedActions: actions, ExpiresAt: now.Add(10 * time.Minute), Credential: credentialRaw}
	envelope, err := nodeprotocol.SealCredentialGrant(node.EncryptionPublicKey, grant, now)
	if err != nil {
		return nodeprotocol.CredentialGrantEnvelope{}, err
	}
	record := models.NodeCredentialGrant{ID: grantID, NodeID: node.ID, TaskID: taskID, OperationKey: operationKey, StorageID: &storage.ID, CredentialRevision: connection.Revision, ExpiresAt: grant.ExpiresAt, CreatedAt: now}
	if err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if existing.ID == "" {
			if err := tx.Create(&record).Error; err != nil {
				return err
			}
		} else if result := tx.Model(&models.NodeCredentialGrant{}).Where("id = ? AND revoked_at IS NULL", grantID).Updates(map[string]any{"expires_at": grant.ExpiresAt, "credential_revision": connection.Revision}); result.Error != nil || result.RowsAffected != 1 {
			return firstNonNil(result.Error, gorm.ErrRecordNotFound)
		}
		return s.audit.Record(tx, nil, "transfer_node.credential_grant", "transfer_node", node.ID, "success", map[string]any{"resource_kind": nodeprotocol.ResourceKindStorage, "actions": actions}, RequestContext{})
	}); err != nil {
		return nodeprotocol.CredentialGrantEnvelope{}, err
	}
	return envelope, nil
}

func normalizeStorageGrantActions(actions []string) ([]string, error) {
	seen := make(map[string]struct{}, len(actions))
	normalized := make([]string, 0, len(actions))
	for _, action := range actions {
		action = strings.TrimSpace(action)
		if action != nodeprotocol.StorageActionUpload && action != nodeprotocol.StorageActionReplace {
			return nil, appError(CodeInvalidRequest, "节点存储授权动作无效", nil)
		}
		if _, exists := seen[action]; exists {
			continue
		}
		seen[action] = struct{}{}
		normalized = append(normalized, action)
	}
	if _, ok := seen[nodeprotocol.StorageActionUpload]; !ok {
		return nil, appError(CodeInvalidRequest, "节点存储授权必须包含上传动作", nil)
	}
	return normalized, nil
}

type TransferNodeSummary struct {
	ID              string                    `json:"id"`
	Name            string                    `json:"name"`
	APIURL          string                    `json:"api_url"`
	Status          string                    `json:"status"`
	Platform        string                    `json:"platform"`
	Architecture    string                    `json:"architecture"`
	ProtocolMin     int                       `json:"protocol_min"`
	ProtocolMax     int                       `json:"protocol_max"`
	AgentVersion    string                    `json:"agent_version"`
	Capabilities    nodeprotocol.Capabilities `json:"capabilities"`
	FreeBytes       *int64                    `json:"free_bytes,omitempty"`
	FreeBytesKnown  bool                      `json:"free_bytes_known"`
	LastErrorCode   string                    `json:"last_error_code,omitempty"`
	LastHeartbeatAt *time.Time                `json:"last_heartbeat_at,omitempty"`
	Revision        uint64                    `json:"revision"`
	CreatedAt       time.Time                 `json:"created_at"`
	UpdatedAt       time.Time                 `json:"updated_at"`
	Default         bool                      `json:"default"`
}

type CreateTransferNodeInput struct {
	Name         string
	APIURL       string
	Platform     string
	Architecture string
}

type UpdateTransferNodeInput struct {
	Name     *string
	APIURL   *string
	Enabled  *bool
	Revision uint64
}

type TransferNodeSettingsSummary struct {
	DefaultNodeID *string `json:"default_node_id"`
	Revision      uint64  `json:"revision"`
}

func (s *TransferNodeService) List(actor Actor) ([]TransferNodeSummary, error) {
	if !actor.Can(authz.PermissionTransferNodesRead) {
		return nil, appError(CodePermissionDenied, "无权查看传输节点", nil)
	}
	var rows []models.TransferNode
	if err := s.db.Order("name_normalized,id").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]TransferNodeSummary, 0, len(rows))
	settings, _ := s.settings()
	for _, row := range rows {
		summary := s.summary(row)
		summary.Default = settings.DefaultNodeID != nil && *settings.DefaultNodeID == row.ID
		out = append(out, summary)
	}
	return out, nil
}

func (s *TransferNodeService) Create(ctx context.Context, actor Actor, input CreateTransferNodeInput, request RequestContext) (TransferNodeSummary, string, error) {
	if !actor.Can(authz.PermissionTransferNodesCreate) {
		return TransferNodeSummary{}, "", appError(CodePermissionDenied, "无权创建传输节点", nil)
	}
	name := strings.Join(strings.Fields(input.Name), " ")
	if name == "" || len([]rune(name)) > 128 {
		return TransferNodeSummary{}, "", appError(CodeInvalidRequest, "节点名称无效", nil)
	}
	apiURL, err := normalizeNodeURL(input.APIURL)
	if err != nil {
		return TransferNodeSummary{}, "", err
	}
	platform, architecture, err := normalizeNodePlatform(input.Platform, input.Architecture)
	if err != nil {
		return TransferNodeSummary{}, "", err
	}
	token, enrollment, err := s.newEnrollment("", platform, architecture)
	if err != nil {
		return TransferNodeSummary{}, "", err
	}
	now := s.now()
	row := models.TransferNode{ID: uuid.NewString(), OwnerID: actor.User.ID, Name: name, NameNormalized: strings.ToLower(name), APIURL: apiURL, Status: models.NodeStatusPending, Platform: platform, Architecture: architecture, ProtocolMin: 1, ProtocolMax: 1, Revision: 1, CreatedAt: now, UpdatedAt: now}
	enrollment.NodeID = row.ID
	err = s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&row).Error; err != nil {
			return err
		}
		if err := tx.Create(&enrollment).Error; err != nil {
			return err
		}
		return s.audit.Record(tx, &actor.User.ID, "transfer_node.create", "transfer_node", row.ID, "success", map[string]any{"platform": row.Platform, "architecture": row.Architecture}, request)
	})
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return TransferNodeSummary{}, "", appError(CodeConflict, "传输节点名称已存在", nil)
		}
		return TransferNodeSummary{}, "", err
	}
	return s.summary(row), token, nil
}

func (s *TransferNodeService) Enroll(ctx context.Context, actor Actor, id string, request RequestContext) (TransferNodeSummary, error) {
	if !actor.Can(authz.PermissionTransferNodesEnroll) {
		return TransferNodeSummary{}, appError(CodePermissionDenied, "无权配对传输节点", nil)
	}
	var row models.TransferNode
	if err := s.db.First(&row, "id = ?", id).Error; err != nil {
		return TransferNodeSummary{}, transferNodeNotFound(err)
	}
	if row.Status == models.NodeStatusRevoked {
		return TransferNodeSummary{}, appError(CodeConflict, "已撤销的节点不能重新配对", nil)
	}
	var enrollment models.NodeEnrollment
	now := s.now()
	if err := s.db.Where("node_id = ? AND consumed_at IS NULL AND expires_at > ?", id, now).Order("created_at DESC").First(&enrollment).Error; err != nil {
		return TransferNodeSummary{}, appError(CodeConflict, "安装命令已失效，请重新创建节点", err)
	}
	token, err := s.credentials.Decrypt("transfer-node-enrollment:"+enrollment.ID, enrollment.TokenCiphertext)
	if err != nil {
		return TransferNodeSummary{}, err
	}
	identity, err := s.controllerIdentity()
	if err != nil {
		return TransferNodeSummary{}, err
	}
	pairedIdentity, err := nodeclient.Enroll(ctx, row.APIURL, row.ID, token, identity)
	if err != nil {
		return TransferNodeSummary{}, appError(CodeConflict, "节点配对失败，请检查公网地址、HTTPS 和安装令牌", nil)
	}
	consumed := s.now()
	err = s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&models.NodeEnrollment{}).Where("id = ? AND consumed_at IS NULL", enrollment.ID).Updates(map[string]any{"consumed_at": consumed, "token_ciphertext": ""}).Error; err != nil {
			return err
		}
		row.PublicKeyFingerprint = pairedIdentity.CertificateFingerprint
		row.EncryptionPublicKey = pairedIdentity.EncryptionPublicKey
		row.Status = models.NodeStatusOffline
		row.LastErrorCode = ""
		row.Revision++
		row.UpdatedAt = consumed
		if err := tx.Save(&row).Error; err != nil {
			return err
		}
		return s.audit.Record(tx, &actor.User.ID, "transfer_node.enroll", "transfer_node", row.ID, "success", nil, request)
	})
	if err != nil {
		return TransferNodeSummary{}, err
	}
	return s.Test(ctx, actor, id, request)
}

func (s *TransferNodeService) Test(ctx context.Context, actor Actor, id string, request RequestContext) (TransferNodeSummary, error) {
	if !actor.Can(authz.PermissionTransferNodesTest) {
		return TransferNodeSummary{}, appError(CodePermissionDenied, "无权测试传输节点", nil)
	}
	var row models.TransferNode
	if err := s.db.First(&row, "id = ?", id).Error; err != nil {
		return TransferNodeSummary{}, transferNodeNotFound(err)
	}
	if row.Status == models.NodeStatusRevoked {
		return TransferNodeSummary{}, appError(CodeConflict, "已撤销的节点不能测试", nil)
	}
	if !nodeprotocol.ValidDigest(row.PublicKeyFingerprint) {
		return TransferNodeSummary{}, appError(CodeConflict, "节点尚未完成安全配对", nil)
	}
	identity, err := s.controllerIdentity()
	if err != nil {
		return TransferNodeSummary{}, err
	}
	client, err := nodeclient.New(row.APIURL, row.PublicKeyFingerprint, identity)
	if err != nil {
		return TransferNodeSummary{}, err
	}
	health, probeErr := client.Health(ctx)
	now := s.now()
	if probeErr != nil {
		row.Status = models.NodeStatusOffline
		row.LastErrorCode = nodeprotocol.ErrorNodeOffline
	} else if health.NodeID != row.ID || health.ProtocolMin > nodeprotocol.VersionV1 || health.ProtocolMax < nodeprotocol.VersionV1 {
		row.Status = models.NodeStatusOffline
		row.LastErrorCode = nodeprotocol.ErrorProtocolIncompatible
	} else {
		row.Status = models.NodeStatusOnline
		row.AgentVersion = health.AgentVersion
		row.Platform = health.OS
		row.Architecture = health.Arch
		row.ProtocolMin = health.ProtocolMin
		row.ProtocolMax = health.ProtocolMax
		raw, _ := json.Marshal(health.Capabilities)
		row.CapabilitiesJSON = string(raw)
		row.FreeBytes = health.Capabilities.ManagedFreeBytes
		row.LastErrorCode = ""
		row.LastHeartbeatAt = &now
	}
	row.Revision++
	row.UpdatedAt = now
	outcome := "success"
	if row.Status != models.NodeStatusOnline {
		outcome = "failure"
	}
	if err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Save(&row).Error; err != nil {
			return err
		}
		return s.audit.Record(tx, &actor.User.ID, "transfer_node.test", "transfer_node", row.ID, outcome, map[string]any{"error_code": row.LastErrorCode}, request)
	}); err != nil {
		return TransferNodeSummary{}, err
	}
	return s.summary(row), nil
}

func (s *TransferNodeService) Revoke(actor Actor, id string, request RequestContext) error {
	if !actor.Can(authz.PermissionTransferNodesRevoke) {
		return appError(CodePermissionDenied, "无权撤销传输节点", nil)
	}
	return s.db.Transaction(func(tx *gorm.DB) error {
		var row models.TransferNode
		if err := tx.First(&row, "id = ?", id).Error; err != nil {
			return transferNodeNotFound(err)
		}
		row.Status = models.NodeStatusRevoked
		row.RevocationEpoch++
		row.Revision++
		row.UpdatedAt = s.now()
		if err := tx.Save(&row).Error; err != nil {
			return err
		}
		now := s.now()
		if err := tx.Model(&models.NodeEnrollment{}).Where("node_id = ? AND consumed_at IS NULL", id).Updates(map[string]any{"consumed_at": now, "token_ciphertext": ""}).Error; err != nil {
			return err
		}
		if err := tx.Model(&models.NodeCredentialGrant{}).Where("node_id = ? AND revoked_at IS NULL", id).Update("revoked_at", now).Error; err != nil {
			return err
		}
		if err := tx.Model(&models.TransferNodeSettings{}).Where("id = ? AND default_node_id = ?", 1, id).Updates(map[string]any{"default_node_id": nil, "revision": gorm.Expr("revision + 1"), "updated_at": now}).Error; err != nil {
			return err
		}
		return s.audit.Record(tx, &actor.User.ID, "transfer_node.revoke", "transfer_node", id, "success", nil, request)
	})
}

func (s *TransferNodeService) Update(actor Actor, id string, input UpdateTransferNodeInput, request RequestContext) (TransferNodeSummary, error) {
	if !actor.Can(authz.PermissionTransferNodesUpdate) {
		return TransferNodeSummary{}, appError(CodePermissionDenied, "无权编辑传输节点", nil)
	}
	if input.Revision == 0 {
		return TransferNodeSummary{}, appError(CodeInvalidRequest, "节点版本无效，请刷新后重试", nil)
	}
	var updated models.TransferNode
	err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.First(&updated, "id = ?", id).Error; err != nil {
			return transferNodeNotFound(err)
		}
		if updated.Revision != input.Revision {
			return appError(CodeConflict, "节点配置已变化，请刷新后重试", nil)
		}
		if updated.Status == models.NodeStatusRevoked {
			return appError(CodeConflict, "已撤销的节点不能编辑", nil)
		}
		if input.Name != nil {
			name := strings.Join(strings.Fields(*input.Name), " ")
			if name == "" || len([]rune(name)) > 128 {
				return appError(CodeInvalidRequest, "节点名称无效", nil)
			}
			updated.Name = name
			updated.NameNormalized = strings.ToLower(name)
		}
		if input.APIURL != nil {
			value, err := normalizeNodeURL(*input.APIURL)
			if err != nil {
				return err
			}
			updated.APIURL = value
			if updated.PublicKeyFingerprint != "" {
				updated.Status = models.NodeStatusOffline
				updated.LastErrorCode = nodeprotocol.ErrorNodeOffline
			}
		}
		if input.Enabled != nil {
			if !*input.Enabled {
				updated.Status = models.NodeStatusDisabled
			} else if updated.PublicKeyFingerprint == "" {
				updated.Status = models.NodeStatusPending
			} else {
				updated.Status = models.NodeStatusOffline
			}
		}
		updated.Revision++
		updated.UpdatedAt = s.now()
		if err := tx.Save(&updated).Error; err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "unique") {
				return appError(CodeConflict, "传输节点名称已存在", nil)
			}
			return err
		}
		if updated.Status != models.NodeStatusOnline {
			if err := tx.Model(&models.TransferNodeSettings{}).Where("id = ? AND default_node_id = ?", 1, id).Updates(map[string]any{"default_node_id": nil, "revision": gorm.Expr("revision + 1"), "updated_at": updated.UpdatedAt}).Error; err != nil {
				return err
			}
		}
		return s.audit.Record(tx, &actor.User.ID, "transfer_node.update", "transfer_node", id, "success", map[string]any{"status": updated.Status}, request)
	})
	if err != nil {
		return TransferNodeSummary{}, err
	}
	return s.summary(updated), nil
}

func (s *TransferNodeService) RegenerateEnrollment(actor Actor, id string, request RequestContext) (string, time.Time, error) {
	if !actor.Can(authz.PermissionTransferNodesEnroll) {
		return "", time.Time{}, appError(CodePermissionDenied, "无权重新生成安装令牌", nil)
	}
	var token string
	var enrollment models.NodeEnrollment
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var row models.TransferNode
		if err := tx.First(&row, "id = ?", id).Error; err != nil {
			return transferNodeNotFound(err)
		}
		if row.Status == models.NodeStatusRevoked || row.PublicKeyFingerprint != "" {
			return appError(CodeConflict, "已配对或已撤销的节点不能重新生成安装令牌", nil)
		}
		var err error
		token, enrollment, err = s.newEnrollment(row.ID, row.Platform, row.Architecture)
		if err != nil {
			return err
		}
		now := s.now()
		if err := tx.Model(&models.NodeEnrollment{}).Where("node_id = ? AND consumed_at IS NULL", id).Updates(map[string]any{"consumed_at": now, "token_ciphertext": ""}).Error; err != nil {
			return err
		}
		if err := tx.Create(&enrollment).Error; err != nil {
			return err
		}
		return s.audit.Record(tx, &actor.User.ID, "transfer_node.enrollment_regenerate", "transfer_node", id, "success", map[string]any{"expires_at": enrollment.ExpiresAt}, request)
	})
	if err != nil {
		return "", time.Time{}, err
	}
	return token, enrollment.ExpiresAt, nil
}

func (s *TransferNodeService) Settings(actor Actor) (TransferNodeSettingsSummary, error) {
	if !actor.Can(authz.PermissionTransferNodesRead) {
		return TransferNodeSettingsSummary{}, appError(CodePermissionDenied, "无权查看传输节点设置", nil)
	}
	row, err := s.settings()
	if err != nil {
		return TransferNodeSettingsSummary{}, err
	}
	return TransferNodeSettingsSummary{DefaultNodeID: row.DefaultNodeID, Revision: row.Revision}, nil
}

func (s *TransferNodeService) UpdateSettings(actor Actor, defaultNodeID *string, revision uint64, request RequestContext) (TransferNodeSettingsSummary, error) {
	if !actor.Can(authz.PermissionTransferNodesUpdate) {
		return TransferNodeSettingsSummary{}, appError(CodePermissionDenied, "无权编辑传输节点设置", nil)
	}
	if revision == 0 {
		return TransferNodeSettingsSummary{}, appError(CodeInvalidRequest, "节点设置版本无效，请刷新后重试", nil)
	}
	if defaultNodeID != nil {
		value := strings.TrimSpace(*defaultNodeID)
		if value == "" {
			defaultNodeID = nil
		} else {
			defaultNodeID = &value
		}
	}
	var settings models.TransferNodeSettings
	err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.First(&settings, 1).Error; err != nil {
			return err
		}
		if settings.Revision != revision {
			return appError(CodeConflict, "节点设置已变化，请刷新后重试", nil)
		}
		if defaultNodeID != nil {
			var node models.TransferNode
			if err := tx.First(&node, "id = ?", *defaultNodeID).Error; err != nil {
				return transferNodeNotFound(err)
			}
			if node.Status != models.NodeStatusOnline {
				return appError(CodeConflict, "只能把在线且已通过测试的节点设为默认节点", nil)
			}
		}
		now := s.now()
		result := tx.Model(&models.TransferNodeSettings{}).Where("id = ? AND revision = ?", 1, revision).Updates(map[string]any{"default_node_id": defaultNodeID, "revision": revision + 1, "updated_at": now})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return appError(CodeConflict, "节点设置已变化，请刷新后重试", nil)
		}
		settings.DefaultNodeID = defaultNodeID
		settings.Revision = revision + 1
		settings.UpdatedAt = now
		return s.audit.Record(tx, &actor.User.ID, "transfer_node.settings_update", "transfer_node_settings", "1", "success", map[string]any{"default_configured": defaultNodeID != nil}, request)
	})
	if err != nil {
		return TransferNodeSettingsSummary{}, err
	}
	return TransferNodeSettingsSummary{DefaultNodeID: settings.DefaultNodeID, Revision: settings.Revision}, nil
}

func (s *TransferNodeService) Delete(actor Actor, id string, request RequestContext) error {
	if !actor.Can(authz.PermissionTransferNodesDelete) {
		return appError(CodePermissionDenied, "无权删除传输节点", nil)
	}
	return s.db.Transaction(func(tx *gorm.DB) error {
		var row models.TransferNode
		if err := tx.First(&row, "id = ?", id).Error; err != nil {
			return transferNodeNotFound(err)
		}
		if row.Status != models.NodeStatusRevoked {
			return appError(CodeConflict, "请先撤销节点，再删除节点配置", nil)
		}
		for _, table := range []string{"node_downloader_bindings", "remote_operations", "node_credential_grants", "download_tasks", "transfer_tasks", "seeding_tasks"} {
			var count int64
			if err := tx.Table(table).Where("node_id = ?", id).Count(&count).Error; err != nil {
				return err
			}
			if count != 0 {
				return appError(CodeConflict, "节点仍有关联的下载器或任务，不能删除恢复事实", nil)
			}
		}
		if err := tx.Where("node_id = ?", id).Delete(&models.NodeEnrollment{}).Error; err != nil {
			return err
		}
		if err := tx.Delete(&row).Error; err != nil {
			return err
		}
		return s.audit.Record(tx, &actor.User.ID, "transfer_node.delete", "transfer_node", id, "success", nil, request)
	})
}

func (s *TransferNodeService) settings() (models.TransferNodeSettings, error) {
	var row models.TransferNodeSettings
	err := s.db.First(&row, 1).Error
	return row, err
}

func (s *TransferNodeService) summary(row models.TransferNode) TransferNodeSummary {
	var caps nodeprotocol.Capabilities
	_ = json.Unmarshal([]byte(row.CapabilitiesJSON), &caps)
	return TransferNodeSummary{ID: row.ID, Name: row.Name, APIURL: row.APIURL, Status: row.Status, Platform: row.Platform, Architecture: row.Architecture, ProtocolMin: row.ProtocolMin, ProtocolMax: row.ProtocolMax, AgentVersion: row.AgentVersion, Capabilities: caps, FreeBytes: row.FreeBytes, FreeBytesKnown: caps.ManagedFreeBytesKnown, LastErrorCode: row.LastErrorCode, LastHeartbeatAt: row.LastHeartbeatAt, Revision: row.Revision, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
}
func normalizeNodeURL(raw string) (string, error) {
	value, err := nodeclient.NormalizePublicURL(raw)
	if err != nil {
		return "", appError(CodeInvalidRequest, "公网节点地址必须是 HTTPS origin", err)
	}
	return value, nil
}

func normalizeNodePlatform(platform, architecture string) (string, string, error) {
	platform = strings.ToLower(strings.TrimSpace(platform))
	architecture = strings.ToLower(strings.TrimSpace(architecture))
	valid := platform == "linux" && (architecture == "amd64" || architecture == "arm64") || platform == "windows" && architecture == "amd64"
	if !valid {
		return "", "", appError(CodeInvalidRequest, "首版节点仅支持 Linux amd64/arm64 和 Windows amd64", nil)
	}
	return platform, architecture, nil
}

func randomEnrollmentToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func (s *TransferNodeService) newEnrollment(nodeID, platform, architecture string) (string, models.NodeEnrollment, error) {
	token, err := randomEnrollmentToken()
	if err != nil {
		return "", models.NodeEnrollment{}, err
	}
	hash := sha256.Sum256([]byte(token))
	id := uuid.NewString()
	ciphertext, err := s.credentials.Encrypt("transfer-node-enrollment:"+id, token)
	if err != nil {
		return "", models.NodeEnrollment{}, err
	}
	now := s.now()
	return token, models.NodeEnrollment{
		ID:              id,
		NodeID:          nodeID,
		TokenHash:       fmt.Sprintf("%x", hash[:]),
		TokenCiphertext: ciphertext,
		Platform:        platform,
		Architecture:    architecture,
		ExpiresAt:       now.Add(10 * time.Minute),
		CreatedAt:       now,
	}, nil
}

func transferNodeNotFound(err error) error {
	if err == gorm.ErrRecordNotFound {
		return appError(CodeNotFound, "传输节点不存在", err)
	}
	return err
}

func (s *TransferNodeService) controllerIdentity() (nodeclient.Identity, error) {
	var row models.NodeControllerIdentity
	err := s.db.First(&row, 1).Error
	if err == nil {
		cert, err := s.credentials.Decrypt("node-controller:certificate", row.CertificateCiphertext)
		if err != nil {
			return nodeclient.Identity{}, err
		}
		key, err := s.credentials.Decrypt("node-controller:private-key", row.PrivateKeyCiphertext)
		if err != nil {
			return nodeclient.Identity{}, err
		}
		return nodeclient.ParseIdentity(row.ServerID, cert, key)
	}
	if err != gorm.ErrRecordNotFound {
		return nodeclient.Identity{}, err
	}
	now := s.now()
	identity, err := nodeclient.GenerateIdentity(uuid.NewString(), now)
	if err != nil {
		return nodeclient.Identity{}, err
	}
	certCipher, err := s.credentials.Encrypt("node-controller:certificate", identity.CertificatePEM)
	if err != nil {
		return nodeclient.Identity{}, err
	}
	keyCipher, err := s.credentials.Encrypt("node-controller:private-key", identity.PrivateKeyPEM)
	if err != nil {
		return nodeclient.Identity{}, err
	}
	row = models.NodeControllerIdentity{ID: 1, ServerID: identity.ServerID, CertificateCiphertext: certCipher, PrivateKeyCiphertext: keyCipher, CertificateFingerprint: identity.Fingerprint, CreatedAt: now, ExpiresAt: identity.ExpiresAt}
	if err := s.db.Create(&row).Error; err != nil {
		return nodeclient.Identity{}, err
	}
	return identity, nil
}
