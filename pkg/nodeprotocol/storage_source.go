package nodeprotocol

import (
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"net/url"
	"regexp"
	"strings"
)

const (
	OperationKindStorageSourceMaterialize = "storage_source_materialize"
	StorageSourceKindPan115OfflineMagnet  = "pan115_offline_magnet"
	StorageSourceKindPan115Share          = "pan115_share"
	StorageSourceKindPan115ShareSelected  = "pan115_share_selected_v1"
	// StorageSourceKindPan115OfflineURL is a route-preview value only in v1.
	// A plain URL has no stable provider task identity, so Node plans reject it
	// instead of discovering the idempotency gap after an external mutation.
	StorageSourceKindPan115OfflineURL = "pan115_offline_url"
	StorageSourceTargetLocal          = "local"
	StorageSourceTargetPan115         = "pan115"

	StorageSourceActionMaterialize  = "materialize"
	StorageSourceGrantOfflineSubmit = "offline_submit"
	StorageSourceGrantOfflineStatus = "offline_status"
	StorageSourceGrantShareInspect  = "share_inspect"
	StorageSourceGrantShareReceive  = "share_receive"
	StorageSourceGrantRead          = "read"
	StorageSourceActionCleanup      = "cleanup_managed_source"

	StorageSourceAccepted           = "accepted"
	StorageSourceSubmitting         = "submitting_offline"
	StorageSourceWaiting            = "waiting_offline"
	StorageSourceEnumerating        = "enumerating_source"
	StorageSourceDownloading        = "downloading_source"
	StorageSourceWaitingCredentials = "waiting_credentials"
	StorageSourceCompleted          = "completed"
	StorageSourceFailed             = "failed"
)

// StorageSourcePlan contains only immutable routing facts. The source URI and
// Cookie are deliberately absent and travel only in a sealed credential grant.
type StorageSourcePlan struct {
	StorageID            string `json:"storage_id"`
	ProviderType         string `json:"provider_type"`
	SourceKind           string `json:"source_kind"`
	TargetKind           string `json:"target_kind"`
	SourceIdentityDigest string `json:"source_identity_digest"`
	TargetIdentityDigest string `json:"target_identity_digest"`
	SourceContentDigest  string `json:"source_content_digest"`
	OfflineDestinationID string `json:"offline_destination_id"`
}

type StorageSourceCredential struct {
	ProviderType string `json:"provider_type"`
	Cookie       string `json:"cookie"`
	SourceURI    string `json:"source_uri,omitempty"`
}

type StorageSourceActionRequest struct {
	RequestID    string `json:"request_id"`
	TaskID       string `json:"task_id"`
	OperationKey string `json:"operation_key"`
	PlanDigest   string `json:"plan_digest"`
	StorageID    string `json:"storage_id"`
	Action       string `json:"action"`
	GrantID      string `json:"grant_id"`
}

// StorageSourceActionResponse is summary-only. Provider files are exposed
// only through the existing paginated FileExport protocol after verification.
type StorageSourceActionResponse struct {
	RequestID       string             `json:"request_id"`
	OperationKey    string             `json:"operation_key"`
	PlanDigest      string             `json:"plan_digest"`
	Status          string             `json:"status"`
	ProviderStatus  string             `json:"provider_status,omitempty"`
	Progress        *float64           `json:"progress,omitempty"`
	TotalFiles      int64              `json:"total_files"`
	DownloadedFiles int64              `json:"downloaded_files"`
	TotalBytes      int64              `json:"total_bytes"`
	DownloadedBytes int64              `json:"downloaded_bytes"`
	FileExport      *FileExportSummary `json:"file_export,omitempty"`
	ErrorCode       string             `json:"error_code,omitempty"`
}

type StorageSourceCleanupRequest struct {
	RequestID    string `json:"request_id"`
	OperationKey string `json:"operation_key"`
	TaskID       string `json:"task_id"`
	PlanDigest   string `json:"plan_digest"`
}

type StorageSourceCleanupResponse struct {
	RequestID    string `json:"request_id"`
	OperationKey string `json:"operation_key"`
	TaskID       string `json:"task_id"`
	Status       string `json:"status"`
	Cleaned      bool   `json:"cleaned"`
}

func (plan StorageSourcePlan) Validate() error {
	if !validID(plan.StorageID) || plan.ProviderType != StorageProviderPan115 || !StorageSourceKindSupported(plan.SourceKind) || !ValidDigest(plan.SourceIdentityDigest) || !ValidDigest(plan.SourceContentDigest) || !validProviderItemID(plan.OfflineDestinationID) {
		return errors.New("node_storage_source_plan_invalid")
	}
	switch plan.TargetKind {
	case StorageSourceTargetLocal:
		if plan.TargetIdentityDigest != "" {
			return errors.New("node_storage_source_plan_invalid")
		}
	case StorageSourceTargetPan115:
		if !ValidDigest(plan.TargetIdentityDigest) || plan.SourceIdentityDigest == plan.TargetIdentityDigest {
			return errors.New("node_storage_source_plan_invalid")
		}
	default:
		return errors.New("node_storage_source_plan_invalid")
	}
	return nil
}

// StorageSourceKindSupported is shared with Server route preview. Values may
// exist in the protocol before they are safe to execute, but only true values
// may be frozen into a Node operation plan.
func StorageSourceKindSupported(kind string) bool {
	return kind == StorageSourceKindPan115OfflineMagnet || StorageSourceIsShare(kind)
}

func StorageSourceIsShare(kind string) bool {
	return kind == StorageSourceKindPan115Share || kind == StorageSourceKindPan115ShareSelected
}

var storageShareCodePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{4,128}$`)

// CanonicalStorageSourceIdentity returns the stable, credential-free identity
// used to make a provider submission recoverable. Display names, trackers and
// a rotated 115 receive code do not change the underlying source identity.
func CanonicalStorageSourceIdentity(kind, rawURI string) (string, error) {
	if kind == StorageSourceKindPan115ShareSelected {
		source, err := cloud.DecodeSelectedShareSource(rawURI)
		if err != nil {
			return "", err
		}
		identity, err := CanonicalStorageSourceIdentity(StorageSourceKindPan115Share, source.URL)
		if err != nil {
			return "", err
		}
		digest, err := source.Selection.Digest()
		return identity + ":" + digest, err
	}
	rawURI = strings.TrimSpace(rawURI)
	if rawURI == "" || len(rawURI) > 8192 || strings.ContainsAny(rawURI, "\x00\r\n") {
		return "", errors.New("node_storage_source_uri_not_idempotent")
	}
	parsed, err := url.Parse(rawURI)
	if err != nil || parsed.User != nil || parsed.Fragment != "" {
		return "", errors.New("node_storage_source_uri_not_idempotent")
	}
	switch kind {
	case StorageSourceKindPan115OfflineMagnet:
		if !strings.EqualFold(parsed.Scheme, "magnet") || parsed.Host != "" || parsed.Path != "" || parsed.Opaque != "" {
			return "", errors.New("node_storage_source_uri_not_idempotent")
		}
		var topics []string
		for key, values := range parsed.Query() {
			if strings.EqualFold(key, "xt") {
				topics = append(topics, values...)
			}
		}
		if len(topics) != 1 {
			return "", errors.New("node_storage_source_uri_not_idempotent")
		}
		const prefix = "urn:btih:"
		topic := strings.TrimSpace(topics[0])
		if len(topic) <= len(prefix) || !strings.EqualFold(topic[:len(prefix)], prefix) {
			return "", errors.New("node_storage_source_uri_not_idempotent")
		}
		value := strings.TrimSpace(topic[len(prefix):])
		switch len(value) {
		case 40:
			decoded, decodeErr := hex.DecodeString(value)
			if decodeErr == nil && len(decoded) == 20 {
				return hex.EncodeToString(decoded), nil
			}
		case 32:
			decoded, decodeErr := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(value))
			if decodeErr == nil && len(decoded) == 20 {
				return hex.EncodeToString(decoded), nil
			}
		}
	case StorageSourceKindPan115Share:
		if parsed.Scheme != "https" || parsed.Opaque != "" {
			break
		}
		host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
		if host != "115.com" && !strings.HasSuffix(host, ".115.com") && host != "115cdn.com" && !strings.HasSuffix(host, ".115cdn.com") {
			break
		}
		segments := strings.Split(strings.Trim(parsed.EscapedPath(), "/"), "/")
		if len(segments) != 2 {
			break
		}
		shareCode, unescapeErr := url.PathUnescape(segments[1])
		if segments[0] == "s" && unescapeErr == nil && storageShareCodePattern.MatchString(shareCode) {
			return shareCode, nil
		}
	}
	return "", errors.New("node_storage_source_uri_not_idempotent")
}

func StorageSourceContentDigest(kind, rawURI string) (string, error) {
	identity, err := CanonicalStorageSourceIdentity(kind, rawURI)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte("ohmycine-storage-source-v1\n" + kind + "\n" + identity))
	return hex.EncodeToString(sum[:]), nil
}

// StorageIdentityDigest gives Server one canonical way to freeze source and
// target account identities. Node cannot derive a connection identity from a
// scoped Cookie, so it enforces the signed digests are valid and different.
func StorageIdentityDigest(providerType, stableIdentity string) (string, error) {
	if providerType != StorageProviderPan115 || !validID(stableIdentity) {
		return "", errors.New("node_storage_identity_invalid")
	}
	sum := sha256.Sum256([]byte("ohmycine-storage-identity-v1\n" + providerType + "\n" + stableIdentity))
	return hex.EncodeToString(sum[:]), nil
}

func (request StorageSourceActionRequest) Validate() error {
	if !validID(request.RequestID) || !validID(request.TaskID) || !validID(request.OperationKey) || !ValidDigest(request.PlanDigest) || !validID(request.StorageID) || request.Action != StorageSourceActionMaterialize || !validID(request.GrantID) {
		return errors.New("node_storage_source_request_invalid")
	}
	return nil
}

func (request StorageSourceActionRequest) Digest() (string, error) {
	copy := request
	copy.GrantID = ""
	raw, err := json.Marshal(copy)
	if err != nil {
		return "", err
	}
	canonical, err := canonicalJSON(raw)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

func (request StorageSourceCleanupRequest) Validate() error {
	if !validID(request.RequestID) || !validID(request.OperationKey) || !validID(request.TaskID) || !ValidDigest(request.PlanDigest) {
		return errors.New("node_storage_source_cleanup_invalid")
	}
	return nil
}

func (request StorageSourceCleanupRequest) Digest() (string, error) {
	if err := request.Validate(); err != nil {
		return "", err
	}
	raw, err := json.Marshal(request)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func ValidateStorageSourceCredential(credential StorageSourceCredential, requireURI bool) error {
	if credential.ProviderType != StorageProviderPan115 || strings.TrimSpace(credential.Cookie) == "" || strings.ContainsAny(credential.Cookie, "\r\n") || len(credential.Cookie) > MaxCredentialGrantBytes {
		return errors.New("node_storage_source_credential_invalid")
	}
	uri := strings.TrimSpace(credential.SourceURI)
	if requireURI && (uri == "" || len(uri) > cloud.MaxSelectedShareSourceBytes || strings.ContainsAny(uri, "\x00\r\n")) {
		return errors.New("node_storage_source_credential_invalid")
	}
	if !requireURI && uri != "" && (len(uri) > cloud.MaxSelectedShareSourceBytes || strings.ContainsAny(uri, "\x00\r\n")) {
		return errors.New("node_storage_source_credential_invalid")
	}
	return nil
}
