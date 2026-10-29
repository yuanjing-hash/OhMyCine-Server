package netresolve

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/time/rate"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func newFakeIPResolver(system LookupFunc) *FakeIPResolver                 { return New(Config{System: system}) }

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
			if err == nil && (len(addresses) == 0 || RequirePublicAddresses(addresses) != nil) {
				t.Fatal("unsafe or empty address escaped resolver")
			}
		})
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
	if _, err := resolver.LookupIPAddr(context.Background(), "large.example.test"); ErrorCode(err) != "dns_fallback_invalid" {
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
	if _, err := resolver.LookupIPAddr(context.Background(), "other.example.test"); ErrorCode(err) != "dns_fallback_busy" {
		t.Fatal("bounded fallback admission exceeded")
	}
	for len(resolver.slots) > 0 {
		<-resolver.slots
	}
	resolver.limit = rate.NewLimiter(rate.Every(time.Hour), 1)
	_ = resolver.limit.Allow()
	if _, err := resolver.LookupIPAddr(context.Background(), "rate.example.test"); ErrorCode(err) != "dns_fallback_busy" {
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

func TestDoHTransportDoesNotDependOnGlobalTransport(t *testing.T) {
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	var globalRequests atomic.Int32
	for _, global := range []struct {
		name      string
		transport http.RoundTripper
	}{
		{name: "custom", transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			globalRequests.Add(1)
			return nil, errors.New("global transport must not receive DoH requests")
		})},
		{name: "nil"},
		{name: "modified-default", transport: &http.Transport{
			Proxy:           http.ProxyFromEnvironment,
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			DialTLSContext: func(context.Context, string, string) (net.Conn, error) {
				globalRequests.Add(1)
				return nil, errors.New("global TLS dialer must not bypass bootstrap")
			},
		}},
	} {
		t.Run(global.name, func(t *testing.T) {
			http.DefaultTransport = global.transport
			// Use the public construction path that is also called eagerly by
			// resource inspection. No DNS query or network I/O is needed.
			resolver := New(Config{System: fakeSystemDNS})
			client := resolver.client
			transport, ok := client.Transport.(*http.Transport)
			if !ok || transport == global.transport || transport.Proxy != nil || transport.DialTLSContext != nil || transport.DialTLS != nil || transport.TLSClientConfig == nil || transport.TLSClientConfig.InsecureSkipVerify || transport.TLSClientConfig.ServerName != "dns.alidns.com" || transport.TLSClientConfig.MinVersion < tls.VersionTLS12 {
				t.Fatal("global transport changed the independent DoH TLS/bootstrap policy")
			}
			defer transport.CloseIdleConnections()
			if client.Timeout != fakeIPDNSTimeout || !transport.DisableCompression || transport.TLSHandshakeTimeout != time.Second || transport.ResponseHeaderTimeout != 2*time.Second || transport.MaxResponseHeaderBytes != 8*1024 {
				t.Fatal("DoH response or timeout bounds changed")
			}
			if _, err := transport.DialContext(context.Background(), "tcp", "127.0.0.1:443"); err == nil {
				t.Fatal("arbitrary bootstrap accepted")
			}
			if err := client.CheckRedirect(&http.Request{}, nil); err == nil {
				t.Fatal("DoH redirect accepted")
			}
		})
	}
	if globalRequests.Load() != 0 {
		t.Fatal("DoH construction invoked the global transport")
	}
}

func TestFakeIPDNSLivePinnedOfficialDoH(t *testing.T) {
	if os.Getenv("OMC_PLUGIN_DNS_LIVE") != "1" {
		t.Skip("set OMC_PLUGIN_DNS_LIVE=1 for anonymous official resolver acceptance")
	}
	resolver := newFakeIPResolver(fakeSystemDNS)
	started := time.Now()
	addresses, err := resolver.LookupIPAddr(context.Background(), "pianku.api.mgtv.com")
	if err != nil || len(addresses) == 0 || RequirePublicAddresses(addresses) != nil {
		t.Fatalf("official pinned DoH failed closed: code=%s", ErrorCode(err))
	}
	t.Logf("official verified-TLS fixed-bootstrap DoH: public_addresses=%d elapsed=%s; no provider HTTP or credentials", len(addresses), time.Since(started))
}
