package nodeagent

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/pan115"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
)

const storageSourcePageSize = 100

type storageSourceDriver interface {
	cloud.Driver
	cloud.NativeOfflineDriver
	cloud.ShareReceiveDriver
	cloud.TreeStreamDriver
	cloud.ReadDriver
}

type storageSourceDriverFactory func(nodeprotocol.StorageSourceCredential, func(context.Context) error) (storageSourceDriver, error)

func newStorageSourceDriver(credential nodeprotocol.StorageSourceCredential, beforeCall func(context.Context) error) (storageSourceDriver, error) {
	if err := nodeprotocol.ValidateStorageSourceCredential(credential, false); err != nil {
		return nil, err
	}
	driver, err := pan115.New(cloud.Config{Cookie: credential.Cookie, BeforeCall: beforeCall})
	if err != nil {
		return nil, err
	}
	source, ok := driver.(storageSourceDriver)
	capabilities := driver.Capabilities()
	if !ok || !capabilities.NativeOfflineDownload || !capabilities.DirectoryList {
		return nil, errors.New(nodeprotocol.ErrorCapabilityMissing)
	}
	return source, nil
}

func (a *Agent) storageSourceAction(w http.ResponseWriter, r *http.Request) {
	var input nodeprotocol.StorageSourceActionRequest
	if err := decodeJSON(w, r, &input); err != nil {
		return
	}
	if err := input.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "来源存储操作无效")
		return
	}
	digest, err := input.Digest()
	if err != nil {
		writeError(w, http.StatusBadRequest, "node_storage_source_request_invalid", "来源存储操作校验失败")
		return
	}
	serverID := r.Header.Get("X-OhMyCine-Verified-Server-ID")
	if receipt, getErr := a.store.ProviderReceipt(r.Context(), serverID, input.RequestID); getErr == nil {
		if receipt.RequestDigest != digest || receipt.OperationKey != input.OperationKey || receipt.Action != input.Action {
			writeError(w, http.StatusConflict, nodeprotocol.ErrorPlanConflict, "相同操作编号对应了不同请求")
			return
		}
		if receipt.Status == "completed" {
			var response nodeprotocol.StorageSourceActionResponse
			if json.Unmarshal([]byte(receipt.ResponseJSON), &response) == nil {
				if response.FileExport != nil && !response.FileExport.ExpiresAt.After(a.now().Add(2*time.Minute)) {
					if renewed, renewErr := a.store.RenewFileExport(r.Context(), serverID, input.OperationKey, input.TaskID, a.now(), a.now().Add(fileExportLifetime)); renewErr == nil {
						response.FileExport = &renewed
						_ = a.store.RefreshCompletedActionReceipt(r.Context(), serverID, input.RequestID, input.OperationKey, response, a.now())
					}
				}
				if response.FileExport != nil && response.FileExport.ExpiresAt.After(a.now()) {
					writeJSON(w, http.StatusOK, response)
					return
				}
			}
		}
	}
	plan, err := a.storageSourcePlan(r.Context(), serverID, input)
	if err != nil {
		writeStorageSourceError(w, err)
		return
	}
	run, err := a.store.PrepareStorageSourceRun(r.Context(), serverID, input, a.now())
	if err != nil {
		writeStorageSourceError(w, err)
		return
	}
	grant, credential, err := a.storageSourceCredential(r.Context(), serverID, input, plan, run)
	if err != nil {
		writeError(w, http.StatusForbidden, storageSourceErrorCode(err), "来源存储临时授权无效或已经过期")
		return
	}
	receipt, created, err := a.store.BeginActionReceipt(r.Context(), serverID, input.RequestID, input.OperationKey, input.Action, digest, a.now())
	if errors.Is(err, ErrPlanConflict) {
		writeError(w, http.StatusConflict, nodeprotocol.ErrorPlanConflict, "相同操作编号对应了不同请求")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "node_persistence_failed", "节点无法保存来源存储操作")
		return
	}
	response := sourceResponse(input, run)
	if receipt.ResponseJSON != "" {
		_ = json.Unmarshal([]byte(receipt.ResponseJSON), &response)
	}
	if created || receipt.ResponseJSON == "" {
		if err := a.store.UpdateActionReceipt(r.Context(), serverID, input.RequestID, response, a.now()); err != nil {
			writeError(w, http.StatusInternalServerError, "node_persistence_failed", "节点无法保存来源存储操作")
			return
		}
	}
	started := a.startStorageSource(serverID, input, plan, grant, credential)
	if started && response.Status == nodeprotocol.StorageSourceAccepted {
		response.Status = nodeprotocol.StorageSourceSubmitting
	}
	writeJSON(w, http.StatusAccepted, response)
}

func (a *Agent) storageSourceActionResult(w http.ResponseWriter, r *http.Request) {
	requestID := r.PathValue("request_id")
	taskID := strings.TrimSpace(r.Header.Get(storageTaskHeader))
	operationKey := strings.TrimSpace(r.Header.Get(storageOperationHeader))
	if requestID == "" || taskID == "" || operationKey == "" {
		writeError(w, http.StatusBadRequest, "node_storage_source_request_invalid", "来源存储结果查询无效")
		return
	}
	serverID := r.Header.Get("X-OhMyCine-Verified-Server-ID")
	receipt, err := a.store.ProviderReceipt(r.Context(), serverID, requestID)
	if errors.Is(err, ErrOperationNotFound) || err == nil && (receipt.OperationKey != operationKey || receipt.Action != nodeprotocol.StorageSourceActionMaterialize) {
		writeError(w, http.StatusNotFound, "node_storage_source_result_not_found", "来源存储操作结果不存在")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "node_persistence_failed", "节点无法读取来源存储操作结果")
		return
	}
	if matches, checkErr := a.store.OperationTaskMatches(r.Context(), serverID, operationKey, taskID); checkErr != nil || !matches {
		writeError(w, http.StatusNotFound, "node_storage_source_result_not_found", "来源存储操作结果不存在")
		return
	}
	var response nodeprotocol.StorageSourceActionResponse
	if receipt.ResponseJSON == "" || json.Unmarshal([]byte(receipt.ResponseJSON), &response) != nil {
		response = nodeprotocol.StorageSourceActionResponse{RequestID: requestID, OperationKey: operationKey, Status: nodeprotocol.StorageSourceAccepted}
	}
	writeJSON(w, http.StatusOK, response)
}

func (a *Agent) storageSourcePlan(ctx context.Context, serverID string, input nodeprotocol.StorageSourceActionRequest) (nodeprotocol.StorageSourcePlan, error) {
	operation, err := a.store.ActiveOperationPlan(ctx, serverID, input.OperationKey, input.PlanDigest, a.now())
	if err != nil {
		return nodeprotocol.StorageSourcePlan{}, err
	}
	if operation.Kind != nodeprotocol.OperationKindStorageSourceMaterialize || operation.TaskID != input.TaskID {
		return nodeprotocol.StorageSourcePlan{}, ErrPlanConflict
	}
	var plan nodeprotocol.StorageSourcePlan
	decoder := json.NewDecoder(bytes.NewReader(operation.Payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&plan); err != nil || plan.Validate() != nil || plan.StorageID != input.StorageID {
		return nodeprotocol.StorageSourcePlan{}, ErrPlanConflict
	}
	return plan, nil
}

func (a *Agent) storageSourceCredential(ctx context.Context, serverID string, input nodeprotocol.StorageSourceActionRequest, plan nodeprotocol.StorageSourcePlan, run StorageSourceRun) (nodeprotocol.CredentialGrant, nodeprotocol.StorageSourceCredential, error) {
	envelope, err := a.store.CredentialGrant(ctx, serverID, input.GrantID, a.now())
	if err != nil {
		return nodeprotocol.CredentialGrant{}, nodeprotocol.StorageSourceCredential{}, errors.New(nodeprotocol.ErrorCredentialExpired)
	}
	grant, err := nodeprotocol.OpenCredentialGrant(a.sealingPrivateKey, envelope, a.now())
	if err != nil {
		return nodeprotocol.CredentialGrant{}, nodeprotocol.StorageSourceCredential{}, err
	}
	if grant.NodeID != a.config.NodeID || grant.TaskID != input.TaskID || grant.OperationKey != input.OperationKey || grant.ResourceKind != nodeprotocol.ResourceKindStorage || grant.ResourceID != input.StorageID {
		return nodeprotocol.CredentialGrant{}, nodeprotocol.StorageSourceCredential{}, errors.New("node_credential_binding_mismatch")
	}
	requireURI := run.OutputRootID == "" && (nodeprotocol.StorageSourceIsShare(plan.SourceKind) || run.ProviderTaskID == "")
	var requiredActions []string
	switch plan.SourceKind {
	case nodeprotocol.StorageSourceKindPan115OfflineMagnet:
		requiredActions = []string{nodeprotocol.StorageSourceGrantOfflineStatus}
		if run.ProviderTaskID == "" {
			requiredActions = append(requiredActions, nodeprotocol.StorageSourceGrantOfflineSubmit)
		}
		if run.OutputRootID != "" {
			requiredActions = []string{nodeprotocol.StorageSourceGrantRead}
		}
	case nodeprotocol.StorageSourceKindPan115Share, nodeprotocol.StorageSourceKindPan115ShareSelected:
		if run.OutputRootID == "" {
			requiredActions = []string{nodeprotocol.StorageSourceGrantShareInspect, nodeprotocol.StorageSourceGrantShareReceive, nodeprotocol.StorageSourceGrantRead}
		} else {
			requiredActions = []string{nodeprotocol.StorageSourceGrantRead}
		}
	default:
		return nodeprotocol.CredentialGrant{}, nodeprotocol.StorageSourceCredential{}, errors.New(nodeprotocol.ErrorCapabilityMissing)
	}
	for _, action := range requiredActions {
		if !containsAction(grant.AllowedActions, action) {
			return nodeprotocol.CredentialGrant{}, nodeprotocol.StorageSourceCredential{}, errors.New("node_credential_binding_mismatch")
		}
	}
	var credential nodeprotocol.StorageSourceCredential
	decoder := json.NewDecoder(bytes.NewReader(grant.Credential))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&credential); err != nil || nodeprotocol.ValidateStorageSourceCredential(credential, requireURI) != nil {
		return nodeprotocol.CredentialGrant{}, nodeprotocol.StorageSourceCredential{}, errors.New("node_credential_payload_invalid")
	}
	if requireURI || strings.TrimSpace(credential.SourceURI) != "" {
		contentDigest, digestErr := nodeprotocol.StorageSourceContentDigest(plan.SourceKind, credential.SourceURI)
		if digestErr != nil || contentDigest != plan.SourceContentDigest {
			return nodeprotocol.CredentialGrant{}, nodeprotocol.StorageSourceCredential{}, errors.New("node_credential_payload_invalid")
		}
	}
	return grant, credential, nil
}

func (a *Agent) startStorageSource(serverID string, input nodeprotocol.StorageSourceActionRequest, plan nodeprotocol.StorageSourcePlan, grant nodeprotocol.CredentialGrant, credential nodeprotocol.StorageSourceCredential) bool {
	a.sourceMu.Lock()
	if _, running := a.sourceRuns[input.OperationKey]; running {
		a.sourceMu.Unlock()
		return false
	}
	a.sourceRuns[input.OperationKey] = struct{}{}
	a.sourceMu.Unlock()
	go func() {
		defer func() {
			a.sourceMu.Lock()
			delete(a.sourceRuns, input.OperationKey)
			a.sourceMu.Unlock()
		}()
		a.runStorageSource(context.Background(), serverID, input, plan, grant, credential)
	}()
	return true
}

func (a *Agent) runStorageSource(ctx context.Context, serverID string, input nodeprotocol.StorageSourceActionRequest, plan nodeprotocol.StorageSourcePlan, grant nodeprotocol.CredentialGrant, credential nodeprotocol.StorageSourceCredential) {
	run, err := a.store.StorageSourceRun(ctx, serverID, input.OperationKey)
	if err != nil {
		return
	}
	response := sourceResponse(input, run)
	response.Status = nodeprotocol.StorageSourceSubmitting
	_ = a.store.UpdateActionReceipt(ctx, serverID, input.RequestID, response, a.now())
	_ = a.store.UpdateOperationExecution(ctx, serverID, input.OperationKey, input.PlanDigest, nodeprotocol.OperationRunning, nodeprotocol.PhasePullingSource, "", nil, a.now())
	baseGuard := func(callCtx context.Context) error {
		if !grant.ExpiresAt.After(a.now()) {
			return cloud.Error(cloud.CodeAuthExpired, false, errors.New(nodeprotocol.ErrorCredentialExpired))
		}
		_, err := a.store.ActiveOperationPlan(callCtx, serverID, input.OperationKey, input.PlanDigest, a.now())
		if errors.Is(err, ErrLeaseConflict) {
			return cloud.Error(cloud.CodeUnavailable, true, errors.New(nodeprotocol.ErrorLeaseExpired))
		}
		return err
	}
	guard := func(callCtx context.Context, action string) error {
		if !containsAction(grant.AllowedActions, action) {
			return errors.New("node_credential_binding_mismatch")
		}
		return baseGuard(callCtx)
	}
	driver, err := a.sourceFactory(credential, baseGuard)
	if err != nil {
		a.pauseStorageSource(ctx, serverID, input, response, err)
		return
	}
	if driver.Provider() != plan.ProviderType {
		a.pauseStorageSource(ctx, serverID, input, response, errors.New(nodeprotocol.ErrorCapabilityMissing))
		return
	}
	if nodeprotocol.StorageSourceIsShare(plan.SourceKind) {
		if run.OutputRootID == "" {
			if err := a.materializeStorageSourceShare(ctx, serverID, input, plan, run, credential, driver, guard); err != nil {
				a.pauseStorageSource(ctx, serverID, input, response, err)
				return
			}
			run, err = a.store.StorageSourceRun(ctx, serverID, input.OperationKey)
			if err != nil {
				return
			}
		}
	} else if run.ProviderTaskID == "" {
		providerID, parseErr := offlineSourceTaskID(credential.SourceURI)
		if parseErr != nil {
			a.pauseStorageSource(ctx, serverID, input, response, parseErr)
			return
		}
		if err := a.store.UpdateStorageSourceRun(ctx, serverID, input.OperationKey, input.PlanDigest, nodeprotocol.StorageSourceSubmitting, providerID, "", a.now()); err != nil {
			a.pauseStorageSource(ctx, serverID, input, response, err)
			return
		}
		run.ProviderTaskID = providerID
		if err := guard(ctx, nodeprotocol.StorageSourceGrantOfflineStatus); err != nil {
			a.pauseStorageSource(ctx, serverID, input, response, err)
			return
		}
		task, getErr := driver.GetOffline(ctx, providerID)
		if getErr != nil {
			code, _ := cloud.ErrorInfo(getErr)
			if code != cloud.CodeNotFound {
				a.pauseStorageSource(ctx, serverID, input, response, getErr)
				return
			}
			if err := guard(ctx, nodeprotocol.StorageSourceGrantOfflineSubmit); err != nil {
				a.pauseStorageSource(ctx, serverID, input, response, err)
				return
			}
			task, err = driver.SubmitOffline(ctx, credential.SourceURI, plan.OfflineDestinationID)
			if err != nil {
				a.pauseStorageSource(ctx, serverID, input, response, err)
				return
			}
		}
		if !strings.EqualFold(task.ID, providerID) {
			a.pauseStorageSource(ctx, serverID, input, response, errors.New(nodeprotocol.ErrorReconciliationNeeded))
			return
		}
	}
	if plan.SourceKind == nodeprotocol.StorageSourceKindPan115OfflineMagnet && run.OutputRootID == "" {
		if err := guard(ctx, nodeprotocol.StorageSourceGrantOfflineStatus); err != nil {
			a.pauseStorageSource(ctx, serverID, input, response, err)
			return
		}
		task, err := driver.GetOffline(ctx, run.ProviderTaskID)
		if err != nil {
			a.pauseStorageSource(ctx, serverID, input, response, err)
			return
		}
		response.ProviderStatus = task.Status
		response.Progress = task.Progress
		if task.Failed {
			response.Status = nodeprotocol.StorageSourceFailed
			response.ErrorCode = "node_source_offline_failed"
			_ = a.store.UpdateActionReceipt(ctx, serverID, input.RequestID, response, a.now())
			_ = a.store.UpdateOperationExecution(ctx, serverID, input.OperationKey, input.PlanDigest, nodeprotocol.OperationFailed, nodeprotocol.PhaseWaitingDownload, response.ErrorCode, task.Progress, a.now())
			return
		}
		if !task.Completed || task.OutputItemID == "" {
			response.Status = nodeprotocol.StorageSourceWaiting
			_ = a.store.UpdateActionReceipt(ctx, serverID, input.RequestID, response, a.now())
			_ = a.store.UpdateStorageSourceRun(ctx, serverID, input.OperationKey, input.PlanDigest, nodeprotocol.StorageSourceWaiting, "", "", a.now())
			_ = a.store.UpdateOperationExecution(ctx, serverID, input.OperationKey, input.PlanDigest, nodeprotocol.OperationRunning, nodeprotocol.PhaseWaitingDownload, "", task.Progress, a.now())
			return
		}
		if err := a.store.UpdateStorageSourceRun(ctx, serverID, input.OperationKey, input.PlanDigest, nodeprotocol.StorageSourceEnumerating, "", task.OutputItemID, a.now()); err != nil {
			a.pauseStorageSource(ctx, serverID, input, response, err)
			return
		}
		run.OutputRootID = task.OutputItemID
	}
	if err := guard(ctx, nodeprotocol.StorageSourceGrantRead); err != nil {
		a.pauseStorageSource(ctx, serverID, input, response, err)
		return
	}
	if !run.ManifestReady {
		if err := a.enumerateStorageSource(ctx, serverID, input, plan, run, driver, guard); err != nil {
			a.pauseStorageSource(ctx, serverID, input, response, err)
			return
		}
		run, err = a.store.StorageSourceRun(ctx, serverID, input.OperationKey)
		if err != nil {
			return
		}
	}
	response.Status = nodeprotocol.StorageSourceDownloading
	for after := int64(-1); ; {
		files, err := a.store.StorageSourceFiles(ctx, input.OperationKey, after, storageSourcePageSize)
		if err != nil {
			a.pauseStorageSource(ctx, serverID, input, response, err)
			return
		}
		if len(files) == 0 {
			break
		}
		for _, file := range files {
			if err := guard(ctx, nodeprotocol.StorageSourceGrantRead); err != nil {
				a.pauseStorageSource(ctx, serverID, input, response, err)
				return
			}
			if err := a.downloadStorageSourceFile(ctx, serverID, input, file, driver, guard); err != nil {
				a.pauseStorageSource(ctx, serverID, input, response, err)
				return
			}
			after = file.Ordinal
			run, _ = a.store.StorageSourceRun(ctx, serverID, input.OperationKey)
			response = sourceResponse(input, run)
			response.Status = nodeprotocol.StorageSourceDownloading
			_ = a.store.UpdateActionReceipt(ctx, serverID, input.RequestID, response, a.now())
			progress := sourceProgress(run)
			_ = a.store.UpdateOperationExecution(ctx, serverID, input.OperationKey, input.PlanDigest, nodeprotocol.OperationRunning, nodeprotocol.PhasePullingSource, "", progress, a.now())
		}
	}
	if err := guard(ctx, nodeprotocol.StorageSourceGrantRead); err != nil {
		a.pauseStorageSource(ctx, serverID, input, response, err)
		return
	}
	export, err := a.publishStorageSourceExport(ctx, serverID, input, run)
	if err != nil {
		a.pauseStorageSource(ctx, serverID, input, response, err)
		return
	}
	if err := guard(ctx, nodeprotocol.StorageSourceGrantRead); err != nil {
		a.pauseStorageSource(ctx, serverID, input, response, err)
		return
	}
	run, _ = a.store.StorageSourceRun(ctx, serverID, input.OperationKey)
	response = sourceResponse(input, run)
	response.Status = nodeprotocol.StorageSourceCompleted
	response.FileExport = &export
	_ = a.store.CompleteStorageSourceAction(ctx, serverID, input.RequestID, input.OperationKey, input.PlanDigest, response, a.now())
}

const storageSourceShareItemLimit = 1000

func (a *Agent) materializeStorageSourceShare(ctx context.Context, serverID string, input nodeprotocol.StorageSourceActionRequest, plan nodeprotocol.StorageSourcePlan, run StorageSourceRun, credential nodeprotocol.StorageSourceCredential, driver storageSourceDriver, guard func(context.Context, string) error) error {
	if plan.SourceKind == nodeprotocol.StorageSourceKindPan115ShareSelected {
		for _, action := range []string{nodeprotocol.StorageSourceGrantShareInspect, nodeprotocol.StorageSourceGrantShareReceive, nodeprotocol.StorageSourceGrantRead} {
			if err := guard(ctx, action); err != nil {
				return err
			}
		}
		browse, ok := driver.(cloud.ShareBrowseDriver)
		if !ok {
			return errors.New(nodeprotocol.ErrorCapabilityMissing)
		}
		source, err := cloud.DecodeSelectedShareSource(credential.SourceURI)
		if err != nil {
			return err
		}
		if err := cloud.ReceiveSelectedShare(ctx, browse, source.URL, source.Selection, plan.OfflineDestinationID); err != nil {
			return err
		}
		return a.store.UpdateStorageSourceRun(ctx, serverID, input.OperationKey, input.PlanDigest, nodeprotocol.StorageSourceEnumerating, "", plan.OfflineDestinationID, a.now())
	}
	if err := guard(ctx, nodeprotocol.StorageSourceGrantShareInspect); err != nil {
		return err
	}
	snapshot, err := driver.InspectShare(ctx, credential.SourceURI)
	if err != nil {
		return err
	}
	sourceIdentity, err := nodeprotocol.CanonicalStorageSourceIdentity(plan.SourceKind, credential.SourceURI)
	if err != nil || snapshot.ShareCode != sourceIdentity {
		return errors.New(nodeprotocol.ErrorReconciliationNeeded)
	}
	receiptDigest, expected, err := storageSourceShareReceipt(snapshot)
	if err != nil {
		return err
	}
	receiptID := "share:" + receiptDigest
	if run.ProviderTaskID == "" {
		if err := a.store.UpdateStorageSourceRun(ctx, serverID, input.OperationKey, input.PlanDigest, nodeprotocol.StorageSourceSubmitting, receiptID, "", a.now()); err != nil {
			return err
		}
	} else if run.ProviderTaskID != receiptID {
		return ErrPlanConflict
	}
	matched, empty, err := reconcileStorageSourceShare(ctx, driver, plan.OfflineDestinationID, expected, guard)
	if err != nil {
		return err
	}
	if !matched {
		if !empty {
			return errors.New(nodeprotocol.ErrorReconciliationNeeded)
		}
		if err := guard(ctx, nodeprotocol.StorageSourceGrantShareReceive); err != nil {
			return err
		}
		receiveErr := driver.ReceiveShare(ctx, snapshot, plan.OfflineDestinationID)
		matched, _, reconcileErr := reconcileStorageSourceShare(ctx, driver, plan.OfflineDestinationID, expected, guard)
		if reconcileErr != nil {
			return reconcileErr
		}
		if !matched {
			if receiveErr != nil {
				return errors.New(nodeprotocol.ErrorReconciliationNeeded)
			}
			return errors.New(nodeprotocol.ErrorReconciliationNeeded)
		}
	}
	return a.store.UpdateStorageSourceRun(ctx, serverID, input.OperationKey, input.PlanDigest, nodeprotocol.StorageSourceEnumerating, "", plan.OfflineDestinationID, a.now())
}

type storageSourceShareItem struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size"`
}

func storageSourceShareReceipt(snapshot cloud.ShareSnapshot) (string, map[string]storageSourceShareItem, error) {
	if snapshot.ShareCode == "" || len(snapshot.Items) == 0 || len(snapshot.Items) > storageSourceShareItemLimit {
		return "", nil, errors.New("node_storage_source_share_invalid")
	}
	items := make([]storageSourceShareItem, 0, len(snapshot.Items))
	expected := make(map[string]storageSourceShareItem, len(snapshot.Items))
	for _, item := range snapshot.Items {
		name := strings.TrimSpace(item.Name)
		key := strings.ToLower(name)
		if !validStorageSourceProviderID(item.ID) || name != item.Name || normalizedExportPath(name) != name || key == "" || item.Size < 0 {
			return "", nil, errors.New("node_storage_source_share_invalid")
		}
		entry := storageSourceShareItem{ID: item.ID, Name: name, IsDir: item.IsDir, Size: item.Size}
		if _, duplicate := expected[key]; duplicate {
			return "", nil, errors.New("node_storage_source_share_invalid")
		}
		expected[key] = entry
		items = append(items, entry)
	}
	sort.Slice(items, func(left, right int) bool {
		if items[left].ID == items[right].ID {
			return items[left].Name < items[right].Name
		}
		return items[left].ID < items[right].ID
	})
	raw, err := json.Marshal(struct {
		ShareCode string                   `json:"share_code"`
		Items     []storageSourceShareItem `json:"items"`
	}{ShareCode: snapshot.ShareCode, Items: items})
	if err != nil {
		return "", nil, err
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), expected, nil
}

func reconcileStorageSourceShare(ctx context.Context, driver storageSourceDriver, destinationID string, expected map[string]storageSourceShareItem, guard func(context.Context, string) error) (matched, empty bool, err error) {
	if err := guard(ctx, nodeprotocol.StorageSourceGrantRead); err != nil {
		return false, false, err
	}
	root, err := driver.Stat(ctx, destinationID)
	if err != nil || root.ID != destinationID || !root.IsDir {
		return false, false, errors.New(nodeprotocol.ErrorReconciliationNeeded)
	}
	actual := make(map[string]cloud.Item, min(len(expected), storageSourceShareItemLimit))
	for offset := int64(0); ; {
		if err := guard(ctx, nodeprotocol.StorageSourceGrantRead); err != nil {
			return false, false, err
		}
		page, listErr := driver.List(ctx, destinationID, cloud.PageRequest{Offset: offset, Limit: 250})
		if listErr != nil {
			return false, false, listErr
		}
		if page.Offset != offset || page.HasMore && len(page.Items) == 0 {
			return false, false, errors.New(nodeprotocol.ErrorReconciliationNeeded)
		}
		for _, item := range page.Items {
			key := strings.ToLower(strings.TrimSpace(item.Name))
			if item.ParentID != destinationID || key == "" {
				return false, false, errors.New(nodeprotocol.ErrorReconciliationNeeded)
			}
			if _, duplicate := actual[key]; duplicate {
				return false, false, errors.New(nodeprotocol.ErrorReconciliationNeeded)
			}
			actual[key] = item
			if len(actual) > storageSourceShareItemLimit || len(actual) > len(expected) {
				return false, false, nil
			}
		}
		if !page.HasMore {
			break
		}
		offset += int64(len(page.Items))
	}
	if len(actual) == 0 {
		return false, true, nil
	}
	if len(actual) != len(expected) {
		return false, false, nil
	}
	for key, want := range expected {
		got, ok := actual[key]
		if !ok || got.Name != want.Name || got.IsDir != want.IsDir || !want.IsDir && got.Size != want.Size {
			return false, false, nil
		}
	}
	return true, false, nil
}

func (a *Agent) enumerateStorageSource(ctx context.Context, serverID string, input nodeprotocol.StorageSourceActionRequest, plan nodeprotocol.StorageSourcePlan, run StorageSourceRun, driver storageSourceDriver, guard func(context.Context, string) error) error {
	if err := a.store.ResetStorageSourceManifest(ctx, serverID, input.OperationKey, input.PlanDigest, a.now()); err != nil {
		return err
	}
	if err := guard(ctx, nodeprotocol.StorageSourceGrantRead); err != nil {
		return err
	}
	root, err := driver.Stat(ctx, run.OutputRootID)
	if err != nil {
		return err
	}
	if err := validateStorageSourceRoot(plan, run, root); err != nil {
		return err
	}
	hasher := sha256.New()
	var ordinal, totalBytes int64
	save := func(entries []cloud.TreeEntry) error {
		files := make([]StorageSourceFile, 0, len(entries))
		for _, entry := range entries {
			if entry.IsDir {
				continue
			}
			relative := normalizedExportPath(strings.TrimPrefix(entry.RelativePath, "/"))
			if relative == "" || entry.ID == "" || entry.Size <= 0 || !validSHA1(entry.SHA1) || totalBytes > int64(^uint64(0)>>1)-entry.Size {
				return errors.New("node_storage_source_manifest_invalid")
			}
			file := StorageSourceFile{OperationKey: input.OperationKey, Ordinal: ordinal, ProviderFileID: entry.ID, RelativePath: relative, Size: entry.Size, SourceSHA1: strings.ToUpper(entry.SHA1), ChunkCount: int((entry.Size + nodeprotocol.FileChunkSize - 1) / nodeprotocol.FileChunkSize)}
			manifestHashFile(hasher, file)
			files = append(files, file)
			ordinal++
			totalBytes += entry.Size
		}
		return a.store.SaveStorageSourceFileBatch(ctx, input.OperationKey, files, a.now())
	}
	if root.IsDir {
		maxEntries := int(^uint(0) >> 1)
		err = driver.StreamTree(ctx, run.OutputRootID, maxEntries, func(batch cloud.TreeBatch) error {
			if batch.Partial {
				return errors.New("node_storage_source_manifest_incomplete")
			}
			if err := guard(ctx, nodeprotocol.StorageSourceGrantRead); err != nil {
				return err
			}
			return save(batch.Entries)
		})
	} else {
		err = save([]cloud.TreeEntry{{Item: root, RelativePath: root.Name}})
	}
	if err != nil {
		return err
	}
	if ordinal == 0 {
		return errors.New("node_storage_source_manifest_empty")
	}
	manifestDigest := hex.EncodeToString(hasher.Sum(nil))
	return a.store.FinalizeStorageSourceManifest(ctx, serverID, input.OperationKey, input.PlanDigest, manifestDigest, ordinal, totalBytes, a.now())
}

func validateStorageSourceRoot(plan nodeprotocol.StorageSourcePlan, run StorageSourceRun, root cloud.Item) error {
	if root.ID != run.OutputRootID || root.Name == "" {
		return errors.New(nodeprotocol.ErrorReconciliationNeeded)
	}
	if nodeprotocol.StorageSourceIsShare(plan.SourceKind) {
		if !root.IsDir || root.ID != plan.OfflineDestinationID {
			return errors.New(nodeprotocol.ErrorReconciliationNeeded)
		}
		return nil
	}
	if root.ID == plan.OfflineDestinationID || root.ParentID != plan.OfflineDestinationID {
		return errors.New(nodeprotocol.ErrorReconciliationNeeded)
	}
	return nil
}

func (a *Agent) downloadStorageSourceFile(ctx context.Context, serverID string, input nodeprotocol.StorageSourceActionRequest, file StorageSourceFile, driver storageSourceDriver, guard func(context.Context, string) error) error {
	root, err := a.storageSourceRoot(input.OperationKey)
	if err != nil {
		return err
	}
	finalPath, partialPath, err := secureStorageSourcePaths(root, file.RelativePath, file.Ordinal)
	if err != nil {
		return err
	}
	if file.Status == "completed" {
		sha256Digest, sha1Digest, verifyErr := hashSourcePath(ctx, finalPath, file.Size)
		if verifyErr == nil && sha256Digest == file.SHA256 && strings.EqualFold(sha1Digest, file.SourceSHA1) {
			return guard(ctx, nodeprotocol.StorageSourceGrantRead)
		}
		return errors.New(nodeprotocol.ErrorChecksumMismatch)
	}
	if info, statErr := os.Lstat(finalPath); statErr == nil {
		if !info.Mode().IsRegular() || info.Size() != file.Size {
			return errors.New(nodeprotocol.ErrorChecksumMismatch)
		}
		sha256Digest, sha1Digest, verifyErr := hashSourcePath(ctx, finalPath, file.Size)
		if verifyErr != nil || !strings.EqualFold(sha1Digest, file.SourceSHA1) {
			return errors.New(nodeprotocol.ErrorChecksumMismatch)
		}
		if err := guard(ctx, nodeprotocol.StorageSourceGrantRead); err != nil {
			return err
		}
		_ = os.Remove(partialPath)
		return a.store.CompleteStorageSourceFile(ctx, serverID, input.OperationKey, input.PlanDigest, file.Ordinal, sha256Digest, a.now())
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return errors.New(nodeprotocol.ErrorChecksumMismatch)
	}
	handle, err := openStorageSourcePartial(partialPath, file.Size)
	if err != nil {
		return err
	}
	chunks, err := a.store.StorageSourceChunks(ctx, input.OperationKey, file.Ordinal)
	if err != nil {
		_ = handle.Close()
		return err
	}
	buffer := make([]byte, nodeprotocol.FileChunkSize)
	for chunkIndex := 0; chunkIndex < file.ChunkCount; chunkIndex++ {
		offset := int64(chunkIndex) * nodeprotocol.FileChunkSize
		size := min(nodeprotocol.FileChunkSize, file.Size-offset)
		if checkpoint, ok := chunks[chunkIndex]; ok && checkpoint.Offset == offset && checkpoint.Size == size {
			local := buffer[:size]
			if _, readErr := handle.ReadAt(local, offset); readErr == nil {
				digest := sha256.Sum256(local)
				if hex.EncodeToString(digest[:]) == checkpoint.SHA256 {
					continue
				}
			}
			if err := a.store.DeleteStorageSourceChunk(ctx, input.OperationKey, file.Ordinal, chunkIndex); err != nil {
				_ = handle.Close()
				return err
			}
		}
		read, err := driver.OpenRead(ctx, cloud.ReadRequest{FileID: file.ProviderFileID, Offset: offset})
		if err != nil {
			_ = handle.Close()
			return err
		}
		if read.TotalSize == nil || *read.TotalSize != file.Size || offset > 0 && !read.OffsetAccepted {
			_ = read.Body.Close()
			_ = handle.Close()
			return errors.New(nodeprotocol.ErrorReconciliationNeeded)
		}
		payload := buffer[:size]
		_, readErr := io.ReadFull(read.Body, payload)
		closeErr := read.Body.Close()
		if readErr != nil || closeErr != nil {
			_ = handle.Close()
			return errors.New("node_source_read_incomplete")
		}
		if _, err := handle.WriteAt(payload, offset); err != nil || handle.Sync() != nil {
			_ = handle.Close()
			return errors.New("node_source_staging_write_failed")
		}
		digest := sha256.Sum256(payload)
		checkpoint := nodeprotocol.FileChunkDigest{Index: chunkIndex, Offset: offset, Size: size, SHA256: hex.EncodeToString(digest[:])}
		if err := a.store.SaveStorageSourceChunk(ctx, input.OperationKey, file.Ordinal, checkpoint, a.now()); err != nil {
			_ = handle.Close()
			return err
		}
	}
	if err := handle.Sync(); err != nil {
		_ = handle.Close()
		return err
	}
	if err := handle.Close(); err != nil {
		return err
	}
	sha256Digest, sha1Digest, err := hashSourcePath(ctx, partialPath, file.Size)
	if err != nil || !strings.EqualFold(sha1Digest, file.SourceSHA1) {
		return errors.New(nodeprotocol.ErrorChecksumMismatch)
	}
	if err := guard(ctx, nodeprotocol.StorageSourceGrantRead); err != nil {
		return err
	}
	// Linking inside the same managed filesystem publishes atomically without
	// replacing a concurrently-created destination. The partial remains the
	// operation's recovery copy until the link succeeds.
	if err := os.Link(partialPath, finalPath); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return errors.New("node_source_publish_failed")
		}
		publishedSHA256, publishedSHA1, verifyErr := hashSourcePath(ctx, finalPath, file.Size)
		if verifyErr != nil || publishedSHA256 != sha256Digest || !strings.EqualFold(publishedSHA1, file.SourceSHA1) {
			return errors.New(nodeprotocol.ErrorChecksumMismatch)
		}
	}
	// A valid final hardlink is authoritative. Failure to remove the task-owned
	// recovery copy must not make a retry redownload or republish the file; the
	// exact operation root remains eligible for later managed cleanup.
	_ = os.Remove(partialPath)
	return a.store.CompleteStorageSourceFile(ctx, serverID, input.OperationKey, input.PlanDigest, file.Ordinal, sha256Digest, a.now())
}

func (a *Agent) publishStorageSourceExport(ctx context.Context, serverID string, input nodeprotocol.StorageSourceActionRequest, run StorageSourceRun) (nodeprotocol.FileExportSummary, error) {
	now := a.now()
	if summary, err := a.store.RenewFileExport(ctx, serverID, input.OperationKey, input.TaskID, now, now.Add(fileExportLifetime)); err == nil {
		return summary, nil
	}
	current, err := a.store.StorageSourceRun(ctx, serverID, input.OperationKey)
	if err != nil || !current.ManifestReady || current.TotalFiles < 1 || current.DownloadedFiles != current.TotalFiles || current.DownloadedBytes != current.TotalBytes {
		return nodeprotocol.FileExportSummary{}, errors.New("node_storage_source_export_incomplete")
	}
	hasher := sha256.New()
	for after := int64(-1); ; {
		files, err := a.store.StorageSourceFiles(ctx, input.OperationKey, after, storageSourcePageSize)
		if err != nil {
			return nodeprotocol.FileExportSummary{}, err
		}
		if len(files) == 0 {
			break
		}
		for _, file := range files {
			chunks, err := a.store.StorageSourceChunks(ctx, input.OperationKey, file.Ordinal)
			if err != nil || validateStorageSourceChunks(file, chunks) != nil || !nodeprotocol.ValidDigest(file.SHA256) {
				return nodeprotocol.FileExportSummary{}, errors.New("node_storage_source_export_incomplete")
			}
			_, _ = io.WriteString(hasher, file.RelativePath+"\x00"+strconv.FormatInt(file.Size, 10)+"\x00"+file.SHA256+"\n")
			for index := 0; index < file.ChunkCount; index++ {
				chunk := chunks[index]
				_, _ = io.WriteString(hasher, strconv.Itoa(chunk.Index)+":"+strconv.FormatInt(chunk.Offset, 10)+":"+strconv.FormatInt(chunk.Size, 10)+":"+chunk.SHA256+"\n")
			}
			after = file.Ordinal
		}
	}
	root, err := a.storageSourceRoot(input.OperationKey)
	if err != nil {
		return nodeprotocol.FileExportSummary{}, err
	}
	summary := nodeprotocol.FileExportSummary{OperationKey: input.OperationKey, TaskID: input.TaskID, Name: input.TaskID, ManifestDigest: hex.EncodeToString(hasher.Sum(nil)), TotalFiles: int(current.TotalFiles), TotalBytes: current.TotalBytes, ChunkSize: nodeprotocol.FileChunkSize, CreatedAt: now.UTC(), ExpiresAt: now.Add(fileExportLifetime).UTC()}
	tx, err := a.store.db.BeginTx(ctx, nil)
	if err != nil {
		return nodeprotocol.FileExportSummary{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `INSERT INTO file_exports(operation_key,paired_server_id,task_id,name,manifest_digest,total_files,total_bytes,chunk_size,created_at,expires_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, summary.OperationKey, serverID, summary.TaskID, summary.Name, summary.ManifestDigest, summary.TotalFiles, summary.TotalBytes, summary.ChunkSize, summary.CreatedAt, summary.ExpiresAt); err != nil {
		return nodeprotocol.FileExportSummary{}, err
	}
	for after := int64(-1); ; {
		files, err := storageSourceFilesQuery(ctx, tx, input.OperationKey, after, storageSourcePageSize)
		if err != nil {
			return nodeprotocol.FileExportSummary{}, err
		}
		if len(files) == 0 {
			break
		}
		for _, file := range files {
			chunks, err := storageSourceChunksQuery(ctx, tx, input.OperationKey, file.Ordinal)
			if err != nil || validateStorageSourceChunks(file, chunks) != nil {
				return nodeprotocol.FileExportSummary{}, errors.New("node_storage_source_export_incomplete")
			}
			finalPath, _, err := secureStorageSourcePaths(root, file.RelativePath, file.Ordinal)
			if err != nil {
				return nodeprotocol.FileExportSummary{}, err
			}
			token, err := randomFileToken()
			if err != nil {
				return nodeprotocol.FileExportSummary{}, err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO file_export_files(operation_key,file_token,ordinal,relative_path,node_path,size,sha256,chunk_count) VALUES(?,?,?,?,?,?,?,?)`, input.OperationKey, token, file.Ordinal, file.RelativePath, finalPath, file.Size, file.SHA256, file.ChunkCount); err != nil {
				return nodeprotocol.FileExportSummary{}, err
			}
			for index := 0; index < file.ChunkCount; index++ {
				chunk := chunks[index]
				if _, err := tx.ExecContext(ctx, `INSERT INTO file_export_chunks(operation_key,file_token,chunk_index,chunk_offset,size,sha256) VALUES(?,?,?,?,?,?)`, input.OperationKey, token, chunk.Index, chunk.Offset, chunk.Size, chunk.SHA256); err != nil {
					return nodeprotocol.FileExportSummary{}, err
				}
			}
			after = file.Ordinal
		}
	}
	if err := tx.Commit(); err != nil {
		return nodeprotocol.FileExportSummary{}, err
	}
	return summary, nil
}

func validateStorageSourceChunks(file StorageSourceFile, chunks map[int]nodeprotocol.FileChunkDigest) error {
	if len(chunks) != file.ChunkCount {
		return errors.New("chunk count mismatch")
	}
	for index := 0; index < file.ChunkCount; index++ {
		chunk, exists := chunks[index]
		offset := int64(index) * nodeprotocol.FileChunkSize
		size := min(nodeprotocol.FileChunkSize, file.Size-offset)
		if !exists || chunk.Index != index || chunk.Offset != offset || chunk.Size != size || !nodeprotocol.ValidDigest(chunk.SHA256) {
			return errors.New("chunk mismatch")
		}
	}
	return nil
}

type storageSourceQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func storageSourceFilesQuery(ctx context.Context, db storageSourceQueryer, operationKey string, afterOrdinal int64, limit int) ([]StorageSourceFile, error) {
	rows, err := db.QueryContext(ctx, `SELECT operation_key,ordinal,provider_file_id,relative_path,size,source_sha1,status,sha256,chunk_count FROM storage_source_files WHERE operation_key=? AND ordinal>? ORDER BY ordinal LIMIT ?`, operationKey, afterOrdinal, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	files := make([]StorageSourceFile, 0, limit)
	for rows.Next() {
		var file StorageSourceFile
		if err := rows.Scan(&file.OperationKey, &file.Ordinal, &file.ProviderFileID, &file.RelativePath, &file.Size, &file.SourceSHA1, &file.Status, &file.SHA256, &file.ChunkCount); err != nil {
			return nil, err
		}
		files = append(files, file)
	}
	return files, rows.Err()
}

func storageSourceChunksQuery(ctx context.Context, db storageSourceQueryer, operationKey string, ordinal int64) (map[int]nodeprotocol.FileChunkDigest, error) {
	rows, err := db.QueryContext(ctx, `SELECT chunk_index,chunk_offset,size,sha256 FROM storage_source_chunks WHERE operation_key=? AND file_ordinal=? ORDER BY chunk_index`, operationKey, ordinal)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := make(map[int]nodeprotocol.FileChunkDigest)
	for rows.Next() {
		var chunk nodeprotocol.FileChunkDigest
		if err := rows.Scan(&chunk.Index, &chunk.Offset, &chunk.Size, &chunk.SHA256); err != nil {
			return nil, err
		}
		result[chunk.Index] = chunk
	}
	return result, rows.Err()
}

func (a *Agent) storageSourceRoot(operationKey string) (string, error) {
	managed, err := filepath.EvalSymlinks(filepath.Clean(a.config.ManagedRoot))
	if err != nil {
		return "", errors.New("node_managed_root_invalid")
	}
	digest := sha256.Sum256([]byte(operationKey))
	return ensureSecureStorageSourceDirectory(managed, []string{"storage-source", hex.EncodeToString(digest[:16])})
}

func secureStorageSourcePaths(root, relative string, ordinal int64) (string, string, error) {
	if normalizedExportPath(relative) != relative {
		return "", "", errors.New("node_storage_source_path_invalid")
	}
	contentRoot, err := ensureSecureStorageSourceDirectory(root, []string{"content"})
	if err != nil {
		return "", "", err
	}
	partialRoot, err := ensureSecureStorageSourceDirectory(root, []string{"partials"})
	if err != nil {
		return "", "", err
	}
	parent := contentRoot
	directory := filepath.Dir(filepath.FromSlash(relative))
	if directory != "." {
		parent, err = ensureSecureStorageSourceDirectory(contentRoot, strings.Split(directory, string(filepath.Separator)))
		if err != nil {
			return "", "", err
		}
	}
	finalPath := filepath.Join(parent, filepath.Base(filepath.FromSlash(relative)))
	if requirePathWithin(contentRoot, finalPath) != nil {
		return "", "", errors.New("node_storage_source_path_invalid")
	}
	return finalPath, filepath.Join(partialRoot, strconv.FormatInt(ordinal, 10)+".partial"), nil
}

func ensureSecureStorageSourceDirectory(root string, segments []string) (string, error) {
	current := root
	for _, segment := range segments {
		if segment == "" || segment == "." || segment == ".." || strings.ContainsAny(segment, "\\/\x00\r\n") {
			return "", errors.New("node_storage_source_path_invalid")
		}
		current = filepath.Join(current, segment)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(current, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
				return "", err
			}
			info, err = os.Lstat(current)
		}
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("node_storage_source_path_invalid")
		}
		resolved, err := filepath.EvalSymlinks(current)
		if err != nil || !sameCleanPath(resolved, current) || requirePathWithin(root, resolved) != nil {
			return "", errors.New("node_storage_source_path_invalid")
		}
	}
	return current, nil
}

func openStorageSourcePartial(filename string, size int64) (*os.File, error) {
	if info, err := os.Lstat(filename); err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("node_source_partial_invalid")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	file, err := os.OpenFile(filename, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, errors.New("node_source_partial_invalid")
	}
	if info.Size() != size {
		if err := file.Truncate(size); err != nil {
			_ = file.Close()
			return nil, err
		}
	}
	return file, nil
}

func hashSourcePath(ctx context.Context, filename string, expectedSize int64) (string, string, error) {
	file, err := os.Open(filename)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != expectedSize {
		return "", "", errors.New("node_source_file_invalid")
	}
	sha256Hash, sha1Hash := sha256.New(), sha1.New()
	written, err := io.Copy(io.MultiWriter(sha256Hash, sha1Hash), &contextReader{ctx: ctx, reader: file})
	if err != nil || written != expectedSize {
		return "", "", errors.New("node_source_file_changed")
	}
	after, err := file.Stat()
	if err != nil || after.Size() != info.Size() || after.ModTime() != info.ModTime() || !os.SameFile(info, after) {
		return "", "", errors.New("node_source_file_changed")
	}
	return hex.EncodeToString(sha256Hash.Sum(nil)), strings.ToUpper(hex.EncodeToString(sha1Hash.Sum(nil))), nil
}

func manifestHashFile(writer io.Writer, file StorageSourceFile) {
	_, _ = io.WriteString(writer, strconv.FormatInt(file.Ordinal, 10)+"\x00"+file.ProviderFileID+"\x00"+file.RelativePath+"\x00"+strconv.FormatInt(file.Size, 10)+"\x00"+strings.ToUpper(file.SourceSHA1)+"\n")
}

func sourceResponse(input nodeprotocol.StorageSourceActionRequest, run StorageSourceRun) nodeprotocol.StorageSourceActionResponse {
	return nodeprotocol.StorageSourceActionResponse{RequestID: input.RequestID, OperationKey: input.OperationKey, PlanDigest: input.PlanDigest, Status: run.Status, TotalFiles: run.TotalFiles, DownloadedFiles: run.DownloadedFiles, TotalBytes: run.TotalBytes, DownloadedBytes: run.DownloadedBytes, Progress: sourceProgress(run)}
}

func sourceProgress(run StorageSourceRun) *float64 {
	if run.TotalBytes <= 0 {
		return nil
	}
	value := float64(run.DownloadedBytes) / float64(run.TotalBytes)
	return &value
}

func (a *Agent) pauseStorageSource(ctx context.Context, serverID string, input nodeprotocol.StorageSourceActionRequest, response nodeprotocol.StorageSourceActionResponse, err error) {
	code := storageSourceErrorCode(err)
	response.ErrorCode = code
	if code == nodeprotocol.ErrorCredentialExpired {
		response.Status = nodeprotocol.StorageSourceWaitingCredentials
	}
	_ = a.store.UpdateActionReceipt(ctx, serverID, input.RequestID, response, a.now())
	phase := nodeprotocol.PhasePullingSource
	if code == nodeprotocol.ErrorCredentialExpired {
		phase = nodeprotocol.PhaseWaitingCredentials
	}
	_ = a.store.UpdateOperationExecution(ctx, serverID, input.OperationKey, input.PlanDigest, nodeprotocol.OperationRunning, phase, code, response.Progress, a.now())
}

func storageSourceErrorCode(err error) string {
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
	if strings.Contains(err.Error(), "node_credential") {
		return nodeprotocol.ErrorCredentialExpired
	}
	code, _ := cloud.ErrorInfo(err)
	switch code {
	case cloud.CodeAuthExpired, cloud.CodeCookieInvalid:
		return nodeprotocol.ErrorCredentialExpired
	case cloud.CodeRateLimited:
		return nodeprotocol.ErrorSourceRateLimited
	case cloud.CodeNotFound, cloud.CodeMutationUnknown:
		return nodeprotocol.ErrorReconciliationNeeded
	case cloud.CodeOfflineNoQuota:
		return "node_source_offline_quota_exhausted"
	case cloud.CodeOfflineBadLink:
		return "node_source_offline_invalid"
	default:
		return "node_source_unavailable"
	}
}

func writeStorageSourceError(w http.ResponseWriter, err error) {
	code := storageSourceErrorCode(err)
	status := http.StatusConflict
	switch code {
	case nodeprotocol.ErrorCredentialExpired:
		status = http.StatusForbidden
	case "node_source_unavailable":
		status = http.StatusBadGateway
	}
	writeError(w, status, code, fmt.Sprintf("节点无法接受来源存储操作：%s", storageSourceSafeMessage(code)))
}

func storageSourceSafeMessage(code string) string {
	switch code {
	case nodeprotocol.ErrorLeaseExpired:
		return "任务租约已经过期"
	case nodeprotocol.ErrorPlanConflict:
		return "任务计划与节点状态不一致"
	case nodeprotocol.ErrorCredentialExpired:
		return "来源存储临时凭据已经过期"
	case nodeprotocol.ErrorSourceRateLimited:
		return "来源存储触发限流，请稍后继续"
	case nodeprotocol.ErrorReconciliationNeeded:
		return "来源结果需要重新核对"
	case nodeprotocol.ErrorChecksumMismatch:
		return "来源文件完整性校验失败"
	case nodeprotocol.ErrorCapabilityMissing:
		return "节点缺少所需来源能力"
	default:
		return "来源存储暂时不可用"
	}
}

func offlineSourceTaskID(raw string) (string, error) {
	return nodeprotocol.CanonicalStorageSourceIdentity(nodeprotocol.StorageSourceKindPan115OfflineMagnet, raw)
}

var _ storageSourceDriver = (*pan115.Client)(nil)
