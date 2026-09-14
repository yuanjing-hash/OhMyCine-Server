package nodeprotocol

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
)

const (
	ResourceKindDownloader = "downloader"

	DownloaderActionTest           = "test"
	DownloaderActionSubmit         = "submit"
	DownloaderActionGet            = "get"
	DownloaderActionPause          = "pause"
	DownloaderActionResume         = "resume"
	DownloaderActionCancel         = "cancel"
	DownloaderActionManifest       = "manifest"
	DownloaderActionDeleteTag      = "delete_managed_tag"
	DownloaderActionCategories     = "categories"
	DownloaderActionEnsureCategory = "ensure_category"
	DownloaderActionUpdateCategory = "update_category"
	DownloaderActionSetCategory    = "set_category"
)

// DownloaderCredential is sealed inside CredentialGrant. It is never part of
// an operation plan, receipt, log or browser response.
type DownloaderCredential struct {
	ProviderType       string `json:"provider_type"`
	BaseURL            string `json:"base_url"`
	Username           string `json:"username,omitempty"`
	Password           string `json:"password,omitempty"`
	DownloaderSaveRoot string `json:"downloader_save_root"`
	NodeMountRoot      string `json:"node_mount_root"`
}

type DownloaderSource struct {
	Kind     string `json:"kind"`
	URL      string `json:"url,omitempty"`
	Torrent  []byte `json:"torrent,omitempty"`
	Filename string `json:"filename,omitempty"`
}

type DownloaderSubmit struct {
	Source       DownloaderSource `json:"source"`
	SavePath     string           `json:"save_path"`
	Tag          string           `json:"tag"`
	MetadataOnly bool             `json:"metadata_only"`
}

// DownloaderActionRequest is an authenticated, bounded Node RPC. RequestID is
// stable for mutating retries; the Node persists only a digest and the safe
// response, never the source URL/torrent or decrypted credential.
type DownloaderActionRequest struct {
	RequestID      string            `json:"request_id"`
	TaskID         string            `json:"task_id"`
	OperationKey   string            `json:"operation_key"`
	DownloaderID   string            `json:"downloader_id"`
	Action         string            `json:"action"`
	GrantID        string            `json:"grant_id"`
	ProviderTaskID string            `json:"provider_task_id,omitempty"`
	DeleteData     bool              `json:"delete_data,omitempty"`
	ManagedTag     string            `json:"managed_tag,omitempty"`
	CategoryName   string            `json:"category_name,omitempty"`
	SavePath       string            `json:"save_path,omitempty"`
	Submit         *DownloaderSubmit `json:"submit,omitempty"`
}

type DownloaderHealth struct {
	Version string `json:"version"`
}

type DownloaderTask struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Status         string   `json:"status"`
	Progress       *float64 `json:"progress,omitempty"`
	BytesCompleted *int64   `json:"bytes_completed,omitempty"`
	BytesTotal     *int64   `json:"bytes_total,omitempty"`
	DownloadSpeed  *int64   `json:"download_speed,omitempty"`
	UploadSpeed    *int64   `json:"upload_speed,omitempty"`
	ETASeconds     *int64   `json:"eta_seconds,omitempty"`
	Ratio          *float64 `json:"ratio,omitempty"`
	SeededSeconds  *int64   `json:"seeded_seconds,omitempty"`
	UploadedBytes  *int64   `json:"uploaded_bytes,omitempty"`
	Seeding        bool     `json:"seeding"`
	Completed      bool     `json:"completed"`
	Failed         bool     `json:"failed"`
	ErrorCode      string   `json:"error_code,omitempty"`
}

type DownloaderFile struct {
	RelativePath string `json:"relative_path"`
	Size         int64  `json:"size"`
}

type DownloaderManifest struct {
	Name     string           `json:"name"`
	Files    []DownloaderFile `json:"files"`
	Complete bool             `json:"complete"`
}

type DownloaderCategory struct {
	Name     string `json:"name"`
	SavePath string `json:"save_path"`
}

type DownloaderActionResponse struct {
	RequestID  string               `json:"request_id"`
	Health     *DownloaderHealth    `json:"health,omitempty"`
	Task       *DownloaderTask      `json:"task,omitempty"`
	Manifest   *DownloaderManifest  `json:"manifest,omitempty"`
	FileExport *FileExportSummary   `json:"file_export,omitempty"`
	Categories []DownloaderCategory `json:"categories,omitempty"`
}

func (r DownloaderActionRequest) Validate() error {
	if !validID(r.RequestID) || !validID(r.TaskID) || !validID(r.OperationKey) || !validID(r.DownloaderID) || !validID(r.Action) || !validID(r.GrantID) {
		return errors.New("node_downloader_request_invalid")
	}
	switch r.Action {
	case DownloaderActionTest:
		if r.Submit != nil || r.ProviderTaskID != "" || r.ManagedTag != "" {
			return errors.New("node_downloader_request_invalid")
		}
	case DownloaderActionSubmit:
		if r.Submit == nil || strings.TrimSpace(r.Submit.Tag) == "" || strings.TrimSpace(r.Submit.SavePath) == "" {
			return errors.New("node_downloader_request_invalid")
		}
	case DownloaderActionGet, DownloaderActionPause, DownloaderActionResume, DownloaderActionCancel, DownloaderActionManifest:
		if strings.TrimSpace(r.ProviderTaskID) == "" || len(r.ProviderTaskID) > 256 || r.Submit != nil {
			return errors.New("node_downloader_request_invalid")
		}
	case DownloaderActionDeleteTag:
		if strings.TrimSpace(r.ManagedTag) == "" || len(r.ManagedTag) > 96 || r.Submit != nil {
			return errors.New("node_downloader_request_invalid")
		}
	case DownloaderActionCategories:
		if r.Submit != nil || r.ProviderTaskID != "" || r.CategoryName != "" || r.SavePath != "" {
			return errors.New("node_downloader_request_invalid")
		}
	case DownloaderActionEnsureCategory, DownloaderActionUpdateCategory:
		if strings.TrimSpace(r.CategoryName) == "" || strings.TrimSpace(r.SavePath) == "" || r.ProviderTaskID != "" || r.Submit != nil {
			return errors.New("node_downloader_request_invalid")
		}
	case DownloaderActionSetCategory:
		if strings.TrimSpace(r.ProviderTaskID) == "" || strings.TrimSpace(r.CategoryName) == "" || strings.TrimSpace(r.SavePath) == "" || r.Submit != nil {
			return errors.New("node_downloader_request_invalid")
		}
	default:
		return errors.New("node_downloader_action_unsupported")
	}
	return nil
}

func (r DownloaderActionRequest) Digest() (string, error) {
	copy := r
	copy.GrantID = ""
	raw, err := json.Marshal(copy)
	if err != nil {
		return "", err
	}
	canonical, err := canonicalJSON(raw)
	if err != nil {
		return "", err
	}
	return digestBytes(canonical), nil
}

func digestBytes(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
