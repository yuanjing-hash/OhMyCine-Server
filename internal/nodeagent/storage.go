package nodeagent

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/pan115"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
)

const (
	storageTaskHeader      = "X-OhMyCine-Task-ID"
	storageOperationHeader = "X-OhMyCine-Operation-Key"
	storageListLimit       = int64(200)
	storageListMaximum     = int64(10000)
)

type storageUploadDriver interface {
	cloud.Driver
	CreateDirectory(context.Context, string, string) (cloud.Item, error)
	Upload(context.Context, cloud.UploadRequest) (cloud.Item, error)
	Recycle(context.Context, string) error
}

type storageDriverFactory func(nodeprotocol.StorageCredential, func(context.Context) error) (storageUploadDriver, error)

func newStorageDriver(credential nodeprotocol.StorageCredential, beforeCall func(context.Context) error) (storageUploadDriver, error) {
	if credential.ProviderType != nodeprotocol.StorageProviderPan115 || strings.TrimSpace(credential.Cookie) == "" {
		return nil, errors.New("node_storage_credential_invalid")
	}
	driver, err := pan115.New(cloud.Config{Cookie: credential.Cookie, BeforeCall: beforeCall})
	if err != nil {
		return nil, err
	}
	upload, ok := driver.(storageUploadDriver)
	if !ok || !driver.Capabilities().FileUpload || !driver.Capabilities().CreateDirectory || !driver.Capabilities().Recycle {
		return nil, errors.New("node_storage_capability_missing")
	}
	return upload, nil
}

func (a *Agent) storageAction(w http.ResponseWriter, r *http.Request) {
	var input nodeprotocol.StorageActionRequest
	if err := decodeJSON(w, r, &input); err != nil {
		return
	}
	if err := input.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "存储操作无效")
		return
	}
	digest, err := input.Digest()
	if err != nil {
		writeError(w, http.StatusBadRequest, "node_storage_request_invalid", "存储操作校验失败")
		return
	}
	serverID := r.Header.Get("X-OhMyCine-Verified-Server-ID")
	if receipt, getErr := a.store.ProviderReceipt(r.Context(), serverID, input.RequestID); getErr == nil {
		if receipt.RequestDigest != digest || receipt.OperationKey != input.OperationKey || receipt.Action != input.Action {
			writeError(w, http.StatusConflict, nodeprotocol.ErrorPlanConflict, "相同操作编号对应了不同请求")
			return
		}
		if receipt.Status == "completed" {
			var response nodeprotocol.StorageActionResponse
			if json.Unmarshal([]byte(receipt.ResponseJSON), &response) == nil {
				writeJSON(w, http.StatusOK, response)
				return
			}
		}
	}

	_, uploadPlan, err := a.storagePlan(r.Context(), serverID, input)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	grant, credential, err := a.storageCredential(r.Context(), serverID, input, uploadPlan)
	if err != nil {
		writeError(w, http.StatusForbidden, storageErrorCode(err), "存储临时授权无效或已经过期")
		return
	}
	receipt, created, err := a.store.BeginActionReceipt(r.Context(), serverID, input.RequestID, input.OperationKey, input.Action, digest, a.now())
	if errors.Is(err, ErrPlanConflict) {
		writeError(w, http.StatusConflict, nodeprotocol.ErrorPlanConflict, "相同操作编号对应了不同请求")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "node_persistence_failed", "节点无法保存存储操作")
		return
	}
	response := nodeprotocol.StorageActionResponse{RequestID: input.RequestID, OperationKey: input.OperationKey, PlanDigest: input.PlanDigest, Status: nodeprotocol.StorageActionAccepted, TotalFiles: len(uploadPlan.Files)}
	if receipt.ResponseJSON != "" {
		_ = json.Unmarshal([]byte(receipt.ResponseJSON), &response)
	}
	if created || receipt.ResponseJSON == "" {
		if err := a.store.UpdateActionReceipt(r.Context(), serverID, input.RequestID, response, a.now()); err != nil {
			writeError(w, http.StatusInternalServerError, "node_persistence_failed", "节点无法保存存储操作")
			return
		}
	}
	started := a.startStorageUpload(serverID, input, uploadPlan, grant, credential)
	if started && response.Status == nodeprotocol.StorageActionAccepted {
		response.Status = nodeprotocol.StorageActionRunning
	}
	writeJSON(w, http.StatusAccepted, response)
}

func (a *Agent) storageActionResult(w http.ResponseWriter, r *http.Request) {
	requestID := r.PathValue("request_id")
	taskID := strings.TrimSpace(r.Header.Get(storageTaskHeader))
	operationKey := strings.TrimSpace(r.Header.Get(storageOperationHeader))
	if requestID == "" || taskID == "" || operationKey == "" {
		writeError(w, http.StatusBadRequest, "node_storage_request_invalid", "存储结果查询无效")
		return
	}
	serverID := r.Header.Get("X-OhMyCine-Verified-Server-ID")
	receipt, err := a.store.ProviderReceipt(r.Context(), serverID, requestID)
	if errors.Is(err, ErrOperationNotFound) || err == nil && (receipt.OperationKey != operationKey || receipt.Action != nodeprotocol.StorageActionUpload) {
		writeError(w, http.StatusNotFound, "node_storage_result_not_found", "存储操作结果不存在")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "node_persistence_failed", "节点无法读取存储操作结果")
		return
	}
	if matches, checkErr := a.store.OperationTaskMatches(r.Context(), serverID, operationKey, taskID); checkErr != nil || !matches {
		writeError(w, http.StatusNotFound, "node_storage_result_not_found", "存储操作结果不存在")
		return
	}
	var response nodeprotocol.StorageActionResponse
	if receipt.ResponseJSON == "" || json.Unmarshal([]byte(receipt.ResponseJSON), &response) != nil {
		response = nodeprotocol.StorageActionResponse{RequestID: requestID, OperationKey: operationKey, Status: nodeprotocol.StorageActionAccepted}
	}
	writeJSON(w, http.StatusOK, response)
}

func (a *Agent) storagePlan(ctx context.Context, serverID string, input nodeprotocol.StorageActionRequest) (nodeprotocol.OperationPlan, nodeprotocol.StorageUploadPlan, error) {
	plan, err := a.store.ActiveOperationPlan(ctx, serverID, input.OperationKey, input.PlanDigest, a.now())
	if err != nil {
		return nodeprotocol.OperationPlan{}, nodeprotocol.StorageUploadPlan{}, err
	}
	if plan.Kind != nodeprotocol.OperationKindStorageUpload || plan.TaskID != input.TaskID {
		return nodeprotocol.OperationPlan{}, nodeprotocol.StorageUploadPlan{}, ErrPlanConflict
	}
	var uploadPlan nodeprotocol.StorageUploadPlan
	decoder := json.NewDecoder(bytes.NewReader(plan.Payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&uploadPlan); err != nil || uploadPlan.Validate() != nil || uploadPlan.StorageID != input.StorageID {
		return nodeprotocol.OperationPlan{}, nodeprotocol.StorageUploadPlan{}, ErrPlanConflict
	}
	summary, err := a.store.FileExportManifest(ctx, serverID, uploadPlan.SourceExportOperationKey, 1, 1, a.now())
	if err != nil || summary.Summary.TaskID != input.TaskID || summary.Summary.ManifestDigest != uploadPlan.SourceManifestDigest {
		return nodeprotocol.OperationPlan{}, nodeprotocol.StorageUploadPlan{}, ErrPlanConflict
	}
	return plan, uploadPlan, nil
}

func (a *Agent) storageCredential(ctx context.Context, serverID string, input nodeprotocol.StorageActionRequest, plan nodeprotocol.StorageUploadPlan) (nodeprotocol.CredentialGrant, nodeprotocol.StorageCredential, error) {
	envelope, err := a.store.CredentialGrant(ctx, serverID, input.GrantID, a.now())
	if err != nil {
		return nodeprotocol.CredentialGrant{}, nodeprotocol.StorageCredential{}, errors.New(nodeprotocol.ErrorCredentialExpired)
	}
	grant, err := nodeprotocol.OpenCredentialGrant(a.sealingPrivateKey, envelope, a.now())
	if err != nil {
		return nodeprotocol.CredentialGrant{}, nodeprotocol.StorageCredential{}, err
	}
	if grant.NodeID != a.config.NodeID || grant.TaskID != input.TaskID || grant.OperationKey != input.OperationKey || grant.ResourceKind != nodeprotocol.ResourceKindStorage || grant.ResourceID != input.StorageID || !containsAction(grant.AllowedActions, nodeprotocol.StorageActionUpload) {
		return nodeprotocol.CredentialGrant{}, nodeprotocol.StorageCredential{}, errors.New("node_credential_binding_mismatch")
	}
	for _, file := range plan.Files {
		if file.ConflictAction == nodeprotocol.StorageConflictReplace && !containsAction(grant.AllowedActions, nodeprotocol.StorageActionReplace) {
			return nodeprotocol.CredentialGrant{}, nodeprotocol.StorageCredential{}, errors.New("node_credential_binding_mismatch")
		}
	}
	var credential nodeprotocol.StorageCredential
	decoder := json.NewDecoder(bytes.NewReader(grant.Credential))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&credential); err != nil || credential.ProviderType != plan.ProviderType || credential.ProviderType != nodeprotocol.StorageProviderPan115 || strings.TrimSpace(credential.Cookie) == "" {
		return nodeprotocol.CredentialGrant{}, nodeprotocol.StorageCredential{}, errors.New("node_credential_payload_invalid")
	}
	return grant, credential, nil
}

func (a *Agent) startStorageUpload(serverID string, input nodeprotocol.StorageActionRequest, plan nodeprotocol.StorageUploadPlan, grant nodeprotocol.CredentialGrant, credential nodeprotocol.StorageCredential) bool {
	a.storageMu.Lock()
	if _, running := a.storageRuns[input.OperationKey]; running {
		a.storageMu.Unlock()
		return false
	}
	a.storageRuns[input.OperationKey] = struct{}{}
	a.storageMu.Unlock()
	go func() {
		defer func() {
			a.storageMu.Lock()
			delete(a.storageRuns, input.OperationKey)
			a.storageMu.Unlock()
		}()
		a.runStorageUpload(context.Background(), serverID, input, plan, grant, credential)
	}()
	return true
}

func (a *Agent) runStorageUpload(ctx context.Context, serverID string, input nodeprotocol.StorageActionRequest, plan nodeprotocol.StorageUploadPlan, grant nodeprotocol.CredentialGrant, credential nodeprotocol.StorageCredential) {
	response := nodeprotocol.StorageActionResponse{RequestID: input.RequestID, OperationKey: input.OperationKey, PlanDigest: input.PlanDigest, Status: nodeprotocol.StorageActionRunning, TotalFiles: len(plan.Files), Files: make([]nodeprotocol.StorageUploadFileResult, 0, len(plan.Files))}
	_ = a.store.UpdateActionReceipt(ctx, serverID, input.RequestID, response, a.now())
	_ = a.store.UpdateOperationExecution(ctx, serverID, input.OperationKey, input.PlanDigest, nodeprotocol.OperationRunning, nodeprotocol.PhaseUploadingTarget, "", nil, a.now())
	guard := func(callCtx context.Context) error {
		if !grant.ExpiresAt.After(a.now()) {
			return cloud.Error(cloud.CodeAuthExpired, false, errors.New(nodeprotocol.ErrorCredentialExpired))
		}
		_, err := a.store.ActiveOperationPlan(callCtx, serverID, input.OperationKey, input.PlanDigest, a.now())
		if errors.Is(err, ErrLeaseConflict) {
			return cloud.Error(cloud.CodeUnavailable, true, errors.New(nodeprotocol.ErrorLeaseExpired))
		}
		return err
	}
	driver, err := a.storageFactory(credential, guard)
	if err != nil {
		a.pauseStorageUpload(ctx, serverID, input, response, err)
		return
	}
	if driver.Provider() != plan.ProviderType {
		a.pauseStorageUpload(ctx, serverID, input, response, errors.New(nodeprotocol.ErrorCapabilityMissing))
		return
	}
	root, err := driver.Stat(ctx, plan.TargetRootID)
	if err != nil || root.ID != plan.TargetRootID || !root.IsDir {
		if err == nil {
			err = errors.New(nodeprotocol.ErrorPlanConflict)
		}
		a.pauseStorageUpload(ctx, serverID, input, response, err)
		return
	}
	for index, filePlan := range plan.Files {
		if err := guard(ctx); err != nil {
			a.pauseStorageUpload(ctx, serverID, input, response, err)
			return
		}
		result, err := a.uploadStorageFile(ctx, serverID, input, plan, filePlan, driver, guard)
		if err != nil {
			response.Files = append(response.Files, result)
			a.pauseStorageUpload(ctx, serverID, input, response, err)
			return
		}
		response.Files = append(response.Files, result)
		response.CompletedFiles = index + 1
		progress := float64(response.CompletedFiles) / float64(response.TotalFiles)
		_ = a.store.UpdateActionReceipt(ctx, serverID, input.RequestID, response, a.now())
		_ = a.store.UpdateOperationExecution(ctx, serverID, input.OperationKey, input.PlanDigest, nodeprotocol.OperationRunning, nodeprotocol.PhaseUploadingTarget, "", &progress, a.now())
	}
	if err := guard(ctx); err != nil {
		a.pauseStorageUpload(ctx, serverID, input, response, err)
		return
	}
	response.Status = nodeprotocol.StorageActionCompleted
	response.ErrorCode = ""
	_ = a.store.CompleteStorageAction(ctx, serverID, input.RequestID, input.OperationKey, input.PlanDigest, response, a.now())
}

func (a *Agent) uploadStorageFile(ctx context.Context, serverID string, input nodeprotocol.StorageActionRequest, plan nodeprotocol.StorageUploadPlan, filePlan nodeprotocol.StorageUploadFile, driver storageUploadDriver, guard func(context.Context) error) (nodeprotocol.StorageUploadFileResult, error) {
	result := nodeprotocol.StorageUploadFileResult{SourceFileToken: filePlan.SourceFileToken, TargetName: path.Base(filePlan.TargetRelativePath), Size: filePlan.Size, Status: nodeprotocol.StorageFilePending}
	checkpoint, err := a.store.PrepareStorageUploadFile(ctx, input.OperationKey, filePlan.SourceFileToken, input.PlanDigest, filePlan.Size, a.now())
	if err != nil {
		return result, err
	}
	if checkpoint.Status == nodeprotocol.StorageFileCompleted || checkpoint.Status == nodeprotocol.StorageFileReused {
		verified, verifyErr := verifyCheckpointTarget(ctx, driver, checkpoint, result.TargetName)
		if verifyErr != nil {
			return result, errors.New(nodeprotocol.ErrorReconciliationNeeded)
		}
		verified.SourceFileToken = filePlan.SourceFileToken
		verified.Status = checkpoint.Status
		return verified, nil
	}
	if checkpoint.Status == nodeprotocol.StorageFileSkipped {
		result.Status = nodeprotocol.StorageFileSkipped
		result.TargetParentID = checkpoint.TargetParentID
		return result, nil
	}

	source, err := a.store.FileExportRangeFile(ctx, serverID, plan.SourceExportOperationKey, input.TaskID, filePlan.SourceFileToken, a.now())
	if err != nil || source.Size != filePlan.Size || source.SHA256 != filePlan.SHA256 {
		return result, errors.New(nodeprotocol.ErrorPlanConflict)
	}
	handle, err := a.openManagedExportFile(source)
	if err != nil {
		return result, errors.New(nodeprotocol.ErrorChecksumMismatch)
	}
	defer handle.Close()
	sha1Digest, err := hashStorageSource(ctx, handle, filePlan.Size, filePlan.SHA256)
	if err != nil {
		return result, err
	}

	parentID, err := ensureStorageParent(ctx, driver, plan.TargetRootID, path.Dir(filePlan.TargetRelativePath), guard)
	if err != nil {
		return result, err
	}
	result.TargetParentID = parentID
	existing, err := listStorageName(ctx, driver, parentID, result.TargetName)
	if err != nil {
		return result, err
	}
	identical := identicalStorageItems(existing, filePlan.Size, sha1Digest)
	if checkpoint.Status == nodeprotocol.StorageFileUploading || checkpoint.Status == nodeprotocol.StorageFileReconciling {
		if len(identical) == 1 {
			return a.finishStorageFile(ctx, input, filePlan, identical[0], parentID, sha1Digest, nodeprotocol.StorageFileCompleted)
		}
		_ = a.store.UpdateStorageUploadFile(ctx, input.OperationKey, filePlan.SourceFileToken, input.PlanDigest, nodeprotocol.StorageFileReconciling, parentID, "", sha1Digest, a.now())
		return result, errors.New(nodeprotocol.ErrorReconciliationNeeded)
	}
	if len(existing) > 0 {
		switch filePlan.ConflictAction {
		case nodeprotocol.StorageConflictSkipIfExists:
			if err := a.store.UpdateStorageUploadFile(ctx, input.OperationKey, filePlan.SourceFileToken, input.PlanDigest, nodeprotocol.StorageFileSkipped, parentID, "", "", a.now()); err != nil {
				return result, err
			}
			result.Status = nodeprotocol.StorageFileSkipped
			return result, nil
		case nodeprotocol.StorageConflictReuseIfIdentical:
			if len(existing) == 1 && len(identical) == 1 {
				return a.finishStorageFile(ctx, input, filePlan, identical[0], parentID, sha1Digest, nodeprotocol.StorageFileReused)
			}
			return result, errors.New(nodeprotocol.ErrorPlanConflict)
		case nodeprotocol.StorageConflictReplace:
			if len(existing) == 1 && len(identical) == 1 {
				return a.finishStorageFile(ctx, input, filePlan, identical[0], parentID, sha1Digest, nodeprotocol.StorageFileReused)
			}
			if len(existing) != 1 || existing[0].IsDir {
				return result, errors.New(nodeprotocol.ErrorReconciliationNeeded)
			}
			if err := guard(ctx); err != nil {
				return result, err
			}
			if err := driver.Recycle(ctx, existing[0].ID); err != nil {
				return result, err
			}
		default:
			return result, errors.New(nodeprotocol.ErrorPlanConflict)
		}
	}
	if err := guard(ctx); err != nil {
		return result, err
	}
	if err := a.store.UpdateStorageUploadFile(ctx, input.OperationKey, filePlan.SourceFileToken, input.PlanDigest, nodeprotocol.StorageFileUploading, parentID, "", sha1Digest, a.now()); err != nil {
		return result, err
	}
	if _, err := handle.Seek(0, io.SeekStart); err != nil {
		return result, errors.New(nodeprotocol.ErrorChecksumMismatch)
	}
	uploaded, err := driver.Upload(ctx, cloud.UploadRequest{ParentID: parentID, Name: result.TargetName, Size: filePlan.Size, Reader: handle})
	if err != nil {
		code := storageErrorCode(err)
		status := nodeprotocol.StorageFileReconciling
		if code == nodeprotocol.ErrorCredentialExpired || code == nodeprotocol.ErrorTargetRateLimited {
			status = nodeprotocol.StorageFilePending
		}
		_ = a.store.UpdateStorageUploadFile(ctx, input.OperationKey, filePlan.SourceFileToken, input.PlanDigest, status, parentID, "", sha1Digest, a.now())
		return result, err
	}
	verified, err := verifyUploadedTarget(ctx, driver, uploaded, parentID, result.TargetName, filePlan.Size, sha1Digest)
	if err != nil {
		_ = a.store.UpdateStorageUploadFile(ctx, input.OperationKey, filePlan.SourceFileToken, input.PlanDigest, nodeprotocol.StorageFileReconciling, parentID, uploaded.ID, sha1Digest, a.now())
		return result, errors.New(nodeprotocol.ErrorReconciliationNeeded)
	}
	return a.finishStorageFile(ctx, input, filePlan, verified, parentID, sha1Digest, nodeprotocol.StorageFileCompleted)
}

func (a *Agent) finishStorageFile(ctx context.Context, input nodeprotocol.StorageActionRequest, plan nodeprotocol.StorageUploadFile, item cloud.Item, parentID, sha1Digest, status string) (nodeprotocol.StorageUploadFileResult, error) {
	if err := a.store.UpdateStorageUploadFile(ctx, input.OperationKey, plan.SourceFileToken, input.PlanDigest, status, parentID, item.ID, sha1Digest, a.now()); err != nil {
		return nodeprotocol.StorageUploadFileResult{}, err
	}
	return nodeprotocol.StorageUploadFileResult{SourceFileToken: plan.SourceFileToken, TargetParentID: parentID, TargetName: path.Base(plan.TargetRelativePath), TargetItemID: item.ID, Status: status, Size: plan.Size, SHA1: strings.ToUpper(sha1Digest)}, nil
}

func (a *Agent) pauseStorageUpload(ctx context.Context, serverID string, input nodeprotocol.StorageActionRequest, response nodeprotocol.StorageActionResponse, err error) {
	code := storageErrorCode(err)
	response.ErrorCode = code
	switch code {
	case nodeprotocol.ErrorCredentialExpired:
		response.Status = nodeprotocol.StorageActionWaitingCredentials
	case nodeprotocol.ErrorReconciliationNeeded:
		response.Status = nodeprotocol.StorageActionReconciliation
	default:
		response.Status = nodeprotocol.StorageActionRunning
	}
	_ = a.store.UpdateActionReceipt(ctx, serverID, input.RequestID, response, a.now())
	phase := nodeprotocol.PhaseUploadingTarget
	if code == nodeprotocol.ErrorCredentialExpired {
		phase = nodeprotocol.PhaseWaitingCredentials
	} else if code == nodeprotocol.ErrorReconciliationNeeded {
		phase = nodeprotocol.PhaseVerifyingTarget
	}
	_ = a.store.UpdateOperationExecution(ctx, serverID, input.OperationKey, input.PlanDigest, nodeprotocol.OperationRunning, phase, code, nil, a.now())
}

func ensureStorageParent(ctx context.Context, driver storageUploadDriver, rootID, directory string, guard func(context.Context) error) (string, error) {
	parentID := rootID
	if directory == "." {
		return parentID, nil
	}
	for _, segment := range strings.Split(directory, "/") {
		items, err := listStorageName(ctx, driver, parentID, segment)
		if err != nil {
			return "", err
		}
		folders := make([]cloud.Item, 0, 1)
		for _, item := range items {
			if !item.IsDir {
				return "", errors.New(nodeprotocol.ErrorPlanConflict)
			}
			folders = append(folders, item)
		}
		if len(folders) > 1 {
			return "", errors.New(nodeprotocol.ErrorReconciliationNeeded)
		}
		if len(folders) == 0 {
			if err := guard(ctx); err != nil {
				return "", err
			}
			created, err := driver.CreateDirectory(ctx, parentID, segment)
			if err != nil {
				// A timeout after creation has an unknown mutation result. Re-list
				// exactly once; never create another same-named directory blindly.
				items, listErr := listStorageName(ctx, driver, parentID, segment)
				if listErr != nil || len(items) != 1 || !items[0].IsDir {
					return "", errors.New(nodeprotocol.ErrorReconciliationNeeded)
				}
				created = items[0]
			}
			if created.ID == "" || created.ParentID != parentID || created.Name != segment || !created.IsDir {
				return "", errors.New(nodeprotocol.ErrorReconciliationNeeded)
			}
			verified, statErr := driver.Stat(ctx, created.ID)
			listed, listErr := listStorageName(ctx, driver, parentID, segment)
			if statErr != nil || listErr != nil || verified.ID != created.ID || verified.ParentID != parentID || verified.Name != segment || !verified.IsDir || len(listed) != 1 || listed[0].ID != created.ID || !listed[0].IsDir {
				return "", errors.New(nodeprotocol.ErrorReconciliationNeeded)
			}
			parentID = created.ID
			continue
		}
		parentID = folders[0].ID
	}
	return parentID, nil
}

func listStorageName(ctx context.Context, driver storageUploadDriver, parentID, name string) ([]cloud.Item, error) {
	matches := make([]cloud.Item, 0, 1)
	for offset := int64(0); offset < storageListMaximum; offset += storageListLimit {
		page, err := driver.List(ctx, parentID, cloud.PageRequest{Offset: offset, Limit: storageListLimit})
		if err != nil {
			return nil, err
		}
		for _, item := range page.Items {
			if item.ParentID == parentID && item.Name == name {
				matches = append(matches, item)
			}
		}
		if !page.HasMore {
			return matches, nil
		}
	}
	return nil, errors.New(nodeprotocol.ErrorReconciliationNeeded)
}

func identicalStorageItems(items []cloud.Item, size int64, sha1Digest string) []cloud.Item {
	result := make([]cloud.Item, 0, 1)
	for _, item := range items {
		if !item.IsDir && item.Size == size && strings.EqualFold(strings.TrimSpace(item.SHA1), sha1Digest) {
			result = append(result, item)
		}
	}
	return result
}

func verifyUploadedTarget(ctx context.Context, driver storageUploadDriver, item cloud.Item, parentID, name string, size int64, sha1Digest string) (cloud.Item, error) {
	if item.ID == "" {
		return cloud.Item{}, errors.New("missing item identity")
	}
	stat, err := driver.Stat(ctx, item.ID)
	if err != nil || stat.ID != item.ID || stat.ParentID != parentID || stat.Name != name || stat.IsDir || stat.Size != size || !strings.EqualFold(strings.TrimSpace(stat.SHA1), sha1Digest) {
		return cloud.Item{}, errors.New("target stat mismatch")
	}
	matches, err := listStorageName(ctx, driver, parentID, name)
	if err != nil {
		return cloud.Item{}, err
	}
	identical := identicalStorageItems(matches, size, sha1Digest)
	if len(identical) != 1 || identical[0].ID != stat.ID {
		return cloud.Item{}, errors.New("target listing mismatch")
	}
	return stat, nil
}

func verifyCheckpointTarget(ctx context.Context, driver storageUploadDriver, checkpoint StorageUploadCheckpoint, name string) (nodeprotocol.StorageUploadFileResult, error) {
	item, err := verifyUploadedTarget(ctx, driver, cloud.Item{ID: checkpoint.TargetItemID}, checkpoint.TargetParentID, name, checkpoint.Size, checkpoint.TargetSHA1)
	if err != nil {
		return nodeprotocol.StorageUploadFileResult{}, err
	}
	return nodeprotocol.StorageUploadFileResult{TargetParentID: checkpoint.TargetParentID, TargetName: name, TargetItemID: item.ID, Size: checkpoint.Size, SHA1: strings.ToUpper(checkpoint.TargetSHA1)}, nil
}

func hashStorageSource(ctx context.Context, file io.ReadSeeker, expectedSize int64, expectedSHA256 string) (string, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", errors.New(nodeprotocol.ErrorChecksumMismatch)
	}
	sha256Hash, sha1Hash := sha256.New(), sha1.New()
	written, err := io.Copy(io.MultiWriter(sha256Hash, sha1Hash), &contextReader{ctx: ctx, reader: io.LimitReader(file, expectedSize+1)})
	if err != nil || written != expectedSize || hex.EncodeToString(sha256Hash.Sum(nil)) != expectedSHA256 {
		return "", errors.New(nodeprotocol.ErrorChecksumMismatch)
	}
	return strings.ToUpper(hex.EncodeToString(sha1Hash.Sum(nil))), nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader *contextReader) Read(buffer []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(buffer)
}

func storageErrorCode(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, ErrLeaseConflict) || strings.Contains(err.Error(), nodeprotocol.ErrorLeaseExpired) {
		return nodeprotocol.ErrorLeaseExpired
	}
	if errors.Is(err, ErrPlanConflict) || strings.Contains(err.Error(), nodeprotocol.ErrorPlanConflict) {
		return nodeprotocol.ErrorPlanConflict
	}
	for _, code := range []string{nodeprotocol.ErrorCredentialExpired, nodeprotocol.ErrorReconciliationNeeded, nodeprotocol.ErrorChecksumMismatch, nodeprotocol.ErrorCapabilityMissing} {
		if strings.Contains(err.Error(), code) {
			return code
		}
	}
	code, _ := cloud.ErrorInfo(err)
	switch code {
	case cloud.CodeAuthExpired, cloud.CodeCookieInvalid:
		return nodeprotocol.ErrorCredentialExpired
	case cloud.CodeRateLimited:
		return nodeprotocol.ErrorTargetRateLimited
	case cloud.CodeMutationUnknown:
		return nodeprotocol.ErrorReconciliationNeeded
	case cloud.CodeResponseInvalid:
		return nodeprotocol.ErrorChecksumMismatch
	default:
		return "node_target_unavailable"
	}
}

func writeStorageError(w http.ResponseWriter, err error) {
	code := storageErrorCode(err)
	status := http.StatusConflict
	if code == nodeprotocol.ErrorCredentialExpired {
		status = http.StatusForbidden
	} else if code == "node_target_unavailable" {
		status = http.StatusBadGateway
	}
	writeError(w, status, code, fmt.Sprintf("节点无法接受存储操作：%s", storageSafeMessage(code)))
}

func storageSafeMessage(code string) string {
	switch code {
	case nodeprotocol.ErrorLeaseExpired:
		return "任务租约已经过期"
	case nodeprotocol.ErrorPlanConflict:
		return "任务计划与节点状态不一致"
	case nodeprotocol.ErrorCredentialExpired:
		return "临时凭据已经过期"
	default:
		return "目标存储暂时不可用"
	}
}

var _ storageUploadDriver = (*pan115.Client)(nil)
