package nodeagent

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
)

var errStorageSourceCleanupNotReady = errors.New("node_storage_source_cleanup_not_ready")

func (a *Agent) storageSourceCleanup(w http.ResponseWriter, r *http.Request) {
	operationKey := r.PathValue("operation_key")
	var input nodeprotocol.StorageSourceCleanupRequest
	if err := decodeJSON(w, r, &input); err != nil {
		return
	}
	if input.Validate() != nil || input.OperationKey != operationKey {
		writeError(w, http.StatusBadRequest, "node_storage_source_cleanup_invalid", "来源暂存清理请求无效")
		return
	}
	digest, err := input.Digest()
	if err != nil {
		writeError(w, http.StatusBadRequest, "node_storage_source_cleanup_invalid", "来源暂存清理请求无效")
		return
	}
	serverID := r.Header.Get("X-OhMyCine-Verified-Server-ID")
	if receipt, getErr := a.store.ProviderReceipt(r.Context(), serverID, input.RequestID); getErr == nil {
		if receipt.RequestDigest != digest || receipt.OperationKey != operationKey || receipt.Action != nodeprotocol.StorageSourceActionCleanup {
			writeError(w, http.StatusConflict, nodeprotocol.ErrorPlanConflict, "相同操作编号对应了不同请求")
			return
		}
		if receipt.Status == "completed" {
			var response nodeprotocol.StorageSourceCleanupResponse
			if json.Unmarshal([]byte(receipt.ResponseJSON), &response) == nil {
				writeJSON(w, http.StatusOK, response)
				return
			}
		}
	}
	a.sourceMu.Lock()
	_, running := a.sourceRuns[operationKey]
	a.sourceMu.Unlock()
	if running {
		writeError(w, http.StatusConflict, "node_storage_source_cleanup_not_ready", "来源暂存仍在使用中")
		return
	}
	if err := a.store.ValidateStorageSourceCleanup(r.Context(), serverID, operationKey, input.TaskID, input.PlanDigest); err != nil {
		if errors.Is(err, errStorageSourceCleanupNotReady) {
			writeError(w, http.StatusConflict, err.Error(), "来源暂存尚不能清理")
			return
		}
		writeError(w, http.StatusNotFound, "node_storage_source_cleanup_not_found", "来源暂存清理目标不存在")
		return
	}
	if _, _, err := a.store.BeginActionReceipt(r.Context(), serverID, input.RequestID, operationKey, nodeprotocol.StorageSourceActionCleanup, digest, a.now()); err != nil {
		writeError(w, http.StatusConflict, nodeprotocol.ErrorPlanConflict, "来源暂存清理请求冲突")
		return
	}
	if err := a.removeStorageSourceRoot(operationKey); err != nil {
		_ = a.store.CompleteActionReceipt(r.Context(), serverID, input.RequestID, nil, "node_storage_source_cleanup_failed", a.now())
		writeError(w, http.StatusInternalServerError, "node_storage_source_cleanup_failed", "来源暂存清理失败，可以安全重试")
		return
	}
	response := nodeprotocol.StorageSourceCleanupResponse{RequestID: input.RequestID, OperationKey: operationKey, TaskID: input.TaskID, Status: nodeprotocol.StorageSourceCompleted, Cleaned: true}
	if err := a.store.CompleteStorageSourceCleanup(r.Context(), serverID, operationKey, input, response, a.now()); err != nil {
		writeError(w, http.StatusInternalServerError, "node_persistence_failed", "来源暂存已经清理，但节点状态尚未保存，可以安全重试")
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (a *Agent) removeStorageSourceRoot(operationKey string) error {
	managed, err := filepath.EvalSymlinks(filepath.Clean(a.config.ManagedRoot))
	if err != nil {
		return errors.New("node_managed_root_invalid")
	}
	base := filepath.Join(managed, "storage-source")
	baseInfo, err := os.Lstat(base)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !baseInfo.IsDir() || baseInfo.Mode()&os.ModeSymlink != 0 {
		return errors.New("node_storage_source_cleanup_path_invalid")
	}
	resolvedBase, err := filepath.EvalSymlinks(base)
	if err != nil || !sameCleanPath(resolvedBase, base) || requirePathWithin(managed, resolvedBase) != nil {
		return errors.New("node_storage_source_cleanup_path_invalid")
	}
	digest := sha256.Sum256([]byte(operationKey))
	root := filepath.Join(resolvedBase, hex.EncodeToString(digest[:16]))
	relative, err := filepath.Rel(resolvedBase, root)
	if err != nil || relative == "." || filepath.Dir(relative) != "." || requirePathWithin(resolvedBase, root) != nil {
		return errors.New("node_storage_source_cleanup_path_invalid")
	}
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("node_storage_source_cleanup_path_invalid")
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil || !sameCleanPath(resolved, root) || requirePathWithin(resolvedBase, resolved) != nil {
		return errors.New("node_storage_source_cleanup_path_invalid")
	}
	if err := os.RemoveAll(root); err != nil {
		return err
	}
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		return errors.New("node_storage_source_cleanup_failed")
	}
	return nil
}

func (s *Store) ValidateStorageSourceCleanup(ctx context.Context, serverID, operationKey, taskID, planDigest string) error {
	var operationStatus, runStatus string
	err := s.db.QueryRowContext(ctx, `SELECT o.status,r.status FROM operations o JOIN storage_source_runs r ON r.operation_key=o.operation_key WHERE o.operation_key=? AND o.paired_server_id=? AND o.task_id=? AND o.plan_digest=? AND r.paired_server_id=? AND r.task_id=? AND r.plan_digest=?`, operationKey, serverID, taskID, planDigest, serverID, taskID, planDigest).Scan(&operationStatus, &runStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrOperationNotFound
	}
	if err != nil {
		return err
	}
	if operationStatus != nodeprotocol.OperationCompleted && operationStatus != nodeprotocol.OperationCancelled {
		return errStorageSourceCleanupNotReady
	}
	if operationStatus == nodeprotocol.OperationCompleted && runStatus != nodeprotocol.StorageSourceCompleted {
		return errStorageSourceCleanupNotReady
	}
	return nil
}

func (s *Store) CompleteStorageSourceCleanup(ctx context.Context, serverID, operationKey string, input nodeprotocol.StorageSourceCleanupRequest, response nodeprotocol.StorageSourceCleanupResponse, now time.Time) error {
	raw, err := json.Marshal(response)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM file_exports WHERE operation_key=? AND paired_server_id=? AND task_id=?`, operationKey, serverID, input.TaskID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM storage_source_chunks WHERE operation_key=?`, operationKey); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM storage_source_files WHERE operation_key=?`, operationKey); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE storage_source_runs SET cleanup_status='completed',cleaned_at=?,updated_at=? WHERE operation_key=? AND paired_server_id=? AND task_id=? AND plan_digest=?`, now.UTC(), now.UTC(), operationKey, serverID, input.TaskID, input.PlanDigest)
	if err != nil {
		return err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return ErrOperationNotFound
	}
	result, err = tx.ExecContext(ctx, `UPDATE provider_receipts SET status='completed',response_json=?,error_code='',updated_at=? WHERE request_id=? AND paired_server_id=? AND operation_key=? AND action=?`, string(raw), now.UTC(), input.RequestID, serverID, operationKey, nodeprotocol.StorageSourceActionCleanup)
	if err != nil {
		return err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return ErrOperationNotFound
	}
	return tx.Commit()
}
