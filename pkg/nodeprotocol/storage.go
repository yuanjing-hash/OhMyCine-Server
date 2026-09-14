package nodeprotocol

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path"
	"strings"
)

const (
	OperationKindStorageUpload = "storage_upload"
	ResourceKindStorage        = "storage"

	StorageProviderPan115 = "pan115"
	StorageActionUpload   = "upload"
	StorageActionReplace  = "replace"

	StorageConflictFailIfExists     = "fail_if_exists"
	StorageConflictReuseIfIdentical = "reuse_if_identical"
	StorageConflictReplace          = "replace"
	StorageConflictSkipIfExists     = "skip_if_exists"

	StorageFilePending     = "pending"
	StorageFileUploading   = "uploading"
	StorageFileReconciling = "reconciling"
	StorageFileCompleted   = "completed"
	StorageFileReused      = "reused"
	StorageFileSkipped     = "skipped"

	StorageActionAccepted           = "accepted"
	StorageActionRunning            = "running"
	StorageActionWaitingCredentials = "waiting_credentials"
	StorageActionReconciliation     = "reconciliation_required"
	StorageActionCompleted          = "completed"

	MaxStorageUploadFiles = 512
)

// StorageCredential is sealed inside a task-bound CredentialGrant. It must
// never be persisted after decryption or copied into an operation plan.
type StorageCredential struct {
	ProviderType string `json:"provider_type"`
	Cookie       string `json:"cookie,omitempty"`
}

// StorageUploadPlan is immutable operation payload. All names and conflict
// decisions are made by the Server; the Node only executes these exact paths.
type StorageUploadPlan struct {
	StorageID                string              `json:"storage_id"`
	ProviderType             string              `json:"provider_type"`
	SourceExportOperationKey string              `json:"source_export_operation_key"`
	SourceManifestDigest     string              `json:"source_manifest_digest"`
	TargetRootID             string              `json:"target_root_id"`
	Files                    []StorageUploadFile `json:"files"`
}

type StorageUploadFile struct {
	SourceFileToken    string `json:"source_file_token"`
	TargetRelativePath string `json:"target_relative_path"`
	Size               int64  `json:"size"`
	SHA256             string `json:"sha256"`
	ConflictAction     string `json:"conflict_action"`
}

type StorageActionRequest struct {
	RequestID    string `json:"request_id"`
	TaskID       string `json:"task_id"`
	OperationKey string `json:"operation_key"`
	PlanDigest   string `json:"plan_digest"`
	StorageID    string `json:"storage_id"`
	Action       string `json:"action"`
	GrantID      string `json:"grant_id"`
}

type StorageUploadFileResult struct {
	SourceFileToken string `json:"source_file_token"`
	TargetParentID  string `json:"target_parent_id,omitempty"`
	TargetName      string `json:"target_name,omitempty"`
	TargetItemID    string `json:"target_item_id,omitempty"`
	Status          string `json:"status"`
	Size            int64  `json:"size"`
	SHA1            string `json:"sha1,omitempty"`
}

type StorageActionResponse struct {
	RequestID      string                    `json:"request_id"`
	OperationKey   string                    `json:"operation_key"`
	PlanDigest     string                    `json:"plan_digest"`
	Status         string                    `json:"status"`
	TotalFiles     int                       `json:"total_files"`
	CompletedFiles int                       `json:"completed_files"`
	Files          []StorageUploadFileResult `json:"files,omitempty"`
	ErrorCode      string                    `json:"error_code,omitempty"`
}

func (plan StorageUploadPlan) Validate() error {
	if !validID(plan.StorageID) || !validID(plan.ProviderType) || !validID(plan.SourceExportOperationKey) || !ValidDigest(plan.SourceManifestDigest) || !validProviderItemID(plan.TargetRootID) || len(plan.Files) == 0 || len(plan.Files) > MaxStorageUploadFiles {
		return errors.New("node_storage_plan_invalid")
	}
	seenTokens := make(map[string]struct{}, len(plan.Files))
	seenTargets := make(map[string]struct{}, len(plan.Files))
	for _, file := range plan.Files {
		if ValidateFileToken(file.SourceFileToken) != nil || file.Size <= 0 || !ValidDigest(file.SHA256) || normalizeStorageRelativePath(file.TargetRelativePath) != file.TargetRelativePath {
			return errors.New("node_storage_plan_invalid")
		}
		switch file.ConflictAction {
		case StorageConflictFailIfExists, StorageConflictReuseIfIdentical, StorageConflictReplace, StorageConflictSkipIfExists:
		default:
			return errors.New("node_storage_plan_invalid")
		}
		if _, exists := seenTokens[file.SourceFileToken]; exists {
			return errors.New("node_storage_plan_invalid")
		}
		seenTokens[file.SourceFileToken] = struct{}{}
		targetKey := strings.ToLower(file.TargetRelativePath)
		if _, exists := seenTargets[targetKey]; exists {
			return errors.New("node_storage_plan_invalid")
		}
		seenTargets[targetKey] = struct{}{}
	}
	return nil
}

func (request StorageActionRequest) Validate() error {
	if !validID(request.RequestID) || !validID(request.TaskID) || !validID(request.OperationKey) || !ValidDigest(request.PlanDigest) || !validID(request.StorageID) || !validID(request.Action) || !validID(request.GrantID) || request.Action != StorageActionUpload {
		return errors.New("node_storage_request_invalid")
	}
	return nil
}

func (request StorageActionRequest) Digest() (string, error) {
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

func normalizeStorageRelativePath(value string) string {
	if value == "" || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\\\x00\r\n") || path.IsAbs(value) {
		return ""
	}
	clean := path.Clean(value)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || clean != value || len(clean) > 2048 {
		return ""
	}
	for _, segment := range strings.Split(clean, "/") {
		if segment == "" || segment == "." || segment == ".." || len(segment) > 255 {
			return ""
		}
	}
	return clean
}

func validProviderItemID(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && len(value) <= 256 && !strings.ContainsAny(value, "\\/\x00\r\n")
}
