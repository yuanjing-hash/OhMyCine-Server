package nodeclient

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
)

type ReadFileChunkRequest struct {
	OperationKey string
	TaskID       string
	FileToken    string
	FileSize     int64
	FileSHA256   string
	Chunk        nodeprotocol.FileChunkDigest
}

func (c *Client) FileExportManifest(ctx context.Context, operationKey, taskID string, page, pageSize int) (nodeprotocol.FileExportManifestPage, error) {
	page, pageSize, err := nodeprotocol.NormalizeManifestPage(page, pageSize)
	if err != nil {
		return nodeprotocol.FileExportManifestPage{}, err
	}
	var result nodeprotocol.FileExportManifestPage
	err = c.fileExportJSON(ctx, fileExportPath(operationKey, "manifest", "", page, pageSize), taskID, &result)
	return result, err
}

func (c *Client) FileExportChunks(ctx context.Context, operationKey, taskID, fileToken string, page, pageSize int) (nodeprotocol.FileChunkDigestPage, error) {
	if err := nodeprotocol.ValidateFileToken(fileToken); err != nil {
		return nodeprotocol.FileChunkDigestPage{}, err
	}
	page, pageSize, err := nodeprotocol.NormalizeChunkPage(page, pageSize)
	if err != nil {
		return nodeprotocol.FileChunkDigestPage{}, err
	}
	var result nodeprotocol.FileChunkDigestPage
	err = c.fileExportJSON(ctx, fileExportPath(operationKey, "chunks", fileToken, page, pageSize), taskID, &result)
	return result, err
}

func (c *Client) ReadFileChunk(ctx context.Context, input ReadFileChunkRequest) ([]byte, error) {
	if input.OperationKey == "" || input.TaskID == "" || input.FileSize <= 0 || input.Chunk.Offset < 0 || input.Chunk.Size <= 0 || input.Chunk.Size > nodeprotocol.FileChunkSize || input.Chunk.Offset > input.FileSize-input.Chunk.Size || len(input.FileSHA256) != sha256.Size*2 || len(input.Chunk.SHA256) != sha256.Size*2 || nodeprotocol.ValidateFileToken(input.FileToken) != nil {
		return nil, errors.New("node_file_chunk_request_invalid")
	}
	path := fileExportPath(input.OperationKey, "files", input.FileToken, 0, 0)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-OhMyCine-Server-ID", c.identity.ServerID)
	req.Header.Set("X-OhMyCine-Task-ID", input.TaskID)
	end := input.Chunk.Offset + input.Chunk.Size - 1
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", input.Chunk.Offset, end))
	response, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusPartialContent {
		return nil, decodeRemoteResponseError(response)
	}
	wantContentRange := fmt.Sprintf("bytes %d-%d/%d", input.Chunk.Offset, end, input.FileSize)
	if response.Header.Get("Content-Range") != wantContentRange || response.Header.Get("Accept-Ranges") != "bytes" || response.Header.Get("ETag") != `"sha256:`+input.FileSHA256+`"` || response.ContentLength != input.Chunk.Size {
		return nil, errors.New("node_file_chunk_response_invalid")
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, input.Chunk.Size+1))
	if err != nil || int64(len(payload)) != input.Chunk.Size {
		return nil, errors.New("node_file_chunk_short_read")
	}
	digest := sha256.Sum256(payload)
	actual := hex.EncodeToString(digest[:])
	if subtle.ConstantTimeCompare([]byte(actual), []byte(input.Chunk.SHA256)) != 1 {
		return nil, errors.New(nodeprotocol.ErrorChecksumMismatch)
	}
	return payload, nil
}

func (c *Client) fileExportJSON(ctx context.Context, path, taskID string, output any) error {
	if strings.TrimSpace(taskID) == "" {
		return errors.New("node_file_task_invalid")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-OhMyCine-Server-ID", c.identity.ServerID)
	req.Header.Set("X-OhMyCine-Task-ID", taskID)
	response, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return decodeRemoteResponseError(response)
	}
	return decodeBoundedJSON(response.Body, output)
}

func decodeRemoteResponseError(response *http.Response) error {
	var remote nodeprotocol.ErrorResponse
	if decodeBoundedJSON(response.Body, &remote) == nil && strings.TrimSpace(remote.Code) != "" {
		return &RemoteError{Code: remote.Code}
	}
	return &RemoteError{Code: "node_request_failed"}
}

func fileExportPath(operationKey, kind, fileToken string, page, pageSize int) string {
	base := "/node/v1/operations/" + url.PathEscape(operationKey) + "/" + kind
	if fileToken != "" {
		base += "/" + url.PathEscape(fileToken)
	}
	if page > 0 {
		query := url.Values{}
		query.Set("page", strconv.Itoa(page))
		query.Set("page_size", strconv.Itoa(pageSize))
		base += "?" + query.Encode()
	}
	return base
}
