package quark

import (
	"context"
	"net"
	"net/http"
	"time"
)

// These requests consume provider-issued object capabilities, never a user
// configured service endpoint. Resolve once at dial time and refuse private
// destinations/rebinding; preserve normal TLS hostname verification.
func newPublicTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	t.DisableCompression = true
	t.DisableKeepAlives = true
	t.ResponseHeaderTimeout = 30 * time.Second
	dialer := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	t.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, e := net.SplitHostPort(address)
		if e != nil {
			return nil, failure(CodeInvalid)
		}
		answers, e := net.DefaultResolver.LookupIPAddr(ctx, host)
		if e != nil || len(answers) == 0 {
			return nil, failure(CodeUnavailable)
		}
		for _, a := range answers {
			if !publicIP(a.IP) {
				return nil, failure(CodeInvalid)
			}
		}
		for _, a := range answers {
			c, e := dialer.DialContext(ctx, network, net.JoinHostPort(a.IP.String(), port))
			if e == nil {
				return c, nil
			}
		}
		return nil, failure(CodeUnavailable)
	}
	return t
}
func publicIP(ip net.IP) bool {
	return ip != nil && ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && !ip.IsUnspecified() && !ip.IsMulticast()
}
