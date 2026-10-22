package nodeagent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
)

// Exercise the real adapter through the Node's encrypted durable receipt store.
// ACK IDs and list IDs deliberately use different provider namespaces. Every
// request is pinned to this TLS fixture; no account or external API is accessed.
func TestQuarkNodeMkdirMappingRecoversOnlyOriginalOperation(t *testing.T) {
	var created, visible, replaced atomic.Bool
	var submits atomic.Int64
	fixture := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var data any
		directory := func(id, parent, name string) map[string]any {
			return map[string]any{"fid": id, "parent_fid": parent, "filename": name, "file_type": "0", "size": 0}
		}
		switch r.URL.Path {
		case "/file":
			if r.URL.Query().Get("method") != "list" {
				t.Error("unexpected TV operation")
				http.Error(w, "unexpected", 500)
				return
			}
			items := []any{}
			switch r.URL.Query().Get("parent_fid") {
			case "0":
				items = append(items, map[string]any{"fid": "tv-root", "filename": "library", "isdir": 1, "size": 0})
			case "tv-root":
				if created.Load() && visible.Load() {
					id := "tv-created"
					if replaced.Load() {
						id = "tv-replacement"
					}
					items = append(items, map[string]any{"fid": id, "filename": "season", "isdir": 1, "size": 0})
				}
			default:
				t.Error("enumerated an unplanned TV subtree")
				http.Error(w, "unexpected", 500)
				return
			}
			data = map[string]any{"total_count": len(items), "files": items}
		case "/open/v1/file/list":
			var input map[string]any
			_ = json.NewDecoder(r.Body).Decode(&input)
			items := []any{}
			switch input["parent_fid"] {
			case "0":
				items = append(items, directory("list|root", "0", "library"))
			case "list|root":
				if created.Load() {
					items = append(items, directory("list|created", "list|root", "season"))
				}
			default:
				t.Error("enumerated an unplanned management subtree")
				http.Error(w, "unexpected", 500)
				return
			}
			data = map[string]any{"last_page": true, "file_list": items}
		case "/open/v1/file/info":
			switch r.URL.Query().Get("fid") {
			case "list|root":
				data = directory("list|root", "0", "library")
			case "ack-root":
				data = directory("ack-root", "0", "library")
			case "created-ack":
				data = directory("created-ack", "ack-root", "season")
			default:
				t.Error("requested an unrelated management identity")
				http.Error(w, "unexpected", 500)
				return
			}
		case "/open/v1/dir":
			var input map[string]string
			_ = json.NewDecoder(r.Body).Decode(&input)
			if r.Method != http.MethodPost || input["pdir_fid"] != "list|root" || input["dir_path"] != "season" {
				t.Error("mutation escaped the frozen destination")
				http.Error(w, "unexpected", 500)
				return
			}
			submits.Add(1)
			created.Store(true)
			data = map[string]any{"fid": "created-ack", "finish": true}
		default:
			t.Error("unexpected provider endpoint")
			http.Error(w, "unexpected", 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": 0, "data": data})
	}))
	defer fixture.Close()
	oldTransport := http.DefaultTransport
	transport := oldTransport.(*http.Transport).Clone()
	roots := x509.NewCertPool()
	roots.AddCert(fixture.Certificate())
	transport.TLSClientConfig = &tls.Config{RootCAs: roots, ServerName: fixture.Certificate().DNSNames[0], MinVersion: tls.VersionTLS12}
	transport.Proxy = nil
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, fixture.Listener.Addr().String())
	}
	http.DefaultTransport = transport
	defer func() { http.DefaultTransport = oldTransport; transport.CloseIdleConnections() }()

	tv := &quark.Authorization{AccessToken: "test-tv", AccountID: "tv-account", DeviceID: "tv-device", Generation: 1, IdentityVerified: true}
	agent := &quark.Authorization{AccessToken: "test-agent", AccountID: "agent-account", DeviceID: "agent-device", ClientDeviceID: "agent-client", Generation: 1, IdentityVerified: true}
	root := quark.QualifyID(quark.RouteTV, tv.AccountID, "tv-root")
	proof := quark.SpaceProof{Version: 1, Generation: 1, TVAccount: tv.AccountID, AgentAccount: agent.AccountID, TVRoot: root, AgentRoot: quark.QualifyID(quark.RouteAgent, agent.AccountID, "list|root"), Path: "/library", ContentSHA256: strings.Repeat("a", 64), VerifiedAt: time.Now()}
	raw, _ := json.Marshal(quark.Credentials{Version: 1, Revision: 1, TV: tv, Agent: agent, Proofs: []quark.SpaceProof{proof}})
	credential := nodeprotocol.StorageCredential{ProviderType: cloud.ProviderQuark, RootID: root, Quark: raw}
	ctx := context.Background()
	node := newQuarkNodeTestAgent(t)
	newDriver := func(a *Agent) *nodeQuarkUploadDriver {
		t.Helper()
		base, err := a.newQuarkUploadDriver(ctx, "server-1", nodeprotocol.StorageActionRequest{OperationKey: "operation-1"}, credential, quarkTestGrantState(t, a, "operation-1", credential))
		if err != nil {
			t.Fatal(err)
		}
		return base.(*nodeQuarkUploadDriver)
	}
	driver := newDriver(node)
	if _, err := driver.CreateDirectory(ctx, root, "season"); err == nil {
		t.Fatal("unconfirmed TV output was accepted")
	}
	opID := nodeQuarkOperationID("operation-1", "mkdir", root, "season")
	receipts, err := node.quarkReceipts("server-1", "operation-1")
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := receipts.Load(ctx, opID)
	if err != nil || receipt.State != "accepted" || submits.Load() != 1 {
		t.Fatal("original accepted operation was not durably retained", err)
	}
	visible.Store(true)
	restarted, err := New(node.config, node.store)
	if err != nil {
		t.Fatal(err)
	}
	driver = newDriver(restarted)
	item, err := driver.CreateDirectory(ctx, root, "season")
	expected := quark.QualifyID(quark.RouteTV, tv.AccountID, "tv-created")
	if err != nil || item.ID != expected || item.ParentID != root || !item.IsDir || submits.Load() != 1 {
		t.Fatal("restart failed to recover the exact original output without resubmitting", err)
	}
	receipt, err = receipts.Load(ctx, opID)
	if err != nil || receipt.State != "confirmed" {
		t.Fatal("confirmed mapping was not persisted", err)
	}

	ack := quark.QualifyID(quark.RouteAgent, agent.AccountID, "created-ack")
	for _, missing := range []bool{true, false} {
		invalidCtx := ctx
		if !missing {
			invalidCtx = quark.WithOperationID(ctx, opID+"-wrong")
		}
		if _, err = driver.ResolveReadItem(invalidCtx, ack); err == nil {
			t.Fatal("ACK/list namespaces were merged without the original receipt")
		}
	}
	opCtx := quark.WithOperationID(ctx, opID)
	if _, err = driver.ResolveReadItem(opCtx, quark.QualifyID(quark.RouteAgent, agent.AccountID, "unrelated-ack")); err == nil {
		t.Fatal("a different ACK borrowed this operation's mapping")
	}
	// Lose the adapter cache once more: the confirmed mapping must be reloaded
	// from the encrypted receipt and revalidated against fresh directory data.
	driver = newDriver(restarted)
	if item, err = driver.ResolveReadItem(opCtx, ack); err != nil || item.ID != expected {
		t.Fatal("cold confirmed mapping did not recover", err)
	}
	replaced.Store(true)
	if _, err = driver.ResolveReadItem(opCtx, ack); err == nil {
		t.Fatal("same-name replacement stole the confirmed output identity")
	}
	if submits.Load() != 1 {
		t.Fatal("a recovery or rejected lookup submitted a second mutation")
	}
}
