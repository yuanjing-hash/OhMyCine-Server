//go:build quark_live

package nodeclient

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/nodeagent"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
)

// This child is a real Store/Handler in another OS process. Its cloud transport
// denies every external call; a failed native request is evidence of fail-closed
// dispatch, not evidence of a cloud upload or content-integrity acceptance.
func TestQuarkValidationNodeProcessChild(t *testing.T) {
	if os.Getenv("OMC_QUARK_OFFLINE_NODE_CHILD") != "1" {
		t.Skip("child entry")
	}
	input := bufio.NewReader(os.Stdin)
	line, err := input.ReadBytes('\n')
	if err != nil {
		t.Fatal("child configuration missing")
	}
	var o nodeagent.QuarkValidationNodeOptions
	if json.Unmarshal(line, &o) != nil {
		t.Fatal("child configuration invalid")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	go func() { _, _ = io.Copy(io.Discard, input); cancel() }()
	var calls atomic.Int64
	wrap := func(cloud.Config, http.RoundTripper, bool) (http.RoundTripper, error) {
		return quarkOfflineDenyTransport{calls: &calls}, nil
	}
	err = nodeagent.RunQuarkValidationNode(ctx, o, wrap, func(ready nodeagent.QuarkValidationNodeReady) error {
		return json.NewEncoder(os.Stdout).Encode(ready)
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = json.NewEncoder(os.Stdout).Encode(map[string]int64{"denied_native_requests": calls.Load()})
}

type quarkOfflineDenyTransport struct{ calls *atomic.Int64 }

func (d quarkOfflineDenyTransport) RoundTrip(*http.Request) (*http.Response, error) {
	d.calls.Add(1)
	return nil, errors.New("offline_fixture_cloud_denied")
}

type quarkOfflineChild struct {
	stdin io.WriteCloser
	cmd   *exec.Cmd
	lines chan []byte
	ready nodeagent.QuarkValidationNodeReady
}

func startQuarkOfflineChild(t *testing.T, o nodeagent.QuarkValidationNodeOptions) *quarkOfflineChild {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^TestQuarkValidationNodeProcessChild$", "-test.timeout=55s")
	cmd.Env = append(os.Environ(), "OMC_QUARK_OFFLINE_NODE_CHILD=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = io.Discard
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	c := &quarkOfflineChild{stdin: stdin, cmd: cmd, lines: make(chan []byte, 16)}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = stdin.Close()
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	go func() {
		defer close(c.lines)
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			c.lines <- append([]byte(nil), scanner.Bytes()...)
		}
	}()
	if err = json.NewEncoder(stdin).Encode(o); err != nil {
		t.Fatal(err)
	}
	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()
	var lastLine string
	for {
		select {
		case line, ok := <-c.lines:
			if !ok {
				t.Fatal("offline fixture child exited before ready:", lastLine)
			}
			if strings.Contains(string(line), "quark_") {
				lastLine = string(line)
			}
			if json.Unmarshal(line, &c.ready) == nil && c.ready.Socket != "" {
				return c
			}
		case <-timer.C:
			t.Fatal("child did not become ready")
		}
	}
}

func stopQuarkOfflineChild(t *testing.T, c *quarkOfflineChild) int64 {
	t.Helper()
	_ = c.stdin.Close()
	var calls int64 = -1
	for line := range c.lines {
		var summary map[string]int64
		if json.Unmarshal(line, &summary) == nil {
			if count, ok := summary["denied_native_requests"]; ok {
				calls = count
			}
		}
	}
	if err := c.cmd.Wait(); err != nil {
		t.Fatal("child failed during clean shutdown", err)
	}
	if calls < 0 {
		t.Fatal("child did not report shutdown")
	}
	return calls
}

func TestQuarkValidationNodeSeparateProcessSignedSealedAndRestart(t *testing.T) {
	parent := t.TempDir()
	parent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(parent, "source")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, (8<<20)+1)
	for i := range data {
		data[i] = byte(i % 251)
	}
	sum := sha256.Sum256(data)
	if err := os.WriteFile(filepath.Join(source, "sample.bin"), data, 0600); err != nil {
		t.Fatal(err)
	}
	o := nodeagent.QuarkValidationNodeOptions{NodeID: "quark-validation-node", ServerID: "quark-validation-server", TaskID: "quark-validation-task", ExportOperationKey: "quark-validation-export", DataDirectory: filepath.Join(parent, "state"), ManagedRoot: source, SourceName: "sample.bin", SourceSize: int64(len(data)), SourceSHA256: hex.EncodeToString(sum[:]), EnrollmentToken: strings.Repeat("offline-enrollment-", 3)}
	child := startQuarkOfflineChild(t, o)
	if child.ready.PID == os.Getpid() || child.ready.PID != child.cmd.Process.Pid {
		t.Fatal("not a distinct Node OS process")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	identity, err := GenerateIdentity(o.ServerID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	enrolled, err := EnrollQuarkValidationNode(ctx, child.ready.Socket, o.NodeID, o.EnrollmentToken, identity)
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewQuarkValidationNodeClient(child.ready.Socket, enrolled.CertificateFingerprint, identity)
	if err != nil {
		t.Fatal(err)
	}
	health, err := client.Health(ctx)
	if err != nil || health.NodeID != o.NodeID {
		t.Fatal("signed health failed", err)
	}
	manifest, err := client.FileExportManifest(ctx, o.ExportOperationKey, o.TaskID, 1, 1)
	if err != nil || len(manifest.Files) != 1 || manifest.Files[0].SHA256 != o.SourceSHA256 {
		t.Fatal("real source manifest failed", err)
	}
	file := manifest.Files[0]
	chunks, err := client.FileExportChunks(ctx, o.ExportOperationKey, o.TaskID, file.FileToken, 1, 16)
	if err != nil || len(chunks.Chunks) != 2 {
		t.Fatal("real source chunks failed", err)
	}
	for _, chunk := range chunks.Chunks {
		got, e := client.ReadFileChunk(ctx, ReadFileChunkRequest{OperationKey: o.ExportOperationKey, TaskID: o.TaskID, FileToken: file.FileToken, FileSize: file.Size, FileSHA256: file.SHA256, Chunk: chunk})
		if e != nil || !bytes.Equal(got, data[chunk.Offset:chunk.Offset+chunk.Size]) {
			t.Fatal("signed source chunk mismatch", e)
		}
	}
	now := time.Now().UTC()
	tv := &quark.Authorization{AccessToken: "offline-tv", AccountID: "offline-tv-account", DeviceID: "offline-tv-device", IdentityVerified: true, Generation: 1}
	agent := &quark.Authorization{AccessToken: "offline-agent", AccountID: "offline-agent-account", DeviceID: "offline-agent-device", ClientDeviceID: "offline-agent-client", IdentityVerified: true, Generation: 1}
	proof := quark.SpaceProof{Version: 1, Generation: 1, TVAccount: tv.AccountID, AgentAccount: agent.AccountID, TVRoot: quark.QualifyID(quark.RouteTV, tv.AccountID, "T"), AgentRoot: quark.QualifyID(quark.RouteAgent, agent.AccountID, "A"), Path: "/library", ContentSHA256: strings.Repeat("a", 64), VerifiedAt: now}
	bundle, _ := json.Marshal(quark.Credentials{Version: 1, Revision: 1, TV: tv, Agent: agent, Proofs: []quark.SpaceProof{proof}})
	credential, _ := json.Marshal(nodeprotocol.StorageCredential{ProviderType: nodeprotocol.StorageProviderQuark, RootID: proof.TVRoot, Quark: bundle})
	upload := nodeprotocol.StorageUploadPlan{DigestVersion: 2, StorageID: "quark-validation-storage", ProviderType: nodeprotocol.StorageProviderQuark, SourceExportOperationKey: o.ExportOperationKey, SourceManifestDigest: manifest.Summary.ManifestDigest, TargetRootID: proof.TVRoot, Files: []nodeprotocol.StorageUploadFile{{SourceFileToken: file.FileToken, TargetRelativePath: "sample.bin", Size: file.Size, SHA256: file.SHA256, ConflictAction: nodeprotocol.StorageConflictFailIfExists}}}
	payload, _ := json.Marshal(upload)
	plan := nodeprotocol.OperationPlan{ProtocolVersion: 1, OperationKey: "quark-validation-upload", TaskID: o.TaskID, NodeID: o.NodeID, Kind: nodeprotocol.OperationKindStorageUpload, PlanRevision: 1, LeaseEpoch: 1, LeaseExpiresAt: now.Add(4 * time.Minute), Payload: payload}
	digest, err := plan.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.PutOperation(ctx, nodeprotocol.PutOperationRequest{Plan: plan, PlanDigest: digest}); err != nil {
		t.Fatal(err)
	}
	grant := nodeprotocol.CredentialGrant{GrantID: "quark-validation-grant", NodeID: o.NodeID, TaskID: o.TaskID, OperationKey: plan.OperationKey, ResourceKind: nodeprotocol.ResourceKindStorage, ResourceID: upload.StorageID, CredentialRevision: 1, AllowedActions: []string{nodeprotocol.StorageActionUpload}, ExpiresAt: now.Add(3 * time.Minute), Credential: credential}
	envelope, err := nodeprotocol.SealCredentialGrant(enrolled.EncryptionPublicKey, grant, now)
	if err != nil {
		t.Fatal(err)
	}
	if err = client.PutCredentialGrant(ctx, envelope); err != nil {
		t.Fatal(err)
	}
	request := nodeprotocol.StorageActionRequest{RequestID: "quark-validation-request", TaskID: o.TaskID, OperationKey: plan.OperationKey, PlanDigest: digest, StorageID: upload.StorageID, Action: nodeprotocol.StorageActionUpload, GrantID: grant.GrantID}
	if _, err = client.StorageAction(ctx, request); err != nil {
		t.Fatal(err)
	}
	var result nodeprotocol.StorageActionResponse
	for attempts := 0; attempts < 100; attempts++ {
		result, err = client.StorageActionResult(ctx, request.RequestID, o.TaskID, plan.OperationKey)
		if err != nil {
			t.Fatal(err)
		}
		if result.ErrorCode != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if result.ErrorCode == "" || result.CompletedFiles != 0 || result.Status == nodeprotocol.StorageActionCompleted {
		t.Fatal("denied native request falsely completed")
	}
	if calls := stopQuarkOfflineChild(t, child); calls != 1 {
		t.Fatal("native request was not denied once", calls)
	}
	child = startQuarkOfflineChild(t, o)
	if child.ready.Manifest.Summary.ManifestDigest != manifest.Summary.ManifestDigest || child.ready.Manifest.Files[0].FileToken != file.FileToken {
		t.Fatal("restart changed source identity")
	}
	client, err = NewQuarkValidationNodeClient(child.ready.Socket, enrolled.CertificateFingerprint, identity)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := client.StorageActionResult(ctx, request.RequestID, o.TaskID, plan.OperationKey)
	if err != nil || loaded.PlanDigest != result.PlanDigest || loaded.ErrorCode != result.ErrorCode || loaded.Status != result.Status {
		t.Fatal("restart lost durable receipt", err)
	}
	if calls := stopQuarkOfflineChild(t, child); calls != 0 {
		t.Fatal("query replayed a native cloud operation", calls)
	}
}

func TestQuarkValidationNodeTransportCannotEscapeLoopback(t *testing.T) {
	for _, socket := range []string{"localhost:80", "127.0.0.2:80", "127.0.0.1:0", "127.0.0.1:080", "[::1]:80", "192.168.1.1:80"} {
		if _, err := quarkValidationTransport(socket); err == nil {
			t.Fatal("invalid validation socket accepted")
		}
	}
	tr, err := quarkValidationTransport("127.0.0.1:80")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tr.DialContext(context.Background(), "tcp", "other.example:80"); err == nil {
		t.Fatal("other origin accepted")
	}
	if _, err = NormalizePublicURL("http://127.0.0.1:80"); err == nil {
		t.Fatal("production origin policy loosened")
	}
}
