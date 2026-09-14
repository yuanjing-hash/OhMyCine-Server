package nodeclient

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
)

func TestFileExportClientBindsTaskAndVerifiesChunk(t *testing.T) {
	payload := []byte("abcdef")
	chunkSum := sha256.Sum256(payload)
	chunkSHA := hex.EncodeToString(chunkSum[:])
	fileSHA := strings.Repeat("a", 64)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-OhMyCine-Server-ID") != "server-1" || r.Header.Get("X-OhMyCine-Task-ID") != "task-1" {
			t.Errorf("missing task binding headers: %v", r.Header)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/manifest"):
			_ = json.NewEncoder(w).Encode(nodeprotocol.FileExportManifestPage{Summary: nodeprotocol.FileExportSummary{OperationKey: "export:task-1", TaskID: "task-1", ManifestDigest: strings.Repeat("b", 64), TotalFiles: 1, TotalBytes: 6, ChunkSize: nodeprotocol.FileChunkSize, CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}, Files: []nodeprotocol.FileExportFile{{FileToken: "file:opaque-1", RelativePath: "movie.mkv", Size: 6, SHA256: fileSHA, ChunkCount: 1}}, Page: 1, PageSize: 10})
		case strings.Contains(r.URL.Path, "/chunks/"):
			_ = json.NewEncoder(w).Encode(nodeprotocol.FileChunkDigestPage{OperationKey: "export:task-1", TaskID: "task-1", FileToken: "file:opaque-1", FileSHA256: fileSHA, FileSize: 6, ChunkSize: nodeprotocol.FileChunkSize, TotalChunks: 1, Chunks: []nodeprotocol.FileChunkDigest{{Index: 0, Offset: 0, Size: 6, SHA256: chunkSHA}}, Page: 1, PageSize: 10})
		case strings.Contains(r.URL.Path, "/files/"):
			if r.Header.Get("Range") != "bytes=0-5" {
				t.Errorf("range=%q", r.Header.Get("Range"))
			}
			w.Header().Set("Accept-Ranges", "bytes")
			w.Header().Set("Content-Range", "bytes 0-5/6")
			w.Header().Set("Content-Length", "6")
			w.Header().Set("ETag", `"sha256:`+fileSHA+`"`)
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(payload)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := &Client{baseURL: server.URL, identity: Identity{ServerID: "server-1"}, http: server.Client()}

	manifest, err := client.FileExportManifest(context.Background(), "export:task-1", "task-1", 1, 10)
	if err != nil || len(manifest.Files) != 1 {
		t.Fatalf("manifest=%+v err=%v", manifest, err)
	}
	chunks, err := client.FileExportChunks(context.Background(), "export:task-1", "task-1", manifest.Files[0].FileToken, 1, 10)
	if err != nil || len(chunks.Chunks) != 1 {
		t.Fatalf("chunks=%+v err=%v", chunks, err)
	}
	got, err := client.ReadFileChunk(context.Background(), ReadFileChunkRequest{OperationKey: "export:task-1", TaskID: "task-1", FileToken: manifest.Files[0].FileToken, FileSize: manifest.Files[0].Size, FileSHA256: manifest.Files[0].SHA256, Chunk: chunks.Chunks[0]})
	if err != nil || string(got) != string(payload) {
		t.Fatalf("chunk=%q err=%v", got, err)
	}
}

func TestFileExportClientRejectsUnverifiedChunkBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Range", "bytes 0-2/3")
		w.Header().Set("Content-Length", "3")
		w.Header().Set("ETag", `"sha256:`+strings.Repeat("a", 64)+`"`)
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("bad"))
	}))
	defer server.Close()
	client := &Client{baseURL: server.URL, identity: Identity{ServerID: "server-1"}, http: server.Client()}
	_, err := client.ReadFileChunk(context.Background(), ReadFileChunkRequest{OperationKey: "export:task-1", TaskID: "task-1", FileToken: "file:opaque-1", FileSize: 3, FileSHA256: strings.Repeat("a", 64), Chunk: nodeprotocol.FileChunkDigest{Index: 0, Offset: 0, Size: 3, SHA256: strings.Repeat("b", 64)}})
	if err == nil || err.Error() != nodeprotocol.ErrorChecksumMismatch {
		t.Fatalf("unverified payload error=%v", err)
	}
}
