package nodeagent

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
	"strings"
	"sync"
	"time"
)

const cloudOfflineTimeout = 30 * time.Minute

// RunCloudOfflineMaintenance is tied to the Node process context. It persists a
// local terminal decision even while Server is disconnected. An expired sealed
// grant/lease never authorizes provider I/O: upstream stop then remains pending.
func (a *Agent) RunCloudOfflineMaintenance(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	defer a.offlineStopWG.Wait()
	for {
		_ = a.ReconcileCloudOfflineDeadlines(ctx)
		_ = a.queueCloudOfflineStops(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func (a *Agent) ReconcileCloudOfflineDeadlines(ctx context.Context) error {
	rows, err := a.store.db.QueryContext(ctx, `SELECT r.paired_server_id,r.operation_key,r.task_id,r.plan_digest,r.storage_id,o.plan_json FROM storage_source_runs r JOIN operations o ON o.operation_key=r.operation_key WHERE json_valid(r.cloud_offline_json) AND json_extract(r.cloud_offline_json,'$.ready')=0 AND COALESCE(json_extract(r.cloud_offline_json,'$.deadline_exceeded'),0)=0 AND COALESCE(json_extract(r.cloud_offline_json,'$.definite_non_acceptance'),0)=0 AND COALESCE(json_extract(r.cloud_offline_json,'$.terminal_failure'),0)=0 AND json_extract(r.cloud_offline_json,'$.started_at')<=? AND o.superseded_by='' AND o.status NOT IN ('completed','failed','cancelled') ORDER BY r.updated_at LIMIT 16`, a.now().Add(-cloudOfflineTimeout).Format(time.RFC3339Nano))
	if err != nil {
		return err
	}
	type due struct {
		server string
		input  nodeprotocol.StorageSourceActionRequest
		plan   nodeprotocol.StorageSourcePlan
	}
	var tasks []due
	for rows.Next() {
		var item due
		var raw string
		if err := rows.Scan(&item.server, &item.input.OperationKey, &item.input.TaskID, &item.input.PlanDigest, &item.input.StorageID, &raw); err != nil {
			rows.Close()
			return err
		}
		var op nodeprotocol.OperationPlan
		if json.Unmarshal([]byte(raw), &op) != nil || json.Unmarshal(op.Payload, &item.plan) != nil || item.plan.Validate() != nil || item.plan.CloudOfflineLifecycleVersion != 1 {
			continue
		}
		item.input.Action = nodeprotocol.StorageSourceActionOfflineStop
		item.input.RequestID = item.input.OperationKey + ":offline:deadline"
		tasks = append(tasks, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	var workers sync.WaitGroup
	permits := make(chan struct{}, 8)
	var firstErr error
	var errorMu sync.Mutex
	for _, item := range tasks {
		if err := ctx.Err(); err != nil {
			errorMu.Lock()
			firstErr = err
			errorMu.Unlock()
			break
		}
		permits <- struct{}{}
		workers.Add(1)
		go func(item due) {
			defer workers.Done()
			defer func() { <-permits }()
			// Slow control I/O never holds another due task's local deadline decision.
			readCtx, cancelRead := context.WithTimeout(ctx, 3*time.Second)
			grant, credential, driver, authorized := a.offlineDeadlineAuthority(readCtx, item.server, item.input, item.plan)
			if authorized {
				_, _ = a.progressCloudOffline(readCtx, item.server, item.input, item.plan, grant, credential, driver, false)
			}
			cancelRead()
			decisionCtx, cancelDecision := context.WithTimeout(ctx, 5*time.Second)
			marked, e := a.markOfflineDeadline(decisionCtx, item.server, item.input, item.plan)
			cancelDecision()
			if e != nil && !errors.Is(e, ErrLeaseConflict) {
				errorMu.Lock()
				if firstErr == nil {
					firstErr = e
				}
				errorMu.Unlock()
			}
			if e == nil && marked {
				a.launchCloudOfflineStop(ctx, item.server, item.input, item.plan)
			}
		}(item)
	}
	workers.Wait()
	if firstErr != nil {
		return firstErr
	}

	return nil
}
func (a *Agent) offlineDeadlineAuthority(ctx context.Context, serverID string, input nodeprotocol.StorageSourceActionRequest, plan nodeprotocol.StorageSourcePlan) (nodeprotocol.CredentialGrant, nodeprotocol.StorageSourceCredential, storageSourceDriver, bool) {
	if _, err := a.cloudControlPlan(ctx, serverID, input); err != nil {
		return nodeprotocol.CredentialGrant{}, nodeprotocol.StorageSourceCredential{}, nil, false
	}
	rows, err := a.store.db.QueryContext(ctx, `SELECT grant_id FROM credential_grants WHERE paired_server_id=? AND operation_key=? AND task_id=? AND expires_at>? ORDER BY credential_revision DESC,created_at DESC LIMIT 16`, serverID, input.OperationKey, input.TaskID, a.now())
	if err != nil {
		return nodeprotocol.CredentialGrant{}, nodeprotocol.StorageSourceCredential{}, nil, false
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	run, err := a.store.StorageSourceRun(ctx, serverID, input.OperationKey)
	if err != nil {
		return nodeprotocol.CredentialGrant{}, nodeprotocol.StorageSourceCredential{}, nil, false
	}
	for _, id := range ids {
		input.GrantID = id
		grant, credential, err := a.storageSourceCredential(ctx, serverID, input, plan, run)
		if err != nil {
			continue
		}
		if _, err := a.loadOfflineLedger(ctx, serverID, input, plan, credential, false); err != nil {
			continue
		}
		guard := func(callCtx context.Context) error {
			if !grant.ExpiresAt.After(a.now()) {
				return errors.New(nodeprotocol.ErrorCredentialExpired)
			}
			_, err := a.cloudControlPlan(callCtx, serverID, input)
			return err
		}
		driver, err := a.sourceFactory(credential, guard)
		if err == nil {
			return grant, credential, driver, true
		}
	}
	return nodeprotocol.CredentialGrant{}, nodeprotocol.StorageSourceCredential{}, nil, false
}
func (a *Agent) markOfflineDeadline(ctx context.Context, serverID string, input nodeprotocol.StorageSourceActionRequest, plan nodeprotocol.StorageSourcePlan) (bool, error) {
	release, err := a.offlineLock(ctx, serverID, input.OperationKey)
	if err != nil {
		return false, err
	}
	defer release()
	store, err := a.offlineReceipts(serverID, input.OperationKey)
	if err != nil {
		return false, err
	}
	receipt, err := store.Load(ctx, "cloud-offline-lifecycle")
	if err != nil {
		return false, err
	}
	var state nativeOfflineLifecycle
	if json.Unmarshal(receipt.Payload, &state) != nil || state.AccountID != receipt.AccountID || offlineAccountNamespace(plan.ProviderType, state.AccountID) != plan.OfflineAccountNamespace || state.DestinationID != plan.OfflineDestinationID {
		return false, ErrPlanConflict
	}
	if state.TerminalFailure || state.DefiniteNonAcceptance || state.ReadyAt != nil || state.DispatchedAt == nil || a.now().Before(state.DispatchedAt.Add(cloudOfflineTimeout)) {
		return false, nil
	}
	if state.StopState == "" {
		state.StopState = "requested"
	}
	state.StopForGoal = false
	state.DeadlineExceeded = true
	ledger := &nativeOfflineLedger{store, receipt, state}
	if err := ledger.save(ctx, "deadline_exceeded", a.now()); err != nil {
		return false, err
	}
	observation := ledger.observation(input.OperationKey, plan.OfflineAccountNamespace)
	raw, _ := json.Marshal(observation)
	tx, err := a.store.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	// This is a local stop intent only. It grants no expired-lease cloud mutation
	// and cannot replace an already committed ready/superseded source operation.
	result, err := tx.ExecContext(ctx, `UPDATE storage_source_runs SET cloud_offline_json=?,offline_stop_state=?,updated_at=? WHERE operation_key=? AND paired_server_id=? AND plan_digest=? AND COALESCE(json_extract(cloud_offline_json,'$.ready'),0)=0 AND EXISTS(SELECT 1 FROM operations WHERE operation_key=? AND paired_server_id=? AND plan_digest=? AND superseded_by='')`, string(raw), state.StopState, a.now(), input.OperationKey, serverID, input.PlanDigest, input.OperationKey, serverID, input.PlanDigest)
	if err != nil {
		return false, err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return false, ErrLeaseConflict
	}
	_, err = tx.ExecContext(ctx, `UPDATE provider_receipts SET response_json=json_set(response_json,'$.offline',json(?),'$.offline_stop_state',?),updated_at=? WHERE paired_server_id=? AND operation_key=? AND action=? AND json_valid(response_json)`, string(raw), state.StopState, a.now(), serverID, input.OperationKey, nodeprotocol.StorageSourceActionMaterialize)
	if err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// Stop convergence has its own eight bounded control workers and never occupies
// media admission or delays another task's local deadline CAS. The durable
// requested/unknown state is retried read-only when an entered ACK was lost.
func (a *Agent) launchCloudOfflineStop(ctx context.Context, serverID string, input nodeprotocol.StorageSourceActionRequest, plan nodeprotocol.StorageSourcePlan) {
	if ctx.Err() != nil {
		return
	}
	key := serverID + "\x00" + input.OperationKey
	a.offlineStopMu.Lock()
	if a.offlineStopRuns == nil {
		a.offlineStopRuns = map[string]struct{}{}
	}
	if _, running := a.offlineStopRuns[key]; running || len(a.offlineStopRuns) >= 8 {
		a.offlineStopMu.Unlock()
		return
	}
	a.offlineStopRuns[key] = struct{}{}
	a.offlineStopWG.Add(1)
	a.offlineStopMu.Unlock()
	go func() {
		defer a.offlineStopWG.Done()
		defer func() { a.offlineStopMu.Lock(); delete(a.offlineStopRuns, key); a.offlineStopMu.Unlock() }()
		callCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
		defer cancel()
		_, credential, driver, authorized := a.offlineDeadlineAuthority(callCtx, serverID, input, plan)
		if authorized {
			_ = a.stopCloudOffline(callCtx, serverID, input, plan, credential, driver)
		}
	}()
}
func (a *Agent) queueCloudOfflineStops(ctx context.Context) error {
	rows, err := a.store.db.QueryContext(ctx, `SELECT r.paired_server_id,r.operation_key,r.task_id,r.plan_digest,r.storage_id,r.offline_stop_state,o.plan_json FROM storage_source_runs r JOIN operations o ON o.operation_key=r.operation_key WHERE r.offline_stop_state IN ('requested','entered','unknown','goal:requested','goal:entered','goal:unknown') AND o.superseded_by='' AND o.lease_expires_at>? AND o.status NOT IN ('cancelled','failed','completed') AND EXISTS(SELECT 1 FROM credential_grants g WHERE g.paired_server_id=r.paired_server_id AND g.operation_key=r.operation_key AND g.task_id=r.task_id AND g.expires_at>?) ORDER BY r.updated_at LIMIT 16`, a.now(), a.now())
	if err != nil {
		return err
	}
	type pending struct {
		server string
		input  nodeprotocol.StorageSourceActionRequest
		plan   nodeprotocol.StorageSourcePlan
	}
	var tasks []pending
	for rows.Next() {
		var item pending
		var stop, raw string
		if err := rows.Scan(&item.server, &item.input.OperationKey, &item.input.TaskID, &item.input.PlanDigest, &item.input.StorageID, &stop, &raw); err != nil {
			rows.Close()
			return err
		}
		var op nodeprotocol.OperationPlan
		if json.Unmarshal([]byte(raw), &op) != nil || json.Unmarshal(op.Payload, &item.plan) != nil || item.plan.CloudOfflineLifecycleVersion != 1 {
			continue
		}
		item.input.Action = nodeprotocol.StorageSourceActionOfflineStop
		item.input.RequestID = item.input.OperationKey + ":offline:reconcile"
		item.input.KeepMaterialization = strings.HasPrefix(stop, "goal:")
		tasks = append(tasks, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, item := range tasks {
		a.launchCloudOfflineStop(ctx, item.server, item.input, item.plan)
	}
	return nil
}
