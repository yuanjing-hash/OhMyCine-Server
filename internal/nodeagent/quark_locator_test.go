package nodeagent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
)

// Uses the real Quark HTTP adapter and Node grant wrapper, with all traffic
// pinned to a local TLS fixture. No provider credential or external API is used.
func TestQuarkNodeColdDirectoryLocatorsStayInsideFrozenScope(t *testing.T) {
	for _, scenario := range []string{"cold", "root_replaced", "directory_replaced", "account_changed", "missing_path"} {
		t.Run(scenario, func(t *testing.T) {
			var changed atomic.Bool
			var calls atomic.Int64
			fixture := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var data any
				if r.URL.Path == "/file" {
					if r.URL.Query().Get("method") != "list" {
						t.Error("unexpected TV write/read route")
						http.Error(w, "unexpected", 500)
						return
					}
					parent := r.URL.Query().Get("parent_fid")
					items := []any{}
					add := func(id, name string) {
						items = append(items, map[string]any{"fid": id, "filename": name, "isdir": 1, "size": 0})
					}
					switch parent {
					case "0":
						id := "T"
						if changed.Load() && scenario == "root_replaced" {
							id = "replacement"
						}
						add(id, "library")
					case "T":
						for i := 0; i < 220; i++ {
							add(fmt.Sprintf("unrelated-%d", i), fmt.Sprintf("unrelated-%d", i))
						}
						add("N", "nested")
					case "N":
						id := "I"
						if changed.Load() && scenario == "directory_replaced" {
							id = "replacement"
						}
						add(id, "intake")
					default:
						t.Errorf("enumerated unrelated directory %s", parent)
						http.Error(w, "unexpected subtree", 500)
						return
					}
					page, _ := strconv.Atoi(r.URL.Query().Get("page_index"))
					size, _ := strconv.Atoi(r.URL.Query().Get("page_size"))
					start := page * size
					end := min(start+size, len(items))
					if start > len(items) {
						t.Error("invalid page")
						http.Error(w, "page", 500)
						return
					}
					data = map[string]any{"total_count": len(items), "files": items[start:end]}
				} else if r.URL.Path == "/open/v1/file/list" {
					var input map[string]any
					_ = json.NewDecoder(r.Body).Decode(&input)
					var id, parent, name string
					switch input["parent_fid"] {
					case "0":
						id, parent, name = "A", "0", "library"
					case "A":
						id, parent, name = "AN", "A", "nested"
					case "AN":
						id, parent, name = "AI", "AN", "intake"
					default:
						t.Error("unexpected management subtree")
						http.Error(w, "unexpected", 500)
						return
					}
					data = map[string]any{"last_page": true, "file_list": []any{map[string]any{"fid": id, "parent_fid": parent, "filename": name, "file_type": "0", "size": 0}}}
				} else {
					t.Errorf("unexpected provider endpoint %s", r.URL.Path)
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
			root, target := quark.QualifyID(quark.RouteTV, tv.AccountID, "T"), quark.QualifyID(quark.RouteTV, tv.AccountID, "I")
			proof := quark.SpaceProof{Version: 1, Generation: 1, TVAccount: tv.AccountID, AgentAccount: agent.AccountID, TVRoot: root, AgentRoot: quark.QualifyID(quark.RouteAgent, agent.AccountID, "A"), Path: "/library", ContentSHA256: strings.Repeat("a", 64), VerifiedAt: time.Now()}
			raw, _ := json.Marshal(quark.Credentials{Version: 1, Revision: 1, TV: tv, Agent: agent, Proofs: []quark.SpaceProof{proof}})
			credential := nodeprotocol.StorageCredential{ProviderType: cloud.ProviderQuark, RootID: root, Quark: raw}
			node := newQuarkNodeTestAgent(t)
			base, err := node.newQuarkUploadDriver(context.Background(), "server-1", nodeprotocol.StorageActionRequest{OperationKey: "operation-1"}, credential, quarkTestGrantState(t, node, "operation-1", credential))
			if err != nil {
				t.Fatal(err)
			}
			driver := base.(*nodeQuarkUploadDriver)
			changed.Store(true)
			relative := "/nested/intake"
			if scenario == "missing_path" {
				relative = ""
			}
			if scenario == "account_changed" {
				target = quark.QualifyID(quark.RouteTV, "different-account", "I")
			}
			err = configureQuarkDirectoryLocator(driver.Client, credential, relative, target)
			if err == nil {
				var item cloud.Item
				item, err = driver.ResolveManagementItem(context.Background(), target)
				if err == nil && item.ID != quark.QualifyID(quark.RouteAgent, agent.AccountID, "AI") {
					t.Fatal("wrong management identity")
				}
			}
			if scenario == "cold" && err != nil {
				t.Fatal(err)
			}
			if scenario != "cold" && err == nil {
				t.Fatal("drift or missing locator accepted")
			}
			if calls.Load() > 20 {
				t.Fatalf("locator unexpectedly expanded to %d requests", calls.Load())
			}
		})
	}
}
