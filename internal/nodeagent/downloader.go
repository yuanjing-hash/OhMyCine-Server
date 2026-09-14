package nodeagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader/qbittorrent"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
)

func (a *Agent) putCredentialGrant(w http.ResponseWriter, r *http.Request) {
	var envelope nodeprotocol.CredentialGrantEnvelope
	if err := decodeJSON(w, r, &envelope); err != nil {
		return
	}
	if envelope.GrantID != r.PathValue("grant_id") || envelope.NodeID != a.config.NodeID {
		writeError(w, http.StatusBadRequest, "node_credential_binding_mismatch", "临时凭据绑定不匹配")
		return
	}
	grant, err := nodeprotocol.OpenCredentialGrant(a.sealingPrivateKey, envelope, a.now())
	if err != nil || grant.NodeID != a.config.NodeID {
		code := nodeprotocol.ErrorCredentialExpired
		if err != nil && err.Error() != nodeprotocol.ErrorCredentialExpired {
			code = "node_credential_envelope_invalid"
		}
		writeError(w, http.StatusBadRequest, code, "临时凭据无效或已经过期")
		return
	}
	serverID := r.Header.Get("X-OhMyCine-Verified-Server-ID")
	if err := a.store.SaveCredentialGrant(r.Context(), serverID, envelope, a.now()); err != nil {
		writeError(w, http.StatusInternalServerError, "node_persistence_failed", "节点无法保存临时凭据")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *Agent) downloaderAction(w http.ResponseWriter, r *http.Request) {
	var input nodeprotocol.DownloaderActionRequest
	if err := decodeJSON(w, r, &input); err != nil {
		return
	}
	if err := input.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "下载器操作无效")
		return
	}
	serverID := r.Header.Get("X-OhMyCine-Verified-Server-ID")
	grant, credential, err := a.downloaderCredential(r.Context(), serverID, input)
	if err != nil {
		writeError(w, http.StatusForbidden, err.Error(), "下载器临时授权无效或已经过期")
		return
	}
	_ = grant

	mutating := input.Action == nodeprotocol.DownloaderActionSubmit || input.Action == nodeprotocol.DownloaderActionPause || input.Action == nodeprotocol.DownloaderActionResume || input.Action == nodeprotocol.DownloaderActionCancel || input.Action == nodeprotocol.DownloaderActionDeleteTag || input.Action == nodeprotocol.DownloaderActionEnsureCategory || input.Action == nodeprotocol.DownloaderActionUpdateCategory || input.Action == nodeprotocol.DownloaderActionSetCategory
	digest := ""
	if mutating {
		digest, err = input.Digest()
		if err != nil {
			writeError(w, http.StatusBadRequest, "node_downloader_request_invalid", "下载器操作校验失败")
			return
		}
		receipt, _, beginErr := a.store.BeginProviderReceipt(r.Context(), serverID, input, digest, a.now())
		if errors.Is(beginErr, ErrPlanConflict) {
			writeError(w, http.StatusConflict, nodeprotocol.ErrorPlanConflict, "相同操作编号对应了不同请求")
			return
		}
		if beginErr != nil {
			writeError(w, http.StatusInternalServerError, "node_persistence_failed", "节点无法保存下载器操作")
			return
		}
		if receipt.Status == "completed" {
			var response nodeprotocol.DownloaderActionResponse
			if json.Unmarshal([]byte(receipt.ResponseJSON), &response) == nil {
				writeJSON(w, http.StatusOK, response)
				return
			}
		}
	}

	response, actionErr := a.executeDownloaderAction(r.Context(), serverID, input, credential)
	if actionErr != nil {
		code, _ := downloadpkg.ErrorInfo(actionErr)
		if mutating {
			_ = a.store.CompleteProviderReceipt(r.Context(), serverID, input.RequestID, nodeprotocol.DownloaderActionResponse{}, code, a.now())
		}
		writeError(w, http.StatusBadGateway, code, downloaderActionMessage(code))
		return
	}
	if mutating {
		if err := a.store.CompleteProviderReceipt(r.Context(), serverID, input.RequestID, response, "", a.now()); err != nil {
			writeError(w, http.StatusInternalServerError, "node_persistence_failed", "节点无法保存下载器执行结果")
			return
		}
	}
	writeJSON(w, http.StatusOK, response)
}

func (a *Agent) downloaderCredential(ctx context.Context, serverID string, input nodeprotocol.DownloaderActionRequest) (nodeprotocol.CredentialGrant, nodeprotocol.DownloaderCredential, error) {
	envelope, err := a.store.CredentialGrant(ctx, serverID, input.GrantID, a.now())
	if err != nil {
		return nodeprotocol.CredentialGrant{}, nodeprotocol.DownloaderCredential{}, errors.New(nodeprotocol.ErrorCredentialExpired)
	}
	grant, err := nodeprotocol.OpenCredentialGrant(a.sealingPrivateKey, envelope, a.now())
	if err != nil {
		return nodeprotocol.CredentialGrant{}, nodeprotocol.DownloaderCredential{}, err
	}
	if grant.NodeID != a.config.NodeID || grant.TaskID != input.TaskID || grant.OperationKey != input.OperationKey || grant.ResourceKind != nodeprotocol.ResourceKindDownloader || grant.ResourceID != input.DownloaderID || !containsAction(grant.AllowedActions, input.Action) {
		return nodeprotocol.CredentialGrant{}, nodeprotocol.DownloaderCredential{}, errors.New("node_credential_binding_mismatch")
	}
	var credential nodeprotocol.DownloaderCredential
	decoder := json.NewDecoder(bytes.NewReader(grant.Credential))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&credential); err != nil || credential.ProviderType != "qbittorrent" {
		return nodeprotocol.CredentialGrant{}, nodeprotocol.DownloaderCredential{}, errors.New("node_credential_payload_invalid")
	}
	if _, err := validateDownloaderRoots(a.config.ManagedRoot, credential); err != nil {
		return nodeprotocol.CredentialGrant{}, nodeprotocol.DownloaderCredential{}, err
	}
	return grant, credential, nil
}

func (a *Agent) executeDownloaderAction(ctx context.Context, serverID string, input nodeprotocol.DownloaderActionRequest, credential nodeprotocol.DownloaderCredential) (nodeprotocol.DownloaderActionResponse, error) {
	client, err := qbittorrent.New(downloadpkg.Config{BaseURL: credential.BaseURL, Username: credential.Username, Password: credential.Password})
	if err != nil {
		return nodeprotocol.DownloaderActionResponse{}, err
	}
	response := nodeprotocol.DownloaderActionResponse{RequestID: input.RequestID}
	switch input.Action {
	case nodeprotocol.DownloaderActionTest:
		health, err := client.Test(ctx)
		if err == nil {
			response.Health = &nodeprotocol.DownloaderHealth{Version: health.Version}
		}
		return response, err
	case nodeprotocol.DownloaderActionSubmit:
		localRoot, err := mapDownloaderPath(credential.DownloaderSaveRoot, credential.NodeMountRoot, input.Submit.SavePath)
		if err != nil {
			return response, downloadpkg.Error(nodeprotocol.ErrorPathMappingInvalid, false, err)
		}
		task, err := client.Submit(ctx, downloadpkg.SubmitRequest{Source: downloadpkg.Source{Kind: input.Submit.Source.Kind, URL: input.Submit.Source.URL, Torrent: input.Submit.Source.Torrent, Filename: input.Submit.Source.Filename}, SavePath: input.Submit.SavePath, Tag: input.Submit.Tag, MetadataOnly: input.Submit.MetadataOnly})
		if err != nil {
			return response, err
		}
		if err := a.store.SaveManagedDownload(ctx, ManagedDownload{ServerID: serverID, DownloaderID: input.DownloaderID, TaskID: input.TaskID, ProviderTaskID: task.ID, Tag: input.Submit.Tag, DownloaderSavePath: input.Submit.SavePath, NodeLocalRoot: localRoot}, a.now()); err != nil {
			return response, downloadpkg.Error("node_persistence_failed", true, err)
		}
		response.Task = protocolDownloaderTask(task)
		return response, nil
	case nodeprotocol.DownloaderActionGet:
		task, err := client.Get(ctx, input.ProviderTaskID)
		if err == nil && task.ID != "" && task.ID != input.ProviderTaskID {
			_ = a.store.AliasManagedDownload(ctx, serverID, input.DownloaderID, input.ProviderTaskID, task.ID, a.now())
		}
		response.Task = protocolDownloaderTask(task)
		return response, err
	case nodeprotocol.DownloaderActionPause:
		return response, client.Pause(ctx, input.ProviderTaskID)
	case nodeprotocol.DownloaderActionResume:
		return response, client.Resume(ctx, input.ProviderTaskID)
	case nodeprotocol.DownloaderActionCancel:
		return response, client.Cancel(ctx, input.ProviderTaskID, input.DeleteData)
	case nodeprotocol.DownloaderActionDeleteTag:
		return response, client.DeleteManagedTag(ctx, input.ManagedTag)
	case nodeprotocol.DownloaderActionManifest:
		now := a.now()
		if page, exportErr := a.store.FileExportManifest(ctx, serverID, input.OperationKey, 1, 1, now); exportErr == nil && page.Summary.TaskID == input.TaskID {
			summary := page.Summary
			if renewed, renewErr := a.store.RenewFileExport(ctx, serverID, input.OperationKey, input.TaskID, now, now.Add(fileExportLifetime)); renewErr == nil {
				summary = renewed
			}
			response.Manifest = &nodeprotocol.DownloaderManifest{Name: summary.Name, Complete: true}
			response.FileExport = &summary
			return response, nil
		}
		if summary, renewErr := a.store.RenewFileExport(ctx, serverID, input.OperationKey, input.TaskID, now, now.Add(fileExportLifetime)); renewErr == nil {
			response.Manifest = &nodeprotocol.DownloaderManifest{Name: summary.Name, Complete: true}
			response.FileExport = &summary
			return response, nil
		}
		manifest, err := client.Manifest(ctx, input.ProviderTaskID)
		if err != nil {
			return response, err
		}
		if task, getErr := client.Get(ctx, input.ProviderTaskID); getErr == nil && task.Completed {
			if err := a.verifyManagedManifest(ctx, serverID, input.DownloaderID, input.ProviderTaskID, manifest); err != nil {
				return response, downloadpkg.Error(nodeprotocol.ErrorPathMappingInvalid, false, err)
			}
			download, err := a.store.ManagedDownload(ctx, serverID, input.DownloaderID, input.ProviderTaskID)
			if err != nil || download.TaskID != input.TaskID {
				return response, downloadpkg.Error(nodeprotocol.ErrorPlanConflict, false, err)
			}
			a.startFileExport(serverID, input.OperationKey, download, manifest)
			return response, downloadpkg.Error(nodeprotocol.ErrorExportPreparing, true, nil)
		}
		response.Manifest = protocolDownloaderManifest(manifest)
		return response, nil
	case nodeprotocol.DownloaderActionCategories:
		categories, err := client.Categories(ctx)
		if err != nil {
			return response, err
		}
		response.Categories = make([]nodeprotocol.DownloaderCategory, 0, len(categories))
		for _, category := range categories {
			response.Categories = append(response.Categories, nodeprotocol.DownloaderCategory{Name: category.Name, SavePath: category.SavePath})
		}
		return response, nil
	case nodeprotocol.DownloaderActionEnsureCategory:
		if _, err := mapDownloaderPath(credential.DownloaderSaveRoot, credential.NodeMountRoot, input.SavePath); err != nil {
			return response, downloadpkg.Error(nodeprotocol.ErrorPathMappingInvalid, false, err)
		}
		return response, client.EnsureCategory(ctx, input.CategoryName, input.SavePath)
	case nodeprotocol.DownloaderActionUpdateCategory:
		if _, err := mapDownloaderPath(credential.DownloaderSaveRoot, credential.NodeMountRoot, input.SavePath); err != nil {
			return response, downloadpkg.Error(nodeprotocol.ErrorPathMappingInvalid, false, err)
		}
		return response, client.UpdateCategory(ctx, input.CategoryName, input.SavePath)
	case nodeprotocol.DownloaderActionSetCategory:
		if _, err := mapDownloaderPath(credential.DownloaderSaveRoot, credential.NodeMountRoot, input.SavePath); err != nil {
			return response, downloadpkg.Error(nodeprotocol.ErrorPathMappingInvalid, false, err)
		}
		return response, client.SetCategory(ctx, input.ProviderTaskID, input.CategoryName, input.SavePath)
	default:
		return response, downloadpkg.Error("node_downloader_action_unsupported", false, nil)
	}
}

func (a *Agent) verifyManagedManifest(ctx context.Context, serverID, downloaderID, providerTaskID string, manifest downloadpkg.Manifest) error {
	download, err := a.store.ManagedDownload(ctx, serverID, downloaderID, providerTaskID)
	if err != nil {
		return errors.New("node_managed_download_not_found")
	}
	for _, file := range manifest.Files {
		candidate := filepath.Join(download.NodeLocalRoot, filepath.FromSlash(file.RelativePath))
		if err := requirePathWithin(download.NodeLocalRoot, candidate); err != nil {
			return err
		}
		info, err := os.Lstat(candidate)
		if err != nil || !info.Mode().IsRegular() || info.Size() != file.Size {
			return errors.New("node_managed_file_invalid")
		}
	}
	return nil
}

func validateDownloaderRoots(managedRoot string, credential nodeprotocol.DownloaderCredential) (string, error) {
	if strings.TrimSpace(credential.BaseURL) == "" || strings.TrimSpace(credential.DownloaderSaveRoot) == "" || strings.TrimSpace(credential.NodeMountRoot) == "" {
		return "", errors.New(nodeprotocol.ErrorPathMappingInvalid)
	}
	root, err := filepath.Abs(filepath.Clean(credential.NodeMountRoot))
	if err != nil {
		return "", errors.New(nodeprotocol.ErrorPathMappingInvalid)
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return "", errors.New(nodeprotocol.ErrorPathMappingInvalid)
	}
	if err := requirePathWithin(managedRoot, root); err != nil {
		return "", errors.New(nodeprotocol.ErrorPathMappingInvalid)
	}
	return root, nil
}

func mapDownloaderPath(saveRoot, nodeRoot, savePath string) (string, error) {
	root := strings.TrimRight(strings.ReplaceAll(strings.TrimSpace(saveRoot), "\\", "/"), "/")
	candidate := strings.TrimRight(strings.ReplaceAll(strings.TrimSpace(savePath), "\\", "/"), "/")
	if root == "" || candidate == "" {
		return "", errors.New("empty path mapping")
	}
	compareRoot, compareCandidate := root, candidate
	if runtime.GOOS == "windows" {
		compareRoot, compareCandidate = strings.ToLower(root), strings.ToLower(candidate)
	}
	if compareCandidate != compareRoot && !strings.HasPrefix(compareCandidate, compareRoot+"/") {
		return "", errors.New("save path outside downloader root")
	}
	relative := strings.TrimPrefix(candidate[len(root):], "/")
	local := filepath.Join(nodeRoot, filepath.FromSlash(relative))
	if err := requirePathWithin(nodeRoot, local); err != nil {
		return "", err
	}
	return local, nil
}

func requirePathWithin(root, candidate string) error {
	rootAbs, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return err
	}
	candidateAbs, err := filepath.Abs(filepath.Clean(candidate))
	if err != nil {
		return err
	}
	relative, err := filepath.Rel(rootAbs, candidateAbs)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return errors.New("path outside managed root")
	}
	return nil
}

func containsAction(actions []string, action string) bool {
	for _, candidate := range actions {
		if candidate == action {
			return true
		}
	}
	return false
}

func protocolDownloaderTask(task downloadpkg.Task) *nodeprotocol.DownloaderTask {
	return &nodeprotocol.DownloaderTask{ID: task.ID, Name: task.Name, Status: task.Status, Progress: task.Progress, BytesCompleted: task.BytesCompleted, BytesTotal: task.BytesTotal, DownloadSpeed: task.DownloadSpeed, UploadSpeed: task.UploadSpeed, ETASeconds: task.ETASeconds, Ratio: task.Ratio, SeededSeconds: task.SeededSeconds, UploadedBytes: task.UploadedBytes, Seeding: task.Seeding, Completed: task.Completed, Failed: task.Failed, ErrorCode: task.ErrorCode}
}

func protocolDownloaderManifest(manifest downloadpkg.Manifest) *nodeprotocol.DownloaderManifest {
	files := make([]nodeprotocol.DownloaderFile, 0, len(manifest.Files))
	for _, file := range manifest.Files {
		files = append(files, nodeprotocol.DownloaderFile{RelativePath: file.RelativePath, Size: file.Size})
	}
	return &nodeprotocol.DownloaderManifest{Name: manifest.Name, Files: files, Complete: manifest.Complete}
}

func downloaderActionMessage(code string) string {
	switch code {
	case "downloader_auth_failed":
		return "节点连接 qBittorrent 认证失败"
	case nodeprotocol.ErrorPathMappingInvalid:
		return "节点无法验证 qBittorrent 下载目录映射"
	case "downloader_rate_limited":
		return "节点访问 qBittorrent 暂时受限"
	case nodeprotocol.ErrorExportPreparing:
		return "节点正在核验下载文件并生成分块清单"
	default:
		return "节点无法执行 qBittorrent 操作"
	}
}
