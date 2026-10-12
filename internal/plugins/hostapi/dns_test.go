package hostapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/plugins/contract"
	pluginruntime "github.com/yuanjing-hash/OhMyCine-Server/internal/plugins/runtime"
	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/time/rate"
)

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

func TestFakeIPDNSFallbackOnlyRepairsRecognizedSystemAnswers(t *testing.T) {
	for _, selection := range []struct {
		name        string
		addresses   []net.IPAddr
		systemError error
		host        string
		allowed     bool
		queries     int32
	}{
		{name: "fake-dual", addresses: []net.IPAddr{{IP: net.ParseIP("198.18.0.149")}, {IP: net.ParseIP("fdfe:dcba:9876::8d")}}, allowed: true, queries: 2},
		{name: "fake-v6", addresses: []net.IPAddr{{IP: net.ParseIP("fdfe:dcba:9876::8d")}}, allowed: true, queries: 2},
		{name: "mixed-public-fake", addresses: []net.IPAddr{{IP: net.ParseIP("203.0.113.20")}, {IP: net.ParseIP("198.19.0.1")}}, allowed: true, queries: 2},
		{name: "public", addresses: []net.IPAddr{{IP: net.ParseIP("203.0.113.20")}}, allowed: true},
		{name: "unknown-private", addresses: []net.IPAddr{{IP: net.ParseIP("10.0.0.1")}}},
		{name: "mixed-fake-private", addresses: []net.IPAddr{{IP: net.ParseIP("198.18.0.149")}, {IP: net.ParseIP("10.0.0.1")}}},
		{name: "mixed-fake-unknown-v6", addresses: []net.IPAddr{{IP: net.ParseIP("198.18.0.149")}, {IP: net.ParseIP("fdfe:dcba:9876:1::1")}}},
		{name: "nil", addresses: []net.IPAddr{{}}},
		{name: "empty-ip", addresses: []net.IPAddr{{IP: net.IP{}}}},
		{name: "malformed-ip", addresses: []net.IPAddr{{IP: net.IP{1, 2, 3, 4, 5}}}},
		{name: "zone", addresses: []net.IPAddr{{IP: net.ParseIP("198.18.0.149"), Zone: "eth0"}}},
		{name: "dns-failure", systemError: errors.New("DNS unavailable")},
		{name: "literal-fake", host: "198.18.0.149"},
		{name: "literal-private", host: "127.0.0.1"},
		{name: "literal-public", host: "203.0.113.10", allowed: true},
		{name: "local-name", host: "host.local", addresses: []net.IPAddr{{IP: net.ParseIP("198.18.0.149")}}},
	} {
		t.Run(selection.name, func(t *testing.T) {
			var queries atomic.Int32
			resolver := newFakeIPResolver(func(context.Context, string) ([]net.IPAddr, error) { return selection.addresses, selection.systemError })
			resolver.client = fixtureDoHClient(t, &queries, nil)
			hostname := selection.host
			if hostname == "" {
				hostname = "api.example.test"
			}
			addresses, err := resolver.LookupIPAddr(context.Background(), hostname)
			if (err == nil) != selection.allowed || queries.Load() != selection.queries {
				t.Fatalf("allowed=%v code=%s queries=%d", err == nil, ErrorCode(err), queries.Load())
			}
			if err == nil && (len(addresses) == 0 || requirePublicAddresses(addresses) != nil) {
				t.Fatal("unsafe or empty address escaped resolver")
			}
		})
	}
	var queries atomic.Int32
	host := New(nil, nil, zerolog.Nop(), WithResolver(fakeSystemDNS))
	if host.dnsFallback != nil || host.requirePublicHost(context.Background(), "api.example.test") == nil || queries.Load() != 0 {
		t.Fatal("custom resolver gained implicit external fallback")
	}
}

func TestFakeIPDNSCacheTTLHTTPAgeCopiesBoundsAndCurrentSystemPolicy(t *testing.T) {
	var queries atomic.Int32
	now := time.Now()
	unsafeSystem := false
	resolver := newFakeIPResolver(func(ctx context.Context, name string) ([]net.IPAddr, error) {
		if unsafeSystem {
			return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
		}
		return fakeSystemDNS(ctx, name)
	})
	resolver.now = func() time.Time { return now }
	resolver.limit = rate.NewLimiter(rate.Inf, 0)
	resolver.client = fixtureDoHClient(t, &queries, func(message *dnsmessage.Message, response *http.Response) {
		for index := range message.Answers {
			message.Answers[index].Header.TTL = 30
		}
		response.Header.Set("Age", "25")
	})
	first, err := resolver.LookupIPAddr(context.Background(), "API.example.test.")
	if err != nil {
		t.Fatal(err)
	}
	first[0].IP[0] = 127
	second, err := resolver.LookupIPAddr(context.Background(), "api.example.test")
	if err != nil || second[0].IP[0] != 203 || queries.Load() != 2 {
		t.Fatal("cache was aliased or ignored canonical identity")
	}
	unsafeSystem = true
	if _, err := resolver.LookupIPAddr(context.Background(), "api.example.test"); err == nil || queries.Load() != 2 {
		t.Fatal("cache bypassed fresh system policy")
	}
	unsafeSystem = false
	now = now.Add(6 * time.Second)
	if _, err := resolver.LookupIPAddr(context.Background(), "api.example.test"); err != nil || queries.Load() != 4 {
		t.Fatal("HTTP Age was not deducted from DNS TTL")
	}
	resolver.client = fixtureDoHClient(t, &queries, nil)
	for index := 0; index <= maxFakeIPDNSCache; index++ {
		if _, err := resolver.LookupIPAddr(context.Background(), fmt.Sprintf("h%d.example.test", index)); err != nil {
			t.Fatal(err)
		}
	}
	if len(resolver.cache) > maxFakeIPDNSCache {
		t.Fatal("DNS cache is unbounded")
	}
	for _, observation := range resolver.cache {
		if observation.expires.Sub(now) > maxFakeIPDNSTTL {
			t.Fatal("provider TTL exceeded fixed maximum")
		}
	}
	resolver.client = fixtureDoHClient(t, &queries, func(message *dnsmessage.Message, response *http.Response) {
		for index := range message.Answers {
			message.Answers[index].Header.TTL = 0
		}
	})
	before := queries.Load()
	for index := 0; index < 2; index++ {
		if _, err := resolver.LookupIPAddr(context.Background(), "zero.example.test"); err != nil {
			t.Fatal(err)
		}
	}
	if queries.Load() != before+4 {
		t.Fatal("zero TTL was cached")
	}
}

func TestDoHAnswerBindingCNAMEAndAddressSafety(t *testing.T) {
	question := dnsmessage.Question{Name: dnsmessage.MustNewName("api.example.test."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}
	for _, selection := range []struct {
		name   string
		change func(*dnsmessage.Message)
	}{
		{"id", func(m *dnsmessage.Message) { m.ID++ }},
		{"not-response", func(m *dnsmessage.Message) { m.Response = false }},
		{"opcode", func(m *dnsmessage.Message) { m.OpCode = 1 }},
		{"truncated", func(m *dnsmessage.Message) { m.Truncated = true }},
		{"rcode", func(m *dnsmessage.Message) { m.RCode = dnsmessage.RCodeServerFailure }},
		{"question-name", func(m *dnsmessage.Message) { m.Questions[0].Name = dnsmessage.MustNewName("evil.example.test.") }},
		{"question-type", func(m *dnsmessage.Message) { m.Questions[0].Type = dnsmessage.TypeAAAA }},
		{"question-class", func(m *dnsmessage.Message) { m.Questions[0].Class = dnsmessage.ClassCHAOS }},
		{"question-count", func(m *dnsmessage.Message) { m.Questions = append(m.Questions, m.Questions[0]) }},
		{"owner", func(m *dnsmessage.Message) { m.Answers[0].Header.Name = dnsmessage.MustNewName("evil.example.test.") }},
		{"class", func(m *dnsmessage.Message) { m.Answers[0].Header.Class = dnsmessage.ClassCHAOS }},
		{"private", func(m *dnsmessage.Message) { m.Answers[0].Body = &dnsmessage.AResource{A: [4]byte{127, 0, 0, 1}} }},
		{"synthetic", func(m *dnsmessage.Message) { m.Answers[0].Body = &dnsmessage.AResource{A: [4]byte{198, 18, 0, 3}} }},
		{"cname-loop", func(m *dnsmessage.Message) {
			m.Answers = []dnsmessage.Resource{{Header: dnsmessage.ResourceHeader{Name: question.Name, Type: dnsmessage.TypeCNAME, Class: dnsmessage.ClassINET, TTL: 20}, Body: &dnsmessage.CNAMEResource{CNAME: question.Name}}}
		}},
	} {
		t.Run(selection.name, func(t *testing.T) {
			message := dnsResponse(question, 42, 60)
			selection.change(&message)
			wire, err := message.Pack()
			if err != nil {
				t.Fatal(err)
			}
			if addresses, _, err := parseDoHAnswer(wire, 42, question); err == nil || len(addresses) > 0 {
				t.Fatal("invalid DNS response accepted")
			}
		})
	}
	message := dnsResponse(question, 42, 60)
	alias := dnsmessage.MustNewName("cdn.example.test.")
	message.Answers[0].Header.Name = alias
	message.Answers = append(message.Answers, dnsmessage.Resource{Header: dnsmessage.ResourceHeader{Name: question.Name, Type: dnsmessage.TypeCNAME, Class: dnsmessage.ClassINET, TTL: 20}, Body: &dnsmessage.CNAMEResource{CNAME: alias}})
	wire, _ := message.Pack()
	addresses, ttl, err := parseDoHAnswer(wire, 42, question)
	if err != nil || len(addresses) != 1 || ttl != 20*time.Second {
		t.Fatal("bound CNAME terminal/TTL rejected")
	}
	message.Answers[0].Header.Name = question.Name
	wire, _ = message.Pack()
	if _, _, err := parseDoHAnswer(wire, 42, question); err == nil {
		t.Fatal("address at nonterminal CNAME owner accepted")
	}
	for _, wire := range [][]byte{[]byte("bad"), make([]byte, maxDoHBodyBytes+1)} {
		if _, _, err := parseDoHAnswer(wire, 42, question); err == nil {
			t.Fatal("invalid wire size accepted")
		}
	}
	message = dnsResponse(question, 42, 60)
	wire, _ = message.Pack()
	binary.BigEndian.PutUint16(wire[6:8], 65535)
	if _, _, err := parseDoHAnswer(wire, 42, question); err == nil {
		t.Fatal("unbounded record count accepted")
	}
	message = dnsResponse(question, 42, 60)
	terminal := question.Name
	for index := 0; index <= maxDoHCNAMEs; index++ {
		next := dnsmessage.MustNewName(fmt.Sprintf("c%d.example.test.", index))
		message.Answers = append(message.Answers, dnsmessage.Resource{Header: dnsmessage.ResourceHeader{Name: terminal, Type: dnsmessage.TypeCNAME, Class: dnsmessage.ClassINET, TTL: 60}, Body: &dnsmessage.CNAMEResource{CNAME: next}})
		terminal = next
	}
	message.Answers[0].Header.Name = terminal
	wire, _ = message.Pack()
	if _, _, err := parseDoHAnswer(wire, 42, question); err == nil {
		t.Fatal("unbounded CNAME chain accepted")
	}
	message = dnsResponse(question, 42, 60)
	extra := dnsmessage.Resource{Header: dnsmessage.ResourceHeader{Name: question.Name, Type: dnsmessage.TypeTXT, Class: dnsmessage.ClassINET, TTL: 60}, Body: &dnsmessage.TXTResource{TXT: []string{"fixture"}}}
	for index := 0; index < 32; index++ {
		message.Authorities = append(message.Authorities, extra)
		message.Additionals = append(message.Additionals, extra)
	}
	wire, err = message.Pack()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := parseDoHAnswer(wire, 42, question); err == nil {
		t.Fatal("combined RR count exceeded 64")
	}
}

func TestFakeIPDNSDoHFailsClosedAndNeverCachesFailures(t *testing.T) {
	for _, selection := range []struct {
		name   string
		change func(*dnsmessage.Message, *http.Response)
	}{
		{"status", func(_ *dnsmessage.Message, r *http.Response) { r.StatusCode = 503 }},
		{"mime", func(_ *dnsmessage.Message, r *http.Response) { r.Header.Set("Content-Type", "text/html") }},
		{"size", func(_ *dnsmessage.Message, r *http.Response) { r.ContentLength = maxDoHBodyBytes + 1 }},
		{"age", func(_ *dnsmessage.Message, r *http.Response) { r.Header.Set("Age", "invalid") }},
		{"private-AAAA", func(m *dnsmessage.Message, _ *http.Response) {
			if m.Questions[0].Type == dnsmessage.TypeAAAA {
				m.Answers = []dnsmessage.Resource{{Header: dnsmessage.ResourceHeader{Name: m.Questions[0].Name, Type: dnsmessage.TypeAAAA, Class: dnsmessage.ClassINET, TTL: 60}, Body: &dnsmessage.AAAAResource{AAAA: [16]byte{0xfd, 0xfe}}}}
			}
		}},
	} {
		t.Run(selection.name, func(t *testing.T) {
			var queries atomic.Int32
			resolver := newFakeIPResolver(fakeSystemDNS)
			resolver.client = fixtureDoHClient(t, &queries, selection.change)
			for index := 0; index < 2; index++ {
				if addresses, err := resolver.LookupIPAddr(context.Background(), "api.example.test"); err == nil || len(addresses) > 0 {
					t.Fatal("bad DoH response escaped")
				}
			}
			if len(resolver.cache) != 0 || len(resolver.flights) != 0 || len(resolver.slots) != 0 || queries.Load() < 2 {
				t.Fatal("failed DNS operation leaked/cached state")
			}
		})
	}
	resolver := newFakeIPResolver(fakeSystemDNS)
	resolver.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/dns-message"}}, Body: io.NopCloser(bytes.NewReader(make([]byte, maxDoHBodyBytes+1))), Request: request}, nil
	})}
	if _, err := resolver.LookupIPAddr(context.Background(), "large.example.test"); ErrorCode(err) != "plugin_http_dns_fallback_invalid" {
		t.Fatal("unannounced oversized streaming body accepted")
	}
}

func TestFakeIPDNSCancellationCoalescingAndAdmissionBounds(t *testing.T) {
	resolver := newFakeIPResolver(fakeSystemDNS)
	entered := make(chan struct{}, 1)
	resolver.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		entered <- struct{}{}
		<-request.Context().Done()
		return nil, request.Context().Err()
	})}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err := resolver.LookupIPAddr(ctx, "api.example.test"); err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("cancellation was lost")
	}
	if time.Since(started) > time.Second || len(resolver.cache) != 0 || len(resolver.flights) != 0 || len(resolver.slots) != 0 {
		t.Fatal("canceled fallback retained state or blocked")
	}
	for index := 0; index < cap(resolver.slots); index++ {
		resolver.slots <- struct{}{}
	}
	if _, err := resolver.LookupIPAddr(context.Background(), "other.example.test"); ErrorCode(err) != "plugin_http_dns_fallback_busy" {
		t.Fatal("bounded fallback admission exceeded")
	}
	for len(resolver.slots) > 0 {
		<-resolver.slots
	}
	resolver.limit = rate.NewLimiter(rate.Every(time.Hour), 1)
	_ = resolver.limit.Allow()
	if _, err := resolver.LookupIPAddr(context.Background(), "rate.example.test"); ErrorCode(err) != "plugin_http_dns_fallback_busy" {
		t.Fatal("bounded rate limit exceeded")
	}

	resolver = newFakeIPResolver(fakeSystemDNS)
	var queries atomic.Int32
	resolver.client = fixtureDoHClient(t, &queries, nil)
	var workers sync.WaitGroup
	for index := 0; index < 20; index++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			addresses, err := resolver.LookupIPAddr(context.Background(), "api.example.test")
			if err != nil || len(addresses) != 1 {
				t.Error("concurrent fallback failed")
			}
		}()
	}
	workers.Wait()
	if queries.Load() != 2 {
		t.Fatalf("same-host fallback did not coalesce: %d", queries.Load())
	}
}

func TestDoHTransportPinsTLSBootstrapAndRejectsRedirects(t *testing.T) {
	client := newDoHClient()
	transport := client.Transport.(*http.Transport)
	if transport.Proxy != nil || transport.TLSClientConfig.InsecureSkipVerify || transport.TLSClientConfig.ServerName != "dns.alidns.com" || client.Timeout != fakeIPDNSTimeout {
		t.Fatal("DoH TLS/proxy/timeout policy weakened")
	}
	if _, err := transport.DialContext(context.Background(), "tcp", "127.0.0.1:443"); err == nil {
		t.Fatal("arbitrary bootstrap accepted")
	}
	if err := client.CheckRedirect(&http.Request{}, nil); err == nil {
		t.Fatal("DoH redirect accepted")
	}
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer server.Close()
	t.Cleanup(transport.CloseIdleConnections)
	local := strings.TrimPrefix(server.URL, "https://")
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, local)
	}
	if response, err := client.Post(doHEndpoint, "application/dns-message", bytes.NewReader(nil)); err == nil {
		if response != nil {
			_ = response.Body.Close()
		}
		t.Fatal("untrusted TLS certificate accepted")
	}
	if requests.Load() != 0 {
		t.Fatal("bad TLS reached resolver application")
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
	// Preserve production constructor wrapping; only its system source and DoH
	// HTTP are controlled. Neither controlled client can access the network.
	host := New(fixture.db, fixture.credentials, zerolog.Nop(), WithHTTPClient(client), func(host *Host) { host.resolve = fakeSystemDNS })
	host.dnsFallback.client = fixtureDoHClient(t, &queries, nil)
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
	host.dnsFallback.system = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("10.0.0.1")}}, nil
	}
	if err := guarded.CheckRedirect(next, []*http.Request{{URL: current}}); ErrorCode(err) != "plugin_http_private_address_denied" || queries.Load() != before {
		t.Fatal("redirect cache bypassed new private DNS")
	}
	if _, err := host.dialPublicContext(context.Background(), "tcp", "cdn.example.test:443"); ErrorCode(err) != "plugin_http_private_address_denied" {
		t.Fatal("dial-time policy bypassed cached DNS")
	}
	host.dnsFallback.system = fakeSystemDNS
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
	api.dnsFallback.client = fixtureDoHClient(t, &queries, nil)
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

// Opt-in network acceptance: only a public hostname is sent to the fixed
// resolver. No account, media, credentials or proxy configuration is involved.
func TestFakeIPDNSLivePinnedOfficialDoH(t *testing.T) {
	if os.Getenv("OMC_PLUGIN_DNS_LIVE") != "1" {
		t.Skip("set OMC_PLUGIN_DNS_LIVE=1 for anonymous official resolver acceptance")
	}
	resolver := newFakeIPResolver(fakeSystemDNS)
	started := time.Now()
	addresses, err := resolver.LookupIPAddr(context.Background(), "pianku.api.mgtv.com")
	if err != nil || len(addresses) == 0 || requirePublicAddresses(addresses) != nil {
		t.Fatalf("official pinned DoH failed closed: code=%s", ErrorCode(err))
	}
	t.Logf("official verified-TLS fixed-bootstrap DoH: public_addresses=%d elapsed=%s; no provider HTTP or credentials", len(addresses), time.Since(started))
}
