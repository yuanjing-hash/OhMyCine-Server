package nodeagent

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
)

func TestFileExportHTTPRequiresTaskBoundExactVerifiedRanges(t *testing.T) {
	dir := t.TempDir()
	managed := filepath.Join(dir, "managed")
	downloadRoot := filepath.Join(managed, "downloads", "task-1")
	if err := os.MkdirAll(downloadRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(downloadRoot, "movie.mkv")
	if err := os.WriteFile(filename, []byte("abcdef"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(filepath.Join(dir, "node.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	now := time.Now().UTC().Truncate(time.Millisecond)
	record, err := buildFileExport(context.Background(), managed, "server-1", "export:task-1", ManagedDownload{TaskID: "task-1", NodeLocalRoot: downloadRoot}, downloadpkg.Manifest{Complete: true, Files: []downloadpkg.File{{RelativePath: "movie.mkv", Size: 6}}}, now, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveFileExport(context.Background(), "server-1", record); err != nil {
		t.Fatal(err)
	}
	agent, err := New(Config{NodeID: "node-1", ListenAddress: "127.0.0.1:0", DataDirectory: dir, ManagedRoot: managed, MaxConcurrentOperations: 2, AllowInsecureDevelopment: true}, store)
	if err != nil {
		t.Fatal(err)
	}
	agent.now = func() time.Time { return now }
	server := httptest.NewServer(agent.Handler())
	defer server.Close()
	request := func(target, taskID, byteRange string) *http.Response {
		req, requestErr := http.NewRequest(http.MethodGet, server.URL+target, nil)
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		req.Header.Set("X-OhMyCine-Server-ID", "server-1")
		if taskID != "" {
			req.Header.Set(fileExportTaskHeader, taskID)
		}
		if byteRange != "" {
			req.Header.Set("Range", byteRange)
		}
		response, requestErr := http.DefaultClient.Do(req)
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		return response
	}

	manifest := request("/node/v1/operations/export:task-1/manifest?page=1&page_size=10", "task-1", "")
	manifestBody, _ := io.ReadAll(manifest.Body)
	_ = manifest.Body.Close()
	if manifest.StatusCode != http.StatusOK || bytes.Contains(manifestBody, []byte(downloadRoot)) || bytes.Contains(manifestBody, []byte("node_path")) {
		t.Fatalf("manifest status=%d body=%s", manifest.StatusCode, manifestBody)
	}
	var page struct {
		Files []struct {
			FileToken string `json:"file_token"`
		} `json:"files"`
	}
	if err := json.Unmarshal(manifestBody, &page); err != nil || len(page.Files) != 1 {
		t.Fatalf("manifest decode: %v body=%s", err, manifestBody)
	}
	token := page.Files[0].FileToken

	wrongTask := request("/node/v1/operations/export:task-1/manifest", "task-2", "")
	_ = wrongTask.Body.Close()
	if wrongTask.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-task manifest status=%d", wrongTask.StatusCode)
	}
	missingRange := request("/node/v1/operations/export:task-1/files/"+token, "task-1", "")
	_ = missingRange.Body.Close()
	if missingRange.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("full-file read status=%d", missingRange.StatusCode)
	}
	partialChunk := request("/node/v1/operations/export:task-1/files/"+token, "task-1", "bytes=0-2")
	_ = partialChunk.Body.Close()
	if partialChunk.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("non-chunk read status=%d", partialChunk.StatusCode)
	}
	valid := request("/node/v1/operations/export:task-1/files/"+token, "task-1", "bytes=0-5")
	validBody, _ := io.ReadAll(valid.Body)
	_ = valid.Body.Close()
	if valid.StatusCode != http.StatusPartialContent || string(validBody) != "abcdef" || valid.Header.Get("Content-Range") != "bytes 0-5/6" || !strings.HasPrefix(valid.Header.Get("ETag"), `"sha256:`) {
		t.Fatalf("range status=%d headers=%v body=%q", valid.StatusCode, valid.Header, validBody)
	}
	if err := os.WriteFile(filename, []byte("ABCDEF"), 0o600); err != nil {
		t.Fatal(err)
	}
	tampered := request("/node/v1/operations/export:task-1/files/"+token, "task-1", "bytes=0-5")
	tamperedBody, _ := io.ReadAll(tampered.Body)
	_ = tampered.Body.Close()
	if tampered.StatusCode != http.StatusConflict || !bytes.Contains(tamperedBody, []byte("node_checksum_mismatch")) {
		t.Fatalf("tampered range status=%d body=%s", tampered.StatusCode, tamperedBody)
	}
}
