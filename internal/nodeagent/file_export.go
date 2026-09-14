package nodeagent

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
)

const maxFileExportFiles = 100000
const fileExportLifetime = 7 * 24 * time.Hour

type fileExportDigestFile struct {
	RelativePath string                         `json:"relative_path"`
	Size         int64                          `json:"size"`
	SHA256       string                         `json:"sha256"`
	Chunks       []nodeprotocol.FileChunkDigest `json:"chunks"`
}

func buildFileExport(ctx context.Context, managedRoot, serverID, operationKey string, download ManagedDownload, manifest downloadpkg.Manifest, now, expiresAt time.Time) (FileExportRecord, error) {
	if !manifest.Complete || len(manifest.Files) == 0 || len(manifest.Files) > maxFileExportFiles || serverID == "" || operationKey == "" || download.TaskID == "" || !expiresAt.After(now) {
		return FileExportRecord{}, errors.New("node_file_export_invalid")
	}
	root, err := secureExportRoot(managedRoot, download.NodeLocalRoot)
	if err != nil {
		return FileExportRecord{}, err
	}
	files := append([]downloadpkg.File(nil), manifest.Files...)
	sort.Slice(files, func(i, j int) bool {
		return normalizedExportPath(files[i].RelativePath) < normalizedExportPath(files[j].RelativePath)
	})
	record := FileExportRecord{Summary: nodeprotocol.FileExportSummary{OperationKey: operationKey, TaskID: download.TaskID, Name: strings.TrimSpace(manifest.Name), ChunkSize: nodeprotocol.FileChunkSize, CreatedAt: now.UTC(), ExpiresAt: expiresAt.UTC()}, Files: make([]FileExportRecordFile, 0, len(files))}
	if record.Summary.Name == "" {
		record.Summary.Name = download.TaskID
	}
	digestFiles := make([]fileExportDigestFile, 0, len(files))
	seen := make(map[string]struct{}, len(files))
	for _, file := range files {
		relative := normalizedExportPath(file.RelativePath)
		if relative == "" || len(relative) > 1024 || file.Size < 0 {
			return FileExportRecord{}, errors.New("node_managed_file_invalid")
		}
		key := strings.ToLower(relative)
		if _, exists := seen[key]; exists {
			return FileExportRecord{}, errors.New("node_file_export_duplicate_path")
		}
		seen[key] = struct{}{}
		candidate, err := secureExportFile(root, relative, file.Size)
		if err != nil {
			return FileExportRecord{}, err
		}
		sha, chunks, err := hashExportFile(ctx, candidate, file.Size)
		if err != nil {
			return FileExportRecord{}, err
		}
		token, err := randomFileToken()
		if err != nil {
			return FileExportRecord{}, err
		}
		public := nodeprotocol.FileExportFile{FileToken: token, RelativePath: relative, Size: file.Size, SHA256: sha, ChunkCount: len(chunks)}
		record.Files = append(record.Files, FileExportRecordFile{Public: public, NodePath: candidate, Chunks: chunks})
		digestFiles = append(digestFiles, fileExportDigestFile{RelativePath: relative, Size: file.Size, SHA256: sha, Chunks: chunks})
		if record.Summary.TotalBytes > int64(^uint64(0)>>1)-file.Size {
			return FileExportRecord{}, errors.New("node_file_export_too_large")
		}
		record.Summary.TotalBytes += file.Size
	}
	digestRaw, err := json.Marshal(digestFiles)
	if err != nil {
		return FileExportRecord{}, err
	}
	digest := sha256.Sum256(digestRaw)
	record.Summary.ManifestDigest = hex.EncodeToString(digest[:])
	record.Summary.TotalFiles = len(record.Files)
	return record, nil
}

func normalizedExportPath(value string) string {
	value = strings.ReplaceAll(strings.TrimSpace(value), "\\", "/")
	if value == "" || strings.ContainsRune(value, '\x00') || path.IsAbs(value) {
		return ""
	}
	value = path.Clean(value)
	if value == "." || value == ".." || strings.HasPrefix(value, "../") {
		return ""
	}
	return value
}

func secureExportRoot(managedRoot, downloadRoot string) (string, error) {
	managed, err := filepath.EvalSymlinks(filepath.Clean(managedRoot))
	if err != nil {
		return "", errors.New("node_managed_root_invalid")
	}
	root, err := filepath.EvalSymlinks(filepath.Clean(downloadRoot))
	if err != nil || requirePathWithin(managed, root) != nil {
		return "", errors.New(nodeprotocol.ErrorPathMappingInvalid)
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return "", errors.New(nodeprotocol.ErrorPathMappingInvalid)
	}
	return root, nil
}

func secureExportFile(root, relative string, expectedSize int64) (string, error) {
	candidate := filepath.Join(root, filepath.FromSlash(relative))
	if err := requirePathWithin(root, candidate); err != nil {
		return "", errors.New("node_managed_file_invalid")
	}
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil || requirePathWithin(root, resolved) != nil {
		return "", errors.New("node_managed_file_invalid")
	}
	info, err := os.Lstat(resolved)
	if err != nil || !info.Mode().IsRegular() || info.Size() != expectedSize {
		return "", errors.New("node_managed_file_invalid")
	}
	return resolved, nil
}

func hashExportFile(ctx context.Context, filename string, expectedSize int64) (string, []nodeprotocol.FileChunkDigest, error) {
	file, err := os.Open(filename)
	if err != nil {
		return "", nil, errors.New("node_managed_file_invalid")
	}
	defer func() { _ = file.Close() }()
	before, err := file.Stat()
	if err != nil || !before.Mode().IsRegular() || before.Size() != expectedSize {
		return "", nil, errors.New("node_managed_file_invalid")
	}
	whole := sha256.New()
	buffer := make([]byte, nodeprotocol.FileChunkSize)
	chunks := make([]nodeprotocol.FileChunkDigest, 0, (expectedSize+nodeprotocol.FileChunkSize-1)/nodeprotocol.FileChunkSize)
	var offset int64
	for offset < expectedSize {
		if err := ctx.Err(); err != nil {
			return "", nil, err
		}
		want := min(int64(len(buffer)), expectedSize-offset)
		read, err := io.ReadFull(file, buffer[:want])
		if err != nil {
			return "", nil, errors.New("node_managed_file_changed")
		}
		part := buffer[:read]
		_, _ = whole.Write(part)
		sum := sha256.Sum256(part)
		chunks = append(chunks, nodeprotocol.FileChunkDigest{Index: len(chunks), Offset: offset, Size: int64(read), SHA256: hex.EncodeToString(sum[:])})
		offset += int64(read)
	}
	if extra := make([]byte, 1); expectedSize >= 0 {
		if read, err := file.Read(extra); read != 0 || err != io.EOF {
			return "", nil, errors.New("node_managed_file_changed")
		}
	}
	after, err := file.Stat()
	if err != nil || after.Size() != before.Size() || after.ModTime() != before.ModTime() || !os.SameFile(before, after) {
		return "", nil, errors.New("node_managed_file_changed")
	}
	return hex.EncodeToString(whole.Sum(nil)), chunks, nil
}

func randomFileToken() (string, error) {
	value := make([]byte, 24)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return "file:" + base64.RawURLEncoding.EncodeToString(value), nil
}

func (a *Agent) startFileExport(serverID, operationKey string, download ManagedDownload, manifest downloadpkg.Manifest) {
	a.exportMu.Lock()
	if _, running := a.exports[operationKey]; running {
		a.exportMu.Unlock()
		return
	}
	// A request can pass the caller's initial lookup while another request is
	// still hashing. Recheck persistence under the single-flight lock so a late
	// request cannot wastefully rebuild an export that has just completed.
	if page, err := a.store.FileExportManifest(context.Background(), serverID, operationKey, 1, 1, a.now()); err == nil && page.Summary.TaskID == download.TaskID {
		a.exportMu.Unlock()
		return
	}
	a.exports[operationKey] = struct{}{}
	a.exportMu.Unlock()
	go func() {
		defer func() {
			a.exportMu.Lock()
			delete(a.exports, operationKey)
			a.exportMu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
		defer cancel()
		now := a.now()
		record, err := buildFileExport(ctx, a.config.ManagedRoot, serverID, operationKey, download, manifest, now, now.Add(fileExportLifetime))
		if err != nil {
			return
		}
		_ = a.store.SaveFileExport(ctx, serverID, record)
	}()
}
