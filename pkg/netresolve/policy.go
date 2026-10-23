package netresolve

import (
	"context"
	"errors"
	"net"
	"net/http"
)

// LookupFunc is a controlled DNS source. Constructing a resolver explicitly
// opts into recognized synthetic-answer repair; it does not change that source.
type LookupFunc func(context.Context, string) ([]net.IPAddr, error)

// Config permits trusted callers and offline fixtures to supply their system
// observation and DNS HTTP boundary. Production callers use the fixed defaults.
// The hostname, bootstrap, TLS policy, limits and cache budgets are not settings.
type Config struct {
	System     LookupFunc
	HTTPClient *http.Client
}

// Error exposes only a fixed DNS-policy code and unwraps for typed inspection.
type Error struct {
	code  string
	cause error
}

func (e *Error) Error() string { return e.code }
func (e *Error) Unwrap() error { return e.cause }

func failure(code string, cause error) error { return &Error{code: code, cause: cause} }

func ErrorCode(err error) string {
	var failure *Error
	if errors.As(err, &failure) {
		return failure.code
	}
	return ""
}

// RequirePublicAddresses preserves the Host's public-address baseline. A caller
// with a stricter predicate must also apply it before repair and before dialing.
func RequirePublicAddresses(addresses []net.IPAddr) error {
	for _, address := range addresses {
		ip := address.IP
		if address.Zone != "" || ip == nil || ip.To16() == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() || IsFakeIP(ip) {
			return failure("private_address_denied", nil)
		}
	}
	return nil
}
