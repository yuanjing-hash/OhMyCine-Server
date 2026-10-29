package resourcecontent

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// This network seam is compiled only in tests and can reach loopback sockets
// only. Every protocol test remains independent of live trackers or accounts.
type loopbackNetwork struct {
	dials   atomic.Int64
	packets atomic.Int64
}

func (n *loopbackNetwork) CheckIP(ip net.IP) error {
	if !ip.IsLoopback() {
		return ErrUnsupported
	}
	return nil
}
func (n *loopbackNetwork) DialContext(ctx context.Context, protocol, address string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(address)
	if err != nil || n.CheckIP(net.ParseIP(host)) != nil {
		return nil, ErrUnsupported
	}
	n.dials.Add(1)
	return (&net.Dialer{}).DialContext(ctx, protocol, address)
}
func (n *loopbackNetwork) ResolveUDP(ctx context.Context, address string) (*net.UDPAddr, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil || n.CheckIP(net.ParseIP(host)) != nil {
		return nil, ErrUnsupported
	}
	p, _ := strconv.Atoi(port)
	return &net.UDPAddr{IP: net.ParseIP(host), Port: p}, ctx.Err()
}
func (n *loopbackNetwork) ListenPacket(ctx context.Context) (net.PacketConn, error) {
	n.packets.Add(1)
	return (&net.ListenConfig{}).ListenPacket(ctx, "udp4", "127.0.0.1:0")
}

type discoverFunc func(context.Context, DiscoveryRequest, func(string) bool) error

func (f discoverFunc) Discover(ctx context.Context, r DiscoveryRequest, emit func(string) bool) error {
	return f(ctx, r, emit)
}

func fakePeer(t *testing.T, info []byte, mutate func([]byte), sizeOverride int) (string, <-chan []byte) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	observed := make(chan []byte, 1)
	go func() {
		conn, e := listener.Accept()
		if e != nil {
			observed <- nil
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(4 * time.Second))
		messages := []byte{}
		defer func() { observed <- messages }()
		var h [68]byte
		if _, e = io.ReadFull(conn, h[:]); e != nil {
			return
		}
		if mutate != nil {
			mutate(h[:])
		}
		if e = writeAll(conn, h[:]); e != nil {
			return
		}
		frame, e := readFrame(conn)
		if e != nil {
			return
		}
		if len(frame) > 0 {
			messages = append(messages, frame[0])
		}
		size := len(info)
		if sizeOverride != 0 {
			size = sizeOverride
		}
		if e = writeExtension(conn, 0, encoded(map[string]any{"m": map[string]any{"ut_metadata": 9}, "metadata_size": size})); e != nil {
			return
		}
		if sizeOverride != 0 {
			_, _ = io.Copy(io.Discard, conn)
			return
		}
		// A peer asking for media is ignored: this reader never serves piece
		// data, even after it possesses the info dictionary.
		mediaRequest := make([]byte, 17)
		binary.BigEndian.PutUint32(mediaRequest, 13)
		mediaRequest[4] = 6
		if e = writeAll(conn, mediaRequest); e != nil {
			return
		}
		count := (len(info) + metadataPieceSize - 1) / metadataPieceSize
		completed := 0
		for completed < count {
			window := 4
			if count-completed < window {
				window = count - completed
			}
			indexes := []int{}
			for len(indexes) < window {
				frame, e = readFrame(conn)
				if e != nil {
					return
				}
				if len(frame) == 0 {
					continue
				}
				messages = append(messages, frame[0])
				if len(frame) < 2 || frame[0] != 20 || frame[1] != 9 {
					return
				}
				header, e := decode(frame[2:], DefaultLimits())
				if e != nil || header.get("msg_type") == nil || header.get("msg_type").n != 0 {
					return
				}
				indexes = append(indexes, int(header.get("piece").n))
			}
			for i := len(indexes) - 1; i >= 0; i-- {
				index := indexes[i]
				start, end := index*metadataPieceSize, (index+1)*metadataPieceSize
				if end > len(info) {
					end = len(info)
				}
				payload := encoded(map[string]any{"msg_type": 1, "piece": index, "total_size": len(info)})
				payload = append(payload, info[start:end]...)
				if e = writeExtension(conn, localMetadataID, payload); e != nil {
					return
				}
				completed++
			}
		}
		// Completion closes the metadata-only connection instead of starting
		// a bitfield, media request, upload or a filesystem-backed torrent.
		for {
			frame, e = readFrame(conn)
			if e != nil {
				return
			}
			if len(frame) > 0 {
				messages = append(messages, frame[0])
			}
		}
	}()
	return listener.Addr().String(), observed
}

func TestMetadataEngineV2FullHash(t *testing.T) {
	info := encoded(map[string]any{"name": "v2.mkv", "meta version": 2, "piece length": 16384, "file tree": map[string]any{"v2.mkv": map[string]any{"": map[string]any{"length": 1, "pieces root": bytes.Repeat([]byte{8}, 32)}}}})
	manifest, err := ParseTorrent(append(append([]byte("d4:info"), info...), 'e'), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	address, closed := fakePeer(t, info, nil, 0)
	engine := NewEngine(EngineConfig{Network: &loopbackNetwork{}, Discoverer: discoverFunc(func(_ context.Context, _ DiscoveryRequest, emit func(string) bool) error { emit(address); return nil }), Timeout: time.Second})
	actual, err := engine.Resolve(context.Background(), ResolveRequest{URI: "magnet:?xt=urn:btmh:1220" + manifest.Identity.V2})
	if err != nil || actual.Identity != manifest.Identity {
		t.Fatal(actual.Identity, err)
	}
	<-closed
}

func TestMetadataEngineFakePeerNoMedia(t *testing.T) {
	rawInfo := encoded(singleInfo("episode.S01E03.mkv", 1<<25)) // metadata > 2 BEP9 blocks; zero payload bytes exist.
	address, observed := fakePeer(t, rawInfo, nil, 0)
	network := &loopbackNetwork{}
	var calls atomic.Int64
	engine := NewEngine(EngineConfig{Network: network, Discoverer: discoverFunc(func(ctx context.Context, r DiscoveryRequest, emit func(string) bool) error {
		calls.Add(1)
		emit(address)
		return nil
	}), Timeout: 2 * time.Second})
	if network.dials.Load() != 0 || calls.Load() != 0 {
		t.Fatal("construction started discovery")
	}
	identity := infoIdentity(rawInfo)
	manifest, err := engine.Resolve(context.Background(), ResolveRequest{URI: "magnet:?xt=urn:btih:" + identity.V1 + "&xs=http%3A%2F%2F127.0.0.1%3A1%2Fauto&ws=http%3A%2F%2F127.0.0.1%3A1%2Fwebseed"})
	if err != nil || manifest.Identity != identity || manifest.Files[0].Path != "episode.S01E03.mkv" {
		t.Fatalf("metadata: %+v %v", manifest, err)
	}
	if network.dials.Load() != 1 {
		t.Fatal("automatic source was dialed", network.dials.Load())
	}
	select {
	case messages := <-observed:
		if len(messages) < 3 {
			t.Fatal("no metadata exchange", messages)
		}
		for _, kind := range messages {
			if kind != 20 {
				t.Fatal("media protocol message sent", messages)
			}
		}
	case <-time.After(time.Second):
		t.Fatal("peer connection leaked")
	}
}

func TestMetadataEngineAuthenticatesBeforeManifest(t *testing.T) {
	info := encoded(singleInfo("movie.mkv", 1))
	for _, tt := range []struct {
		name     string
		mutate   func([]byte)
		override int
		hash     string
	}{
		{"wrong handshake", func(h []byte) { h[28] ^= 1 }, 0, ""},
		{"wrong raw hash", nil, 0, strings.Repeat("a", 40)},
		{"oversized metadata", nil, DefaultLimits().MaxInfoBytes + 1, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			address, closed := fakePeer(t, info, tt.mutate, tt.override)
			identity := infoIdentity(info)
			if tt.hash != "" {
				identity.V1 = tt.hash
			}
			engine := NewEngine(EngineConfig{Network: &loopbackNetwork{}, Discoverer: discoverFunc(func(_ context.Context, _ DiscoveryRequest, emit func(string) bool) error { emit(address); return nil }), Timeout: time.Second})
			if _, err := engine.Resolve(context.Background(), ResolveRequest{URI: "magnet:?xt=urn:btih:" + identity.V1}); err == nil {
				t.Fatal("untrusted metadata accepted")
			}
			select {
			case <-closed:
			case <-time.After(time.Second):
				t.Fatal("failure leaked peer")
			}
		})
	}
}

func TestMetadataAdmissionAndCancellation(t *testing.T) {
	var running atomic.Int64
	started := make(chan struct{}, 4)
	d := discoverFunc(func(ctx context.Context, r DiscoveryRequest, emit func(string) bool) error {
		if !r.Private {
			t.Error("lost private provenance")
		}
		if running.Add(1) != 1 {
			t.Error("unbounded sessions")
		}
		defer running.Add(-1)
		started <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	})
	engine := NewEngine(EngineConfig{Network: &loopbackNetwork{}, Discoverer: d, MaxConcurrent: 1, MaxPending: 2, Timeout: time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := ResolveRequest{URI: "magnet:?xt=urn:btih:" + strings.Repeat("a", 40) + "&x.pe=127.0.0.1%3A1&tr=http%3A%2F%2F127.0.0.1%3A1%2Fauthorized", Private: true, AuthorizedTrackers: []string{"http://127.0.0.1:1/authorized"}}
	finished := make(chan error, 2)
	go func() { _, err := engine.Resolve(ctx, request); finished <- err }()
	<-started
	go func() { _, err := engine.Resolve(ctx, request); finished <- err }()
	deadline := time.Now().Add(time.Second)
	for engine.pending.Load() != 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if _, err := engine.Resolve(ctx, request); !errors.Is(err, ErrLimitExceeded) {
		t.Fatal("queue limit", err)
	}
	cancel()
	for i := 0; i < 2; i++ {
		select {
		case err := <-finished:
			if !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("cancel did not converge")
		}
	}
	if running.Load() != 0 || engine.pending.Load() != 0 || len(engine.slots) != 0 {
		t.Fatal("lease leaked")
	}
}

func TestPrivateDiscoveryOnlyAuthorizedTracker(t *testing.T) {
	info := encoded(singleInfo("movie.mkv", 123))
	peer, closed := fakePeer(t, info, nil, 0)
	peerHost, peerPort, _ := net.SplitHostPort(peer)
	port, _ := strconv.Atoi(peerPort)
	compact := append([]byte(net.ParseIP(peerHost).To4()), byte(port>>8), byte(port))
	var trackerCalls atomic.Int64
	tracker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		trackerCalls.Add(1)
		if r.URL.Query().Get("key") != "private-secret" {
			t.Error("passkey changed")
		}
		if len(r.URL.Query().Get("info_hash")) != 20 {
			t.Error("bad binary hash")
		}
		_, _ = w.Write(encoded(map[string]any{"interval": 1800, "peers": compact}))
	}))
	defer tracker.Close()
	network := &loopbackNetwork{}
	engine := NewEngine(EngineConfig{Network: network, Timeout: 2 * time.Second})
	manifest, err := engine.Resolve(context.Background(), ResolveRequest{URI: "magnet:?xt=urn:btih:" + infoIdentity(info).V1 + "&tr=" + url.QueryEscape(tracker.URL+"?key=private-secret") + "&x.pe=127.0.0.1%3A1", Private: true, AuthorizedTrackers: []string{tracker.URL + "?key=private-secret"}})
	if err != nil || manifest.Files[0].Size != 123 {
		t.Fatalf("private: %v", err)
	}
	if trackerCalls.Load() != 1 || network.packets.Load() != 0 || network.dials.Load() != 2 {
		t.Fatalf("private source used other discovery: trackers=%d packets=%d dials=%d", trackerCalls.Load(), network.packets.Load(), network.dials.Load())
	}
	<-closed
}

func TestPrivateMagnetCannotAuthorizeOwnTracker(t *testing.T) {
	network := &loopbackNetwork{}
	engine := NewEngine(EngineConfig{Network: network})
	if _, err := engine.Resolve(context.Background(), ResolveRequest{URI: "magnet:?xt=urn:btih:" + strings.Repeat("a", 40) + "&tr=http%3A%2F%2F127.0.0.1%3A1%2Funtrusted", Private: true}); !errors.Is(err, ErrUnsupported) {
		t.Fatal(err)
	}
	if network.dials.Load() != 0 || network.packets.Load() != 0 {
		t.Fatal("private discovery escaped before authority validation")
	}
}

func TestUDPTrackerProtocol(t *testing.T) {
	tracker, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tracker.Close()
	finished := make(chan error, 1)
	go func() {
		buffer := make([]byte, 4096)
		_ = tracker.SetDeadline(time.Now().Add(2 * time.Second))
		n, peer, e := tracker.ReadFrom(buffer)
		if e != nil {
			finished <- e
			return
		}
		if n != 16 || binary.BigEndian.Uint64(buffer[:8]) != 0x41727101980 {
			finished <- fmt.Errorf("invalid connect")
			return
		}
		response := make([]byte, 16)
		copy(response[4:8], buffer[12:16])
		binary.BigEndian.PutUint64(response[8:], 123)
		_, _ = tracker.WriteTo(response, peer)
		n, peer, e = tracker.ReadFrom(buffer)
		if e != nil {
			finished <- e
			return
		}
		if n < 98 || binary.BigEndian.Uint64(buffer[:8]) != 123 || binary.BigEndian.Uint64(buffer[56:64]) != 0 || binary.BigEndian.Uint64(buffer[72:80]) != 0 || binary.BigEndian.Uint32(buffer[80:84]) != 0 || !bytes.Contains(buffer[98:n], []byte("key=secret")) {
			finished <- fmt.Errorf("invalid metadata announce")
			return
		}
		response = make([]byte, 26)
		binary.BigEndian.PutUint32(response[:4], 1)
		copy(response[4:8], buffer[12:16])
		copy(response[20:24], net.IPv4(127, 0, 0, 1).To4())
		binary.BigEndian.PutUint16(response[24:], 1234)
		_, _ = tracker.WriteTo(response, peer)
		finished <- nil
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	peers := []string{}
	err = discoverTracker(ctx, &loopbackNetwork{}, "udp://"+tracker.LocalAddr().String()+"/announce?key=secret", Identity{V1: strings.Repeat("a", 40)}, func(p string) bool { peers = append(peers, p); return true })
	if err != nil || len(peers) != 1 || peers[0] != "127.0.0.1:1234" {
		t.Fatal(err, peers)
	}
	if e := <-finished; e != nil {
		t.Fatal(e)
	}
}

func TestNetworkAndTrackerBoundaries(t *testing.T) {
	public := NewPublicNetwork()
	for _, ip := range []string{"127.0.0.1", "10.0.0.1", "169.254.169.254", "198.18.0.1", "::1", "fe80::1", "::"} {
		if public.CheckIP(net.ParseIP(ip)) == nil {
			t.Fatal("unsafe IP accepted", ip)
		}
		if _, err := public.DialContext(context.Background(), "tcp", net.JoinHostPort(ip, "80")); err == nil {
			t.Fatal("unsafe IP dialed", ip)
		}
	}
	var hits atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }))
	defer redirect.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := discoverTracker(ctx, &loopbackNetwork{}, redirect.URL, Identity{V1: strings.Repeat("a", 40)}, func(string) bool { return true }); err == nil || hits.Load() != 0 {
		t.Fatal("redirect followed", err, hits.Load())
	}
	// Returned private peers are discarded by the same policy used by every
	// other protocol, before any dial. Public peer facts remain available.
	raw := encoded(map[string]any{"peers": append([]byte{127, 0, 0, 1, 1, 1}, []byte{8, 8, 8, 8, 1, 1}...)})
	peers := []string{}
	if err := readTrackerPeers(raw, public, func(p string) bool { peers = append(peers, p); return true }); err != nil || !reflectStrings(peers, []string{"8.8.8.8:257"}) {
		t.Fatal(err, peers)
	}
}

func reflectStrings(a, b []string) bool { return strings.Join(a, "|") == strings.Join(b, "|") }
