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
	// Keep status/cancel/pause control available while data lanes are full.
	if input.Action != nodeprotocol.DownloaderActionGet && input.Action != nodeprotocol.DownloaderActionCancel && input.Action != nodeprotocol.DownloaderActionPause {
		release, err := a.acquireOperation(r.Context())
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "node_operation_busy", "节点执行队列暂时不可用")
			return
		}
		defer release()
		if !grant.ExpiresAt.After(a.now()) {
			writeError(w, http.StatusForbidden, nodeprotocol.ErrorCredentialExpired, "临时授权已经过期")
			return
		}
	}

	mutating := input.Action == nodeprotocol.DownloaderActionSelectionGroupCleanup || input.Action == nodeprotocol.DownloaderActionSelectionGroup || input.Action == nodeprotocol.DownloaderActionSubmit || input.Action == nodeprotocol.DownloaderActionPause || input.Action == nodeprotocol.DownloaderActionResume || input.Action == nodeprotocol.DownloaderActionCancel || input.Action == nodeprotocol.DownloaderActionDeleteTag || input.Action == nodeprotocol.DownloaderActionEnsureCategory || input.Action == nodeprotocol.DownloaderActionUpdateCategory || input.Action == nodeprotocol.DownloaderActionSetCategory
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

	executionCtx, cancelExecution := context.WithTimeout(r.Context(), grant.ExpiresAt.Sub(a.now()))
	defer cancelExecution()
	response, actionErr := a.executeDownloaderAction(executionCtx, serverID, input, credential)
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
	root, err := validateDownloaderRoots(credential)
	if err != nil {
		return nodeprotocol.CredentialGrant{}, nodeprotocol.DownloaderCredential{}, err
	}
	credential.NodeMountRoot = root
	return grant, credential, nil
}

func (a *Agent) executeDownloaderAction(ctx context.Context, serverID string, input nodeprotocol.DownloaderActionRequest, credential nodeprotocol.DownloaderCredential) (nodeprotocol.DownloaderActionResponse, error) {
	base, err := qbittorrent.New(downloadpkg.Config{BaseURL: credential.BaseURL, Username: credential.Username, Password: credential.Password})
	if err != nil {
		return nodeprotocol.DownloaderActionResponse{}, err
	}
	bound, err := a.bindManagedSelection(ctx, serverID, input, base)
	if err != nil {
		return nodeprotocol.DownloaderActionResponse{}, err
	}
	client := bound.(interface {
		downloadpkg.Client
		downloadpkg.MetadataClient
	})
	response := nodeprotocol.DownloaderActionResponse{RequestID: input.RequestID}
	switch input.Action {
	case nodeprotocol.DownloaderActionTest:
		health, err := client.Test(ctx)
		if err == nil {
			response.Health = &nodeprotocol.DownloaderHealth{Version: health.Version}
		}
		return response, err
	case nodeprotocol.DownloaderActionSubmit, nodeprotocol.DownloaderActionSelectionGroup, nodeprotocol.DownloaderActionSelectionGroupCleanup:
		if input.Submit.SourceEnvelope != nil {
			opened, err := nodeprotocol.OpenDownloaderSource(input.Submit.SourceEnvelope, credential.SourceKey, nodeprotocol.DownloaderSourceBinding{ServerID: serverID, TaskID: input.TaskID, OperationKey: input.OperationKey, DownloaderID: input.DownloaderID, SavePath: input.Submit.SavePath, Tag: input.Submit.Tag, ConfigurationScope: credential.SourceScope})
			if err != nil || opened.Kind != input.Submit.Source.Kind || opened.ContentSelection == nil {
				return response, downloadpkg.Error(nodeprotocol.ErrorPlanConflict, false, err)
			}
			copySubmit := *input.Submit
			copySubmit.Source = opened
			input.Submit = &copySubmit
		} else if input.Submit.Source.ContentSelection != nil {
			return response, downloadpkg.Error("node_selection_source_envelope_required", false, nil)
		}
		localRoot, err := mapDownloaderPath(credential.DownloaderSaveRoot, credential.NodeMountRoot, input.Submit.SavePath)
		if err != nil {
			return response, downloadpkg.Error(nodeprotocol.ErrorPathMappingInvalid, false, err)
		}
		source := downloadpkg.Source{Kind: input.Submit.Source.Kind, URL: input.Submit.Source.URL, Torrent: input.Submit.Source.Torrent, Filename: input.Submit.Source.Filename, ContentSelection: input.Submit.Source.ContentSelection}
		if input.Action == nodeprotocol.DownloaderActionSelectionGroupCleanup {
			task, err := a.executeSelectionGroupCleanup(ctx, serverID, input, credential, base, source, localRoot)
			if err == nil {
				response.Task = protocolDownloaderTask(task)
			}
			return response, err
		}
		if input.Action == nodeprotocol.DownloaderActionSelectionGroup {
			task, err := a.executeSelectionGroup(ctx, serverID, input, credential, base, source, localRoot)
			if err == nil {
				response.Task = protocolDownloaderTask(task)
			}
			return response, err
		}
		var selection *downloadpkg.SelectionContext
		if source.ContentSelection != nil {
			manifest, err := downloadpkg.ValidateContentSource(source)
			if err != nil {
				return response, err
			}
			if input.Submit.Tag != "omc-"+input.TaskID {
				return response, downloadpkg.Error(nodeprotocol.ErrorPlanConflict, false, nil)
			}
			selection = &downloadpkg.SelectionContext{Tag: input.Submit.Tag, Selection: *source.ContentSelection, RootName: manifest.Name, MultiFile: manifest.MultiFile}
		}
		task, err := client.Submit(ctx, downloadpkg.SubmitRequest{Source: source, SavePath: input.Submit.SavePath, Tag: input.Submit.Tag, MetadataOnly: input.Submit.MetadataOnly})
		if err != nil {
			return response, err
		}
		if selection != nil {
			if err := a.store.saveDownloadSelection(ctx, serverID, input.DownloaderID, input.TaskID, task.ID, *selection); err != nil {
				return response, downloadpkg.Error(nodeprotocol.ErrorPlanConflict, false, err)
			}
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
		a.exportMu.Lock()
		for key, owner := range a.exportOwners {
			if owner == input.TaskID {
				a.exportCancels[key]()
			}
		}
		a.exportMu.Unlock()
		return response, client.Cancel(ctx, input.ProviderTaskID, input.DeleteData)
	case nodeprotocol.DownloaderActionDeleteTag:
		var grouped int
		if err := a.store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM managed_download_group_refs WHERE paired_server_id=? AND downloader_id=? AND task_id=?`, serverID, input.DownloaderID, input.TaskID).Scan(&grouped); err != nil || grouped != 0 {
			return response, downloadpkg.Error(nodeprotocol.ErrorPlanConflict, false, err)
		}
		if _, _, err := a.store.downloadSelection(ctx, serverID, input.DownloaderID, input.TaskID); err == nil {
			return response, downloadpkg.Error(nodeprotocol.ErrorPlanConflict, false, nil)
		}
		return response, base.DeleteManagedTag(ctx, input.ManagedTag)
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
			if err := a.verifyManagedManifest(ctx, serverID, input.DownloaderID, input.ProviderTaskID, credential.NodeMountRoot, manifest, input.TaskID); err != nil {
				return response, downloadpkg.Error(nodeprotocol.ErrorPathMappingInvalid, false, err)
			}
			download, err := a.managedDownloadForTask(ctx, serverID, input.DownloaderID, input.TaskID, input.ProviderTaskID)
			if err != nil || download.TaskID != input.TaskID {
				return response, downloadpkg.Error(nodeprotocol.ErrorPlanConflict, false, err)
			}
			a.startFileExport(serverID, input.OperationKey, credential.NodeMountRoot, download, manifest)
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

func (a *Agent) verifyManagedManifest(ctx context.Context, serverID, downloaderID, providerTaskID, mountRoot string, manifest downloadpkg.Manifest, taskIDs ...string) error {
	var download ManagedDownload
	var err error
	if len(taskIDs) > 0 {
		download, err = a.managedDownloadForTask(ctx, serverID, downloaderID, taskIDs[0], providerTaskID)
	} else {
		download, err = a.store.ManagedDownload(ctx, serverID, downloaderID, providerTaskID)
	}
	if err != nil {
		return errors.New("node_managed_download_not_found")
	}
	root, err := secureExportRoot(mountRoot, download.NodeLocalRoot)
	if err != nil {
		return err
	}
	for _, file := range manifest.Files {
		relative := normalizedExportPath(file.RelativePath)
		if relative == "" || file.Size < 0 {
			return errors.New("node_managed_file_invalid")
		}
		if _, err := secureExportFile(root, relative, file.Size); err != nil {
			return errors.New("node_managed_file_invalid")
		}
	}
	return nil
}

func validateDownloaderRoots(credential nodeprotocol.DownloaderCredential) (string, error) {
	if strings.TrimSpace(credential.BaseURL) == "" || strings.TrimSpace(credential.DownloaderSaveRoot) == "" || strings.TrimSpace(credential.NodeMountRoot) == "" {
		return "", errors.New(nodeprotocol.ErrorPathMappingInvalid)
	}
	root := filepath.Clean(credential.NodeMountRoot)
	if !filepath.IsAbs(root) {
		return "", errors.New(nodeprotocol.ErrorPathMappingInvalid)
	}
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", errors.New(nodeprotocol.ErrorPathMappingInvalid)
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return "", errors.New(nodeprotocol.ErrorPathMappingInvalid)
	}
	handle, err := os.Open(root)
	if err != nil {
		return "", errors.New(nodeprotocol.ErrorPathMappingInvalid)
	}
	_ = handle.Close()
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
		return errors.New("path outside configured root")
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
		files = append(files, nodeprotocol.DownloaderFile{RelativePath: file.RelativePath, Size: file.Size, NativeIndex: file.NativeIndex})
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
