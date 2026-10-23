package hostapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/plugins/contract"
	pluginruntime "github.com/yuanjing-hash/OhMyCine-Server/internal/plugins/runtime"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/netresolve"
	"golang.org/x/net/dns/dnsmessage"
)

const doHEndpoint = "https://dns.alidns.com/dns-query"

func fakeSystemDNS(context.Context, string) ([]net.IPAddr, error) {
	return []net.IPAddr{{IP: net.ParseIP("198.18.0.149")}, {IP: net.ParseIP("fdfe:dcba:9876::8d")}}, nil
}

func dnsResponse(question dnsmessage.Question, id uint16, ttl uint32) dnsmessage.Message {
	message := dnsmessage.Message{Header: dnsmessage.Header{ID: id, Response: true, RecursionAvailable: true}, Questions: []dnsmessage.Question{question}}
	if question.Type == dnsmessage.TypeA {
		message.Answers = []dnsmessage.Resource{{Header: dnsmessage.ResourceHeader{Name: question.Name, Type: question.Type, Class: dnsmessage.ClassINET, TTL: ttl}, Body: &dnsmessage.AResource{A: [4]byte{203, 0, 113, 10}}}}
	}
	return message
}

func fixtureDoHClient(t *testing.T, count *atomic.Int32, change func(*dnsmessage.Message, *http.Response)) *http.Client {
	t.Helper()
	return &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		count.Add(1)
		if request.Method != http.MethodPost || request.URL.String() != doHEndpoint || request.Header.Get("Accept") != "application/dns-message" || request.Header.Get("Content-Type") != "application/dns-message" || request.Header.Get("Cookie") != "" || request.Header.Get("Authorization") != "" {
			t.Error("DNS query escaped fixed origin, wire protocol or credential boundary")
		}
		wire, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		var query dnsmessage.Message
		if err := query.Unpack(wire); err != nil || len(query.Questions) != 1 || query.Response {
			return nil, errors.New("invalid fixture query")
		}
		message := dnsResponse(query.Questions[0], query.ID, 120)
		response := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/dns-message"}}, Request: request}
		if change != nil {
			change(&message, response)
		}
		wire, err = message.Pack()
		if err != nil {
			return nil, err
		}
		response.Body = io.NopCloser(bytes.NewReader(wire))
		return response, nil
	})}
}

func TestHostInjectedResolverDoesNotGainFakeIPFallback(t *testing.T) {
	var queries atomic.Int32
	host := New(nil, nil, zerolog.Nop(), WithResolver(fakeSystemDNS))
	if host.dnsFallback != nil || host.requirePublicHost(context.Background(), "api.example.test") == nil || queries.Load() != 0 {
		t.Fatal("custom resolver gained implicit external fallback")
	}
}

func TestHostFakeIPDNSAllConsumersAndRedirectsRecheckCurrentPolicy(t *testing.T) {
	fixture := newHostFixture(t, []contract.Permission{{Kind: contract.PermissionNetworkHTTP, Domains: []string{"api.example.test", "cdn.example.test"}}, {Kind: contract.PermissionDownloadPlan}, {Kind: contract.PermissionCredentialUse, Scopes: []string{"site.session"}}})
	var queries atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if strings.HasSuffix(request.URL.Path, ".jpg") {
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"image/jpeg"}}, Body: io.NopCloser(bytes.NewReader([]byte{0xff, 0xd8, 0xff, 0xd9})), Request: request}, nil
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/octet-stream"}}, Body: io.NopCloser(strings.NewReader("media")), Request: request}, nil
	})}
	// Preserve production constructor wrapping; only the shared resolver's
	// controlled system/DoH boundaries are replaced with offline fixtures.
	unsafeSystem := false
	system := func(ctx context.Context, name string) ([]net.IPAddr, error) {
		if unsafeSystem {
			return []net.IPAddr{{IP: net.ParseIP("10.0.0.1")}}, nil
		}
		return fakeSystemDNS(ctx, name)
	}
	host := New(fixture.db, fixture.credentials, zerolog.Nop(), WithHTTPClient(client), func(host *Host) { host.resolve = system })
	host.dnsFallback.resolver = netresolve.New(netresolve.Config{System: system, HTTPClient: fixtureDoHClient(t, &queries, nil)})
	payload, _ := json.Marshal(httpRequest{Method: http.MethodGet, URL: "https://api.example.test/feed"})
	if _, err := host.Call(context.Background(), fixture.pluginID, OperationHTTP, payload); err != nil {
		t.Fatal(err)
	}
	browserCalled := false
	host.SetBrowserRequest(func(_ context.Context, _, _, _ string, request *http.Request) (*http.Response, bool, error) {
		browserCalled = true
		if request.URL.Hostname() != "api.example.test" {
			t.Fatal("browser dispatch lost original hostname after DNS repair")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("{}")), Request: request}, true, nil
	})
	payload, _ = json.Marshal(httpRequest{Method: http.MethodGet, URL: "https://api.example.test/browser", ConnectionID: fixture.connection.ID, Credential: "site.session"})
	if _, err := host.Call(context.Background(), fixture.pluginID, OperationHTTP, payload); err != nil || !browserCalled {
		t.Fatal("default repaired DNS did not reach managed browser dispatch")
	}
	register, _ := json.Marshal(assetRegisterRequest{ConnectionID: fixture.connection.ID, URL: "https://cdn.example.test/movie.ts", TTLSeconds: 60})
	output, err := host.Call(context.Background(), fixture.pluginID, OperationAssetRegister, register)
	if err != nil {
		t.Fatal(err)
	}
	reference := assetReference(t, output)
	stream, err := host.OpenAsset(context.Background(), reference, http.MethodGet, "")
	if err != nil {
		t.Fatal(err)
	}
	_ = stream.Body.Close()
	artwork, err := host.RegisterArtwork(context.Background(), fixture.pluginID, fixture.connection.ID, "https://cdn.example.test/poster.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := host.OpenArtwork(context.Background(), artwork); err != nil {
		t.Fatal(err)
	}
	if _, err := host.OfflineAssetTransport(context.Background(), fixture.pluginID, fixture.connection.ID, reference); err != nil {
		t.Fatal(err)
	}
	if _, _, err := host.ReadOfflineControl(context.Background(), fixture.pluginID, fixture.connection.ID, reference, 1024); err != nil {
		t.Fatal(err)
	}
	before := queries.Load()
	if before != 4 {
		t.Fatalf("public fallback did not share bounded per-name DNS observations: %d", before)
	}
	current, _ := url.Parse("https://api.example.test/start")
	next, _ := http.NewRequest(http.MethodGet, "https://cdn.example.test/end", nil)
	next.Header.Set("Cookie", "fixture-secret")
	next.Header.Set("Authorization", "fixture-secret")
	guarded := host.clientForPermissions([]contract.Permission{{Kind: contract.PermissionNetworkHTTP, Domains: []string{"api.example.test", "cdn.example.test"}}}, false)
	if err := guarded.CheckRedirect(next, []*http.Request{{URL: current}}); err != nil || next.Header.Get("Cookie") != "" || next.Header.Get("Authorization") != "" {
		t.Fatal("safe redirect lost validation/credential stripping")
	}
	unsafeSystem = true
	if err := guarded.CheckRedirect(next, []*http.Request{{URL: current}}); ErrorCode(err) != "plugin_http_private_address_denied" || queries.Load() != before {
		t.Fatal("redirect cache bypassed new private DNS")
	}
	if _, err := host.dialPublicContext(context.Background(), "tcp", "cdn.example.test:443"); ErrorCode(err) != "plugin_http_private_address_denied" {
		t.Fatal("dial-time policy bypassed cached DNS")
	}
	unsafeSystem = false
	outside, _ := http.NewRequest(http.MethodGet, "https://evil.example.test/end", nil)
	if err := guarded.CheckRedirect(outside, []*http.Request{{URL: current}}); err == nil || queries.Load() != before {
		t.Fatal("ungranted redirect reached DNS fallback")
	}
	if _, err := host.RegisterArtwork(context.Background(), fixture.pluginID, fixture.connection.ID, "https://evil.example.test/poster.jpg"); err == nil || queries.Load() != before {
		t.Fatal("ungranted artwork reached DNS fallback")
	}
}

func TestPublicDialUsesOnlyValidatedIPAndPort(t *testing.T) {
	var called string
	dial := func(_ context.Context, _ string, address string) (net.Conn, error) {
		called = address
		left, right := net.Pipe()
		_ = right.Close()
		return left, nil
	}
	connection, err := dialPublicIPs(context.Background(), "tcp", "443", []net.IPAddr{{IP: net.ParseIP("203.0.113.10")}}, dial)
	if err != nil || called != "203.0.113.10:443" {
		t.Fatal("dial used hostname/system resolver after validation")
	}
	_ = connection.Close()
	called = ""
	if _, err := dialPublicIPs(context.Background(), "tcp", "443", []net.IPAddr{{IP: net.ParseIP("198.18.0.149")}}, dial); err == nil || called != "" {
		t.Fatal("synthetic IP reached dialer")
	}
}

func TestMangoWASMHostFakeIPSystemDNSUsesPublicFallback(t *testing.T) {
	entry, fixtures := os.Getenv("OMC_MGTV_WASM"), os.Getenv("OMC_MGTV_FIXTURES")
	if entry == "" || fixtures == "" {
		t.Skip("set actual Mango WASM and redacted fixture paths")
	}
	storageBytes := int64(8 << 20)
	fixture := newHostFixture(t, []contract.Permission{{Kind: contract.PermissionNetworkHTTP, Domains: []string{"pianku.api.mgtv.com"}}, {Kind: contract.PermissionPrivateStorage, MaxBytes: &storageBytes}})
	var queries atomic.Int32
	controls := 0
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		controls++
		if request.Method != http.MethodGet || request.URL.Hostname() != "pianku.api.mgtv.com" || request.URL.Path != "/rider/config/platformChannels/v1" || request.Header.Get("Cookie") != "" {
			t.Fatal("fake-IP smoke escaped public catalogue scope")
		}
		body, err := os.ReadFile(filepath.Join(fixtures, "channels.json"))
		if err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(body)), Request: request}, nil
	})}
	api := New(fixture.db, fixture.credentials, zerolog.Nop(), WithHTTPClient(client), func(host *Host) { host.resolve = fakeSystemDNS })
	api.dnsFallback.resolver = netresolve.New(netresolve.Config{System: fakeSystemDNS, HTTPClient: fixtureDoHClient(t, &queries, nil)})
	runtime := pluginruntime.NewHost(context.Background())
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	runtime.SetCapabilityHost(api)
	if err := runtime.Start(context.Background(), fixture.pluginID, entry, 1); err != nil {
		t.Fatal(err)
	}
	input, _ := json.Marshal(map[string]string{"connectionId": fixture.connection.ID})
	output, err := runtime.Invoke(context.Background(), fixture.pluginID, "site.navigation", input)
	var result struct {
		Version int    `json:"version"`
		Mode    string `json:"mode"`
		Nodes   []struct {
			ID          string `json:"id"`
			Title       string `json:"title"`
			Kind        string `json:"kind"`
			NodeKey     string `json:"nodeKey,omitempty"`
			RouteKey    string `json:"routeKey,omitempty"`
			HasChildren bool   `json:"hasChildren,omitempty"`
		} `json:"nodes"`
	}
	if err != nil || strictJSON(output, &result) != nil || result.Version != 2 || result.Mode != "hierarchical" || len(result.Nodes) != 8 || controls != 1 || queries.Load() != 2 {
		t.Fatalf("actual WASM catalogue did not survive fake DNS: code=%s controls=%d dnsqueries=%d", ErrorCode(err), controls, queries.Load())
	}
	seen := map[string]bool{}
	var expected struct {
		Data []struct {
			ChannelID   string `json:"channelId"`
			ChannelName string `json:"channelName"`
			Status      int    `json:"status"`
			Rank        int    `json:"rank"`
		} `json:"data"`
	}
	fixtureBytes, err := os.ReadFile(filepath.Join(fixtures, "channels.json"))
	if err != nil || json.Unmarshal(fixtureBytes, &expected) != nil {
		t.Fatal("official catalogue fixture unavailable")
	}
	active := expected.Data[:0]
	for _, channel := range expected.Data {
		if channel.Status == 1 {
			active = append(active, channel)
		}
	}
	sort.SliceStable(active, func(left, right int) bool { return active[left].Rank < active[right].Rank })
	if len(active) != 7 {
		t.Fatal("official active category fixture changed")
	}
	for index, node := range result.Nodes {
		if node.ID == "" || node.Title == "" || seen[node.ID] {
			t.Fatal("actual WASM returned invalid/repeated node identity")
		}
		seen[node.ID] = true
		if index < 7 && (node.Kind != "branch" || !node.HasChildren || !strings.HasPrefix(node.NodeKey, "category:") || node.RouteKey != "") {
			t.Fatal("official category topology changed")
		}
		if index < 7 && (node.ID != "channel-"+active[index].ChannelID || node.NodeKey != "category:"+active[index].ChannelID || node.Title != active[index].ChannelName) {
			t.Fatal("actual WASM category identity/title/order differs from official fixture")
		}
		if index == 7 && (node.Kind != "search" || node.RouteKey != "search" || node.NodeKey != "") {
			t.Fatal("search node contract changed")
		}
	}
}
