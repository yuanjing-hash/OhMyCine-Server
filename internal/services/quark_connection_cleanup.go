package services

import (
	"context"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
)

const quarkCleanupPendingMessage = "目录验证已通过；验证文件清理尚未确认，不影响添加或使用数据源。可点击“重新核对验证文件”继续处理原文件，不会再次上传。"

type quarkProofCleaner interface {
	ProofCleanupState(context.Context, string) (string, error)
	CleanupCommonSpace(context.Context, string) (string, error)
}

type QuarkCleanupResult struct {
	Pending int    `json:"pending"`
	Message string `json:"message"`
}

// Retry existing proof receipts only. The caller cannot supply an item ID,
// directory, name or arbitrary deletion target, and cannot create a proof.
func (s *ConnectionService) RetryQuarkProofCleanup(ctx context.Context, actor Actor, id uint, request RequestContext) (QuarkCleanupResult, error) {
	if !actor.Can(authz.PermissionConnectionsRead) || !actor.Can(authz.PermissionConnectionsUpdate) || !actor.Can(authz.PermissionStoragesBrowse) || (!actor.Can(authz.PermissionStoragesCreate) && !actor.Can(authz.PermissionStoragesUpdate)) {
		return QuarkCleanupResult{}, appError(CodePermissionDenied, "无权清理目录验证文件", nil)
	}
	record, bundle, err := s.quarkCredentials(ctx, id)
	if err != nil {
		return QuarkCleanupResult{}, err
	}
	if !record.Enabled {
		return QuarkCleanupResult{}, appError(CodeConnectionUnavailable, "连接已停用", nil)
	}
	_, driver, err := s.driver(id)
	if err != nil {
		return QuarkCleanupResult{}, err
	}
	cleaner, ok := driver.(quarkProofCleaner)
	if !ok {
		return QuarkCleanupResult{}, appError(CodeConnectionUnavailable, "验证文件清理当前不可用", nil)
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	attempts := 0
	for _, proof := range bundle.Proofs {
		if !bundle.HasProof(proof.TVRoot, proof.AgentRoot) {
			continue
		}
		state, err := cleaner.ProofCleanupState(ctx, proof.ReceiptID)
		if err == nil && state == "complete" {
			continue
		}
		if attempts >= 8 || ctx.Err() != nil {
			break
		}
		attempts++
		_, _ = cleaner.CleanupCommonSpace(ctx, proof.ReceiptID)
	}
	status, err := s.QuarkStatus(ctx, actor, id)
	if err != nil {
		return QuarkCleanupResult{}, err
	}
	result := QuarkCleanupResult{Pending: status.RootCleanupPending, Message: "已确认原验证文件不在所选目录中"}
	outcome := "success"
	if result.Pending > 0 {
		result.Message = quarkCleanupPendingMessage
		outcome = "pending"
	} else if status.VerifiedRoots == 0 {
		result.Message = "没有需要核对的验证文件"
	}
	_ = s.audit.Record(nil, &actor.User.ID, "connection.quark.root.cleanup", "connection", uintID(id), outcome, map[string]any{"pending": result.Pending}, request)
	return result, nil
}
