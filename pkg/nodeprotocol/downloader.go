package nodeprotocol

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/resourcecontent"
	"strings"
)

const (
	ResourceKindDownloader = "downloader"

	DownloaderActionTest                  = "test"
	DownloaderActionSubmit                = "submit"
	DownloaderActionSelectionGroup        = "selection_group"
	DownloaderActionSelectionGroupCleanup = "selection_group_cleanup"
	DownloaderActionGet                   = "get"
	DownloaderActionPause                 = "pause"
	DownloaderActionResume                = "resume"
	DownloaderActionCancel                = "cancel"
	DownloaderActionManifest              = "manifest"
	DownloaderActionDeleteTag             = "delete_managed_tag"
	DownloaderActionCategories            = "categories"
	DownloaderActionEnsureCategory        = "ensure_category"
	DownloaderActionUpdateCategory        = "update_category"
	DownloaderActionSetCategory           = "set_category"
)

// DownloaderCredential is sealed inside CredentialGrant. It is never part of
// an operation plan, receipt, log or browser response.
type DownloaderCredential struct {
	SourceKey          string `json:"source_key,omitempty"`
	SourceScope        string `json:"source_scope,omitempty"`
	ProviderType       string `json:"provider_type"`
	BaseURL            string `json:"base_url"`
	Username           string `json:"username,omitempty"`
	Password           string `json:"password,omitempty"`
	DownloaderSaveRoot string `json:"downloader_save_root"`
	NodeMountRoot      string `json:"node_mount_root"`
}

type DownloaderSource struct {
	ContentSelection *resourcecontent.Selection `json:"content_selection,omitempty"`
	Kind             string                     `json:"kind"`
	URL              string                     `json:"url,omitempty"`
	Torrent          []byte                     `json:"torrent,omitempty"`
	Filename         string                     `json:"filename,omitempty"`
}

type DownloaderSubmit struct {
	SourceEnvelope *DownloaderSourceEnvelope `json:"source_envelope,omitempty"`
	Source         DownloaderSource          `json:"source"`
	SavePath       string                    `json:"save_path"`
	Tag            string                    `json:"tag"`
	MetadataOnly   bool                      `json:"metadata_only"`
}

// DownloaderActionRequest is an authenticated, bounded Node RPC. RequestID is
// stable for mutating retries; the Node persists only a digest and the safe
// response, never the source URL/torrent or decrypted credential.
type DownloaderActionRequest struct {
	Cleanup        *downloadpkg.GroupCleanup   `json:"cleanup,omitempty"`
	Group          *downloadpkg.SelectionGroup `json:"group,omitempty"`
	Selection      *DownloaderSelection        `json:"selection,omitempty"`
	RequestID      string                      `json:"request_id"`
	TaskID         string                      `json:"task_id"`
	OperationKey   string                      `json:"operation_key"`
	DownloaderID   string                      `json:"downloader_id"`
	Action         string                      `json:"action"`
	GrantID        string                      `json:"grant_id"`
	ProviderTaskID string                      `json:"provider_task_id,omitempty"`
	DeleteData     bool                        `json:"delete_data,omitempty"`
	ManagedTag     string                      `json:"managed_tag,omitempty"`
	CategoryName   string                      `json:"category_name,omitempty"`
	SavePath       string                      `json:"save_path,omitempty"`
	Submit         *DownloaderSubmit           `json:"submit,omitempty"`
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
	NativeIndex  *int   `json:"native_index,omitempty"`
	RelativePath string `json:"relative_path"`
	Size         int64  `json:"size"`
}

type DownloaderSelection struct {
	Group     *downloadpkg.SelectionGroup `json:"group,omitempty"`
	Tag       string                      `json:"tag"`
	Selection resourcecontent.Selection   `json:"selection"`
	RootName  string                      `json:"root_name"`
	MultiFile bool                        `json:"multi_file"`
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
	if r.Group != nil {
		if (r.Action != DownloaderActionSelectionGroup && r.Action != DownloaderActionSelectionGroupCleanup) || r.Submit == nil || r.Submit.SourceEnvelope == nil || r.Group.Tag != "omc-group-"+safeGroupPrefix(r.Group.ID) || r.Submit.SavePath != r.Group.Workspace || r.Submit.Tag != "omc-"+r.TaskID || r.Group.Validate(r.Group.Union) != nil {
			return errors.New("node_selection_group_invalid")
		}
	}
	if r.Cleanup != nil && (r.Action != DownloaderActionSelectionGroupCleanup || r.Group == nil || r.Cleanup.Group.ID != r.Group.ID || !ValidDigest(r.Cleanup.Key)) {
		return errors.New("node_selection_cleanup_invalid")
	}
	if r.Selection != nil {
		if _, err := r.Selection.Selection.Digest(); err != nil || r.Selection.RootName == "" || len(r.Selection.RootName) > 255 || r.Selection.Tag != "omc-"+r.TaskID {
			return errors.New("node_selection_invalid")
		}
		if r.Action != DownloaderActionGet && r.Action != DownloaderActionPause && r.Action != DownloaderActionResume && r.Action != DownloaderActionCancel && r.Action != DownloaderActionManifest && r.Action != DownloaderActionSetCategory {
			return errors.New("node_selection_invalid")
		}
	}
	if !validID(r.RequestID) || !validID(r.TaskID) || !validID(r.OperationKey) || !validID(r.DownloaderID) || !validID(r.Action) || !validID(r.GrantID) {
		return errors.New("node_downloader_request_invalid")
	}
	switch r.Action {
	case DownloaderActionTest:
		if r.Submit != nil || r.ProviderTaskID != "" || r.ManagedTag != "" {
			return errors.New("node_downloader_request_invalid")
		}
	case DownloaderActionSubmit, DownloaderActionSelectionGroup, DownloaderActionSelectionGroupCleanup:
		if r.Submit == nil || strings.TrimSpace(r.Submit.Tag) == "" || strings.TrimSpace(r.Submit.SavePath) == "" {
			return errors.New("node_downloader_request_invalid")
		}
		if (r.Action == DownloaderActionSelectionGroup || r.Action == DownloaderActionSelectionGroupCleanup) && r.Group == nil {
			return errors.New("node_selection_group_invalid")
		}
		if r.Action == DownloaderActionSelectionGroupCleanup && r.Cleanup == nil {
			return errors.New("node_selection_cleanup_invalid")
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

func safeGroupPrefix(id string) string {
	if len(id) != 64 {
		return ""
	}
	return id[:32]
}

func (r DownloaderActionRequest) Digest() (string, error) {
	copy := r
	copy.GrantID = ""
	if copy.Submit != nil && copy.Submit.SourceEnvelope != nil {
		submit := *copy.Submit
		submit.SourceEnvelope = &DownloaderSourceEnvelope{Digest: submit.SourceEnvelope.Digest, SelectionDigest: submit.SourceEnvelope.SelectionDigest, ConfigurationScope: submit.SourceEnvelope.ConfigurationScope}
		copy.Submit = &submit
	}
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
