package hostapi

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/binary"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/time/rate"
)

const (
	fakeIPDNSTimeout  = 3 * time.Second
	maxDoHBodyBytes   = 16 * 1024
	maxDoHRecords     = 64
	maxDoHCNAMEs      = 8
	maxFakeIPDNSCache = 256
	maxFakeIPDNSTTL   = 60 * time.Second
	doHOrigin         = "dns.alidns.com:443"
	doHEndpoint       = "https://dns.alidns.com/dns-query"
)

var fakeIPv4 = netip.MustParsePrefix("198.18.0.0/15")
var fakeIPv6 = netip.MustParsePrefix("fdfe:dcba:9876::/64")

type dnsObservation struct {
	addresses []net.IPAddr
	expires   time.Time
}

type dnsFlight struct {
	done      chan struct{}
	addresses []net.IPAddr
	err       error
}

// fakeIPResolver is only installed around the default system resolver. It
// repairs recognized synthetic DNS answers, never arbitrary private answers,
// DNS failures or explicit IPs. It contains no plugin credentials or URLs.
type fakeIPResolver struct {
	system  Resolver
	client  *http.Client
	now     func() time.Time
	mu      sync.Mutex
	cache   map[string]dnsObservation
	flights map[string]*dnsFlight
	slots   chan struct{}
	limit   *rate.Limiter
}

func newFakeIPResolver(system Resolver) *fakeIPResolver {
	return &fakeIPResolver{
		system: system, client: newDoHClient(), now: time.Now,
		cache: make(map[string]dnsObservation), flights: make(map[string]*dnsFlight),
		slots: make(chan struct{}, 8), limit: rate.NewLimiter(8, 16),
	}
}

func newDoHClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DisableCompression = true
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, ServerName: "dns.alidns.com"}
	transport.TLSHandshakeTimeout = time.Second
	transport.ResponseHeaderTimeout = 2 * time.Second
	transport.MaxResponseHeaderBytes = 8 * 1024
	transport.MaxIdleConnsPerHost = 4
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != doHOrigin || (network != "tcp" && network != "tcp4") {
			return nil, denied("plugin_http_dns_fallback_denied", nil)
		}
		// Official resolver bootstrap IPs only; never provider/CDN addresses.
		// Each attempt is short enough to try the second within the outer bound.
		dialer := net.Dialer{Timeout: time.Second, KeepAlive: 30 * time.Second}
		var last error
		for _, bootstrap := range []string{"223.5.5.5:443", "223.6.6.6:443"} {
			connection, err := dialer.DialContext(ctx, network, bootstrap)
			if err == nil {
				return connection, nil
			}
			last = err
		}
		return nil, last
	}
	return &http.Client{
		Transport: transport, Timeout: fakeIPDNSTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return denied("plugin_http_dns_fallback_redirect_denied", nil)
		},
	}
}

func isFakeIP(ip net.IP) bool {
	address, ok := netip.AddrFromSlice(ip)
	return ok && (fakeIPv4.Contains(address.Unmap()) || fakeIPv6.Contains(address))
}

func cloneDNSAddresses(addresses []net.IPAddr) []net.IPAddr {
	result := make([]net.IPAddr, len(addresses))
	for index, address := range addresses {
		result[index] = net.IPAddr{IP: append(net.IP(nil), address.IP...), Zone: address.Zone}
	}
	return result
}

func dnsHostname(name string) (string, bool) {
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	if len(name) > 253 || net.ParseIP(name) != nil || !strings.Contains(name, ".") || strings.HasSuffix(name, ".local") || strings.HasSuffix(name, ".localhost") {
		return "", false
	}
	for _, label := range strings.Split(name, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", false
		}
		for _, character := range label {
			if !(character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '-') {
				return "", false
			}
		}
	}
	return name, true
}

func (resolver *fakeIPResolver) LookupIPAddr(ctx context.Context, hostname string) ([]net.IPAddr, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// IP literals never trigger a resolver lookup or a repair.
	if ip := net.ParseIP(hostname); ip != nil {
		addresses := []net.IPAddr{{IP: ip}}
		return addresses, requirePublicAddresses(addresses)
	}
	addresses, err := resolver.system(ctx, hostname)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil || len(addresses) == 0 {
		return nil, denied("plugin_http_dns_unavailable", err)
	}
	fake := false
	for _, address := range addresses {
		if address.Zone != "" {
			return nil, denied("plugin_http_private_address_denied", nil)
		}
		if isFakeIP(address.IP) {
			fake = true
			continue
		}
		if err := requirePublicAddresses([]net.IPAddr{address}); err != nil {
			return nil, err
		}
	}
	if !fake {
		return addresses, nil
	}
	name, valid := dnsHostname(hostname)
	if !valid {
		return nil, denied("plugin_http_dns_fallback_denied", nil)
	}
	resolver.mu.Lock()
	if observation, exists := resolver.cache[name]; exists && resolver.now().Before(observation.expires) {
		resolver.mu.Unlock()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return cloneDNSAddresses(observation.addresses), nil
	}
	if flight, exists := resolver.flights[name]; exists {
		resolver.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-flight.done:
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return cloneDNSAddresses(flight.addresses), flight.err
		}
	}
	select {
	case resolver.slots <- struct{}{}:
	default:
		resolver.mu.Unlock()
		return nil, denied("plugin_http_dns_fallback_busy", nil)
	}
	flight := &dnsFlight{done: make(chan struct{})}
	resolver.flights[name] = flight
	resolver.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, fakeIPDNSTimeout)
	defer cancel()
	var expires time.Time
	if err := resolver.limit.Wait(ctx); err != nil {
		flight.err = denied("plugin_http_dns_fallback_busy", err)
	} else {
		flight.addresses, expires, flight.err = resolver.lookupDoH(ctx, name)
	}
	if ctx.Err() != nil {
		flight.addresses, flight.err = nil, ctx.Err()
	}
	resolver.mu.Lock()
	if flight.err == nil && expires.After(resolver.now()) {
		if limit := resolver.now().Add(maxFakeIPDNSTTL); expires.After(limit) {
			expires = limit
		}
		for key, observation := range resolver.cache {
			if !resolver.now().Before(observation.expires) {
				delete(resolver.cache, key)
			}
		}
		if len(resolver.cache) >= maxFakeIPDNSCache {
			var oldest string
			var expires time.Time
			for key, observation := range resolver.cache {
				if oldest == "" || observation.expires.Before(expires) {
					oldest, expires = key, observation.expires
				}
			}
			delete(resolver.cache, oldest)
		}
		resolver.cache[name] = dnsObservation{cloneDNSAddresses(flight.addresses), expires}
	}
	delete(resolver.flights, name)
	close(flight.done)
	<-resolver.slots
	resolver.mu.Unlock()
	return cloneDNSAddresses(flight.addresses), flight.err
}

func (resolver *fakeIPResolver) lookupDoH(ctx context.Context, name string) ([]net.IPAddr, time.Time, error) {
	addresses := []net.IPAddr{}
	expires := resolver.now().Add(maxFakeIPDNSTTL)
	for _, recordType := range []dnsmessage.Type{dnsmessage.TypeA, dnsmessage.TypeAAAA} {
		found, observedTTL, err := resolver.queryDoH(ctx, name, recordType)
		if err != nil {
			return nil, time.Time{}, err
		}
		if observedExpiry := resolver.now().Add(observedTTL); len(found) > 0 && observedExpiry.Before(expires) {
			expires = observedExpiry
		}
		addresses = append(addresses, found...)
	}
	if len(addresses) == 0 || len(addresses) > maxDoHRecords {
		return nil, time.Time{}, denied("plugin_http_dns_fallback_invalid", nil)
	}
	if err := requirePublicAddresses(addresses); err != nil {
		return nil, time.Time{}, err
	}
	return addresses, expires, nil
}

func (resolver *fakeIPResolver) queryDoH(ctx context.Context, name string, recordType dnsmessage.Type) ([]net.IPAddr, time.Duration, error) {
	var randomID [2]byte
	if _, err := rand.Read(randomID[:]); err != nil {
		return nil, 0, err
	}
	question := dnsmessage.Question{Name: dnsmessage.MustNewName(name + "."), Type: recordType, Class: dnsmessage.ClassINET}
	message := dnsmessage.Message{Header: dnsmessage.Header{ID: binary.BigEndian.Uint16(randomID[:]), RecursionDesired: true}, Questions: []dnsmessage.Question{question}}
	wire, err := message.Pack()
	if err != nil {
		return nil, 0, denied("plugin_http_dns_fallback_invalid", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, doHEndpoint, strings.NewReader(string(wire)))
	if err != nil {
		return nil, 0, err
	}
	request.Header.Set("Content-Type", "application/dns-message")
	request.Header.Set("Accept", "application/dns-message")
	request.Header.Set("Cache-Control", "no-cache")
	response, err := resolver.client.Do(request)
	if err != nil {
		return nil, 0, denied("plugin_http_dns_fallback_unavailable", err)
	}
	defer func() { _ = response.Body.Close() }()
	mediaType, _, parseErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if response.StatusCode != http.StatusOK || parseErr != nil || mediaType != "application/dns-message" || response.ContentLength > maxDoHBodyBytes {
		return nil, 0, denied("plugin_http_dns_fallback_invalid", nil)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxDoHBodyBytes+1))
	if err != nil || len(body) > maxDoHBodyBytes {
		return nil, 0, denied("plugin_http_dns_fallback_invalid", err)
	}
	addresses, ttl, err := parseDoHAnswer(body, message.ID, question)
	if err != nil {
		return nil, 0, err
	}
	if rawAge := response.Header.Get("Age"); rawAge != "" {
		age, err := strconv.ParseUint(rawAge, 10, 32)
		if err != nil {
			return nil, 0, denied("plugin_http_dns_fallback_invalid", err)
		}
		ttl -= time.Duration(age) * time.Second
		if ttl < 0 {
			ttl = 0
		}
	}
	return addresses, ttl, nil
}

func parseDoHAnswer(wire []byte, id uint16, question dnsmessage.Question) ([]net.IPAddr, time.Duration, error) {
	bad := func() ([]net.IPAddr, time.Duration, error) {
		return nil, 0, denied("plugin_http_dns_fallback_invalid", nil)
	}
	if len(wire) < 12 || len(wire) > maxDoHBodyBytes || binary.BigEndian.Uint16(wire[4:6]) != 1 {
		return bad()
	}
	var recordCount uint32
	for _, offset := range []int{6, 8, 10} {
		recordCount += uint32(binary.BigEndian.Uint16(wire[offset : offset+2]))
		if recordCount > maxDoHRecords {
			return bad()
		}
	}
	var message dnsmessage.Message
	if err := message.Unpack(wire); err != nil || message.ID != id || !message.Response || message.OpCode != 0 || message.Truncated || message.RCode != dnsmessage.RCodeSuccess || len(message.Questions) != 1 {
		return bad()
	}
	actual := message.Questions[0]
	if !strings.EqualFold(actual.Name.String(), question.Name.String()) || actual.Type != question.Type || actual.Class != question.Class {
		return bad()
	}
	cname := map[string]string{}
	for _, resource := range message.Answers {
		if resource.Header.Class != dnsmessage.ClassINET {
			return bad()
		}
		if body, ok := resource.Body.(*dnsmessage.CNAMEResource); ok {
			name, valid := dnsHostname(resource.Header.Name.String())
			target, targetValid := dnsHostname(body.CNAME.String())
			if !valid || !targetValid || resource.Header.Type != dnsmessage.TypeCNAME {
				return bad()
			}
			if previous, duplicate := cname[name]; duplicate && previous != target {
				return bad()
			}
			cname[name] = target
		} else if resource.Header.Type != question.Type {
			return bad()
		}
	}
	name := strings.ToLower(strings.TrimSuffix(question.Name.String(), "."))
	chain := map[string]bool{name: true}
	for count := 0; cname[name] != ""; count++ {
		if count >= maxDoHCNAMEs {
			return bad()
		}
		name = cname[name]
		if chain[name] {
			return bad()
		}
		chain[name] = true
	}
	addresses := []net.IPAddr{}
	ttl := time.Duration(1<<32-1) * time.Second
	for _, resource := range message.Answers {
		owner := strings.ToLower(strings.TrimSuffix(resource.Header.Name.String(), "."))
		if !chain[owner] {
			return bad()
		}
		if observed := time.Duration(resource.Header.TTL) * time.Second; observed < ttl {
			ttl = observed
		}
		switch body := resource.Body.(type) {
		case *dnsmessage.CNAMEResource:
			if cname[owner] == "" {
				return bad()
			}
		case *dnsmessage.AResource:
			if question.Type != dnsmessage.TypeA || owner != name {
				return bad()
			}
			addresses = append(addresses, net.IPAddr{IP: append(net.IP(nil), body.A[:]...)})
		case *dnsmessage.AAAAResource:
			if question.Type != dnsmessage.TypeAAAA || owner != name {
				return bad()
			}
			addresses = append(addresses, net.IPAddr{IP: append(net.IP(nil), body.AAAA[:]...)})
		default:
			return bad()
		}
	}
	if err := requirePublicAddresses(addresses); err != nil {
		return nil, 0, err
	}
	if len(addresses) == 0 {
		ttl = 0
	}
	return addresses, ttl, nil
}
