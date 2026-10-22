package nodeagent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
)

// The real Node SQLite/sealed-grant and native adapter request path is exercised
// against a pinned local TLS provider, never a live account. A closure-only grant
// renewal would keep using old access tokens and fail this test.
func TestQuarkNodeGrantContinuationUsesNewNativeCredentials(t *testing.T) {
	a, state, bundle := quarkGrantFixture(t)
	var renewed atomic.Bool
	var tvNew, agentNew, allCalls atomic.Int64
	fixture := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		allCalls.Add(1)
		var data any
		switch r.URL.Path {
		case "/file":
			want := "old-tv"
			if renewed.Load() {
				want = "new-tv"
				tvNew.Add(1)
			}
			if r.URL.Query().Get("access_token") != want || r.URL.Query().Get("method") != "list" {
				t.Error("TV request used stale access or escaped route")
				http.Error(w, "rejected", 401)
				return
			}
			items := []any{}
			if r.URL.Query().Get("parent_fid") == "0" {
				items = append(items, map[string]any{"fid": "T", "filename": "library", "isdir": 1, "size": 0})
			}
			data = map[string]any{"total_count": len(items), "files": items}
		case "/open/v1/file/list":
			want := "old-agent"
			if renewed.Load() {
				want = "new-agent"
				agentNew.Add(1)
			}
			if r.URL.Query().Get("access_token") != want || r.Header.Get("Authorization") != "Bearer "+want {
				t.Error("Agent request used stale access")
				http.Error(w, "rejected", 401)
				return
			}
			data = map[string]any{"last_page": true, "file_list": []any{map[string]any{"fid": "A", "parent_fid": "0", "filename": "library", "file_type": "0", "size": 0}}}
		default:
			t.Error("unexpected provider/refresh endpoint")
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
	base, err := a.newQuarkUploadDriver(context.Background(), state.serverID, nodeprotocol.StorageActionRequest{OperationKey: state.grant.OperationKey}, state.credential, state)
	if err != nil {
		t.Fatal(err)
	}
	bundle.Revision++
	bundle.TV.Generation++
	bundle.Agent.Generation++
	bundle.TV.AccessToken = "new-tv"
	bundle.Agent.AccessToken = "new-agent"
	saveNextQuarkGrant(t, state, bundle, nil)
	// The late pre-dispatch guard must never substitute credentials itself.
	if err = state.validateCurrent(context.Background()); err != nil || state.grant.GrantID != "grant-old" {
		t.Fatal("late guard adopted an unsigned credential change", err)
	}
	renewed.Store(true)
	if _, err = base.List(context.Background(), state.credential.RootID, cloud.PageRequest{Limit: 100}); err != nil {
		t.Fatal(err)
	}
	if tvNew.Load() == 0 || agentNew.Load() == 0 {
		t.Fatal("both native routes were not tested after renewal")
	}
	before := allCalls.Load()
	if _, err = a.store.Cancel(context.Background(), state.serverID, state.grant.OperationKey, a.now()); err != nil {
		t.Fatal(err)
	}
	if _, err = base.List(context.Background(), state.credential.RootID, cloud.PageRequest{Limit: 100}); err == nil || allCalls.Load() != before {
		t.Fatal("cancelled operation reached provider")
	}
}
