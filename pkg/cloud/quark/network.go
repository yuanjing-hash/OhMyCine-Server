package quark

import (
	"context"
	"net"
	"net/http"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/netresolve"
)

// These requests consume provider-issued object capabilities, never a user
// configured service endpoint. Resolve once at dial time and refuse private
// destinations/rebinding; preserve normal TLS hostname verification.
func newPublicTransport() *http.Transport {
	return newPublicTransportWithDNS(net.DefaultResolver.LookupIPAddr, nil, nil)
}

func newPublicTransportWithDNS(system netresolve.LookupFunc, dnsClient *http.Client, dial func(context.Context, string, string) (net.Conn, error)) *http.Transport {
	if system == nil {
		system = net.DefaultResolver.LookupIPAddr
	}
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	t.DisableCompression = true
	t.DisableKeepAlives = true
	t.ResponseHeaderTimeout = 30 * time.Second
	if dial == nil {
		dialer := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
		dial = dialer.DialContext
	}
	resolver := netresolve.New(netresolve.Config{System: nativeObjectSystemLookup(system), HTTPClient: dnsClient})
	t.DialContext = publicDialContext(resolver.LookupIPAddr, dial)
	return t
}

// Preserve Quark's predicate before the shared resolver can contact DoH. The
// shared Host policy intentionally has a different public-address baseline.
func nativeObjectSystemLookup(system netresolve.LookupFunc) netresolve.LookupFunc {
	return func(ctx context.Context, hostname string) ([]net.IPAddr, error) {
		addresses, err := system(ctx, hostname)
		if err != nil {
			return nil, err
		}
		for _, address := range addresses {
			if address.Zone != "" || !netresolve.IsFakeIP(address.IP) && !publicIP(address.IP) {
				return nil, &Failure{Code: CodeInvalid, SubmitReason: "transport_address_rejected"}
			}
		}
		return addresses, nil
	}
}

func publicDialContext(
	lookup func(context.Context, string) ([]net.IPAddr, error),
	dial func(context.Context, string, string) (net.Conn, error),
) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, e := net.SplitHostPort(address)
		if e != nil {
			return nil, &Failure{Code: CodeInvalid, SubmitReason: "transport_address_rejected"}
		}
		answers, e := lookup(ctx, host)
		if e != nil || len(answers) == 0 {
			return nil, publicDialFailure(ctx, "transport_dns", e)
		}
		for _, a := range answers {
			if a.Zone != "" || netresolve.IsFakeIP(a.IP) || !publicIP(a.IP) {
				return nil, &Failure{Code: CodeInvalid, SubmitReason: "transport_address_rejected"}
			}
		}
		var lastError error
		for _, a := range answers {
			c, e := dial(ctx, network, net.JoinHostPort(a.IP.String(), port))
			if e == nil {
				return c, nil
			}
			lastError = e
		}
		return nil, publicDialFailure(ctx, "transport_connect", lastError)
	}
}

// Preserve only a finite local classification, never the resolver/dial error
// text or its host/address. This remains an ambiguous Do failure for uploads.
func publicDialFailure(ctx context.Context, fallback string, err error) error {
	reason := uploadTransportReason(ctx, err)
	if reason == "transport_error" {
		if netresolve.ErrorCode(err) == "private_address_denied" {
			return &Failure{Code: CodeInvalid, SubmitReason: "transport_address_rejected"}
		}
		reason = fallback
	}
	return &Failure{Code: CodeUnavailable, SubmitReason: reason}
}

func publicIP(ip net.IP) bool {
	return ip != nil && ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && !ip.IsUnspecified() && !ip.IsMulticast()
}
