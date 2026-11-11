package nodeagent

import (
	"bytes"
	"context"
	"encoding/json"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
)

const storageUploadRetrySealed = "!upload-retry-sealed"

// AcceptStorageUploadRetry is called while storageMu proves that the exact old
// executor has joined. The transaction seals its persisted write authority;
// delayed lease renewals cannot resurrect the unknown original upload.
func (s *Store) AcceptStorageUploadRetry(ctx context.Context, serverID string, input nodeprotocol.StorageActionRequest, payload nodeprotocol.StorageUploadPlan, now time.Time) error {
	previous := payload.RetryOf
	if previous == nil || payload.Validate() != nil || nodeprotocol.StorageUploadRetryKey(previous.OperationKey, previous.ActionVersion) != input.OperationKey {
		return ErrPlanConflict
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var priorRaw, priorDigest, priorStatus, superseded, nextRaw, nextDigest, nextStatus string
	var priorExpiry, nextExpiry time.Time
	if err := tx.QueryRowContext(ctx, `SELECT plan_json,plan_digest,status,lease_expires_at,superseded_by FROM operations WHERE operation_key=? AND paired_server_id=?`, previous.OperationKey, serverID).Scan(&priorRaw, &priorDigest, &priorStatus, &priorExpiry, &superseded); err != nil {
		return ErrPlanConflict
	}
	if err := tx.QueryRowContext(ctx, `SELECT plan_json,plan_digest,status,lease_expires_at FROM operations WHERE operation_key=? AND paired_server_id=?`, input.OperationKey, serverID).Scan(&nextRaw, &nextDigest, &nextStatus, &nextExpiry); err != nil {
		return ErrPlanConflict
	}
	var prior, next nodeprotocol.OperationPlan
	var priorPayload nodeprotocol.StorageUploadPlan
	if priorDigest != previous.PlanDigest || nextDigest != input.PlanDigest || json.Unmarshal([]byte(priorRaw), &prior) != nil || json.Unmarshal([]byte(nextRaw), &next) != nil || json.Unmarshal(prior.Payload, &priorPayload) != nil || priorPayload.Validate() != nil || prior.Kind != nodeprotocol.OperationKindStorageUpload || next.Kind != prior.Kind || prior.TaskID != input.TaskID || next.TaskID != prior.TaskID || next.NodeID != prior.NodeID || next.OperationKey != input.OperationKey || prior.OperationKey != previous.OperationKey {
		return ErrPlanConflict
	}
	priorPayload.RetryOf = nil
	currentPayload := payload
	currentPayload.RetryOf = nil
	before, _ := json.Marshal(priorPayload)
	after, _ := json.Marshal(currentPayload)
	if !bytes.Equal(before, after) || !nextExpiry.After(now) || nextStatus == nodeprotocol.OperationCompleted || nextStatus == nodeprotocol.OperationCancelled || nextStatus == nodeprotocol.OperationFailed {
		return ErrPlanConflict
	}
	if superseded == input.OperationKey && priorStatus == nodeprotocol.OperationCancelled {
		return tx.Commit() // lost ACK/restart resumes this same child, never another
	}
	if superseded != storageUploadRetrySealed || priorExpiry.After(now) || priorStatus == nodeprotocol.OperationCompleted || priorStatus == nodeprotocol.OperationCancelled {
		return ErrLeaseConflict
	}
	result, err := tx.ExecContext(ctx, `UPDATE operations SET superseded_by=?,status=?,phase=?,error_code=?,revision=revision+1,updated_at=? WHERE operation_key=? AND paired_server_id=? AND plan_digest=? AND superseded_by=? AND lease_expires_at<=? AND status NOT IN (?,?)`, input.OperationKey, nodeprotocol.OperationCancelled, nodeprotocol.OperationCancelled, "node_upload_attempt_superseded", now.UTC(), previous.OperationKey, serverID, previous.PlanDigest, storageUploadRetrySealed, now.UTC(), nodeprotocol.OperationCompleted, nodeprotocol.OperationCancelled)
	if err != nil {
		return err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return ErrLeaseConflict
	}
	return tx.Commit()
}
