package resourcecontent

import (
	"context"
	"encoding/binary"
	"net"
	"strings"
	"testing"
	"time"
)

func TestDHTLocalMockGetPeersOnly(t *testing.T) {
	node, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer node.Close()
	queries := make(chan string, 16)
	go func() {
		buffer := make([]byte, 4096)
		_ = node.SetReadDeadline(time.Now().Add(3 * time.Second))
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
			queries <- query
			response := map[string]any{"id": strings.Repeat("a", 20), "token": "mock-token"}
			if query == "get_peers" {
				response["values"] = []any{[]byte{127, 0, 0, 1, 4, 210}}
			}
			_, _ = node.WriteTo(encoded(map[string]any{"t": packet.get("t").data, "y": "r", "r": response}), address)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	network := &loopbackNetwork{}
	d := &discovery{network: network, bootstrap: []string{node.LocalAddr().String()}}
	peers := []string{}
	err = d.discoverDHT(ctx, Identity{V1: strings.Repeat("a", 40)}, func(address string) bool { peers = append(peers, address); return false })
	if err != nil || len(peers) != 1 || peers[0] != "127.0.0.1:1234" {
		t.Fatalf("DHT mock: %v %v", peers, err)
	}
	if network.packets.Load() != 1 {
		t.Fatal("uncontrolled DHT socket count")
	}
	seen := 0
	for {
		select {
		case query := <-queries:
			seen++
			if query != "get_peers" {
				t.Fatal("non-lookup DHT operation", query)
			}
		default:
			if seen == 0 {
				t.Fatal("no DHT query")
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
