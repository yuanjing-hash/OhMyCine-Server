package hostapi

import (
	"context"
	"net"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/netresolve"
)

// Keep plugin DNS error codes and default-only installation at the Host
// boundary while sharing the fixed resolver algorithm with native consumers.
type fakeIPResolver struct{ resolver *netresolve.FakeIPResolver }

func newFakeIPResolver(system Resolver) *fakeIPResolver {
	return &fakeIPResolver{resolver: netresolve.New(netresolve.Config{System: netresolve.LookupFunc(system)})}
}

func (resolver *fakeIPResolver) LookupIPAddr(ctx context.Context, hostname string) ([]net.IPAddr, error) {
	addresses, err := resolver.resolver.LookupIPAddr(ctx, hostname)
	return addresses, pluginDNSError(err)
}

func pluginDNSError(err error) error {
	switch code := netresolve.ErrorCode(err); code {
	case "dns_unavailable", "private_address_denied", "dns_fallback_denied", "dns_fallback_redirect_denied", "dns_fallback_busy", "dns_fallback_invalid", "dns_fallback_unavailable":
		return denied("plugin_http_"+code, err)
	}
	return err
}

func isFakeIP(ip net.IP) bool { return netresolve.IsFakeIP(ip) }
func requirePublicAddresses(addresses []net.IPAddr) error {
	return pluginDNSError(netresolve.RequirePublicAddresses(addresses))
}
