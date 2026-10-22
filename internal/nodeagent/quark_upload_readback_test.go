package nodeagent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
)

// Real native mapping and media requests are pinned to a verified local TLS
// listener, with the actual Node encrypted receipt/checkpoint/grant stores.
// This is process-local fault injection, never real-provider acceptance.
type quarkNodeReadbackFixture struct {
	created, corrupt, revision, duplicate atomic.Bool
	submits, reads, readBytes             atomic.Int64
	mode                                  string
	onFinal                               func()
}

func newQuarkNodeReadbackFixture(t *testing.T, mode string) *quarkNodeReadbackFixture {
	t.Helper()
	f := &quarkNodeReadbackFixture{mode: mode}
	fixture := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var data any
		file := func(id, parent, name string, directory bool) map[string]any {
			kind, size := "1", 3
			if directory {
				kind, size = "0", 0
			}
			return map[string]any{"fid": id, "parent_fid": parent, "filename": name, "file_type": kind, "size": size}
		}
		switch r.URL.Path {
		case "/open/v1/file/info":
			id := r.URL.Query().Get("fid")
			switch id {
			case "list|root", "ack-root":
				data = file(id, "0", "library", true)
			case "creation-ack":
				data = file(id, "ack-root", "created.bin", false)
			case "list|created":
				data = file(id, "list|root", "created.bin", false)
			default:
				t.Error("unplanned management identity")
				http.Error(w, "unexpected", 500)
				return
			}
		case "/open/v1/file/list":
			var input struct {
				Parent string `json:"parent_fid"`
			}
			_ = json.NewDecoder(r.Body).Decode(&input)
			items := []any{}
			switch input.Parent {
			case "0":
				items = append(items, file("list|root", "0", "library", true))
			case "list|root":
				if f.created.Load() {
					items = append(items, file("list|created", input.Parent, "created.bin", false))
				}
			default:
				t.Error("unplanned management directory")
				http.Error(w, "unexpected", 500)
				return
			}
			data = map[string]any{"last_page": true, "file_list": items}
		case "/open/v1/file/upload_pre":
			f.created.Store(true)
			f.submits.Add(1)
			if f.mode == "unknown-ack" {
				http.Error(w, "lost response", http.StatusServiceUnavailable)
				return
			}
			data = map[string]any{"fid": "creation-ack", "finish": true}
		case "/file":
			if r.URL.Query().Get("method") == "download" {
				if r.URL.Query().Get("fid") != "tv-created" {
					t.Error("non-TV identity used for readback")
				}
				data = map[string]any{"fid": "tv-created", "file_name": "created.bin", "size": 3, "download_url": "https://objects.example/readback"}
				break
			}
			items := []any{}
			switch r.URL.Query().Get("parent_fid") {
			case "0":
				items = append(items, map[string]any{"fid": "T", "filename": "library", "isdir": 1, "size": 0})
			case "T":
				if f.created.Load() {
					version := "v1"
					if f.revision.Load() {
						version = "v2"
					}
					items = append(items, map[string]any{"fid": "tv-created", "filename": "created.bin", "isdir": 0, "size": 3, "version": version})
					if f.duplicate.Load() {
						items = append(items, map[string]any{"fid": "other", "filename": "created.bin", "isdir": 0, "size": 3, "version": version})
					}
				}
			default:
				t.Error("unplanned TV directory")
				http.Error(w, "unexpected", 500)
				return
			}
			data = map[string]any{"total_count": len(items), "files": items}
		case "/readback":
			if r.Method != http.MethodGet || r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" || r.Header.Get("Range") != "bytes=0-2" {
				t.Error("unsafe or unbounded original read")
			}
			n := f.reads.Add(1)
			if n == 2 {
				switch f.mode {
				case "revision":
					f.revision.Store(true)
				case "duplicate":
					f.duplicate.Store(true)
				}
				if f.onFinal != nil {
					f.onFinal()
					<-r.Context().Done()
					return
				}
			}
			body := []byte("abc")
			if n > 1 && f.corrupt.Load() {
				body = []byte("xyz")
			}
			w.Header().Set("Content-Length", "3")
			w.Header().Set("Content-Range", "bytes 0-2/3")
			w.WriteHeader(http.StatusPartialContent)
			written, _ := w.Write(body)
			f.readBytes.Add(int64(written))
			return
		default:
			t.Error("unplanned provider or token endpoint")
			http.Error(w, "unexpected", 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": 0, "data": data})
	}))
	t.Cleanup(fixture.Close)
	old := http.DefaultTransport
	transport := old.(*http.Transport).Clone()
	roots := x509.NewCertPool()
	roots.AddCert(fixture.Certificate())
	config := &tls.Config{RootCAs: roots, ServerName: fixture.Certificate().DNSNames[0], MinVersion: tls.VersionTLS12}
	transport.Proxy = nil
	transport.DialTLSContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&tls.Dialer{NetDialer: &net.Dialer{}, Config: config}).DialContext(ctx, network, fixture.Listener.Addr().String())
	}
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = old; transport.CloseIdleConnections() })
	return f
}

func TestQuarkNativeNodeUploadReadbackCountAndRecovery(t *testing.T) {
	for _, mode := range []string{"success", "corruption", "revision", "duplicate", "cancel", "revoked", "unknown-ack"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newQuarkNodeReadbackFixture(t, mode)
			a := newQuarkNodeTestAgent(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			body := []byte("abc")
			if err := os.WriteFile(filepath.Join(a.config.ManagedRoot, "created.bin"), body, 0600); err != nil {
				t.Fatal(err)
			}
			now := a.now()
			export, err := buildFileExport(ctx, a.config.ManagedRoot, "server-1", "export-1", ManagedDownload{TaskID: "task-1", NodeLocalRoot: a.config.ManagedRoot}, downloadpkg.Manifest{Complete: true, Files: []downloadpkg.File{{RelativePath: "created.bin", Size: 3}}}, now, now.Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			if err = a.store.SaveFileExport(ctx, "server-1", export); err != nil {
				t.Fatal(err)
			}
			tv := &quark.Authorization{AccountID: "tv-account", AccessToken: "fixture-tv", DeviceID: "fixture-tv-device", Generation: 1, IdentityVerified: true}
			agent := &quark.Authorization{AccountID: "agent-account", AccessToken: "fixture-agent", DeviceID: "fixture-agent-device", ClientDeviceID: "fixture-agent-client", Generation: 1, IdentityVerified: true}
			root := quark.QualifyID(quark.RouteTV, tv.AccountID, "T")
			proof := quark.SpaceProof{Version: 1, Generation: 1, TVAccount: tv.AccountID, AgentAccount: agent.AccountID, TVRoot: root, AgentRoot: quark.QualifyID(quark.RouteAgent, agent.AccountID, "list|root"), Path: "/library", ReceiptID: "fixture-proof", ContentSHA256: strings.Repeat("a", 64), VerifiedAt: now}
			raw, _ := json.Marshal(quark.Credentials{Version: 1, Revision: 1, TV: tv, Agent: agent, Proofs: []quark.SpaceProof{proof}})
			credential := nodeprotocol.StorageCredential{ProviderType: cloud.ProviderQuark, RootID: root, Quark: raw}
			file := export.Files[0].Public
			plan := nodeprotocol.StorageUploadPlan{DigestVersion: 2, StorageID: "storage-1", ProviderType: cloud.ProviderQuark, SourceExportOperationKey: "export-1", SourceManifestDigest: export.Summary.ManifestDigest, TargetRootID: root, Files: []nodeprotocol.StorageUploadFile{{SourceFileToken: file.FileToken, TargetRelativePath: "created.bin", Size: file.Size, SHA256: file.SHA256, ConflictAction: nodeprotocol.StorageConflictFailIfExists}}}
			raw, _ = json.Marshal(plan)
			op := nodeprotocol.OperationPlan{ProtocolVersion: 1, OperationKey: "upload-1", TaskID: "task-1", NodeID: "node-1", Kind: nodeprotocol.OperationKindStorageUpload, PlanRevision: 1, LeaseEpoch: 1, LeaseExpiresAt: now.Add(time.Hour), Payload: raw}
			digest, _ := op.Digest()
			if _, _, err = a.store.Put(ctx, "server-1", digest, op, now); err != nil {
				t.Fatal(err)
			}
			input := nodeprotocol.StorageActionRequest{OperationKey: op.OperationKey, TaskID: op.TaskID, PlanDigest: digest}
			newDriver := func(node *Agent) (storageUploadDriver, *quarkGrantState) {
				t.Helper()
				grant := quarkTestGrantState(t, node, op.OperationKey, credential)
				driver, err := node.newQuarkUploadDriver(context.Background(), "server-1", input, credential, grant)
				if err != nil {
					t.Fatal(err)
				}
				return driver, grant
			}
			driver, grant := newDriver(a)
			fixture.corrupt.Store(mode == "corruption")
			if mode == "cancel" {
				fixture.onFinal = cancel
			}
			if mode == "revoked" {
				fixture.onFinal = func() {
					if _, err := a.store.Cancel(context.Background(), "server-1", op.OperationKey, a.now()); err != nil {
						t.Error(err)
					}
				}
			}
			result, err := a.uploadQuarkStorageFile(ctx, "server-1", input, plan, plan.Files[0], driver, grant.check)
			if (err == nil) != (mode == "success") || fixture.submits.Load() != 1 {
				t.Fatalf("mode=%s submissions=%d error=%v", mode, fixture.submits.Load(), err)
			}
			if mode == "success" {
				if fixture.reads.Load() != 2 || fixture.readBytes.Load() != 6 || result.Status != nodeprotocol.StorageFileCompleted || result.ContentDigest == nil || !result.ContentDigest.ValidReadbackSHA256(file.SHA256) {
					t.Fatal("Node did not perform exactly creation plus final full readback")
				}
			} else if result.Status == nodeprotocol.StorageFileCompleted || result.ContentDigest != nil {
				t.Fatal("failed verification completed the Node file")
			}
			checkpoint, err := a.store.StorageUploadCheckpoint(context.Background(), op.OperationKey, file.FileToken)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "unknown-ack" {
				if checkpoint.TargetItemID != "" || fixture.reads.Load() != 0 {
					t.Fatal("unknown ACK guessed a target or read content")
				}
				if _, err = a.uploadQuarkStorageFile(context.Background(), "server-1", input, plan, plan.Files[0], driver, grant.check); err == nil || fixture.submits.Load() != 1 {
					t.Fatal("unknown upload was resubmitted or marked complete")
				}
				return
			}
			if checkpoint.TargetItemID != quark.QualifyID(quark.RouteTV, tv.AccountID, "tv-created") {
				t.Fatal("exact original output was not checkpointed before verification")
			}
			if mode == "revoked" {
				if _, err = driver.Stat(context.Background(), checkpoint.TargetItemID); err == nil {
					t.Fatal("revoked Node operation retained native read access")
				}
				return
			}
			fixture.corrupt.Store(false)
			fixture.duplicate.Store(false)
			// A new Agent and native Client load the same encrypted original
			// receipts. The exact checkpoint is freshly checked, not re-uploaded.
			restarted, err := New(a.config, a.store)
			if err != nil {
				t.Fatal(err)
			}
			driver, grant = newDriver(restarted)
			reads := fixture.reads.Load()
			result, err = restarted.uploadQuarkStorageFile(context.Background(), "server-1", input, plan, plan.Files[0], driver, grant.check)
			if err != nil || result.Status != nodeprotocol.StorageFileCompleted || result.ContentDigest == nil || !result.ContentDigest.ValidReadbackSHA256(file.SHA256) || fixture.reads.Load() != reads+1 || fixture.submits.Load() != 1 {
				t.Fatalf("cold Node verification skipped bytes or repeated upload: reads=%d before=%d writes=%d error=%v", fixture.reads.Load(), reads, fixture.submits.Load(), err)
			}
		})
	}
}
