// Package nodeprotocol defines the stable, provider-neutral wire contract
// shared by OhMyCine Server and ohmycine-node.
package nodeprotocol

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	VersionV1 = 1

	CapabilityQBittorrentControl = "qbittorrent_control"
	CapabilityPan115Offline      = "pan115_offline"
	CapabilityPan115ShareReceive = "pan115_share_receive"
	CapabilityPan115Read         = "pan115_read"
	CapabilityPan115Upload       = "pan115_upload"
	CapabilityRangeExport        = "range_export"

	OperationPending   = "pending"
	OperationRunning   = "running"
	OperationPaused    = "paused"
	OperationCompleted = "completed"
	OperationFailed    = "failed"
	OperationCancelled = "cancelled"

	PhaseAccepted             = "accepted"
	PhaseSubmittingDownload   = "submitting_download"
	PhaseWaitingDownload      = "waiting_download"
	PhaseVerifyingRemoteFiles = "verifying_remote_files"
	PhaseWaitingServerPlan    = "waiting_server_plan"
	PhasePullingSource        = "pulling_source"
	PhaseUploadingTarget      = "uploading_target"
	PhaseVerifyingTarget      = "verifying_target"
	PhaseWaitingNode          = "waiting_node"
	PhaseWaitingCredentials   = "waiting_credentials"
	PhaseCompleted            = "completed"

	ErrorNodeOffline           = "node_offline"
	ErrorProtocolIncompatible  = "node_protocol_incompatible"
	ErrorCapabilityMissing     = "node_capability_missing"
	ErrorSpaceInsufficient     = "node_space_insufficient"
	ErrorDownloaderUnreachable = "node_downloader_unreachable"
	ErrorPathMappingInvalid    = "node_path_mapping_invalid"
	ErrorCredentialExpired     = "node_credential_expired"
	ErrorSourceShareExpired    = "node_source_share_expired"
	ErrorSourceSharePassword   = "node_source_share_password_invalid"
	ErrorSourceRateLimited     = "node_source_rate_limited"
	ErrorTargetRateLimited     = "node_target_rate_limited"
	ErrorChecksumMismatch      = "node_checksum_mismatch"
	ErrorReconciliationNeeded  = "node_reconciliation_required"
	ErrorPlanConflict          = "node_operation_plan_conflict"
	ErrorLeaseExpired          = "node_lease_expired"
	ErrorExportPreparing       = "node_file_export_preparing"
)

const MaxWireBodyBytes = 1 << 20

var identifierPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

type Capabilities struct {
	Codes                 []string `json:"codes"`
	MaxConcurrent         int      `json:"max_concurrent"`
	ManagedFreeBytes      *int64   `json:"managed_free_bytes,omitempty"`
	ManagedFreeBytesKnown bool     `json:"managed_free_bytes_known"`
}

func (c Capabilities) Has(code string) bool {
	for _, candidate := range c.Codes {
		if candidate == code {
			return true
		}
	}
	return false
}

func (c Capabilities) Canonical() Capabilities {
	result := c
	result.Codes = append([]string(nil), c.Codes...)
	sort.Strings(result.Codes)
	result.Codes = compactUnique(result.Codes)
	return result
}

type HealthResponse struct {
	Status       string       `json:"status"`
	AgentVersion string       `json:"agent_version"`
	ProtocolMin  int          `json:"protocol_min"`
	ProtocolMax  int          `json:"protocol_max"`
	OS           string       `json:"os"`
	Arch         string       `json:"arch"`
	NodeID       string       `json:"node_id"`
	Capabilities Capabilities `json:"capabilities"`
	Revision     uint64       `json:"revision"`
}

// OperationPlan is immutable after first acceptance. Payload is deliberately
// provider-neutral at this layer; concrete executors validate their typed body.
type OperationPlan struct {
	ProtocolVersion int             `json:"protocol_version"`
	OperationKey    string          `json:"operation_key"`
	TaskID          string          `json:"task_id"`
	NodeID          string          `json:"node_id"`
	Kind            string          `json:"kind"`
	PlanRevision    uint64          `json:"plan_revision"`
	LeaseEpoch      uint64          `json:"lease_epoch"`
	LeaseExpiresAt  time.Time       `json:"lease_expires_at"`
	Payload         json.RawMessage `json:"payload"`
}

type PutOperationRequest struct {
	Plan       OperationPlan `json:"plan"`
	PlanDigest string        `json:"plan_digest"`
}

type LeaseRequest struct {
	PlanDigest     string    `json:"plan_digest"`
	LeaseEpoch     uint64    `json:"lease_epoch"`
	LeaseExpiresAt time.Time `json:"lease_expires_at"`
}

type OperationResponse struct {
	OperationKey   string    `json:"operation_key"`
	PlanDigest     string    `json:"plan_digest"`
	Status         string    `json:"status"`
	Phase          string    `json:"phase"`
	LeaseEpoch     uint64    `json:"lease_epoch"`
	LeaseExpiresAt time.Time `json:"lease_expires_at"`
	Progress       *float64  `json:"progress,omitempty"`
	ErrorCode      string    `json:"error_code,omitempty"`
	Revision       uint64    `json:"revision"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type ErrorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type EnrollmentChallengeRequest struct {
	NodeID   string `json:"node_id"`
	ServerID string `json:"server_id"`
	Nonce    string `json:"nonce"`
}

type EnrollmentChallengeResponse struct {
	NodeID                     string    `json:"node_id"`
	ServerID                   string    `json:"server_id"`
	Nonce                      string    `json:"nonce"`
	NodeCertificateFingerprint string    `json:"node_certificate_fingerprint"`
	NodeEncryptionPublicKey    string    `json:"node_encryption_public_key"`
	NodeProof                  string    `json:"node_proof"`
	ExpiresAt                  time.Time `json:"expires_at"`
}

type EnrollmentCompleteRequest struct {
	NodeID                       string `json:"node_id"`
	ServerID                     string `json:"server_id"`
	Nonce                        string `json:"nonce"`
	NodeCertificateFingerprint   string `json:"node_certificate_fingerprint"`
	NodeEncryptionPublicKey      string `json:"node_encryption_public_key"`
	ServerCertificateFingerprint string `json:"server_certificate_fingerprint"`
	ServerProof                  string `json:"server_proof"`
}

type EnrollmentCompleteResponse struct {
	NodeProof string    `json:"node_proof,omitempty"`
	Paired    bool      `json:"paired"`
	PairedAt  time.Time `json:"paired_at"`
	NodeID    string    `json:"node_id"`
	ServerID  string    `json:"server_id"`
}

func EnrollmentProof(token []byte, role, nodeID, serverID, nonce, nodeFingerprint, nodeEncryptionPublicKey, serverFingerprint string) string {
	mac := hmac.New(sha256.New, token)
	_, _ = mac.Write([]byte(strings.Join([]string{"ohmycine-node-enrollment-v1", role, nodeID, serverID, nonce, nodeFingerprint, nodeEncryptionPublicKey, serverFingerprint}, "\n")))
	return hex.EncodeToString(mac.Sum(nil))
}

func SecureEqualHex(left, right string) bool {
	leftBytes, leftErr := hex.DecodeString(left)
	rightBytes, rightErr := hex.DecodeString(right)
	return leftErr == nil && rightErr == nil && hmac.Equal(leftBytes, rightBytes)
}

func (p OperationPlan) Validate(now time.Time) error {
	if p.ProtocolVersion != VersionV1 {
		return errors.New(ErrorProtocolIncompatible)
	}
	if !validID(p.OperationKey) || !validID(p.TaskID) || !validID(p.NodeID) || !validID(p.Kind) {
		return errors.New("node_operation_invalid")
	}
	if p.PlanRevision == 0 || p.LeaseEpoch == 0 || p.LeaseExpiresAt.IsZero() {
		return errors.New("node_operation_invalid")
	}
	if p.LeaseExpiresAt.After(now.Add(15 * time.Minute)) {
		return errors.New("node_lease_invalid")
	}
	if len(p.Payload) > MaxWireBodyBytes || !json.Valid(p.Payload) {
		return errors.New("node_operation_payload_invalid")
	}
	if p.Kind == OperationKindStorageUpload {
		var plan StorageUploadPlan
		decoder := json.NewDecoder(bytes.NewReader(p.Payload))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&plan); err != nil || plan.Validate() != nil {
			return errors.New("node_storage_plan_invalid")
		}
	}
	if p.Kind == OperationKindStorageSourceMaterialize {
		var plan StorageSourcePlan
		decoder := json.NewDecoder(bytes.NewReader(p.Payload))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&plan); err != nil || plan.Validate() != nil {
			return errors.New("node_storage_source_plan_invalid")
		}
	}
	return nil
}

func (p OperationPlan) Digest() (string, error) {
	canonicalPayload, err := canonicalJSON(p.Payload)
	if err != nil {
		return "", fmt.Errorf("canonicalize operation payload: %w", err)
	}
	p.Payload = canonicalPayload
	// A lease is mutable liveness authority, not part of the immutable business
	// plan. Renewals and Server restarts must retain the same plan digest while
	// the persisted lease epoch/expiry advance independently.
	p.LeaseEpoch = 0
	p.LeaseExpiresAt = time.Time{}
	encoded, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func ValidDigest(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil && strings.ToLower(value) == value
}

func validID(value string) bool { return identifierPattern.MatchString(strings.TrimSpace(value)) }

func canonicalJSON(raw json.RawMessage) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errors.New("multiple JSON values")
	}
	return json.Marshal(value)
}

func compactUnique(values []string) []string {
	result := values[:0]
	for _, value := range values {
		if value == "" || len(result) > 0 && result[len(result)-1] == value {
			continue
		}
		result = append(result, value)
	}
	return result
}
