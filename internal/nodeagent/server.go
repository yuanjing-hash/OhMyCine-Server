package nodeagent

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/buildinfo"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
)

type Agent struct {
	config            Config
	store             *Store
	sealingPrivateKey *ecdh.PrivateKey
	sealingPublicKey  string
	now               func() time.Time
	exportMu          sync.Mutex
	exports           map[string]struct{}
	storageMu         sync.Mutex
	storageRuns       map[string]struct{}
	storageFactory    storageDriverFactory
	sourceMu          sync.Mutex
	sourceRuns        map[string]struct{}
	sourceFactory     storageSourceDriverFactory
	httpStarted       time.Time
}

func New(config Config, store *Store) (*Agent, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if store == nil {
		return nil, errors.New("node store is required")
	}
	if err := os.MkdirAll(config.ManagedRoot, 0o700); err != nil {
		return nil, err
	}
	if !config.AllowInsecureDevelopment && config.EnrollmentToken == "" {
		if _, err := store.Identity(context.Background()); err != nil {
			return nil, errors.New("node_enrollment_or_paired_identity_required")
		}
	}
	privateKey, publicKey, err := loadSealingIdentity(config.SealingPrivateKeyFile)
	if err != nil && config.AllowInsecureDevelopment {
		privateKey, err = ecdh.X25519().GenerateKey(rand.Reader)
		if err == nil {
			publicKey = base64.RawURLEncoding.EncodeToString(privateKey.PublicKey().Bytes())
		}
	}
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	return &Agent{config: config, store: store, sealingPrivateKey: privateKey, sealingPublicKey: publicKey, now: func() time.Time { return time.Now().UTC() }, httpStarted: now, exports: map[string]struct{}{}, storageRuns: map[string]struct{}{}, storageFactory: newStorageDriver, sourceRuns: map[string]struct{}{}, sourceFactory: newStorageSourceDriver}, nil
}

func (a *Agent) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /node/v1/health", a.health)
	mux.HandleFunc("POST /node/v1/enrollment/challenge", a.enrollmentChallenge)
	mux.HandleFunc("POST /node/v1/enrollment/complete", a.enrollmentComplete)
	mux.HandleFunc("PUT /node/v1/operations/{operation_key}", a.requireServer(a.putOperation))
	mux.HandleFunc("GET /node/v1/operations/{operation_key}", a.requireServer(a.getOperation))
	mux.HandleFunc("POST /node/v1/operations/{operation_key}/lease", a.requireServer(a.renewLease))
	mux.HandleFunc("POST /node/v1/operations/{operation_key}/cancel", a.requireServer(a.cancelOperation))
	mux.HandleFunc("GET /node/v1/operations/{operation_key}/manifest", a.requireServer(a.fileExportManifest))
	mux.HandleFunc("GET /node/v1/operations/{operation_key}/chunks/{file_token}", a.requireServer(a.fileExportChunks))
	mux.HandleFunc("GET /node/v1/operations/{operation_key}/files/{file_token}", a.requireServer(a.fileExportRange))
	mux.HandleFunc("PUT /node/v1/credential-grants/{grant_id}", a.requireServer(a.putCredentialGrant))
	mux.HandleFunc("POST /node/v1/downloader/actions", a.requireServer(a.downloaderAction))
	mux.HandleFunc("POST /node/v1/storage/actions", a.requireServer(a.storageAction))
	mux.HandleFunc("GET /node/v1/storage/actions/{request_id}", a.requireServer(a.storageActionResult))
	mux.HandleFunc("POST /node/v1/storage-source/actions", a.requireServer(a.storageSourceAction))
	mux.HandleFunc("GET /node/v1/storage-source/actions/{request_id}", a.requireServer(a.storageSourceActionResult))
	mux.HandleFunc("POST /node/v1/operations/{operation_key}/cleanup", a.requireServer(a.storageSourceCleanup))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if a.config.Transport == "http" && !a.config.AllowInsecureDevelopment && !strings.HasPrefix(r.URL.Path, "/node/v1/enrollment/") {
			a.signedHTTPHandler(mux, w, r)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (a *Agent) health(w http.ResponseWriter, r *http.Request) {
	if !a.config.AllowInsecureDevelopment {
		if _, err := a.store.Identity(r.Context()); err == nil {
			if !a.authorizeServer(w, r) {
				return
			}
		} else if !errors.Is(err, ErrOperationNotFound) {
			writeError(w, http.StatusInternalServerError, "node_persistence_failed", "节点无法读取配对状态")
			return
		} else {
			writeJSON(w, http.StatusOK, map[string]string{"status": "pending_enrollment"})
			return
		}
	}
	free := managedFreeBytes(a.config.ManagedRoot)
	osName, arch := Platform()
	writeJSON(w, http.StatusOK, nodeprotocol.HealthResponse{Status: "ok", AgentVersion: buildinfo.Current().Version, ProtocolMin: 1, ProtocolMax: 1, OS: osName, Arch: arch, NodeID: a.config.NodeID, Capabilities: a.config.Capabilities(free), Revision: 1})
}

func (a *Agent) enrollmentChallenge(w http.ResponseWriter, r *http.Request) {
	if _, err := a.store.Identity(r.Context()); err == nil {
		writeError(w, http.StatusForbidden, "node_enrollment_unavailable", "节点已经完成配对")
		return
	} else if !errors.Is(err, ErrOperationNotFound) {
		writeError(w, http.StatusInternalServerError, "node_persistence_failed", "节点无法读取配对状态")
		return
	}
	if a.config.EnrollmentToken == "" {
		writeError(w, http.StatusForbidden, "node_enrollment_unavailable", "节点已经完成配对或未配置配对令牌")
		return
	}
	var input nodeprotocol.EnrollmentChallengeRequest
	if err := decodeJSON(w, r, &input); err != nil {
		return
	}
	if input.NodeID != a.config.NodeID || !validEnrollmentID(input.ServerID) || strings.TrimSpace(input.Nonce) == "" || len(input.Nonce) > 128 {
		writeError(w, http.StatusBadRequest, "node_enrollment_invalid", "配对身份无效")
		return
	}
	if !a.validEnrollmentRequest(r, input) {
		writeError(w, http.StatusUnauthorized, "node_enrollment_token_invalid", "配对令牌无效")
		return
	}
	now := a.now()
	if err := a.store.SaveChallenge(r.Context(), input.Nonce, input.ServerID, now.Add(10*time.Minute)); err != nil {
		writeError(w, http.StatusInternalServerError, "node_persistence_failed", "节点无法保存配对挑战")
		return
	}
	nodeFP := a.nodeCertificateFingerprint()
	proof := nodeprotocol.EnrollmentProof([]byte(a.config.EnrollmentToken), "node", input.NodeID, input.ServerID, input.Nonce, nodeFP, a.sealingPublicKey, "")
	writeJSON(w, http.StatusOK, nodeprotocol.EnrollmentChallengeResponse{NodeID: input.NodeID, ServerID: input.ServerID, Nonce: input.Nonce, NodeCertificateFingerprint: nodeFP, NodeEncryptionPublicKey: a.sealingPublicKey, NodeProof: proof, ExpiresAt: now.Add(10 * time.Minute)})
}

func (a *Agent) enrollmentComplete(w http.ResponseWriter, r *http.Request) {
	if _, err := a.store.Identity(r.Context()); err == nil {
		writeError(w, http.StatusForbidden, "node_enrollment_unavailable", "节点已经完成配对")
		return
	} else if !errors.Is(err, ErrOperationNotFound) {
		writeError(w, http.StatusInternalServerError, "node_persistence_failed", "节点无法读取配对状态")
		return
	}
	if a.config.EnrollmentToken == "" {
		writeError(w, http.StatusForbidden, "node_enrollment_unavailable", "节点已经完成配对或未配置配对令牌")
		return
	}
	var input nodeprotocol.EnrollmentCompleteRequest
	if err := decodeJSON(w, r, &input); err != nil {
		return
	}
	if input.NodeID != a.config.NodeID || strings.TrimSpace(input.ServerID) == "" || strings.TrimSpace(input.Nonce) == "" || input.NodeEncryptionPublicKey != a.sealingPublicKey || !nodeprotocol.ValidDigest(input.ServerCertificateFingerprint) {
		writeError(w, http.StatusBadRequest, "node_enrollment_invalid", "配对身份无效")
		return
	}
	if !a.validEnrollmentRequest(r, input) {
		writeError(w, http.StatusUnauthorized, "node_enrollment_token_invalid", "配对令牌无效")
		return
	}
	nodeFP := a.nodeCertificateFingerprint()
	if !nodeprotocol.SecureEqualHex(nodeFP, input.NodeCertificateFingerprint) {
		writeError(w, http.StatusForbidden, "node_enrollment_identity_changed", "节点证书与配对挑战不一致")
		return
	}
	if !a.config.AllowInsecureDevelopment && a.config.Transport != "http" {
		if r.TLS == nil || len(r.TLS.PeerCertificates) != 1 {
			writeError(w, http.StatusUnauthorized, "node_enrollment_server_certificate_required", "配对需要主服务器证书")
			return
		}
		sum := sha256.Sum256(r.TLS.PeerCertificates[0].Raw)
		if !nodeprotocol.SecureEqualHex(hex.EncodeToString(sum[:]), input.ServerCertificateFingerprint) {
			writeError(w, http.StatusForbidden, "node_enrollment_server_certificate_mismatch", "主服务器证书与配对证明不一致")
			return
		}
	}
	want := nodeprotocol.EnrollmentProof([]byte(a.config.EnrollmentToken), "server", input.NodeID, input.ServerID, input.Nonce, nodeFP, a.sealingPublicKey, input.ServerCertificateFingerprint)
	if !nodeprotocol.SecureEqualHex(want, input.ServerProof) {
		writeError(w, http.StatusForbidden, "node_enrollment_proof_invalid", "主服务器配对证明无效")
		return
	}
	identity, err := a.store.CompleteEnrollment(r.Context(), input.Nonce, input.ServerID, input.ServerCertificateFingerprint, nodeFP, a.now())
	if err != nil {
		writeError(w, http.StatusConflict, err.Error(), "节点配对失败")
		return
	}
	result := nodeprotocol.EnrollmentCompleteResponse{Paired: true, PairedAt: identity.PairedAt, NodeID: input.NodeID, ServerID: input.ServerID}
	result.NodeProof = nodeprotocol.EnrollmentProof([]byte(a.config.EnrollmentToken), "complete", input.NodeID, input.ServerID, input.Nonce, nodeFP, a.sealingPublicKey, input.ServerCertificateFingerprint)
	writeJSON(w, http.StatusOK, result)
}

func (a *Agent) validEnrollmentToken(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 512 {
		return false
	}
	want := sha256.Sum256([]byte(a.config.EnrollmentToken))
	got := sha256.Sum256([]byte(value))
	return subtle.ConstantTimeCompare(want[:], got[:]) == 1
}

func (a *Agent) validEnrollmentRequest(r *http.Request, input any) bool {
	if a.config.Transport == "http" && !a.config.AllowInsecureDevelopment {
		return nodeprotocol.SecureEqualHex(r.Header.Get("X-OhMyCine-Enrollment-Proof"), nodeprotocol.HTTPEnrollmentProof(a.config.EnrollmentToken, input))
	}
	return a.validEnrollmentToken(r.Header.Get("X-OhMyCine-Enrollment-Token"))
}

func validEnrollmentID(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' || r == ':') {
			return false
		}
	}
	return true
}

func (a *Agent) nodeCertificateFingerprint() string {
	if a.config.TLSCertificateFile == "" {
		return ""
	}
	pair, err := tls.LoadX509KeyPair(a.config.TLSCertificateFile, a.config.TLSPrivateKeyFile)
	if err != nil || len(pair.Certificate) == 0 {
		return ""
	}
	sum := sha256.Sum256(pair.Certificate[0])
	return hex.EncodeToString(sum[:])
}

func (a *Agent) requireServer(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.authorizeServer(w, r) {
			return
		}
		next(w, r)
	}
}

func (a *Agent) authorizeServer(w http.ResponseWriter, r *http.Request) bool {
	serverID := strings.TrimSpace(r.Header.Get("X-OhMyCine-Server-ID"))
	if serverID == "" || len(serverID) > 128 {
		writeError(w, http.StatusUnauthorized, "node_server_identity_required", "需要已配对的主服务器身份")
		return false
	}
	if !a.config.AllowInsecureDevelopment {
		if a.config.Transport == "http" {
			identity, err := a.store.Identity(r.Context())
			if err != nil || identity.ServerID != serverID || nodeprotocol.VerifyHTTPRequest(r, identity.ServerCertificateFingerprint, identity.NodeCertificateFingerprint, a.now(), a.httpStarted) != nil || !a.consumeHTTPNonce(r.Header.Get(nodeprotocol.HTTPNonceHeader)) {
				writeError(w, http.StatusUnauthorized, "node_request_signature_invalid", "HTTP 请求签名无效")
				return false
			}
		} else {
			if r.TLS == nil || len(r.TLS.PeerCertificates) != 1 {
				writeError(w, http.StatusUnauthorized, "node_mtls_required", "需要双向身份认证")
				return false
			}
			identity, err := a.store.Identity(r.Context())
			if err != nil {
				writeError(w, http.StatusUnauthorized, "node_server_not_paired", "节点尚未完成安全配对")
				return false
			}
			sum := sha256.Sum256(r.TLS.PeerCertificates[0].Raw)
			actual := hex.EncodeToString(sum[:])
			if serverID != identity.ServerID {
				writeError(w, http.StatusForbidden, "node_server_identity_mismatch", "主服务器身份不匹配")
				return false
			}
			if subtle.ConstantTimeCompare([]byte(actual), []byte(identity.ServerCertificateFingerprint)) != 1 {
				writeError(w, http.StatusForbidden, "node_server_identity_mismatch", "主服务器身份不匹配")
				return false
			}
		}
	}
	r.Header.Set("X-OhMyCine-Verified-Server-ID", serverID)
	return true
}

func (a *Agent) consumeHTTPNonce(nonce string) bool {
	now := a.now()
	// Persist replay receipts: a future-dated request within allowed clock skew
	// must not become valid again after a process restart.
	tx, err := a.store.db.BeginTx(context.Background(), nil)
	if err != nil {
		return false
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`DELETE FROM http_request_nonces WHERE expires_at <= ?`, now); err != nil {
		return false
	}
	if _, err = tx.Exec(`INSERT INTO http_request_nonces(nonce,expires_at) VALUES(?,?)`, nonce, now.Add(2*time.Minute)); err != nil {
		return false
	}
	return tx.Commit() == nil
}

func (a *Agent) putOperation(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("operation_key")
	var input nodeprotocol.PutOperationRequest
	if err := decodeJSON(w, r, &input); err != nil {
		return
	}
	if input.Plan.OperationKey != key || input.Plan.NodeID != a.config.NodeID {
		writeError(w, http.StatusBadRequest, "node_operation_identity_mismatch", "任务身份不匹配")
		return
	}
	now := a.now()
	if err := input.Plan.Validate(now); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "远端任务计划无效")
		return
	}
	digest, err := input.Plan.Digest()
	if err != nil || !nodeprotocol.ValidDigest(input.PlanDigest) || subtle.ConstantTimeCompare([]byte(digest), []byte(input.PlanDigest)) != 1 {
		writeError(w, http.StatusBadRequest, "node_plan_digest_invalid", "任务计划校验失败")
		return
	}
	result, created, err := a.store.Put(r.Context(), r.Header.Get("X-OhMyCine-Verified-Server-ID"), digest, input.Plan, now)
	if errors.Is(err, ErrPlanConflict) {
		writeError(w, http.StatusConflict, nodeprotocol.ErrorPlanConflict, "相同任务编号对应了不同计划")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "node_persistence_failed", "节点无法保存任务")
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, result)
}

func (a *Agent) getOperation(w http.ResponseWriter, r *http.Request) {
	result, err := a.store.Get(r.Context(), r.Header.Get("X-OhMyCine-Verified-Server-ID"), r.PathValue("operation_key"))
	if errors.Is(err, ErrOperationNotFound) {
		writeError(w, http.StatusNotFound, err.Error(), "远端任务不存在")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "node_persistence_failed", "节点无法读取任务")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *Agent) renewLease(w http.ResponseWriter, r *http.Request) {
	var input nodeprotocol.LeaseRequest
	if err := decodeJSON(w, r, &input); err != nil {
		return
	}
	result, err := a.store.RenewLease(r.Context(), r.Header.Get("X-OhMyCine-Verified-Server-ID"), r.PathValue("operation_key"), input.PlanDigest, input.LeaseEpoch, input.LeaseExpiresAt, a.now())
	if errors.Is(err, ErrLeaseConflict) {
		writeError(w, http.StatusConflict, "node_lease_conflict", "任务租约与节点状态不一致")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "node_persistence_failed", "节点无法续期任务")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *Agent) cancelOperation(w http.ResponseWriter, r *http.Request) {
	result, err := a.store.Cancel(r.Context(), r.Header.Get("X-OhMyCine-Verified-Server-ID"), r.PathValue("operation_key"), a.now())
	if errors.Is(err, ErrOperationNotFound) {
		writeError(w, http.StatusNotFound, err.Error(), "远端任务不存在")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "node_persistence_failed", "节点无法取消任务")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) error {
	r.Body = http.MaxBytesReader(w, r.Body, nodeprotocol.MaxWireBodyBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeError(w, http.StatusBadRequest, "node_request_invalid", "请求内容无效")
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "node_request_invalid", "请求只能包含一个对象")
		return errors.New("trailing JSON")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, nodeprotocol.ErrorResponse{Code: code, Message: message})
}
