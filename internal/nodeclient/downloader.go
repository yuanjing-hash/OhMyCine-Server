package nodeclient

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	pathpkg "path"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
)

type CredentialGrantFactory func(context.Context, string, string, string) (nodeprotocol.CredentialGrantEnvelope, error)

// DownloaderClient implements the ordinary provider contract while ensuring
// every qBittorrent request is executed by the paired Node.
type DownloaderClient struct {
	node         *Client
	downloaderID string
	saveRoot     string
	localRoot    string
	taskID       string
	grant        CredentialGrantFactory
}

func NewDownloaderClient(node *Client, downloaderID, saveRoot, localRoot, taskID string, grant CredentialGrantFactory) (*DownloaderClient, error) {
	if node == nil || strings.TrimSpace(downloaderID) == "" || strings.TrimSpace(saveRoot) == "" || grant == nil {
		return nil, errors.New("node_downloader_config_invalid")
	}
	return &DownloaderClient{node: node, downloaderID: downloaderID, saveRoot: strings.TrimSpace(saveRoot), localRoot: strings.TrimSpace(localRoot), taskID: strings.TrimSpace(taskID), grant: grant}, nil
}

func (c *DownloaderClient) Test(ctx context.Context) (downloadpkg.Health, error) {
	response, err := c.call(ctx, c.request(nodeprotocol.DownloaderActionTest, c.downloaderID, "", false))
	if err != nil || response.Health == nil {
		return downloadpkg.Health{}, remoteDownloaderError(err, "downloader_response_invalid")
	}
	return downloadpkg.Health{Version: response.Health.Version}, nil
}

func (c *DownloaderClient) Submit(ctx context.Context, request downloadpkg.SubmitRequest) (downloadpkg.Task, error) {
	tagTaskID := strings.TrimPrefix(strings.TrimSpace(request.Tag), "omc-")
	if c.taskID != "" && tagTaskID != "" && tagTaskID != c.taskID {
		return downloadpkg.Task{}, downloadpkg.Error(nodeprotocol.ErrorPlanConflict, false, nil)
	}
	taskID := c.boundTaskID()
	if c.taskID == "" && tagTaskID != "" {
		taskID = tagTaskID
	}
	operationKey := stableOperationKey(nodeprotocol.DownloaderActionSubmit, c.downloaderID, request.Tag)
	input := nodeprotocol.DownloaderActionRequest{
		RequestID: operationKey, TaskID: taskID, OperationKey: operationKey,
		DownloaderID: c.downloaderID, Action: nodeprotocol.DownloaderActionSubmit,
		Submit: &nodeprotocol.DownloaderSubmit{
			Source:   nodeprotocol.DownloaderSource{Kind: request.Source.Kind, URL: request.Source.URL, Torrent: request.Source.Torrent, Filename: request.Source.Filename},
			SavePath: joinRemoteRoot(c.saveRoot, taskID), Tag: request.Tag, MetadataOnly: request.MetadataOnly,
		},
	}
	response, err := c.call(ctx, input)
	if err != nil || response.Task == nil {
		return downloadpkg.Task{}, remoteDownloaderError(err, "downloader_response_invalid")
	}
	return localDownloaderTask(*response.Task), nil
}

func (c *DownloaderClient) Get(ctx context.Context, id string) (downloadpkg.Task, error) {
	input := c.request(nodeprotocol.DownloaderActionGet, c.boundTaskID(), id, false)
	response, err := c.call(ctx, input)
	if err != nil || response.Task == nil {
		return downloadpkg.Task{}, remoteDownloaderError(err, "downloader_response_invalid")
	}
	return localDownloaderTask(*response.Task), nil
}

func (c *DownloaderClient) Pause(ctx context.Context, id string) error {
	_, err := c.call(ctx, c.request(nodeprotocol.DownloaderActionPause, c.boundTaskID(), id, true))
	return remoteDownloaderError(err, "")
}

func (c *DownloaderClient) Resume(ctx context.Context, id string) error {
	_, err := c.call(ctx, c.request(nodeprotocol.DownloaderActionResume, c.boundTaskID(), id, true))
	return remoteDownloaderError(err, "")
}

func (c *DownloaderClient) Cancel(ctx context.Context, id string, deleteData bool) error {
	input := c.request(nodeprotocol.DownloaderActionCancel, c.boundTaskID(), id, true)
	input.DeleteData = deleteData
	input.OperationKey = stableOperationKey(input.Action, c.downloaderID, id, boolText(deleteData))
	input.RequestID = input.OperationKey
	_, err := c.call(ctx, input)
	return remoteDownloaderError(err, "")
}

func (c *DownloaderClient) Manifest(ctx context.Context, id string) (downloadpkg.Manifest, error) {
	input := c.request(nodeprotocol.DownloaderActionManifest, c.boundTaskID(), id, false)
	input.OperationKey = stableOperationKey("file_export", c.downloaderID, c.boundTaskID(), id)
	input.RequestID = "qb:manifest:" + uuid.NewString()
	response, err := c.call(ctx, input)
	if err != nil || response.Manifest == nil {
		return downloadpkg.Manifest{}, remoteDownloaderError(err, "downloader_manifest_invalid")
	}
	if response.FileExport != nil {
		return c.fileExportManifest(ctx, *response.FileExport, input.OperationKey)
	}
	files := make([]downloadpkg.File, 0, len(response.Manifest.Files))
	for _, file := range response.Manifest.Files {
		files = append(files, downloadpkg.File{RelativePath: file.RelativePath, Size: file.Size})
	}
	return downloadpkg.Manifest{Name: response.Manifest.Name, Files: files, Complete: response.Manifest.Complete}, nil
}

func (c *DownloaderClient) fileExportManifest(ctx context.Context, summary nodeprotocol.FileExportSummary, expectedOperationKey string) (downloadpkg.Manifest, error) {
	return readFileExportManifest(ctx, c.node, summary, expectedOperationKey, c.boundTaskID(), "qb:file_export:")
}

func readFileExportManifest(ctx context.Context, node *Client, summary nodeprotocol.FileExportSummary, expectedOperationKey, taskID, operationPrefix string) (downloadpkg.Manifest, error) {
	if node == nil || summary.OperationKey != expectedOperationKey || !strings.HasPrefix(summary.OperationKey, operationPrefix) || summary.TaskID != taskID || summary.TotalFiles < 1 || summary.TotalBytes < 0 || summary.ChunkSize != nodeprotocol.FileChunkSize || !nodeprotocol.ValidDigest(summary.ManifestDigest) || summary.CreatedAt.IsZero() || !summary.ExpiresAt.After(time.Now().UTC()) || !summary.ExpiresAt.After(summary.CreatedAt) {
		return downloadpkg.Manifest{}, downloadpkg.Error("downloader_manifest_invalid", false, nil)
	}
	files := make([]downloadpkg.File, 0)
	seenTokens := make(map[string]struct{})
	seenPaths := make(map[string]struct{})
	var totalBytes int64
	for page := 1; len(files) < summary.TotalFiles; page++ {
		result, err := node.FileExportManifest(ctx, summary.OperationKey, summary.TaskID, page, nodeprotocol.MaxManifestPageSize)
		if err != nil {
			return downloadpkg.Manifest{}, remoteDownloaderError(err, "downloader_manifest_invalid")
		}
		if !sameFileExportSummary(result.Summary, summary) || result.Page != page || result.PageSize != nodeprotocol.MaxManifestPageSize || len(result.Files) == 0 || len(result.Files) > result.PageSize || result.HasMore != (len(files)+len(result.Files) < summary.TotalFiles) {
			return downloadpkg.Manifest{}, downloadpkg.Error("downloader_manifest_invalid", false, nil)
		}
		for _, file := range result.Files {
			relative := strings.ReplaceAll(strings.TrimSpace(file.RelativePath), "\\", "/")
			expectedChunks := 0
			if file.Size > 0 {
				expectedChunks = int((file.Size-1)/summary.ChunkSize + 1)
			}
			if relative == "" || pathpkg.Clean(relative) != relative || pathpkg.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, "../") || strings.Contains(relative, ":") || file.Size < 0 || nodeprotocol.ValidateFileToken(file.FileToken) != nil || !nodeprotocol.ValidDigest(file.SHA256) || file.ChunkCount != expectedChunks {
				return downloadpkg.Manifest{}, downloadpkg.Error("downloader_manifest_invalid", false, nil)
			}
			if _, duplicate := seenTokens[file.FileToken]; duplicate {
				return downloadpkg.Manifest{}, downloadpkg.Error("downloader_manifest_invalid", false, nil)
			}
			pathKey := relative
			if _, duplicate := seenPaths[pathKey]; duplicate {
				return downloadpkg.Manifest{}, downloadpkg.Error("downloader_manifest_invalid", false, nil)
			}
			if totalBytes > summary.TotalBytes-file.Size {
				return downloadpkg.Manifest{}, downloadpkg.Error("downloader_manifest_invalid", false, nil)
			}
			totalBytes += file.Size
			seenTokens[file.FileToken] = struct{}{}
			seenPaths[pathKey] = struct{}{}
			files = append(files, downloadpkg.File{RelativePath: relative, Size: file.Size, SHA256: file.SHA256, RemoteFileToken: file.FileToken})
		}
		if !result.HasMore {
			break
		}
	}
	if len(files) != summary.TotalFiles || totalBytes != summary.TotalBytes {
		return downloadpkg.Manifest{}, downloadpkg.Error("downloader_manifest_invalid", false, nil)
	}
	expiresAt := summary.ExpiresAt
	return downloadpkg.Manifest{Name: summary.Name, Files: files, Complete: true, RemoteExportOperationKey: summary.OperationKey, RemoteExportDigest: summary.ManifestDigest, RemoteExportExpiresAt: &expiresAt}, nil
}

func sameFileExportSummary(left, right nodeprotocol.FileExportSummary) bool {
	return left.OperationKey == right.OperationKey && left.TaskID == right.TaskID && left.Name == right.Name && left.ManifestDigest == right.ManifestDigest && left.TotalFiles == right.TotalFiles && left.TotalBytes == right.TotalBytes && left.ChunkSize == right.ChunkSize && left.CreatedAt.Equal(right.CreatedAt) && left.ExpiresAt.Equal(right.ExpiresAt)
}

func (c *DownloaderClient) RemoteFileChunks(ctx context.Context, manifest downloadpkg.Manifest, file downloadpkg.File) ([]downloadpkg.RemoteFileChunk, error) {
	return readRemoteFileChunks(ctx, c.node, c.boundTaskID(), manifest, file)
}

func readRemoteFileChunks(ctx context.Context, node *Client, taskID string, manifest downloadpkg.Manifest, file downloadpkg.File) ([]downloadpkg.RemoteFileChunk, error) {
	if node == nil || strings.TrimSpace(taskID) == "" || manifest.RemoteExportOperationKey == "" || !nodeprotocol.ValidDigest(manifest.RemoteExportDigest) || manifest.RemoteExportExpiresAt == nil || !manifest.RemoteExportExpiresAt.After(time.Now().UTC()) || file.Size < 0 || !nodeprotocol.ValidDigest(file.SHA256) || nodeprotocol.ValidateFileToken(file.RemoteFileToken) != nil {
		return nil, downloadpkg.Error("downloader_manifest_invalid", false, nil)
	}
	totalChunks := 0
	if file.Size > 0 {
		totalChunks = int((file.Size-1)/nodeprotocol.FileChunkSize + 1)
	}
	chunks := make([]downloadpkg.RemoteFileChunk, 0, totalChunks)
	for page := 1; len(chunks) < totalChunks; page++ {
		result, err := node.FileExportChunks(ctx, manifest.RemoteExportOperationKey, taskID, file.RemoteFileToken, page, nodeprotocol.MaxChunkPageSize)
		if err != nil {
			return nil, remoteDownloaderError(err, "downloader_manifest_invalid")
		}
		if result.OperationKey != manifest.RemoteExportOperationKey || result.TaskID != taskID || result.FileToken != file.RemoteFileToken || result.FileSHA256 != file.SHA256 || result.FileSize != file.Size || result.ChunkSize != nodeprotocol.FileChunkSize || result.TotalChunks != totalChunks || result.Page != page || result.PageSize != nodeprotocol.MaxChunkPageSize || len(result.Chunks) == 0 || len(result.Chunks) > result.PageSize || result.HasMore != (len(chunks)+len(result.Chunks) < totalChunks) {
			return nil, downloadpkg.Error("downloader_manifest_invalid", false, nil)
		}
		for _, chunk := range result.Chunks {
			expectedIndex := len(chunks)
			expectedOffset := int64(expectedIndex) * nodeprotocol.FileChunkSize
			expectedSize := min(nodeprotocol.FileChunkSize, file.Size-expectedOffset)
			if chunk.Index != expectedIndex || chunk.Offset != expectedOffset || chunk.Size != expectedSize || !nodeprotocol.ValidDigest(chunk.SHA256) {
				return nil, downloadpkg.Error("downloader_manifest_invalid", false, nil)
			}
			chunks = append(chunks, downloadpkg.RemoteFileChunk{Index: chunk.Index, Offset: chunk.Offset, Size: chunk.Size, SHA256: chunk.SHA256})
		}
		if !result.HasMore {
			break
		}
	}
	if len(chunks) != totalChunks {
		return nil, downloadpkg.Error("downloader_manifest_invalid", false, nil)
	}
	return chunks, nil
}

func (c *DownloaderClient) ReadRemoteFileChunk(ctx context.Context, manifest downloadpkg.Manifest, file downloadpkg.File, chunk downloadpkg.RemoteFileChunk) ([]byte, error) {
	return readRemoteFileChunk(ctx, c.node, c.boundTaskID(), manifest, file, chunk)
}

func readRemoteFileChunk(ctx context.Context, node *Client, taskID string, manifest downloadpkg.Manifest, file downloadpkg.File, chunk downloadpkg.RemoteFileChunk) ([]byte, error) {
	if node == nil || strings.TrimSpace(taskID) == "" || manifest.RemoteExportOperationKey == "" || file.RemoteFileToken == "" {
		return nil, downloadpkg.Error("downloader_manifest_invalid", false, nil)
	}
	payload, err := node.ReadFileChunk(ctx, ReadFileChunkRequest{OperationKey: manifest.RemoteExportOperationKey, TaskID: taskID, FileToken: file.RemoteFileToken, FileSize: file.Size, FileSHA256: file.SHA256, Chunk: nodeprotocol.FileChunkDigest{Index: chunk.Index, Offset: chunk.Offset, Size: chunk.Size, SHA256: chunk.SHA256}})
	if err != nil {
		if err.Error() == nodeprotocol.ErrorChecksumMismatch || err.Error() == "node_file_chunk_response_invalid" {
			return nil, downloadpkg.Error(nodeprotocol.ErrorChecksumMismatch, false, err)
		}
		return nil, remoteDownloaderError(err, "node_file_chunk_read_failed")
	}
	return payload, nil
}

func (c *DownloaderClient) DeleteManagedTag(ctx context.Context, tag string) error {
	input := c.request(nodeprotocol.DownloaderActionDeleteTag, c.boundTaskID(), "", true)
	input.ManagedTag = tag
	input.OperationKey = stableOperationKey(input.Action, c.downloaderID, tag)
	input.RequestID = input.OperationKey
	_, err := c.call(ctx, input)
	return remoteDownloaderError(err, "")
}

func (c *DownloaderClient) Categories(ctx context.Context) ([]downloadpkg.Category, error) {
	response, err := c.call(ctx, c.request(nodeprotocol.DownloaderActionCategories, c.boundTaskID(), "", false))
	if err != nil {
		return nil, remoteDownloaderError(err, "")
	}
	items := make([]downloadpkg.Category, 0, len(response.Categories))
	for _, category := range response.Categories {
		items = append(items, downloadpkg.Category{Name: category.Name, SavePath: c.toLocalPath(category.SavePath)})
	}
	return items, nil
}

func (c *DownloaderClient) EnsureCategory(ctx context.Context, name, savePath string) error {
	return c.categoryAction(ctx, nodeprotocol.DownloaderActionEnsureCategory, "", name, savePath)
}

func (c *DownloaderClient) UpdateCategory(ctx context.Context, name, savePath string) error {
	return c.categoryAction(ctx, nodeprotocol.DownloaderActionUpdateCategory, "", name, savePath)
}

func (c *DownloaderClient) SetCategory(ctx context.Context, id, name, savePath string) error {
	return c.categoryAction(ctx, nodeprotocol.DownloaderActionSetCategory, id, name, savePath)
}

func (c *DownloaderClient) categoryAction(ctx context.Context, action, providerTaskID, name, localPath string) error {
	remotePath, ok := c.toRemotePath(localPath)
	if !ok {
		return downloadpkg.Error(nodeprotocol.ErrorPathMappingInvalid, false, nil)
	}
	input := c.request(action, c.boundTaskID(), providerTaskID, true)
	input.CategoryName, input.SavePath = name, remotePath
	input.OperationKey = stableOperationKey(action, c.downloaderID, c.boundTaskID(), providerTaskID, name, remotePath)
	input.RequestID = input.OperationKey
	_, err := c.call(ctx, input)
	return remoteDownloaderError(err, "")
}

func (c *DownloaderClient) call(ctx context.Context, input nodeprotocol.DownloaderActionRequest) (nodeprotocol.DownloaderActionResponse, error) {
	envelope, err := c.grant(ctx, input.TaskID, input.OperationKey, input.Action)
	if err != nil {
		return nodeprotocol.DownloaderActionResponse{}, err
	}
	input.GrantID = envelope.GrantID
	if err := c.node.PutCredentialGrant(ctx, envelope); err != nil {
		return nodeprotocol.DownloaderActionResponse{}, err
	}
	return c.node.DownloaderAction(ctx, input)
}

func (c *DownloaderClient) request(action, taskID, providerTaskID string, mutating bool) nodeprotocol.DownloaderActionRequest {
	operationKey := stableOperationKey(action, c.downloaderID, providerTaskID)
	requestID := operationKey
	if !mutating {
		requestID = "qb:" + action + ":" + uuid.NewString()
		operationKey = requestID
	}
	return nodeprotocol.DownloaderActionRequest{RequestID: requestID, TaskID: taskID, OperationKey: operationKey, DownloaderID: c.downloaderID, Action: action, ProviderTaskID: providerTaskID}
}

func stableOperationKey(parts ...string) string {
	digest := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return "qb:" + parts[0] + ":" + hex.EncodeToString(digest[:20])
}

func joinRemoteRoot(root, child string) string {
	separator := "/"
	if strings.Contains(root, "\\") && !strings.Contains(root, "/") {
		separator = "\\"
	}
	return strings.TrimRight(root, "/\\") + separator + strings.Trim(child, "/\\")
}

func (c *DownloaderClient) boundTaskID() string {
	if c.taskID != "" {
		return c.taskID
	}
	return c.downloaderID
}

func (c *DownloaderClient) remoteTaskRoot() string {
	if c.taskID == "" {
		return c.saveRoot
	}
	return joinRemoteRoot(c.saveRoot, c.taskID)
}

func (c *DownloaderClient) toRemotePath(localPath string) (string, bool) {
	if c.localRoot == "" || c.taskID == "" {
		return "", false
	}
	if strings.ContainsAny(localPath, "\x00\r\n") {
		return "", false
	}
	root := filepath.Clean(c.localRoot)
	candidate := filepath.Clean(strings.TrimSpace(localPath))
	if !filepath.IsAbs(root) || !filepath.IsAbs(candidate) {
		return "", false
	}
	relative, err := filepath.Rel(root, candidate)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return "", false
	}
	remote := c.remoteTaskRoot()
	if relative != "." {
		remote = joinRemoteRoot(remote, filepath.ToSlash(relative))
	}
	return remote, true
}

func (c *DownloaderClient) toLocalPath(remotePath string) string {
	if c.localRoot == "" || c.taskID == "" {
		return remotePath
	}
	rootRaw := c.remoteTaskRoot()
	root := pathpkg.Clean(strings.ReplaceAll(rootRaw, "\\", "/"))
	candidate := pathpkg.Clean(strings.ReplaceAll(strings.TrimSpace(remotePath), "\\", "/"))
	windowsPath := len(root) >= 2 && root[1] == ':' || strings.HasPrefix(rootRaw, `\\`)
	compareRoot, compareCandidate := root, candidate
	if windowsPath {
		compareRoot, compareCandidate = strings.ToLower(root), strings.ToLower(candidate)
	}
	if compareCandidate != compareRoot && !strings.HasPrefix(compareCandidate, compareRoot+"/") {
		return remotePath
	}
	relative := strings.TrimPrefix(candidate[len(root):], "/")
	if relative == "" {
		return c.localRoot
	}
	return filepath.Join(c.localRoot, filepath.FromSlash(relative))
}

func boolText(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

func localDownloaderTask(task nodeprotocol.DownloaderTask) downloadpkg.Task {
	return downloadpkg.Task{ID: task.ID, Name: task.Name, Status: task.Status, Progress: task.Progress, BytesCompleted: task.BytesCompleted, BytesTotal: task.BytesTotal, DownloadSpeed: task.DownloadSpeed, UploadSpeed: task.UploadSpeed, ETASeconds: task.ETASeconds, Ratio: task.Ratio, SeededSeconds: task.SeededSeconds, UploadedBytes: task.UploadedBytes, Seeding: task.Seeding, Completed: task.Completed, Failed: task.Failed, ErrorCode: task.ErrorCode}
}

func remoteDownloaderError(err error, fallback string) error {
	if err == nil {
		if fallback == "" {
			return nil
		}
		return downloadpkg.Error(fallback, false, nil)
	}
	var remote *RemoteError
	if errors.As(err, &remote) {
		retryable := remote.Code == nodeprotocol.ErrorNodeOffline || remote.Code == nodeprotocol.ErrorCredentialExpired || remote.Code == nodeprotocol.ErrorExportPreparing || remote.Code == "node_request_failed" || remote.Code == "downloader_unavailable" || remote.Code == "downloader_rate_limited"
		return downloadpkg.Error(remote.Code, retryable, err)
	}
	return downloadpkg.Error(nodeprotocol.ErrorNodeOffline, true, err)
}
