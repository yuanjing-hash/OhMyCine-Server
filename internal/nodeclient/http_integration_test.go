package nodeclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/nodeagent"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
)

type captureHTTPTransport struct {
	base http.RoundTripper
	last *http.Request
}

func (c *captureHTTPTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.last = r.Clone(r.Context())
	return c.base.RoundTrip(r)
}

func TestProductionHTTPEnrollmentHealthAndFullChunk(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := nodeagent.Config{NodeID: "node-1", Transport: "http", ListenAddress: "127.0.0.1:0", DataDirectory: dir, ManagedRoot: filepath.Join(dir, "managed"), MaxConcurrentOperations: 2, EnrollmentToken: strings.Repeat("x", 64), TLSCertificateFile: filepath.Join(dir, "node.crt"), TLSPrivateKeyFile: filepath.Join(dir, "node.key"), SealingPrivateKeyFile: filepath.Join(dir, "seal.key")}
	if err := nodeagent.EnsureTLSIdentity(cfg); err != nil {
		t.Fatal(err)
	}
	if err := nodeagent.EnsureSealingIdentity(cfg); err != nil {
		t.Fatal(err)
	}
	store, err := nodeagent.OpenStore(filepath.Join(dir, "node.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	agent, err := nodeagent.New(cfg, store)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(agent.Handler())
	defer server.Close()
	// Only this test transport maps the public origin to a loopback listener.
	transport := publicTransport()
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	defer transport.CloseIdleConnections()
	identity, err := GenerateIdentity("server-1", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	enrollment, err := enrollWithTransport(ctx, "http://node.example.com", cfg.NodeID, cfg.EnrollmentToken, identity, transport)
	if err != nil {
		t.Fatal(err)
	}
	client, err := New("http://node.example.com", enrollment.CertificateFingerprint, identity)
	if err != nil {
		t.Fatal(err)
	}
	capture := &captureHTTPTransport{base: transport}
	client.http.Transport = &signedTransport{base: capture, identity: identity, nodeFingerprint: enrollment.CertificateFingerprint}
	health, err := client.Health(ctx)
	if err != nil || health.NodeID != cfg.NodeID {
		t.Fatalf("health=%+v err=%v", health, err)
	}
	replay, err := transport.RoundTrip(capture.last.Clone(ctx))
	if err != nil {
		t.Fatal(err)
	}
	replayBody, _ := io.ReadAll(replay.Body)
	replay.Body.Close()
	if replay.StatusCode != http.StatusUnauthorized {
		t.Fatalf("replay status=%d", replay.StatusCode)
	}
	if err := nodeprotocol.VerifyHTTPResponse(capture.last, replay, replayBody, enrollment.CertificateFingerprint); err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte{0x5a}, int(nodeprotocol.FileChunkSize))
	filename := filepath.Join(cfg.ManagedRoot, "movie.mkv")
	if err := os.WriteFile(filename, payload, 0600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(payload)
	digest := hex.EncodeToString(sum[:])
	token := "file:" + strings.Repeat("a", 38)
	now := time.Now().UTC()
	chunk := nodeprotocol.FileChunkDigest{Index: 0, Offset: 0, Size: nodeprotocol.FileChunkSize, SHA256: digest}
	record := nodeagent.FileExportRecord{Summary: nodeprotocol.FileExportSummary{OperationKey: "export-1", TaskID: "task-1", Name: "Movie", ManifestDigest: digest, TotalFiles: 1, TotalBytes: nodeprotocol.FileChunkSize, ChunkSize: nodeprotocol.FileChunkSize, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}, Files: []nodeagent.FileExportRecordFile{{Public: nodeprotocol.FileExportFile{FileToken: token, RelativePath: "movie.mkv", Size: nodeprotocol.FileChunkSize, SHA256: digest, ChunkCount: 1}, NodePath: filename, Chunks: []nodeprotocol.FileChunkDigest{chunk}}}}
	if err := store.SaveFileExport(ctx, "server-1", record); err != nil {
		t.Fatal(err)
	}
	manifest, err := client.FileExportManifest(ctx, "export-1", "task-1", 1, 10)
	if err != nil || len(manifest.Files) != 1 {
		t.Fatalf("manifest err=%v", err)
	}
	got, err := client.ReadFileChunk(ctx, ReadFileChunkRequest{OperationKey: "export-1", TaskID: "task-1", FileToken: token, FileSize: nodeprotocol.FileChunkSize, FileSHA256: digest, Chunk: chunk})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("full 8MiB chunk differs")
	}
}
