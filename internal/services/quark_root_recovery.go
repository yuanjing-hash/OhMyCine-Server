package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
	"gorm.io/gorm"
)

const quarkRootRecoveryPurpose = "quark-root-recovery:v1"
const quarkRootRecoveryMessage = "原验证上传的结果仍未确认，旧记录会保留。可明确开启一次独立的新验证：上传新的极小随机文件，完整读回并保存证明后清理该文件；这不表示旧上传已取消或已清理。"

var quarkRootVerificationLocks sync.Map

type quarkRootAttemptFenceKey struct{}

// One Server owns the runtime DB lock. Serialize all proof calls for a
// connection, including Storage's ordinary ensure and explicit recovery. The
// persisted cursor/CAS, not this process lock, provides restart idempotency.
func lockQuarkRootVerification(ctx context.Context, id uint) (func(), error) {
	value, _ := quarkRootVerificationLocks.LoadOrStore(id, make(chan struct{}, 1))
	gate := value.(chan struct{})
	select {
	case gate <- struct{}{}:
		return func() { <-gate }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type quarkRootRecoveryInspector interface {
	CommonSpaceRecoveryState(context.Context, string, string, string) (bool, string, error)
}

type QuarkRootRecovery struct {
	Available     bool   `json:"available"`
	RecoveryToken string `json:"recovery_token,omitempty"`
	Message       string `json:"message"`
}

type quarkRootCursor struct {
	Version            int    `json:"version"`
	Base               string `json:"base"`
	Active             string `json:"active"`
	Previous           string `json:"previous,omitempty"`
	Configuration      uint64 `json:"configuration"`
	CredentialRevision uint64 `json:"credential_revision"`
	Identity           string `json:"identity"`
	Checkpoint         string `json:"checkpoint"`
}

type quarkRootRecoveryClaims struct {
	Version            int    `json:"v"`
	Actor              uint   `json:"actor"`
	Connection         uint   `json:"connection"`
	Configuration      uint64 `json:"configuration"`
	CredentialRevision uint64 `json:"credential_revision"`
	Identity           string `json:"identity"`
	Root               string `json:"root"`
	Path               string `json:"path"`
	Parent             string `json:"parent"`
	Checkpoint         string `json:"checkpoint"`
	Expires            int64  `json:"expires"`
}

func quarkRootProofBase(id uint, c quark.Credentials, root, path string) string {
	h := sha256.Sum256([]byte(uintID(id) + "\x00" + c.TV.AccountID + "\x00" + c.Agent.AccountID + "\x00" + root + "\x00" + path))
	return "quark-space-" + hex.EncodeToString(h[:])
}

func quarkRootSuccessor(parent string) string {
	h := sha256.Sum256([]byte("quark-root-successor:v1\x00" + parent))
	return "quark-space-" + hex.EncodeToString(h[:])
}

func quarkRootIdentity(c quark.Credentials) string {
	// Access/refresh rotation preserves identity, while reauthorization with a
	// different device or either account cannot inherit an accepted intent.
	raw, _ := json.Marshal([]string{c.TV.AccountID, c.TV.DeviceID, c.TV.ClientDeviceID, c.Agent.AccountID, c.Agent.DeviceID, c.Agent.ClientDeviceID})
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:])
}

func quarkRootProofID(id string) bool {
	if !strings.HasPrefix(id, "quark-space-") || len(id) != len("quark-space-")+64 {
		return false
	}
	decoded, err := hex.DecodeString(strings.TrimPrefix(id, "quark-space-"))
	return err == nil && len(decoded) == 32
}

func (s *ConnectionService) loadQuarkRootCursor(ctx context.Context, id uint, base string, tx *gorm.DB) (cloud.OperationReceipt, quarkRootCursor, error) {
	db := s.db
	if tx != nil {
		db = tx
	}
	store := NewQuarkOperationStore(db, s.credentials, id)
	r, err := store.Load(ctx, base+"/attempts")
	if errors.Is(err, cloud.ErrOperationNotFound) {
		return cloud.OperationReceipt{}, quarkRootCursor{Version: 1, Base: base, Active: base}, nil
	}
	if err != nil {
		return r, quarkRootCursor{}, err
	}
	var cursor quarkRootCursor
	if r.Provider != cloud.ProviderQuark || r.Route != string(quark.RouteAgent) || r.Kind != "space_proof_cursor" || r.State != "confirmed" || json.Unmarshal(r.Payload, &cursor) != nil || cursor.Version != 1 || cursor.Base != base || !quarkRootProofID(cursor.Active) || !quarkRootProofID(cursor.Previous) || cursor.Active != quarkRootSuccessor(cursor.Previous) || len(cursor.Identity) != 64 || len(cursor.Checkpoint) != 64 {
		return r, cursor, ErrObservationInvalid
	}
	return r, cursor, nil
}

func (s *ConnectionService) quarkRootRecoveryScope(ctx context.Context, actor Actor, id uint, root, path string) (models.Connection, quark.Credentials, error) {
	if !actor.Can(authz.PermissionConnectionsRead) || !actor.Can(authz.PermissionConnectionsUpdate) || !actor.Can(authz.PermissionStoragesBrowse) || (!actor.Can(authz.PermissionStoragesCreate) && !actor.Can(authz.PermissionStoragesUpdate)) {
		return models.Connection{}, quark.Credentials{}, appError(CodePermissionDenied, "无权开启独立目录验证", nil)
	}
	clean, err := normalizeProviderRelativePath(path)
	if err != nil || clean != path || strings.TrimSpace(root) == "" {
		return models.Connection{}, quark.Credentials{}, appError(CodeInvalidRequest, "目录验证范围无效，请重新选择", nil)
	}
	record, c, err := s.quarkCredentials(ctx, id)
	if err != nil {
		return record, c, err
	}
	if !record.Enabled || c.TV == nil || c.Agent == nil || !c.TV.IdentityVerified || !c.Agent.IdentityVerified {
		return record, c, appError(CodeConnectionUnavailable, "请先启用账号并完成原画播放和文件管理授权", nil)
	}
	return record, c, nil
}

// QuarkRootRecoveryInfo is local/read-only. The caller has already resolved a
// current actor-bound picker selection; this method never dispatches a write.
func (s *ConnectionService) QuarkRootRecoveryInfo(ctx context.Context, actor Actor, id uint, root, path string) (QuarkRootRecovery, error) {
	unlock, err := lockQuarkRootVerification(ctx, id)
	if err != nil {
		return QuarkRootRecovery{}, err
	}
	defer unlock()
	record, c, err := s.quarkRootRecoveryScope(ctx, actor, id, root, path)
	if err != nil {
		return QuarkRootRecovery{}, err
	}
	base := quarkRootProofBase(id, c, root, path)
	_, cursor, err := s.loadQuarkRootCursor(ctx, id, base, nil)
	if err != nil {
		return QuarkRootRecovery{}, s.quarkRootFailure(id, "proof_receipt_load", err)
	}
	_, driver, err := s.cachedDriver(id)
	if err != nil {
		return QuarkRootRecovery{}, err
	}
	inspector, ok := driver.(quarkRootRecoveryInspector)
	if !ok {
		return QuarkRootRecovery{}, appError(CodeConnectionUnavailable, "目录验证恢复当前不可用", nil)
	}
	available, version, err := inspector.CommonSpaceRecoveryState(ctx, cursor.Active, path, root)
	if err != nil {
		return QuarkRootRecovery{}, s.quarkRootFailure(id, "proof_receipt_load", err)
	}
	result := QuarkRootRecovery{Message: "无需开启独立验证；可继续核对当前验证结果"}
	if !available {
		return result, nil
	}
	claims := quarkRootRecoveryClaims{Version: 1, Actor: actor.User.ID, Connection: id, Configuration: record.Revision, CredentialRevision: c.Revision, Identity: quarkRootIdentity(c), Root: root, Path: path, Parent: cursor.Active, Checkpoint: version, Expires: time.Now().Add(10 * time.Minute).Unix()}
	raw, _ := json.Marshal(claims)
	token, err := s.credentials.Encrypt(quarkRootRecoveryPurpose, string(raw))
	if err != nil {
		return QuarkRootRecovery{}, appError(CodeConnectionUnavailable, "无法准备目录验证恢复", nil)
	}
	result.Available, result.RecoveryToken, result.Message = true, token, quarkRootRecoveryMessage
	return result, nil
}

func (s *ConnectionService) StartQuarkRootAttempt(ctx context.Context, actor Actor, id uint, root, path, token string, request RequestContext) (QuarkRootVerification, error) {
	unlock, err := lockQuarkRootVerification(ctx, id)
	if err != nil {
		return QuarkRootVerification{}, err
	}
	defer unlock()
	_, _, err = s.quarkRootRecoveryScope(ctx, actor, id, root, path)
	if err != nil {
		return QuarkRootVerification{}, err
	}
	var claims quarkRootRecoveryClaims
	if len(token) == 0 || len(token) > 8192 {
		return QuarkRootVerification{}, quarkRootRecoveryConflict()
	}
	raw, err := s.credentials.Decrypt(quarkRootRecoveryPurpose, token)
	if err != nil || json.Unmarshal([]byte(raw), &claims) != nil || claims.Version != 1 || claims.Actor != actor.User.ID || claims.Connection != id || claims.Root != root || claims.Path != path || !quarkRootProofID(claims.Parent) || len(claims.Identity) != 64 || len(claims.Checkpoint) != 64 {
		return QuarkRootVerification{}, quarkRootRecoveryConflict()
	}
	// Adoption is a short local transaction under the shared credential lock.
	// Never hold that lock across provider I/O (which may refresh credentials).
	err = quark.WithCredentialUpdate(ctx, id, func() error {
		record, c, err := s.quarkRootRecoveryScope(ctx, actor, id, root, path)
		if err != nil {
			return err
		}
		if record.Revision != claims.Configuration || quarkRootIdentity(c) != claims.Identity {
			return quarkRootRecoveryConflict()
		}
		base := quarkRootProofBase(id, c, root, path)
		return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			r, cursor, err := s.loadQuarkRootCursor(ctx, id, base, tx)
			if err != nil {
				return err
			}
			if cursor.Previous == claims.Parent && cursor.Active == quarkRootSuccessor(claims.Parent) {
				// A durable accepted intent joins its existing child even after a
				// lost response, restart, expiry or same-identity token refresh.
				if cursor.Configuration != claims.Configuration || cursor.CredentialRevision != claims.CredentialRevision || cursor.Identity != claims.Identity || cursor.Checkpoint != claims.Checkpoint {
					return quarkRootRecoveryConflict()
				}
				return nil
			}
			if cursor.Active != claims.Parent || claims.Expires <= time.Now().Unix() || c.Revision != claims.CredentialRevision {
				return quarkRootRecoveryConflict()
			}
			// Use a new native inspector with the transaction's receipt store;
			// inspection is strictly local and cannot submit/query the provider.
			encoded, _ := json.Marshal(c)
			inspector, err := quark.NewClient(cloud.Config{Credentials: encoded, Operations: NewQuarkOperationStore(tx, s.credentials, id)})
			if err != nil {
				return err
			}
			available, version, err := inspector.CommonSpaceRecoveryState(ctx, claims.Parent, path, root)
			if err != nil {
				return err
			}
			if !available || version != claims.Checkpoint {
				return quarkRootRecoveryConflict()
			}
			privateStore := &quarkOperationStore{db: tx, credentials: s.credentials, connectionID: id}
			oldProof, err := privateStore.Load(ctx, claims.Parent)
			if err != nil {
				return err
			}
			oldUpload, err := privateStore.Load(ctx, claims.Parent+"/upload")
			if err != nil {
				return err
			}
			if err = privateStore.classifyCommonSpaceTx(ctx, tx, oldProof, oldUpload); err != nil {
				return err
			}
			next := quarkRootCursor{Version: 1, Base: base, Active: quarkRootSuccessor(claims.Parent), Previous: claims.Parent, Configuration: claims.Configuration, CredentialRevision: claims.CredentialRevision, Identity: claims.Identity, Checkpoint: claims.Checkpoint}
			payload, _ := json.Marshal(next)
			store := NewQuarkOperationStore(tx, s.credentials, id)
			if r.ID == "" {
				r = cloud.OperationReceipt{ID: base + "/attempts", Provider: cloud.ProviderQuark, Route: string(quark.RouteAgent), AccountID: c.Agent.AccountID, Kind: "space_proof_cursor", State: "confirmed", Revision: 1, UpdatedAt: time.Now().UTC(), Payload: payload}
				return store.Create(ctx, r)
			}
			if r.AccountID != c.Agent.AccountID {
				return quarkRootRecoveryConflict()
			}
			expected := r.Revision
			r.Revision++
			r.UpdatedAt, r.Payload = time.Now().UTC(), payload
			return store.Update(ctx, expected, r)
		})
	})
	if err != nil {
		if errors.Is(err, ErrQuarkOperationConflict) || ErrorCode(err) == CodeConflict {
			return QuarkRootVerification{}, quarkRootRecoveryConflict()
		}
		return QuarkRootVerification{}, s.quarkRootFailure(id, "proof_receipt_save", err)
	}
	_ = s.audit.Record(nil, &actor.User.ID, "connection.quark.root.new_attempt", "connection", uintID(id), "accepted", map[string]any{"scope": "selected_root", "previous_retained": true}, request)
	ctx = context.WithValue(ctx, quarkRootAttemptFenceKey{}, claims)
	if err = s.ensureQuarkRootProofLocked(ctx, actor, id, root, path, request); err != nil {
		return QuarkRootVerification{}, err
	}
	return s.quarkRootVerificationResult(ctx, id, root, path), nil
}

func quarkRootRecoveryConflict() error {
	return appError(CodeConflict, "目录、授权或验证记录已变化，请重新读取恢复选项；已接受的验证记录会保留", nil)
}
