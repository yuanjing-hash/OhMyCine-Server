package services

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/nodeclient"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
	"gorm.io/gorm"
)

func normalizeExecutionLocation(value string) string {
	if strings.EqualFold(strings.TrimSpace(value), models.NodeLocationRemote) {
		return models.NodeLocationRemote
	}
	return models.NodeLocationServer
}

func (s *DownloaderService) prepareNodeBinding(downloaderID, nodeID, baseURL, username, password, downloaderSaveRoot, nodeMountRoot string) (models.NodeDownloaderBinding, error) {
	nodeID = strings.TrimSpace(nodeID)
	if nodeID == "" || s.transferNodes == nil {
		return models.NodeDownloaderBinding{}, appError(CodeInvalidRequest, "请选择传输节点", nil)
	}
	_, node, err := s.transferNodes.nodeClient(nodeID)
	if err != nil {
		return models.NodeDownloaderBinding{}, err
	}
	var capabilities nodeprotocol.Capabilities
	if json.Unmarshal([]byte(node.CapabilitiesJSON), &capabilities) != nil || !capabilities.Has(nodeprotocol.CapabilityQBittorrentControl) {
		return models.NodeDownloaderBinding{}, appError(CodeDownloaderUnavailable, "所选节点不支持 qBittorrent", nil)
	}
	baseURL, _, err = s.validateDownloaderConfig(models.DownloaderTypeQBittorrent, baseURL, username, password, nil)
	if err != nil {
		return models.NodeDownloaderBinding{}, err
	}
	downloaderSaveRoot = strings.TrimSpace(downloaderSaveRoot)
	nodeMountRoot = strings.TrimSpace(nodeMountRoot)
	if !validRemoteAbsolutePath(downloaderSaveRoot, node.Platform) || !validRemoteAbsolutePath(nodeMountRoot, node.Platform) {
		return models.NodeDownloaderBinding{}, appError(CodeInvalidRequest, "请填写节点平台上的绝对下载根目录和挂载根目录", nil)
	}
	usernameCiphertext, err := s.credentials.Encrypt(nodeDownloaderPurpose(downloaderID, "username"), username)
	if err != nil {
		return models.NodeDownloaderBinding{}, err
	}
	passwordCiphertext, err := s.credentials.Encrypt(nodeDownloaderPurpose(downloaderID, "password"), password)
	if err != nil {
		return models.NodeDownloaderBinding{}, err
	}
	now := time.Now().UTC()
	return models.NodeDownloaderBinding{ID: uuid.NewString(), DownloaderID: downloaderID, NodeID: node.ID, BaseURL: baseURL, UsernameCiphertext: usernameCiphertext, PasswordCiphertext: passwordCiphertext, DownloaderSaveRoot: downloaderSaveRoot, NodeMountRoot: nodeMountRoot, Revision: 1, CreatedAt: now, UpdatedAt: now}, nil
}

func (s *DownloaderService) prepareNodeStorageDownloader(nodeID string) (models.TransferNode, error) {
	nodeID = strings.TrimSpace(nodeID)
	if nodeID == "" || s.transferNodes == nil {
		return models.TransferNode{}, appError(CodeInvalidRequest, "请选择传输节点", nil)
	}
	_, node, err := s.transferNodes.nodeClient(nodeID)
	if err != nil {
		return models.TransferNode{}, err
	}
	var capabilities nodeprotocol.Capabilities
	if json.Unmarshal([]byte(node.CapabilitiesJSON), &capabilities) != nil ||
		!capabilities.Has(nodeprotocol.CapabilityPan115Offline) ||
		!capabilities.Has(nodeprotocol.CapabilityPan115ShareReceive) ||
		!capabilities.Has(nodeprotocol.CapabilityPan115Read) ||
		!capabilities.Has(nodeprotocol.CapabilityRangeExport) {
		return models.TransferNode{}, appError(CodeDownloaderUnavailable, "所选节点不支持 115 离线下载与受控文件导出", nil)
	}
	return node, nil
}

func validRemoteAbsolutePath(value, platform string) bool {
	if value == "" || len(value) > 1024 || strings.ContainsAny(value, "\x00\r\n") {
		return false
	}
	if platform == "windows" {
		return len(value) >= 3 && ((value[0] >= 'A' && value[0] <= 'Z') || (value[0] >= 'a' && value[0] <= 'z')) && value[1] == ':' && (value[2] == '\\' || value[2] == '/') || strings.HasPrefix(value, `\\`)
	}
	return strings.HasPrefix(value, "/")
}

func nodeDownloaderPurpose(id, field string) string { return "node-downloader:" + id + ":" + field }

func (s *DownloaderService) nodeDownloaderClient(record models.Downloader, localRoot, taskID string) (*nodeclient.DownloaderClient, error) {
	if record.NodeID == nil || s.transferNodes == nil {
		return nil, appError(CodeDownloaderUnavailable, "下载器绑定的传输节点不可用", nil)
	}
	binding, err := nodeDownloaderBinding(s.db, record)
	if err != nil {
		return nil, appError(CodeDownloaderUnavailable, "下载器缺少节点绑定配置", err)
	}
	return s.nodeDownloaderClientWithBinding(record, binding, localRoot, taskID)
}

func (s *DownloaderService) nodeDownloaderClientWithBinding(record models.Downloader, binding models.NodeDownloaderBinding, localRoot, taskID string) (*nodeclient.DownloaderClient, error) {
	if record.NodeID == nil || s.transferNodes == nil || binding.NodeID != *record.NodeID || binding.DownloaderID != record.ID {
		return nil, appError(CodeDownloaderUnavailable, "下载器绑定的传输节点不可用", nil)
	}
	client, node, err := s.transferNodes.nodeClient(binding.NodeID)
	if err != nil {
		return nil, err
	}
	username, err := s.credentials.Decrypt(nodeDownloaderPurpose(record.ID, "username"), binding.UsernameCiphertext)
	if err != nil {
		return nil, err
	}
	password, err := s.credentials.Decrypt(nodeDownloaderPurpose(record.ID, "password"), binding.PasswordCiphertext)
	if err != nil {
		return nil, err
	}
	credential := nodeprotocol.DownloaderCredential{ProviderType: record.Type, BaseURL: binding.BaseURL, Username: username, Password: password, DownloaderSaveRoot: binding.DownloaderSaveRoot, NodeMountRoot: binding.NodeMountRoot}
	return nodeclient.NewDownloaderClient(client, record.ID, binding.DownloaderSaveRoot, localRoot, taskID, func(ctx context.Context, taskID, operationKey, action string) (nodeprotocol.CredentialGrantEnvelope, error) {
		return s.transferNodes.sealDownloaderGrant(ctx, node, binding, taskID, operationKey, action, credential)
	})
}

func (s *DownloaderService) clientForTask(ctx context.Context, record models.Downloader, task models.DownloadTask, localRoot string) (downloadpkg.Client, error) {
	taskLocation := normalizeExecutionLocation(task.ExecutionLocation)
	if taskLocation != normalizeExecutionLocation(record.ExecutionLocation) {
		return nil, appError(CodeDownloaderUnavailable, "下载任务冻结的运行位置与下载器当前配置不一致", nil)
	}
	if taskLocation == models.NodeLocationRemote {
		if task.NodeID == nil || record.NodeID == nil || *task.NodeID != *record.NodeID {
			return nil, appError(CodeDownloaderUnavailable, "下载任务冻结的传输节点与下载器当前绑定不一致", nil)
		}
		if task.ProtocolVersion != nodeprotocol.VersionV1 || task.RoutePlanRevision == 0 || !nodeprotocol.ValidDigest(task.RoutePlanDigest) {
			return nil, appError(CodeDownloaderUnavailable, "下载任务缺少有效的远端路线快照", nil)
		}
		binding, revision, digest, err := nodeTaskRoutePlan(s.db, record, localRoot, requestedTargetID(task.TargetLibraryID))
		if err != nil {
			return nil, err
		}
		if revision != task.RoutePlanRevision || digest != task.RoutePlanDigest {
			return nil, appError(CodeDownloaderUnavailable, "下载器的传输节点或路径映射已变化，已阻止任务漂移", nil)
		}
		if record.Type == models.DownloaderTypePan115Offline {
			return s.nodeStorageSourceClient(ctx, record, task)
		}
		return s.nodeDownloaderClientWithBinding(record, binding, localRoot, task.ID)
	}
	return s.clientFor(record)
}

func (s *DownloaderService) remoteFileReaderForTask(ctx context.Context, task models.DownloadTask) (downloadpkg.RemoteFileReader, error) {
	if normalizeExecutionLocation(task.ExecutionLocation) != models.NodeLocationRemote || task.DownloaderID == nil {
		return nil, appError(CodeDownloaderUnavailable, "下载任务没有冻结远端文件读取器", nil)
	}
	var record models.Downloader
	if err := s.db.First(&record, "id = ?", *task.DownloaderID).Error; err != nil {
		return nil, err
	}
	client, err := s.clientForTask(ctx, record, task, task.StagingAbsolutePath)
	if err != nil {
		return nil, err
	}
	reader, ok := client.(downloadpkg.RemoteFileReader)
	if !ok {
		return nil, appError(CodeDownloaderUnavailable, "远端下载器不支持受控文件读取", nil)
	}
	return reader, nil
}

func (s *DownloaderService) clientForDownloadTask(ctx context.Context, task models.DownloadTask) (downloadpkg.Client, error) {
	if task.DownloaderID == nil {
		return nil, appError(CodeDownloaderUnavailable, "原下载器配置已不存在", nil)
	}
	var record models.Downloader
	if err := s.db.First(&record, "id = ?", *task.DownloaderID).Error; err != nil {
		return nil, err
	}
	return s.clientForTask(ctx, record, task, task.StagingAbsolutePath)
}

func (s *DownloaderService) refreshRemoteFileExport(ctx context.Context, task models.DownloadTask) (downloadpkg.Manifest, error) {
	client, err := s.clientForDownloadTask(ctx, task)
	if err != nil {
		return downloadpkg.Manifest{}, err
	}
	manifestClient, ok := client.(downloadpkg.ManifestClient)
	if !ok || strings.TrimSpace(task.ProviderTaskID) == "" {
		return downloadpkg.Manifest{}, appError(CodeDownloaderUnavailable, "远端下载器不支持续期文件导出", nil)
	}
	return manifestClient.Manifest(ctx, task.ProviderTaskID)
}

func nodeDownloaderBinding(db *gorm.DB, record models.Downloader) (models.NodeDownloaderBinding, error) {
	if db == nil || record.NodeID == nil {
		return models.NodeDownloaderBinding{}, appError(CodeDownloaderUnavailable, "下载器绑定的传输节点不可用", nil)
	}
	var binding models.NodeDownloaderBinding
	if err := db.First(&binding, "downloader_id = ? AND node_id = ?", record.ID, *record.NodeID).Error; err != nil {
		return models.NodeDownloaderBinding{}, appError(CodeDownloaderUnavailable, "下载器缺少节点绑定配置", err)
	}
	return binding, nil
}

func nodeTaskRoutePlan(db *gorm.DB, record models.Downloader, localRoot string, targetLibraryID uint) (models.NodeDownloaderBinding, uint64, string, error) {
	if record.Type == models.DownloaderTypePan115Offline {
		if db == nil || record.NodeID == nil || record.StorageID == nil || strings.TrimSpace(record.ProviderDirectoryID) == "" || targetLibraryID == 0 {
			return models.NodeDownloaderBinding{}, 0, "", appError(CodeDownloaderUnavailable, "节点 115 下载器的远端路线快照不完整", nil)
		}
		var storage models.Storage
		if err := db.Select("id", "type", "connection_id", "root_path").First(&storage, *record.StorageID).Error; err != nil || storage.Type != models.StorageTypePan115 || storage.ConnectionID == nil {
			return models.NodeDownloaderBinding{}, 0, "", appError(CodeDownloaderStorageUnavailable, "节点 115 下载器的来源 Storage 不可用", err)
		}
		const routePlanRevision = 1
		payload := fmt.Sprintf("remote-storage-source-v1\x00%s\x00%s\x00%d\x00%d\x00%d\x00%s\x00%s", *record.NodeID, record.ID, targetLibraryID, storage.ID, *storage.ConnectionID, storage.RootPath, record.ProviderDirectoryID)
		digest := sha256.Sum256([]byte(payload))
		return models.NodeDownloaderBinding{}, routePlanRevision, fmt.Sprintf("%x", digest[:]), nil
	}
	binding, err := nodeDownloaderBinding(db, record)
	if err != nil {
		return models.NodeDownloaderBinding{}, 0, "", err
	}
	if strings.TrimSpace(localRoot) == "" {
		return models.NodeDownloaderBinding{}, 0, "", appError(CodeDownloaderUnavailable, "下载器的远端路径快照不完整", nil)
	}
	const routePlanRevision = 1
	payload := fmt.Sprintf("remote-route-v1\x00%s\x00%s\x00%d\x00%s\x00%s\x00%s\x00%s", binding.NodeID, record.ID, targetLibraryID, binding.ID, localRoot, binding.DownloaderSaveRoot, binding.NodeMountRoot)
	digest := sha256.Sum256([]byte(payload))
	return binding, routePlanRevision, fmt.Sprintf("%x", digest[:]), nil
}

func (s *DownloaderService) nodeBindingSummary(record models.Downloader) (models.NodeDownloaderBinding, bool) {
	if record.ExecutionLocation != models.NodeLocationRemote {
		return models.NodeDownloaderBinding{}, false
	}
	var binding models.NodeDownloaderBinding
	if s.db.First(&binding, "downloader_id = ?", record.ID).Error != nil {
		return models.NodeDownloaderBinding{}, false
	}
	return binding, true
}

func (s *DownloaderService) saveNodeBindingUpdate(tx *gorm.DB, binding *models.NodeDownloaderBinding) error {
	if binding == nil {
		return nil
	}
	return tx.Save(binding).Error
}

func (s *DownloaderService) updateNodeDownloader(ctx context.Context, actor Actor, record models.Downloader, input UpdateDownloaderInput, request RequestContext) (DownloaderSummary, error) {
	if record.Type == models.DownloaderTypePan115Offline {
		return s.updateNodeStorageDownloader(ctx, actor, record, input, request)
	}
	var existing models.NodeDownloaderBinding
	if err := s.db.WithContext(ctx).First(&existing, "downloader_id = ?", record.ID).Error; err != nil {
		return DownloaderSummary{}, appError(CodeDownloaderUnavailable, "下载器缺少节点绑定配置", err)
	}
	username, err := s.credentials.Decrypt(nodeDownloaderPurpose(record.ID, "username"), existing.UsernameCiphertext)
	if err != nil {
		return DownloaderSummary{}, err
	}
	password, err := s.credentials.Decrypt(nodeDownloaderPurpose(record.ID, "password"), existing.PasswordCiphertext)
	if err != nil {
		return DownloaderSummary{}, err
	}
	if input.ClearUsername {
		username = ""
	} else if input.Username != nil && *input.Username != "" {
		username = *input.Username
	}
	if input.ClearPassword {
		password = ""
	} else if input.Password != nil && *input.Password != "" {
		password = *input.Password
	}
	baseURL, nodeID := existing.BaseURL, existing.NodeID
	saveRoot, mountRoot := existing.DownloaderSaveRoot, existing.NodeMountRoot
	if input.BaseURL != nil {
		baseURL = *input.BaseURL
	}
	if input.NodeID != nil {
		nodeID = *input.NodeID
	}
	if input.DownloaderSaveRoot != nil {
		saveRoot = *input.DownloaderSaveRoot
	}
	if input.NodeMountRoot != nil {
		mountRoot = *input.NodeMountRoot
	}
	if nodeDownloaderRouteChanged(existing, nodeID, saveRoot, mountRoot) {
		var taskCount int64
		if err := s.db.WithContext(ctx).Model(&models.DownloadTask{}).Where("downloader_id = ?", record.ID).Count(&taskCount).Error; err != nil {
			return DownloaderSummary{}, err
		}
		if taskCount > 0 {
			return DownloaderSummary{}, appError(CodeConflict, "该下载器已有任务，不能更换传输节点或路径映射；请新建下载器配置", nil)
		}
	}
	prepared, err := s.prepareNodeBinding(record.ID, nodeID, baseURL, username, password, saveRoot, mountRoot)
	if err != nil {
		return DownloaderSummary{}, err
	}
	prepared.ID = existing.ID
	prepared.CreatedAt = existing.CreatedAt
	prepared.Revision = existing.Revision + 1
	prepared.LastTestCode = ""
	prepared.LastTestedAt = nil
	record.ExecutionLocation = models.NodeLocationRemote
	record.NodeID = &prepared.NodeID
	record.BaseURL = ""
	record.UsernameCiphertext = ""
	record.PasswordCiphertext = ""
	record.LastHealthStatus, record.LastHealthErrorCode, record.LastHealthVersion, record.LastHealthCheckedAt = "unknown", "", "", nil
	record.UpdatedAt = time.Now().UTC()
	var node models.TransferNode
	if s.db.WithContext(ctx).Select("name").First(&node, "id = ?", prepared.NodeID).Error == nil {
		record.NodeName = node.Name
	}
	routeChanged := nodeDownloaderRouteChanged(existing, nodeID, saveRoot, mountRoot)
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if routeChanged {
			var taskCount int64
			if err := tx.Model(&models.DownloadTask{}).Where("downloader_id = ?", record.ID).Count(&taskCount).Error; err != nil {
				return err
			}
			if taskCount > 0 {
				return appError(CodeConflict, "该下载器已有任务，不能更换传输节点或路径映射；请新建下载器配置", nil)
			}
		}
		if err := tx.Save(&record).Error; err != nil {
			return err
		}
		if err := s.saveNodeBindingUpdate(tx, &prepared); err != nil {
			return err
		}
		return s.audit.Record(tx, &actor.User.ID, "downloader.update", "downloader", record.ID, "success", map[string]any{"type": record.Type, "enabled": record.Enabled, "execution_location": record.ExecutionLocation, "node_id": prepared.NodeID}, request)
	})
	if err != nil {
		if conflict := downloaderConstraintError(err); conflict != nil {
			return DownloaderSummary{}, conflict
		}
		return DownloaderSummary{}, err
	}
	return s.summary(record), nil
}

func (s *DownloaderService) updateNodeStorageDownloader(ctx context.Context, actor Actor, record models.Downloader, input UpdateDownloaderInput, request RequestContext) (DownloaderSummary, error) {
	if record.Type != models.DownloaderTypePan115Offline || record.NodeID == nil {
		return DownloaderSummary{}, appError(CodeDownloaderUnavailable, "节点 115 下载器配置不完整", nil)
	}
	nodeID := *record.NodeID
	if input.NodeID != nil {
		nodeID = strings.TrimSpace(*input.NodeID)
	}
	if input.AutoListenLifeEvents != nil && *input.AutoListenLifeEvents {
		return DownloaderSummary{}, appError(CodeInvalidRequest, "节点 115 下载器暂不承接生活事件自动摄取", nil)
	}
	storageID := record.StorageID
	providerDirectoryID := record.ProviderDirectoryID
	providerDirectoryPath := record.ProviderDirectoryPath
	if input.StorageID != nil || input.ProviderDirectoryToken != nil {
		if input.StorageID == nil || input.ProviderDirectoryToken == nil {
			return DownloaderSummary{}, appError(CodeDownloaderStorageRequired, "请选择完整的 115 离线下载目录", nil)
		}
		_, selectedStorageID, validateErr := s.validateDownloaderConfig(record.Type, "", "", "", input.StorageID)
		if validateErr != nil {
			return DownloaderSummary{}, validateErr
		}
		selection, resolveErr := s.resolveProviderDirectory(ctx, actor, selectedStorageID, *input.ProviderDirectoryToken)
		if resolveErr != nil {
			return DownloaderSummary{}, resolveErr
		}
		storageID = selectedStorageID
		providerDirectoryID = selection.ProviderID
		providerDirectoryPath = selection.RelativeRoot
	}
	routeChanged := nodeStorageDownloaderRouteChanged(record, nodeID, storageID, providerDirectoryID)
	if err := ensureNodeStorageDownloaderRouteMutable(s.db.WithContext(ctx), record.ID, routeChanged); err != nil {
		return DownloaderSummary{}, err
	}
	node, err := s.prepareNodeStorageDownloader(nodeID)
	if err != nil {
		return DownloaderSummary{}, err
	}
	record.NodeID = &node.ID
	record.NodeName = node.Name
	record.StorageID = storageID
	record.ProviderDirectoryID = providerDirectoryID
	record.ProviderDirectoryPath = providerDirectoryPath
	record.AutoListenLifeEvents = false
	record.BaseURL = ""
	record.UsernameCiphertext = ""
	record.PasswordCiphertext = ""
	record.LastHealthStatus, record.LastHealthErrorCode, record.LastHealthVersion, record.LastHealthCheckedAt = "unknown", "", "", nil
	record.UpdatedAt = time.Now().UTC()
	if err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := ensureNodeStorageDownloaderRouteMutable(tx, record.ID, routeChanged); err != nil {
			return err
		}
		if err := tx.Save(&record).Error; err != nil {
			return err
		}
		return s.audit.Record(tx, &actor.User.ID, "downloader.update", "downloader", record.ID, "success", map[string]any{"type": record.Type, "enabled": record.Enabled, "execution_location": record.ExecutionLocation, "node_id": node.ID}, request)
	}); err != nil {
		if conflict := downloaderConstraintError(err); conflict != nil {
			return DownloaderSummary{}, conflict
		}
		return DownloaderSummary{}, err
	}
	return s.summary(record), nil
}

func nodeStorageDownloaderRouteChanged(record models.Downloader, nodeID string, storageID *uint, providerDirectoryID string) bool {
	if record.NodeID == nil || strings.TrimSpace(nodeID) != strings.TrimSpace(*record.NodeID) || record.StorageID == nil || storageID == nil || *record.StorageID != *storageID {
		return true
	}
	return strings.TrimSpace(providerDirectoryID) != strings.TrimSpace(record.ProviderDirectoryID)
}

func ensureNodeStorageDownloaderRouteMutable(db *gorm.DB, downloaderID string, routeChanged bool) error {
	if !routeChanged {
		return nil
	}
	var taskCount int64
	if err := db.Model(&models.DownloadTask{}).Where("downloader_id = ?", downloaderID).Count(&taskCount).Error; err != nil {
		return err
	}
	if taskCount > 0 {
		return appError(CodeConflict, "该下载器已有任务，不能更换传输节点、来源 Storage 或来源目录；请新建下载器配置", nil)
	}
	return nil
}

func nodeDownloaderRouteChanged(existing models.NodeDownloaderBinding, nodeID, downloaderSaveRoot, nodeMountRoot string) bool {
	return strings.TrimSpace(nodeID) != existing.NodeID ||
		!remoteRootsEqual(downloaderSaveRoot, existing.DownloaderSaveRoot) ||
		!remoteRootsEqual(nodeMountRoot, existing.NodeMountRoot)
}

func remoteRootsEqual(left, right string) bool {
	normalize := func(value string) (string, bool) {
		value = strings.ReplaceAll(strings.TrimSpace(value), `\`, "/")
		windows := len(value) >= 2 && value[1] == ':' || strings.HasPrefix(value, "//")
		value = strings.TrimRight(value, "/")
		if value == "" {
			value = "/"
		}
		return value, windows
	}
	left, leftWindows := normalize(left)
	right, rightWindows := normalize(right)
	if leftWindows != rightWindows {
		return false
	}
	if leftWindows {
		return strings.EqualFold(left, right)
	}
	return left == right
}
