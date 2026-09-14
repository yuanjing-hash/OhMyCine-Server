package nodeagent

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
)

const fileExportTaskHeader = "X-OhMyCine-Task-ID"

func (a *Agent) fileExportManifest(w http.ResponseWriter, r *http.Request) {
	page, pageSize, err := exportPageQuery(r, false)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "文件清单分页参数无效")
		return
	}
	result, err := a.store.FileExportManifest(r.Context(), r.Header.Get("X-OhMyCine-Verified-Server-ID"), r.PathValue("operation_key"), page, pageSize, a.now())
	if errors.Is(err, ErrOperationNotFound) {
		writeError(w, http.StatusNotFound, "node_file_export_not_found", "远端文件清单不存在或已经过期")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "node_persistence_failed", "节点无法读取远端文件清单")
		return
	}
	if taskID := strings.TrimSpace(r.Header.Get(fileExportTaskHeader)); taskID == "" || taskID != result.Summary.TaskID {
		writeError(w, http.StatusNotFound, "node_file_export_not_found", "远端文件清单不存在或已经过期")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *Agent) fileExportChunks(w http.ResponseWriter, r *http.Request) {
	page, pageSize, err := exportPageQuery(r, true)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "分块清单分页参数无效")
		return
	}
	result, err := a.store.FileExportChunks(r.Context(), r.Header.Get("X-OhMyCine-Verified-Server-ID"), r.PathValue("operation_key"), r.PathValue("file_token"), page, pageSize, a.now())
	if errors.Is(err, ErrOperationNotFound) {
		writeError(w, http.StatusNotFound, "node_file_export_not_found", "远端文件分块清单不存在或已经过期")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "远端文件分块清单无效")
		return
	}
	if taskID := strings.TrimSpace(r.Header.Get(fileExportTaskHeader)); taskID == "" || taskID != result.TaskID {
		writeError(w, http.StatusNotFound, "node_file_export_not_found", "远端文件分块清单不存在或已经过期")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *Agent) fileExportRange(w http.ResponseWriter, r *http.Request) {
	serverID := r.Header.Get("X-OhMyCine-Verified-Server-ID")
	operationKey := r.PathValue("operation_key")
	taskID := strings.TrimSpace(r.Header.Get(fileExportTaskHeader))
	file, err := a.store.FileExportRangeFile(r.Context(), serverID, operationKey, taskID, r.PathValue("file_token"), a.now())
	if errors.Is(err, ErrOperationNotFound) {
		writeError(w, http.StatusNotFound, "node_file_export_not_found", "远端文件不存在或已经过期")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "node_persistence_failed", "节点无法读取远端文件")
		return
	}
	start, end, err := explicitFileRange(r.Header.Get("Range"), file.Size)
	if err != nil {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", file.Size))
		writeError(w, http.StatusRequestedRangeNotSatisfiable, "node_file_range_invalid", "只允许读取一个不超过 8 MiB 的明确字节范围")
		return
	}
	chunk, err := a.store.FileExportChunkDigest(r.Context(), serverID, operationKey, taskID, file.FileToken, start, end-start+1, a.now())
	if err != nil {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", file.Size))
		writeError(w, http.StatusRequestedRangeNotSatisfiable, "node_file_range_invalid", "读取范围必须与已核验分块完全一致")
		return
	}
	handle, err := a.openManagedExportFile(file)
	if err != nil {
		writeError(w, http.StatusConflict, "node_managed_file_changed", "远端文件已变化，需要重新核验清单")
		return
	}
	defer handle.Close()
	if _, err := handle.Seek(start, io.SeekStart); err != nil {
		writeError(w, http.StatusConflict, "node_managed_file_changed", "远端文件已变化，需要重新核验清单")
		return
	}
	length := end - start + 1
	payload := make([]byte, length)
	if _, err := io.ReadFull(handle, payload); err != nil {
		writeError(w, http.StatusConflict, "node_managed_file_changed", "远端文件已变化，需要重新核验清单")
		return
	}
	digest := sha256.Sum256(payload)
	actual := hex.EncodeToString(digest[:])
	if subtle.ConstantTimeCompare([]byte(actual), []byte(chunk.SHA256)) != 1 {
		writeError(w, http.StatusConflict, nodeprotocol.ErrorChecksumMismatch, "远端文件分块校验失败，需要重新核验清单")
		return
	}
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, file.Size))
	w.Header().Set("ETag", `"sha256:`+file.SHA256+`"`)
	w.WriteHeader(http.StatusPartialContent)
	_, _ = w.Write(payload)
}

func exportPageQuery(r *http.Request, chunks bool) (int, int, error) {
	page, err := optionalPositiveQuery(r, "page")
	if err != nil {
		return 0, 0, errors.New("node_export_page_invalid")
	}
	pageSize, err := optionalPositiveQuery(r, "page_size")
	if err != nil {
		return 0, 0, errors.New("node_export_page_invalid")
	}
	if chunks {
		return nodeprotocol.NormalizeChunkPage(page, pageSize)
	}
	return nodeprotocol.NormalizeManifestPage(page, pageSize)
}

func optionalPositiveQuery(r *http.Request, name string) (int, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return 0, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 || value > 1000000 {
		return 0, errors.New("invalid page")
	}
	return value, nil
}

func explicitFileRange(value string, size int64) (int64, int64, error) {
	if size <= 0 || !strings.HasPrefix(value, "bytes=") || strings.Contains(value, ",") {
		return 0, 0, errors.New("invalid range")
	}
	parts := strings.Split(strings.TrimPrefix(value, "bytes="), "-")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return 0, 0, errors.New("invalid range")
	}
	start, startErr := strconv.ParseInt(parts[0], 10, 64)
	end, endErr := strconv.ParseInt(parts[1], 10, 64)
	if startErr != nil || endErr != nil || start < 0 || end < start || end >= size || end-start+1 > nodeprotocol.FileChunkSize {
		return 0, 0, errors.New("invalid range")
	}
	return start, end, nil
}

func (a *Agent) openManagedExportFile(file FileExportRangeFile) (*os.File, error) {
	managedRoot, err := filepath.EvalSymlinks(filepath.Clean(a.config.ManagedRoot))
	if err != nil || requirePathWithin(managedRoot, file.NodePath) != nil {
		return nil, errors.New("managed root invalid")
	}
	resolved, err := filepath.EvalSymlinks(file.NodePath)
	if err != nil || requirePathWithin(managedRoot, resolved) != nil || !sameCleanPath(resolved, file.NodePath) {
		return nil, errors.New("managed file changed")
	}
	handle, err := os.Open(resolved)
	if err != nil {
		return nil, err
	}
	info, err := handle.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != file.Size {
		_ = handle.Close()
		return nil, errors.New("managed file changed")
	}
	return handle, nil
}

func sameCleanPath(left, right string) bool {
	left, right = filepath.Clean(left), filepath.Clean(right)
	if filepath.Separator == '\\' {
		return strings.EqualFold(left, right)
	}
	return left == right
}
