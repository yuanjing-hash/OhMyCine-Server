package resourcecontent

import (
	"context"
	"net"
	"strconv"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/netresolve"
)

// Network is an internal trusted integration seam, not an HTTP/user setting.
// All peer, tracker and DHT traffic passes through the same address policy.
type Network interface {
	DialContext(context.Context, string, string) (net.Conn, error)
	ResolveUDP(context.Context, string) (*net.UDPAddr, error)
	ListenPacket(context.Context) (net.PacketConn, error)
	CheckIP(net.IP) error
}

type publicNetwork struct{ resolver *netresolve.FakeIPResolver }

func NewPublicNetwork() Network { return &publicNetwork{resolver: netresolve.New(netresolve.Config{})} }

func (n *publicNetwork) CheckIP(ip net.IP) error {
	if err := netresolve.RequirePublicAddresses([]net.IPAddr{{IP: ip}}); err != nil {
		return invalid("network_address_denied", err)
	}
	return nil
}

func (n *publicNetwork) resolve(ctx context.Context, address string) ([]net.IP, string, error) {
	if !validHostPort(address) {
		return nil, "", invalid("network_address_invalid", ErrInvalidMetainfo)
	}
	host, port, _ := net.SplitHostPort(address)
	if ip := net.ParseIP(host); ip != nil {
		if err := n.CheckIP(ip); err != nil {
			return nil, "", err
		}
		return []net.IP{ip}, port, nil
	}
	addresses, err := n.resolver.LookupIPAddr(ctx, host)
	if err != nil || len(addresses) == 0 {
		return nil, "", invalid("network_dns_failed", err)
	}
	if err = netresolve.RequirePublicAddresses(addresses); err != nil {
		return nil, "", invalid("network_address_denied", err)
	}
	ips := make([]net.IP, 0, len(addresses))
	for _, address := range addresses {
		ips = append(ips, address.IP)
	}
	return ips, port, nil
}

func (n *publicNetwork) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if network != "tcp" && network != "udp" {
		return nil, invalid("network_protocol_denied", ErrUnsupported)
	}
	ips, port, err := n.resolve(ctx, address)
	if err != nil {
		return nil, err
	}
	dialer := net.Dialer{Timeout: 5 * time.Second, KeepAlive: -1}
	for _, ip := range ips {
		conn, e := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if e == nil {
			return conn, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, invalid("network_dial_failed", ErrUnavailable)
}

func (n *publicNetwork) ResolveUDP(ctx context.Context, address string) (*net.UDPAddr, error) {
	ips, port, err := n.resolve(ctx, address)
	if err != nil {
		return nil, err
	}
	number, _ := strconv.Atoi(port)
	for _, ip := range ips {
		if ip4 := ip.To4(); ip4 != nil {
			return &net.UDPAddr{IP: ip4, Port: number}, nil
		}
	}
	return nil, invalid("dht_ipv4_unavailable", ErrUnavailable)
}

func (*publicNetwork) ListenPacket(ctx context.Context) (net.PacketConn, error) {
	return (&net.ListenConfig{}).ListenPacket(ctx, "udp4", "0.0.0.0:0")
}
