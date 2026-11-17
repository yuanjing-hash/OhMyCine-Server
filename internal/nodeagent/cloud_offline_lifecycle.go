package nodeagent

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/pan115"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
	"net/http"
	"strings"
	"time"
)

type nativeOfflineLifecycle struct {
	TerminalFailure       bool                            `json:"terminal_failure,omitempty"`
	DefiniteNonAcceptance bool                            `json:"definite_non_acceptance,omitempty"`
	DeadlineExceeded      bool                            `json:"deadline_exceeded,omitempty"`
	Version               int                             `json:"version"`
	AccountID             string                          `json:"account_id"`
	ProviderTaskID        string                          `json:"provider_task_id"`
	DestinationID         string                          `json:"destination_id"`
	DispatchedAt          *time.Time                      `json:"dispatched_at,omitempty"`
	AcceptedAt            *time.Time                      `json:"accepted_at,omitempty"`
	ReadyAt               *time.Time                      `json:"ready_at,omitempty"`
	OutputRootID          string                          `json:"output_root_id,omitempty"`
	CompletionMode        string                          `json:"completion_mode,omitempty"`
	RequiredDigest        string                          `json:"required_digest,omitempty"`
	Required              *downloader.OfflineRequiredPlan `json:"required,omitempty"`
	ReadyManifest         *downloader.Manifest            `json:"ready_manifest,omitempty"`
	StopState             string                          `json:"stop_state,omitempty"`
	StopForGoal           bool                            `json:"stop_for_goal,omitempty"`
}
type nativeOfflineLedger struct {
	store   *nodeOfflineReceiptStore
	receipt cloud.OperationReceipt
	state   nativeOfflineLifecycle
}

func (a *Agent) offlineLock(ctx context.Context, serverID, key string) (func(), error) {
	a.offlineLockOnce.Do(func() {
		for i := range a.offlineLocks {
			a.offlineLocks[i] = make(chan struct{}, 1)
		}
	})
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(serverID + "\x00" + key))
	gate := a.offlineLocks[int(sum[0])%len(a.offlineLocks)]
	select {
	case gate <- struct{}{}:
		return func() { <-gate }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func offlineAccountNamespace(provider, account string) string {
	sum := sha256.Sum256([]byte("offline-account-v1\x00" + provider + "\x00" + account))
	return hex.EncodeToString(sum[:])
}
func (a *Agent) loadOfflineLedger(ctx context.Context, serverID string, input nodeprotocol.StorageSourceActionRequest, plan nodeprotocol.StorageSourcePlan, credential nodeprotocol.StorageSourceCredential, create bool) (*nativeOfflineLedger, error) {
	account, err := pan115.CredentialResourceAccount(credential.Cookie)
	if err != nil || offlineAccountNamespace(plan.ProviderType, account) != plan.OfflineAccountNamespace {
		return nil, ErrPlanConflict
	}
	store, err := a.offlineReceipts(serverID, input.OperationKey)
	if err != nil {
		return nil, err
	}
	receipt, err := store.Load(ctx, "cloud-offline-lifecycle")
	if errors.Is(err, cloud.ErrOperationNotFound) && create {
		providerID, e := offlineSourceTaskID(credential.SourceURI)
		if e != nil {
			return nil, e
		}
		state := nativeOfflineLifecycle{Version: 1, AccountID: account, ProviderTaskID: providerID, DestinationID: plan.OfflineDestinationID, RequiredDigest: plan.OfflineRequiredDigest, Required: credential.OfflineRequired}
		if state.RequiredDigest != "" {
			if state.Required == nil {
				return nil, ErrPlanConflict
			}
			digest, e := state.Required.Digest()
			if e != nil || digest != state.RequiredDigest {
				return nil, ErrPlanConflict
			}
		} else if state.Required != nil {
			return nil, ErrPlanConflict
		}
		raw, _ := json.Marshal(state)
		receipt = cloud.OperationReceipt{ID: "cloud-offline-lifecycle", Provider: cloud.ProviderPan115, Route: "cookie", AccountID: account, Kind: nodeprotocol.CapabilityCloudOfflineLifecycleV1, Revision: 1, State: "prepared", Payload: raw, UpdatedAt: a.now()}
		if err = store.Create(ctx, receipt); err != nil {
			return nil, err
		}
	}
	if err != nil {
		return nil, err
	}
	var state nativeOfflineLifecycle
	if receipt.AccountID != account || receipt.Kind != nodeprotocol.CapabilityCloudOfflineLifecycleV1 || json.Unmarshal(receipt.Payload, &state) != nil || state.Version != 1 || state.AccountID != account || state.DestinationID != plan.OfflineDestinationID || state.RequiredDigest != plan.OfflineRequiredDigest {
		return nil, ErrPlanConflict
	}
	if credential.OfflineRequired != nil {
		digest, e := credential.OfflineRequired.Digest()
		if e != nil || digest != state.RequiredDigest {
			return nil, ErrPlanConflict
		}
	}
	return &nativeOfflineLedger{store, receipt, state}, nil
}
func (l *nativeOfflineLedger) save(ctx context.Context, state string, now time.Time) error {
	next := l.receipt
	next.Revision++
	next.State = state
	next.UpdatedAt = now
	next.Payload, _ = json.Marshal(l.state)
	if err := l.store.Update(ctx, l.receipt.Revision, next); err != nil {
		return err
	}
	l.receipt = next
	return nil
}
func (l *nativeOfflineLedger) observation(operationKey, namespace string) *downloader.OfflineObservation {
	mode := l.state.CompletionMode
	if mode == "" {
		mode = downloader.OfflineCompletionUnknown
	}
	return &downloader.OfflineObservation{TerminalFailure: l.state.TerminalFailure, DefiniteNonAcceptance: l.state.DefiniteNonAcceptance, DeadlineExceeded: l.state.DeadlineExceeded, Version: 1, Kind: downloader.OfflineOperationCloud, OperationID: operationKey, AccountNamespace: namespace, OutputRootID: l.state.OutputRootID, StartedAt: l.state.DispatchedAt, StartProvenance: downloader.OfflineStartDispatch, Ready: l.state.ReadyAt != nil, ReadyAt: l.state.ReadyAt, CompletionMode: mode, RequiredDigest: l.state.RequiredDigest, KeepOutputStop: true, StopPending: l.state.StopState != "" && l.state.StopState != "stopped", UpstreamStopped: l.state.StopState == "stopped" || mode == downloader.OfflineCompletionAggregate && l.state.ReadyAt != nil}
}
func (a *Agent) publishOfflineLifecycle(ctx context.Context, serverID string, input nodeprotocol.StorageSourceActionRequest, plan nodeprotocol.StorageSourcePlan, l *nativeOfflineLedger) error {
	observation := l.observation(input.OperationKey, plan.OfflineAccountNamespace)
	raw, _ := json.Marshal(observation)
	stop := l.state.StopState
	if l.state.StopForGoal && stop != "" {
		stop = "goal:" + stop
	}
	tx, err := a.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// A cancelled operation may reconcile its original stop, but cannot publish
	// fresh readiness. Superseded identity/expired lease never authorizes writes.
	result, err := tx.ExecContext(ctx, `UPDATE storage_source_runs SET cloud_offline_json=?,offline_stop_state=?,updated_at=? WHERE operation_key=? AND paired_server_id=? AND plan_digest=? AND EXISTS(SELECT 1 FROM operations WHERE operation_key=? AND paired_server_id=? AND plan_digest=? AND superseded_by='' AND lease_expires_at>? AND (status<>? OR ?<>'') )`, string(raw), stop, a.now(), input.OperationKey, serverID, input.PlanDigest, input.OperationKey, serverID, input.PlanDigest, a.now(), nodeprotocol.OperationCancelled, l.state.StopState)
	if err != nil {
		return err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return ErrLeaseConflict
	}
	_, err = tx.ExecContext(ctx, `UPDATE provider_receipts SET response_json=json_set(response_json,'$.offline',json(?),'$.offline_stop_state',?),updated_at=? WHERE paired_server_id=? AND operation_key=? AND action=? AND json_valid(response_json)`, string(raw), stop, a.now(), serverID, input.OperationKey, nodeprotocol.StorageSourceActionMaterialize)
	if err != nil {
		return err
	}
	if l.state.StopState == "stopped" {
		_, err = tx.ExecContext(ctx, `UPDATE operations SET status=?,phase=?,revision=revision+1,updated_at=? WHERE operation_key=? AND paired_server_id=? AND plan_digest=? AND status NOT IN (?,?) AND EXISTS(SELECT 1 FROM storage_source_runs WHERE operation_key=? AND status=?)`, nodeprotocol.OperationCompleted, nodeprotocol.PhaseCompleted, a.now(), input.OperationKey, serverID, input.PlanDigest, nodeprotocol.OperationCancelled, nodeprotocol.OperationCompleted, input.OperationKey, nodeprotocol.StorageSourceCompleted)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// progressCloudOffline owns only cloud metadata/native writes. Its caller may
// hold data admission, but status/stop controls invoke it without that admission.
func (a *Agent) progressCloudOffline(ctx context.Context, serverID string, input nodeprotocol.StorageSourceActionRequest, plan nodeprotocol.StorageSourcePlan, grant nodeprotocol.CredentialGrant, credential nodeprotocol.StorageSourceCredential, driver storageSourceDriver, allowSubmit bool) (bool, error) {
	release, err := a.offlineLock(ctx, serverID, input.OperationKey)
	if err != nil {
		return false, err
	}
	defer release()
	ledger, err := a.loadOfflineLedger(ctx, serverID, input, plan, credential, allowSubmit)
	if errors.Is(err, cloud.ErrOperationNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	state := &ledger.state
	if state.TerminalFailure || state.DefiniteNonAcceptance {
		return false, a.publishOfflineLifecycle(ctx, serverID, input, plan, ledger)
	}
	if state.StopState != "" {
		native, e := driver.GetOffline(ctx, state.ProviderTaskID)
		code, _ := cloud.ErrorInfo(e)
		if e == nil && (native.DirectoryID != state.DestinationID || !strings.EqualFold(native.ID, state.ProviderTaskID)) {
			return false, errors.New(nodeprotocol.ErrorReconciliationNeeded)
		}
		if code == cloud.CodeNotFound || e == nil && native.Completed {
			state.StopState = "stopped"
			if err := ledger.save(ctx, "stop_stopped", a.now()); err != nil {
				return false, err
			}
		} else if e != nil {
			return false, e
		} else if native.DirectoryID != state.DestinationID || !strings.EqualFold(native.ID, state.ProviderTaskID) {
			return false, errors.New(nodeprotocol.ErrorReconciliationNeeded)
		}
		return state.ReadyAt != nil, a.publishOfflineLifecycle(ctx, serverID, input, plan, ledger)
	}
	if state.ReadyAt != nil {
		if err := a.publishOfflineLifecycle(ctx, serverID, input, plan, ledger); err != nil {
			return false, err
		}
		return true, nil
	}
	if !containsAction(grant.AllowedActions, nodeprotocol.StorageSourceGrantOfflineStatus) {
		return false, ErrPlanConflict
	}
	native, getErr := driver.GetOffline(ctx, state.ProviderTaskID)
	if getErr != nil {
		code, _ := cloud.ErrorInfo(getErr)
		if code != cloud.CodeNotFound {
			return false, getErr
		}
		if state.DispatchedAt != nil || !allowSubmit {
			return false, a.publishOfflineLifecycle(ctx, serverID, input, plan, ledger)
		}
		if !containsAction(grant.AllowedActions, nodeprotocol.StorageSourceGrantOfflineSubmit) || !containsAction(grant.AllowedActions, nodeprotocol.StorageSourceGrantOfflineStop) {
			return false, ErrPlanConflict
		}
		callCtx := cloud.WithOfflineDispatchRecorder(ctx, func(at time.Time) error {
			if state.DispatchedAt != nil {
				return ErrPlanConflict
			}
			state.DispatchedAt = &at
			if err := ledger.save(ctx, "entered", a.now()); err != nil {
				return err
			}
			return a.publishOfflineLifecycle(ctx, serverID, input, plan, ledger)
		})
		callCtx = cloud.WithOfflineNonAcceptanceRecorder(callCtx, func() error {
			settle, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			state.DefiniteNonAcceptance = true
			state.StopState = "stopped"
			if err := ledger.save(settle, "not_accepted", a.now()); err != nil {
				return err
			}
			return a.publishOfflineLifecycle(settle, serverID, input, plan, ledger)
		})
		native, err = driver.SubmitOffline(callCtx, credential.SourceURI, state.DestinationID)
		if err != nil {
			if downloader.DefiniteSubmissionNonAcceptance(err) && !state.DefiniteNonAcceptance {
				state.DefiniteNonAcceptance = true
				state.StopState = "stopped"
				if e := ledger.save(ctx, "not_accepted", a.now()); e != nil {
					return false, e
				}
				if e := a.publishOfflineLifecycle(ctx, serverID, input, plan, ledger); e != nil {
					return false, e
				}
			}
			return false, err
		}
		if state.DispatchedAt == nil {
			return false, errors.New(nodeprotocol.ErrorReconciliationNeeded)
		}
	} else if state.DispatchedAt == nil {
		// A matching hash by itself cannot adopt a pre-existing native operation.
		return false, errors.New(nodeprotocol.ErrorReconciliationNeeded)
	}
	if !strings.EqualFold(native.ID, state.ProviderTaskID) || native.DirectoryID != state.DestinationID {
		return false, errors.New(nodeprotocol.ErrorReconciliationNeeded)
	}
	if state.AcceptedAt == nil {
		now := a.now()
		state.AcceptedAt = &now
	}
	if native.Failed {
		state.TerminalFailure = true
		if err := ledger.save(ctx, "terminal_failure", a.now()); err != nil {
			return false, err
		}
		if err := a.store.UpdateStorageSourceRun(ctx, serverID, input.OperationKey, input.PlanDigest, nodeprotocol.StorageSourceFailed, state.ProviderTaskID, "", a.now()); err != nil {
			return false, err
		}
		if err := a.publishOfflineLifecycle(ctx, serverID, input, plan, ledger); err != nil {
			return false, err
		}
		if !allowSubmit {
			return false, nil
		}
		return false, downloader.WithOfflineTerminalFailure(errors.New("node_source_offline_failed"))
	}
	state.OutputRootID = native.OutputItemID
	if native.Completed && native.OutputItemID != "" {
		now := a.now()
		state.ReadyAt = &now
		state.CompletionMode = downloader.OfflineCompletionAggregate
	} else if state.Required != nil && native.OutputItemID != "" && containsAction(grant.AllowedActions, nodeprotocol.StorageSourceGrantRead) {
		root, e := driver.Stat(ctx, native.OutputItemID)
		if e == nil && root.ParentID == state.DestinationID && root.ID != state.DestinationID {
			proofCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			manifest, ready := downloader.ObserveRequiredOutput(proofCtx, driver, native.OutputItemID, *state.Required)
			cancel()
			if ready {
				now := a.now()
				state.ReadyAt = &now
				state.CompletionMode = downloader.OfflineCompletionRequiredFiles
				state.ReadyManifest = &manifest
			}
		}
	}
	phase := "accepted"
	status := nodeprotocol.StorageSourceWaiting
	if state.ReadyAt != nil {
		phase = "ready"
		status = nodeprotocol.StorageSourceEnumerating
	}
	if err := ledger.save(ctx, phase, a.now()); err != nil {
		return false, err
	}
	output := ""
	if state.ReadyAt != nil {
		output = state.OutputRootID
	}
	if err := a.store.UpdateStorageSourceRun(ctx, serverID, input.OperationKey, input.PlanDigest, status, state.ProviderTaskID, output, a.now()); err != nil {
		return false, err
	}
	if err := a.publishOfflineLifecycle(ctx, serverID, input, plan, ledger); err != nil {
		return false, err
	}
	if state.ReadyAt == nil && input.Action == nodeprotocol.StorageSourceActionMaterialize {
		run, _ := a.store.StorageSourceRun(ctx, serverID, input.OperationKey)
		response := sourceResponse(input, run)
		response.ProviderStatus = native.Status
		response.Progress = native.Progress
		if err := a.store.UpdateActionReceipt(ctx, serverID, input.RequestID, response, a.now()); err != nil {
			return false, err
		}
	}
	return state.ReadyAt != nil, nil
}

func (a *Agent) cloudControlPlan(ctx context.Context, serverID string, input nodeprotocol.StorageSourceActionRequest) (nodeprotocol.StorageSourcePlan, error) {
	var raw string
	var expires time.Time
	err := a.store.db.QueryRowContext(ctx, `SELECT plan_json,lease_expires_at FROM operations WHERE paired_server_id=? AND operation_key=? AND task_id=? AND plan_digest=? AND superseded_by=''`, serverID, input.OperationKey, input.TaskID, input.PlanDigest).Scan(&raw, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nodeprotocol.StorageSourcePlan{}, ErrOperationNotFound
	}
	if err != nil {
		return nodeprotocol.StorageSourcePlan{}, err
	}
	if !expires.After(a.now()) {
		return nodeprotocol.StorageSourcePlan{}, ErrLeaseConflict
	}
	var op nodeprotocol.OperationPlan
	var plan nodeprotocol.StorageSourcePlan
	if json.Unmarshal([]byte(raw), &op) != nil || op.Kind != nodeprotocol.OperationKindStorageSourceMaterialize || json.Unmarshal(op.Payload, &plan) != nil || plan.Validate() != nil || plan.StorageID != input.StorageID || plan.CloudOfflineLifecycleVersion != 1 {
		return plan, errors.New(nodeprotocol.ErrorCapabilityMissing)
	}
	return plan, nil
}
func (a *Agent) cloudOfflineControl(w http.ResponseWriter, r *http.Request, serverID string, input nodeprotocol.StorageSourceActionRequest) {
	plan, err := a.cloudControlPlan(r.Context(), serverID, input)
	if err != nil {
		writeStorageSourceError(w, err)
		return
	}
	run, err := a.store.StorageSourceRun(r.Context(), serverID, input.OperationKey)
	if err != nil {
		writeStorageSourceError(w, err)
		return
	}
	grant, credential, err := a.storageSourceCredential(r.Context(), serverID, input, plan, run)
	if err != nil {
		writeStorageSourceError(w, err)
		return
	}
	guard := func(ctx context.Context) error {
		if !grant.ExpiresAt.After(a.now()) {
			return errors.New(nodeprotocol.ErrorCredentialExpired)
		}
		_, err := a.cloudControlPlan(ctx, serverID, input)
		return err
	}
	driver, err := a.sourceFactory(credential, guard)
	if err != nil {
		writeStorageSourceError(w, err)
		return
	}
	if input.Action == nodeprotocol.StorageSourceActionOfflineStop {
		err = a.stopCloudOffline(r.Context(), serverID, input, plan, credential, driver)
	} else {
		_, err = a.progressCloudOffline(r.Context(), serverID, input, plan, grant, credential, driver, false)
	}
	// A durable unknown stop is a truthful successful control observation, not an
	// acknowledgement that the original native operation has stopped.
	if err != nil {
		writeStorageSourceError(w, err)
		return
	}
	run, err = a.store.StorageSourceRun(r.Context(), serverID, input.OperationKey)
	if err != nil {
		writeStorageSourceError(w, err)
		return
	}
	response := sourceResponse(input, run)
	writeJSON(w, http.StatusOK, response)
}
func (a *Agent) stopCloudOffline(ctx context.Context, serverID string, input nodeprotocol.StorageSourceActionRequest, plan nodeprotocol.StorageSourcePlan, credential nodeprotocol.StorageSourceCredential, driver storageSourceDriver) error {
	release, err := a.offlineLock(ctx, serverID, input.OperationKey)
	if err != nil {
		return err
	}
	defer release()
	ledger, err := a.loadOfflineLedger(ctx, serverID, input, plan, credential, false)
	if err != nil {
		return err
	}
	state := &ledger.state
	if input.KeepMaterialization && state.ReadyAt == nil {
		return ErrPlanConflict
	}
	if state.StopState == "stopped" {
		return a.publishOfflineLifecycle(ctx, serverID, input, plan, ledger)
	}
	if state.StopState != "" && state.StopForGoal != input.KeepMaterialization {
		if state.StopForGoal && !input.KeepMaterialization {
			state.StopForGoal = false
			if err := ledger.save(ctx, "stopping", a.now()); err != nil {
				return err
			}
		} else {
			return ErrPlanConflict
		}
	}
	first := state.StopState == ""
	if first {
		state.StopState = "requested"
		state.StopForGoal = input.KeepMaterialization
		if err := ledger.save(ctx, "stopping", a.now()); err != nil {
			return err
		}
	}
	if err := a.publishOfflineLifecycle(ctx, serverID, input, plan, ledger); err != nil {
		return err
	}
	native, getErr := driver.GetOffline(ctx, state.ProviderTaskID)
	code, _ := cloud.ErrorInfo(getErr)
	if code == cloud.CodeNotFound || state.DispatchedAt == nil {
		state.StopState = "stopped"
	} else if getErr != nil {
		return getErr
	} else if !strings.EqualFold(native.ID, state.ProviderTaskID) || native.DirectoryID != state.DestinationID {
		return errors.New(nodeprotocol.ErrorReconciliationNeeded)
	} else if native.Completed {
		state.StopState = "stopped"
	} else if first || state.StopState == "requested" {
		state.StopState = "entered"
		if err := ledger.save(ctx, "stopping", a.now()); err != nil {
			return err
		}
		// One exact original native cancel, explicitly retaining all cloud output.
		if err := driver.CancelOffline(ctx, state.ProviderTaskID, false); err != nil {
			state.StopState = "unknown"
		} else {
			state.StopState = "stopped"
		}
	} else {
		state.StopState = "unknown"
	} // lost ACK: status reconciliation only, no replay
	settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := ledger.save(settleCtx, "stop_"+state.StopState, a.now()); err != nil {
		return err
	}
	return a.publishOfflineLifecycle(settleCtx, serverID, input, plan, ledger)
}

func (a *Agent) readOfflineMaterializationIntent(ctx context.Context, serverID, key string, plan nodeprotocol.StorageSourcePlan) (*nativeOfflineLifecycle, error) {
	store, err := a.offlineReceipts(serverID, key)
	if err != nil {
		return nil, err
	}
	receipt, err := store.Load(ctx, "cloud-offline-lifecycle")
	if err != nil {
		return nil, err
	}
	var state nativeOfflineLifecycle
	if json.Unmarshal(receipt.Payload, &state) != nil || state.ReadyAt == nil || state.Required == nil || state.RequiredDigest != plan.OfflineRequiredDigest || state.DestinationID != plan.OfflineDestinationID {
		return nil, ErrPlanConflict
	}
	digest, err := state.Required.Digest()
	if err != nil || digest != state.RequiredDigest {
		return nil, ErrPlanConflict
	}
	return &state, nil
}
