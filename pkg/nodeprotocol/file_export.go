package nodeprotocol

import (
	"errors"
	"strings"
	"time"
)

const (
	FileChunkSize           int64 = 8 << 20
	DefaultManifestPageSize       = 100
	MaxManifestPageSize           = 250
	DefaultChunkPageSize          = 256
	MaxChunkPageSize              = 1024
)

// FileExportSummary is the immutable, task-bound description of files a Node
// makes available to its paired Server. It never includes a Node filesystem
// path or provider identity.
type FileExportSummary struct {
	OperationKey   string    `json:"operation_key"`
	TaskID         string    `json:"task_id"`
	Name           string    `json:"name"`
	ManifestDigest string    `json:"manifest_digest"`
	TotalFiles     int       `json:"total_files"`
	TotalBytes     int64     `json:"total_bytes"`
	ChunkSize      int64     `json:"chunk_size"`
	CreatedAt      time.Time `json:"created_at"`
	ExpiresAt      time.Time `json:"expires_at"`
}

type FileExportFile struct {
	FileToken    string `json:"file_token"`
	RelativePath string `json:"relative_path"`
	Size         int64  `json:"size"`
	SHA256       string `json:"sha256"`
	ChunkCount   int    `json:"chunk_count"`
}

type FileExportManifestPage struct {
	Summary  FileExportSummary `json:"summary"`
	Files    []FileExportFile  `json:"files"`
	Page     int               `json:"page"`
	PageSize int               `json:"page_size"`
	HasMore  bool              `json:"has_more"`
}

type FileChunkDigest struct {
	Index  int    `json:"index"`
	Offset int64  `json:"offset"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type FileChunkDigestPage struct {
	OperationKey string            `json:"operation_key"`
	TaskID       string            `json:"task_id"`
	FileToken    string            `json:"file_token"`
	FileSHA256   string            `json:"file_sha256"`
	FileSize     int64             `json:"file_size"`
	ChunkSize    int64             `json:"chunk_size"`
	TotalChunks  int               `json:"total_chunks"`
	Chunks       []FileChunkDigest `json:"chunks"`
	Page         int               `json:"page"`
	PageSize     int               `json:"page_size"`
	HasMore      bool              `json:"has_more"`
}

func NormalizeManifestPage(page, pageSize int) (int, int, error) {
	return normalizeExportPage(page, pageSize, DefaultManifestPageSize, MaxManifestPageSize)
}

func NormalizeChunkPage(page, pageSize int) (int, int, error) {
	return normalizeExportPage(page, pageSize, DefaultChunkPageSize, MaxChunkPageSize)
}

func ValidateFileToken(value string) error {
	value = strings.TrimSpace(value)
	if !validID(value) || !strings.HasPrefix(value, "file:") {
		return errors.New("node_file_token_invalid")
	}
	return nil
}

func normalizeExportPage(page, pageSize, defaultPageSize, maxPageSize int) (int, int, error) {
	if page == 0 {
		page = 1
	}
	if pageSize == 0 {
		pageSize = defaultPageSize
	}
	if page < 1 || pageSize < 1 || pageSize > maxPageSize {
		return 0, 0, errors.New("node_export_page_invalid")
	}
	return page, pageSize, nil
}
