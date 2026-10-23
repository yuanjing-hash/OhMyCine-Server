package services

import (
	"context"
	"strings"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
)

type quarkCommonSpaceVerifier interface {
	VerifyCommonSpace(context.Context, string, string) (quark.SpaceProof, error)
}
type QuarkRootVerification struct {
	Verified       bool   `json:"verified"`
	Path           string `json:"path"`
	Message        string `json:"message"`
	CleanupState   string `json:"cleanup_state,omitempty"`
	CleanupMessage string `json:"cleanup_message,omitempty"`
}

func (s *ConnectionService) VerifyQuarkRoot(ctx context.Context, actor Actor, id uint, tvRoot, displayPath string, request RequestContext) (QuarkRootVerification, error) {
	if err := s.ensureQuarkRootProof(ctx, actor, id, tvRoot, displayPath, request); err != nil {
		return QuarkRootVerification{}, err
	}
	return s.quarkRootVerificationResult(ctx, id, tvRoot, displayPath), nil
}

func (s *ConnectionService) quarkRootVerificationResult(ctx context.Context, id uint, tvRoot, displayPath string) QuarkRootVerification {
	result := QuarkRootVerification{Verified: true, Path: displayPath, Message: "播放与管理授权已在此目录完成共同访问验证"}
	_, bundle, err := s.quarkCredentials(ctx, id)
	if err != nil {
		return result
	}
	_, driver, err := s.driver(id)
	if err != nil {
		return result
	}
	if cleanup, ok := driver.(quarkProofCleaner); ok {
		for _, proof := range bundle.Proofs {
			if proof.TVRoot == tvRoot && proof.Path == displayPath && bundle.HasProof(proof.TVRoot, proof.AgentRoot) {
				result.CleanupState, err = cleanup.ProofCleanupState(ctx, proof.ReceiptID)
				if err != nil || result.CleanupState != "complete" {
					result.CleanupState, result.CleanupMessage = "pending", quarkCleanupPendingMessage
				}
				break
			}
		}
	}
	return result
}
func (s *ConnectionService) ensureQuarkRootProof(ctx context.Context, actor Actor, id uint, tvRoot, displayPath string, request RequestContext) error {
	unlock, err := lockQuarkRootVerification(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()
	return s.ensureQuarkRootProofLocked(ctx, actor, id, tvRoot, displayPath, request)
}

func (s *ConnectionService) ensureQuarkRootProofLocked(ctx context.Context, actor Actor, id uint, tvRoot, displayPath string, request RequestContext) error {
	if !actor.Can(authz.PermissionConnectionsRead) || !actor.Can(authz.PermissionStoragesBrowse) || (!actor.Can(authz.PermissionStoragesCreate) && !actor.Can(authz.PermissionStoragesUpdate)) {
		return appError(CodePermissionDenied, "无权验证数据源目录", nil)
	}
	clean, err := normalizeProviderRelativePath(displayPath)
	if err != nil || clean != displayPath || strings.TrimSpace(tvRoot) == "" {
		return appError(CodeInvalidRequest, "目录验证范围无效，请重新选择", nil)
	}
	record, bundle, err := s.quarkCredentials(ctx, id)
	if err != nil {
		return err
	}
	if bundle.TV == nil || bundle.Agent == nil || !bundle.TV.IdentityVerified || !bundle.Agent.IdentityVerified {
		return appError(CodeConnectionUnavailable, "请先完成原画播放和文件管理授权", nil)
	}
	identity := quarkRootIdentity(bundle)
	if expected, ok := ctx.Value(quarkRootAttemptFenceKey{}).(quarkRootRecoveryClaims); ok && (expected.Configuration != record.Revision || expected.Identity != identity) {
		return quarkRootRecoveryConflict()
	}
	ctx = quark.WithCommonSpaceGuard(ctx, func(ctx context.Context) error {
		current, saved, err := s.quarkCredentials(ctx, id)
		if err != nil || !current.Enabled || current.Revision != record.Revision || saved.TV == nil || saved.Agent == nil || !saved.TV.IdentityVerified || !saved.Agent.IdentityVerified || quarkRootIdentity(saved) != identity {
			return appError(CodeConflict, "连接或授权身份已变化，请重新读取状态", nil)
		}
		return nil
	})
	_, driver, err := s.driver(id)
	if err != nil {
		return err
	}
	resolver, ok := driver.(cloud.DirectoryPathResolver)
	if !ok {
		return appError(CodeConnectionUnavailable, "夸克目录定位不可用", nil)
	}
	current, err := resolver.ResolveDirectory(ctx, clean)
	if err != nil {
		return s.quarkRootFailure(id, "playback_directory", err)
	}
	if !current.IsDir || current.ID != tvRoot {
		return appError(CodeConflict, "目录位置已变化，请重新选择", nil)
	}
	verifier, ok := driver.(quarkCommonSpaceVerifier)
	if !ok {
		return appError(CodeConnectionUnavailable, "夸克目录验证能力不可用", nil)
	}
	// Ordinary verification follows the durably selected attempt. It never
	// creates a successor for an unresolved upload; only the separate explicit
	// administrator recovery action can advance this cursor.
	base := quarkRootProofBase(id, bundle, tvRoot, clean)
	_, cursor, err := s.loadQuarkRootCursor(ctx, id, base, nil)
	if err != nil {
		return s.quarkRootFailure(id, "proof_receipt_load", err)
	}
	if cursor.Previous != "" && (cursor.Configuration != record.Revision || cursor.Identity != identity) {
		alreadyVerified := false
		for _, saved := range bundle.Proofs {
			if saved.TVRoot == tvRoot && saved.Path == clean && bundle.HasProof(saved.TVRoot, saved.AgentRoot) {
				alreadyVerified = true
			}
		}
		if !alreadyVerified {
			return quarkRootRecoveryConflict()
		}
	}
	receiptID := cursor.Active
	proof, err := verifier.VerifyCommonSpace(ctx, receiptID, clean)
	if err != nil {
		return s.quarkRootFailure(id, "verification_input", err)
	}
	if proof.TVRoot != tvRoot || proof.Path != clean || proof.TVAccount != bundle.TV.AccountID || proof.AgentAccount != bundle.Agent.AccountID {
		return appError(CodeConflict, "目录或授权身份发生变化，请重新读取状态", nil)
	}
	if err = s.guardQuarkConnection(ctx, id, record.Revision); err != nil {
		return appError(CodeConflict, "连接配置已变化，请重新读取状态", nil)
	}
	_, saved, err := s.quarkCredentials(ctx, id)
	if err != nil {
		return s.quarkRootFailure(id, "proof_credentials_save", err)
	}
	if !saved.HasProof(proof.TVRoot, proof.AgentRoot) {
		return appError(CodeConnectionUnavailable, "目录验证尚未保存，请稍后重试", nil)
	}
	_ = s.audit.Record(nil, &actor.User.ID, "connection.quark.root.verify", "connection", uintID(id), "success", map[string]any{"scope": "selected_root"}, request)
	return nil
}
