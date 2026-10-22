package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
)

// configuredShareReceiver checks saved receiving configuration only. Search and
// summaries must not resume authorization, probe a provider, or turn transient
// health/cooldown into a configuration change. Preview/submit still perform
// their current-operation and target checks after this structural gate.
// The returned private digest excludes health and rotating access credentials.
func (s *DownloaderService) configuredShareReceiver(ctx context.Context, receiver models.Downloader) (string, error) {
	if !receiver.Enabled || !isCloudShareDownloader(receiver.Type) || receiver.StorageID == nil || strings.TrimSpace(receiver.ProviderDirectoryID) == "" || s.connections == nil {
		return "", appError(CodeDownloaderStorageUnavailable, "请先配置并启用网盘分享接收目录", nil)
	}
	var storage models.Storage
	if err := s.db.WithContext(ctx).First(&storage, *receiver.StorageID).Error; err != nil || !storage.Enabled || storage.Type != shareStorageType(receiver.Type) || storage.ConnectionID == nil || strings.TrimSpace(storage.RootPath) == "" {
		return "", appError(CodeDownloaderStorageUnavailable, "网盘分享接收目录的数据源不可用", err)
	}
	connection, driver, err := s.connections.cachedDriver(*storage.ConnectionID)
	if err != nil || connection.Provider != storage.Type || connection.CredentialCiphertext == "" {
		return "", appError(CodeConnectionUnavailable, "网盘分享接收器的连接尚未配置完成或已停用", err)
	}
	capabilities := driver.Capabilities()
	if !capabilities.ShareReceive || !capabilities.CreateDirectory {
		return "", appError(CodeDownloaderUnavailable, "网盘连接尚未完成分享接收所需的授权与目录验证", nil)
	}
	var proof *quark.SpaceProof
	if connection.Provider == models.ConnectionProviderQuark {
		_, bundle, err := s.connections.quarkCredentials(ctx, connection.ID)
		if err != nil {
			return "", err
		}
		for _, candidate := range bundle.Proofs {
			if candidate.TVRoot == storage.RootPath && bundle.HasProof(candidate.TVRoot, candidate.AgentRoot) {
				copy := candidate
				proof = &copy
				break
			}
		}
		if proof == nil {
			return "", appError(quark.CodeProof, "夸克分享接收目录尚未完成播放与管理授权的共同目录验证", nil)
		}
	}
	var nodeScope any
	if normalizeExecutionLocation(receiver.ExecutionLocation) == models.NodeLocationRemote {
		if receiver.NodeID == nil || s.transferNodes == nil {
			return "", appError(CodeDownloaderUnavailable, "网盘接收器的传输节点绑定不完整", nil)
		}
		var node models.TransferNode
		if err := s.db.WithContext(ctx).First(&node, "id = ?", *receiver.NodeID).Error; err != nil || node.Status != models.NodeStatusOnline && node.Status != models.NodeStatusOffline || !nodeprotocol.ValidDigest(node.PublicKeyFingerprint) || strings.TrimSpace(node.EncryptionPublicKey) == "" {
			return "", appError(CodeDownloaderUnavailable, "网盘接收器的传输节点尚未配置完成或已停用", err)
		}
		var capabilities nodeprotocol.Capabilities
		if json.Unmarshal([]byte(node.CapabilitiesJSON), &capabilities) != nil || receiver.Type == models.DownloaderTypeQuarkShare && !nodeprotocol.QuarkShareSupported(capabilities) || receiver.Type == models.DownloaderTypePan115Offline && (!capabilities.Has(nodeprotocol.CapabilityPan115Offline) || !capabilities.Has(nodeprotocol.CapabilityPan115ShareReceive) || !capabilities.Has(nodeprotocol.CapabilityPan115Read) || !capabilities.Has(nodeprotocol.CapabilityRangeExport)) {
			return "", appError(CodeDownloaderUnavailable, "传输节点缺少该网盘分享接收所需的能力，请升级节点", nil)
		}
		nodeScope = []any{node.ID, node.APIURL, node.PublicKeyFingerprint, node.EncryptionPublicKey}
	}
	raw, err := json.Marshal([]any{receiver.ID, receiver.Type, receiver.OwnerID, normalizeExecutionLocation(receiver.ExecutionLocation), receiver.NodeID, receiver.ProviderDirectoryID, receiver.ProviderDirectoryPath, storage.ID, storage.RootPath, storage.RootDisplayPath, connection.ID, connection.Revision, connection.AccountID, proof, nodeScope})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}
