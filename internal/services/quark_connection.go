package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
	"gorm.io/gorm"
)

// Quark status is an explicit allowlist. Neither the encrypted bundle, route
// identifiers nor private verification receipts cross the HTTP boundary.
type QuarkAuthorizationStatus struct {
	Configured bool   `json:"configured"`
	Ready      bool   `json:"ready"`
	State      string `json:"state"`
}
type QuarkConnectionStatus struct {
	ConnectionID       uint                     `json:"connection_id"`
	Playback           QuarkAuthorizationStatus `json:"playback"`
	Management         QuarkAuthorizationStatus `json:"management"`
	Web                QuarkAuthorizationStatus `json:"additional_management"`
	VerifiedRoots      int                      `json:"verified_roots"`
	RootCleanupPending int                      `json:"root_cleanup_pending"`
	Capabilities       cloud.Capabilities       `json:"capabilities"`
}

func quarkAuthorizationStatus(a *quark.Authorization) QuarkAuthorizationStatus {
	if a == nil || a.AccessToken == "" {
		return QuarkAuthorizationStatus{State: "not_authorized"}
	}
	if !a.IdentityVerified {
		return QuarkAuthorizationStatus{Configured: true, State: "identity_pending"}
	}
	return QuarkAuthorizationStatus{Configured: true, Ready: true, State: "ready"}
}

func (s *ConnectionService) quarkCredentials(ctx context.Context, id uint) (models.Connection, quark.Credentials, error) {
	var record models.Connection
	if err := s.db.WithContext(ctx).First(&record, id).Error; err != nil {
		return record, quark.Credentials{}, connectionNotFound(err)
	}
	if record.Provider != cloud.ProviderQuark {
		return record, quark.Credentials{}, appError(CodeConnectionProviderUnsupported, "该连接不是夸克网盘", nil)
	}
	raw, err := s.credentials.Decrypt(connectionPurpose(id, record.Provider), record.CredentialCiphertext)
	if err != nil {
		return record, quark.Credentials{}, appError(CodeConnectionUnavailable, "夸克授权数据不可用", nil)
	}
	bundle, err := quark.ParseCredentials(json.RawMessage(raw))
	if err != nil {
		return record, quark.Credentials{}, appError(CodeConnectionUnavailable, "夸克授权数据不可用", nil)
	}
	return record, bundle, nil
}

func (s *ConnectionService) QuarkStatus(ctx context.Context, actor Actor, id uint) (QuarkConnectionStatus, error) {
	if !actor.Can(authz.PermissionConnectionsRead) {
		return QuarkConnectionStatus{}, appError(CodePermissionDenied, "无权查看连接", nil)
	}
	record, bundle, err := s.quarkCredentials(ctx, id)
	if err != nil {
		return QuarkConnectionStatus{}, err
	}
	result := QuarkConnectionStatus{ConnectionID: id, Playback: quarkAuthorizationStatus(bundle.TV), Management: quarkAuthorizationStatus(bundle.Agent), Web: QuarkAuthorizationStatus{State: "not_required"}}
	if bundle.Web != nil {
		result.Web = QuarkAuthorizationStatus{Configured: bundle.Web.Cookie != "", State: "unavailable"}
	}
	for _, proof := range bundle.Proofs {
		if bundle.HasProof(proof.TVRoot, proof.AgentRoot) {
			result.VerifiedRoots++
			// An unavailable driver must not turn unconfirmed cleanup into zero.
			result.RootCleanupPending++
		}
	}
	if record.Enabled {
		if _, driver, err := s.driver(id); err == nil {
			result.Capabilities = driver.Capabilities()
			if cleaner, ok := driver.(quarkProofCleaner); ok {
				for _, proof := range bundle.Proofs {
					if !bundle.HasProof(proof.TVRoot, proof.AgentRoot) {
						continue
					}
					state, err := cleaner.ProofCleanupState(ctx, proof.ReceiptID)
					if err == nil && state == "complete" {
						result.RootCleanupPending--
					}
				}
			}
		}
	}
	return result, nil
}

func (s *ConnectionService) guardQuarkConnection(ctx context.Context, id uint, revision uint64) error {
	var current models.Connection
	if err := s.db.WithContext(ctx).Select("id", "provider", "revision", "enabled").First(&current, id).Error; err != nil {
		return cloud.Error(cloud.CodeUnavailable, true, nil)
	}
	if current.Provider != cloud.ProviderQuark || current.Revision != revision || !current.Enabled {
		return cloud.Error(cloud.CodeUnavailable, false, nil)
	}
	return nil
}

func (s *ConnectionService) guardQuarkRequest(ctx context.Context, id uint, revision uint64) error {
	if err := s.guardQuarkConnection(ctx, id, revision); err != nil {
		return err
	}
	return CheckQuarkAccountCooldown(ctx, s.db, id)
}

// Quark rotations use their own revision. Advancing an access token does not
// change a Storage source lifetime or revoke a still-valid same-account proof.
// CAS checks both the administrator revision and the encrypted bundle revision.
func (s *ConnectionService) quarkCredentialPersister(id uint, configurationRevision uint64) quark.PersistFunc {
	return func(ctx context.Context, expected uint64, raw json.RawMessage) error {
		next, err := quark.ParseCredentials(raw)
		if err != nil || next.Revision != expected+1 {
			return appError(CodeConflict, "夸克授权版本不一致，请重新读取连接状态", nil)
		}
		err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			var record models.Connection
			if err := tx.First(&record, id).Error; err != nil {
				return connectionNotFound(err)
			}
			if record.Provider != cloud.ProviderQuark || record.Revision != configurationRevision || !record.Enabled {
				return appError(CodeConflict, "连接配置已变化，请重新授权", nil)
			}
			previousRaw, err := s.credentials.Decrypt(connectionPurpose(id, record.Provider), record.CredentialCiphertext)
			if err != nil {
				return appError(CodeConnectionUnavailable, "无法读取已保存的夸克授权", nil)
			}
			previous, err := quark.ParseCredentials(json.RawMessage(previousRaw))
			if err != nil || previous.Revision != expected {
				return appError(CodeConflict, "夸克授权已被更新，请重新读取连接状态", nil)
			}
			identityChanged := quarkIdentityChanged(previous.TV, next.TV) || quarkIdentityChanged(previous.Agent, next.Agent)
			if identityChanged {
				var bound int64
				if err := tx.Model(&models.Storage{}).Where("connection_id = ?", id).Count(&bound).Error; err != nil {
					return err
				}
				if bound > 0 {
					return appError(CodeConflict, "该连接已有数据源，不能更换为另一个夸克账号；请新建连接", nil)
				}
				next.Proofs = nil
			}
			encoded, err := json.Marshal(next)
			if err != nil {
				return err
			}
			ciphertext, err := s.credentials.Encrypt(connectionPurpose(id, record.Provider), string(encoded))
			if err != nil {
				return err
			}
			updates := map[string]any{"credential_ciphertext": ciphertext, "updated_at": time.Now().UTC()}
			if next.TV != nil && next.TV.IdentityVerified {
				updates["account_id"] = quarkAccountReference(next.TV.AccountID)
				updates["account_name"] = "夸克网盘"
			}
			result := tx.Model(&models.Connection{}).Where("id=? AND revision=? AND credential_ciphertext=?", id, configurationRevision, record.CredentialCiphertext).Updates(updates)
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return appError(CodeConflict, "夸克授权已被更新", nil)
			}
			return nil
		})
		if err == nil {
			s.invalidate(id)
		}
		return err
	}
}

func quarkIdentityChanged(before, after *quark.Authorization) bool {
	return before != nil && after != nil && before.AccountID != "" && after.AccountID != "" && before.AccountID != after.AccountID
}
func quarkAccountReference(account string) string {
	sum := sha256.Sum256([]byte("quark-tv-account\x00" + account))
	return "quark:" + hex.EncodeToString(sum[:16])
}

func (s *ConnectionService) testQuarkConnection(ctx context.Context, actor Actor, record models.Connection, request RequestContext) (ConnectionSummary, error) {
	_, driver, err := s.driver(record.ID)
	if err == nil {
		_, err = driver.Probe(ctx)
	}
	now := time.Now().UTC()
	status, code, outcome := "online", "", "success"
	if err != nil {
		status, code, outcome = "offline", quarkSafeErrorCode(err), "failure"
	}
	result := s.db.WithContext(ctx).Model(&models.Connection{}).Where("id=? AND revision=?", record.ID, record.Revision).Updates(map[string]any{"last_health_status": status, "last_health_error_code": code, "last_health_checked_at": now})
	if result.Error != nil {
		return ConnectionSummary{}, result.Error
	}
	if result.RowsAffected != 1 {
		return ConnectionSummary{}, appError(CodeConflict, "连接配置已变化，请重试", nil)
	}
	_ = s.audit.Record(nil, &actor.User.ID, "connection.test", "connection", uintID(record.ID), outcome, map[string]any{"provider": cloud.ProviderQuark, "error_code": code}, request)
	if err != nil {
		return ConnectionSummary{}, quarkServiceError(err)
	}
	if err = s.db.WithContext(ctx).First(&record, record.ID).Error; err != nil {
		return ConnectionSummary{}, err
	}
	return connectionSummary(record), nil
}

func quarkErrorMessage(code string) string {
	switch code {
	case quark.CodeAuth, cloud.CodeAuthExpired:
		return "夸克授权未完成或已失效，请重新登录对应功能"
	case quark.CodeLimited, cloud.CodeRateLimited:
		return "夸克请求受到限速，请稍后重试"
	case quark.CodeIdentity:
		return "夸克账号身份与已保存授权不一致"
	case quark.CodeProof:
		return "尚未验证该目录的播放与管理授权，请重新选择目录完成验证"
	case quark.CodePending:
		return "夸克已接收操作，正在核对原操作结果，请稍后重试"
	case quark.CodeUnsupported:
		return "该夸克功能当前不可用"
	case CodeConflict:
		return "连接或授权已更新，请重新读取状态"
	default:
		return "无法完成夸克请求，请检查网络并重试"
	}
}

// Cache identity is intentionally opaque and never contains provider tokens.
// It changes for rotated credentials and is safe to include only internally.
func (s *ConnectionService) CloudCredentialCacheKey(ctx context.Context, id uint) (string, error) {
	var record models.Connection
	if err := s.db.WithContext(ctx).Select("id", "provider", "revision", "credential_ciphertext", "enabled").First(&record, id).Error; err != nil {
		return "", connectionNotFound(err)
	}
	if !record.Enabled {
		return "", appError(CodeConnectionUnavailable, "连接已停用", nil)
	}
	h := sha256.Sum256([]byte(record.Provider + "\x00" + record.CredentialCiphertext))
	return hex.EncodeToString(h[:]), nil
}
