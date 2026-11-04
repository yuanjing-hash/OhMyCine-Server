package resourcecontent

import (
	"context"
	"encoding/binary"
	"net"
	"testing"
	"time"

	"github.com/anacrolix/dht/v2"
	"github.com/anacrolix/dht/v2/krpc"
)

// Only the two fixture nodes can be reached: public advertised addresses are
// mapped to actual loopback sockets. This lets the real DHT server enforce
// BEP42 for public node IDs without using NoSecurity or external networking.
type dhtProtocolNetwork struct {
	*loopbackNetwork
	physical map[string]*net.UDPAddr
	public   map[string]*net.UDPAddr
}

func (n *dhtProtocolNetwork) CheckIP(ip net.IP) error {
	for _, address := range n.public {
		if address.IP.Equal(ip) {
			return nil
		}
	}
	return n.loopbackNetwork.CheckIP(ip)
}
func (n *dhtProtocolNetwork) ResolveUDP(ctx context.Context, value string) (*net.UDPAddr, error) {
	for _, address := range n.public {
		if address.String() == value {
			return address, ctx.Err()
		}
	}
	return nil, ErrUnsupported
}
func (n *dhtProtocolNetwork) ListenPacket(ctx context.Context) (net.PacketConn, error) {
	conn, err := n.loopbackNetwork.ListenPacket(ctx)
	if err != nil {
		return nil, err
	}
	return &dhtProtocolPacketConn{PacketConn: conn, network: n}, nil
}

type dhtProtocolPacketConn struct {
	net.PacketConn
	network *dhtProtocolNetwork
}

func (c *dhtProtocolPacketConn) WriteTo(raw []byte, address net.Addr) (int, error) {
	if target := c.network.physical[address.String()]; target != nil {
		return c.PacketConn.WriteTo(raw, target)
	}
	return 0, ErrUnsupported
}
func (c *dhtProtocolPacketConn) ReadFrom(raw []byte) (int, net.Addr, error) {
	n, address, err := c.PacketConn.ReadFrom(raw)
	if err == nil {
		if mapped := c.network.public[address.String()]; mapped != nil {
			address = mapped
		}
	}
	return n, address, err
}

func TestDHTLocalMockGetPeersOnly(t *testing.T) {
	info := encoded(singleInfo("dht.S01E01.mkv", 1))
	peer, metadataClosed := fakePeer(t, info, nil, 0)
	peerAddress, err := net.ResolveTCPAddr("tcp", peer)
	if err != nil {
		t.Fatal(err)
	}
	compactPeer := append(append([]byte(nil), peerAddress.IP.To4()...), byte(peerAddress.Port>>8), byte(peerAddress.Port))
	network := &dhtProtocolNetwork{loopbackNetwork: &loopbackNetwork{}, physical: map[string]*net.UDPAddr{}, public: map[string]*net.UDPAddr{}}
	nodes := make([]net.PacketConn, 2)
	public := make([]*net.UDPAddr, 2)
	ids := make([]krpc.ID, 2)
	for i, ip := range []string{"8.8.8.8", "1.1.1.1"} {
		nodes[i], err = net.ListenPacket("udp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer nodes[i].Close()
		physical := nodes[i].LocalAddr().(*net.UDPAddr)
		public[i] = &net.UDPAddr{IP: net.ParseIP(ip).To4(), Port: physical.Port}
		network.physical[public[i].String()] = physical
		network.public[physical.String()] = public[i]
		ids[i][19] = byte(i + 1)
		dht.SecureNodeId(&ids[i], public[i].IP)
	}
	nextHop, err := (krpc.NodeInfo{ID: ids[1], Addr: krpc.NodeAddr{IP: public[1].IP, Port: public[1].Port}}).MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	type observedQuery struct {
		hop   int
		query string
	}
	queries := make(chan observedQuery, 16)
	for hop, node := range nodes {
		hop, node := hop, node
		go func() {
			buffer := make([]byte, 4096)
			_ = node.SetReadDeadline(time.Now().Add(5 * time.Second))
			for {
				n, address, e := node.ReadFrom(buffer)
				if e != nil {
					return
				}
				packet, e := decode(buffer[:n], DefaultLimits())
				if e != nil || packet.kind != 'd' || packet.get("q") == nil || packet.get("t") == nil {
					continue
				}
				query := string(packet.get("q").data)
				queries <- observedQuery{hop, query}
				response := map[string]any{"id": ids[hop][:], "token": "mock-token"}
				if query == "get_peers" {
					if hop == 0 {
						response["nodes"] = nextHop // The router itself never returns a metadata peer.
					} else {
						// Above the removed one-second final reply window, below
						// the dependency's default two-second window.
						time.Sleep(1200 * time.Millisecond)
						response["values"] = []any{compactPeer}
					}
				}
				_, _ = node.WriteTo(encoded(map[string]any{"t": packet.get("t").data, "y": "r", "r": response}), address)
			}
		}()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	d := &discovery{network: network, bootstrap: []string{public[0].String()}}
	engine := NewEngine(EngineConfig{Network: network, Discoverer: d, Timeout: 4 * time.Second})
	identity := infoIdentity(info)
	manifest, err := engine.Resolve(ctx, ResolveRequest{URI: "magnet:?xt=urn:btih:" + identity.V1})
	if err != nil || manifest.Identity != identity || len(manifest.Files) != 1 || manifest.Files[0].Path != "dht.S01E01.mkv" {
		t.Fatalf("two-hop DHT metadata handoff failed: %v", err)
	}
	if network.packets.Load() != 1 || network.dials.Load() != 1 {
		t.Fatal("uncontrolled DHT socket or metadata peer count")
	}
	select {
	case messages := <-metadataClosed:
		if len(messages) < 2 {
			t.Fatal("DHT handoff did not read metadata")
		}
		for _, message := range messages {
			if message != 20 {
				t.Fatal("DHT handoff started media traffic")
			}
		}
	case <-time.After(time.Second):
		t.Fatal("DHT metadata peer was not closed")
	}
	seen := map[int]bool{}
	for {
		select {
		case query := <-queries:
			seen[query.hop] = true
			if query.query != "get_peers" {
				t.Fatal("non-lookup DHT operation", query.query)
			}
		default:
			if len(seen) != 2 {
				t.Fatal("returned DHT node was not traversed")
			}
			return
		}
	}
}

func TestDiscoveryPacketConnRejectsUncontactedAndLimitsRequests(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	peer, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	guard := &discoveryPacketConn{PacketConn: conn, ctx: ctx, network: &loopbackNetwork{}, contacted: map[string]bool{}}
	if _, err = guard.WriteTo([]byte("x"), &net.UDPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 1}); err == nil {
		t.Fatal("private DHT target accepted")
	}
	guard.writes.Store(128)
	if _, err = guard.WriteTo([]byte("x"), peer.LocalAddr()); err == nil {
		t.Fatal("query budget exceeded")
	}
	guard.writes.Store(0)
	if _, err = guard.WriteTo([]byte("contact"), peer.LocalAddr()); err != nil {
		t.Fatal(err)
	}
	_, _ = peer.WriteTo([]byte{1, 2, 3}, conn.LocalAddr())
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	buffer := make([]byte, 8)
	if n, _, err := guard.ReadFrom(buffer); err != nil || n != 3 || binary.BigEndian.Uint16(buffer[:2]) != 258 {
		t.Fatal(n, err)
	}
}
