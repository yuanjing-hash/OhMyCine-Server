package resourcecontent

import (
	"context"
	"net"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/anacrolix/dht/v2"
	"github.com/anacrolix/dht/v2/krpc"
	"github.com/anacrolix/log"
	"golang.org/x/time/rate"
)

// discovery uses a maintained DHT traversal implementation, not a torrent
// client. The transport admits only contacted public endpoints. No library
// socket, DNS resolver, peer listener, storage or announce_peer is enabled.
type discovery struct {
	network   Network
	bootstrap []string
}

func newDiscovery(network Network) *discovery {
	return &discovery{network: network, bootstrap: []string{
		"router.bittorrent.com:6881", "router.utorrent.com:6881", "dht.transmissionbt.com:6881",
	}}
}

func (d *discovery) Discover(ctx context.Context, request DiscoveryRequest, emit func(string) bool) error {
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	emitPeer := func(address string) bool {
		if !emit(address) {
			cancel()
			return false
		}
		return true
	}
	var jobs sync.WaitGroup
	// Four tracker requests at most run concurrently; the URI parser limits
	// the total. A private source never enters public DHT, PEX or LSD discovery.
	trackerSlots := make(chan struct{}, 4)
	for _, tracker := range request.Magnet.Trackers {
		tracker := tracker
		jobs.Add(1)
		go func() {
			defer jobs.Done()
			select {
			case trackerSlots <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-trackerSlots }()
			trackerCtx, trackerCancel := context.WithTimeout(ctx, 8*time.Second)
			defer trackerCancel()
			_ = discoverTracker(trackerCtx, d.network, tracker, request.Magnet.Identity, emitPeer)
		}()
	}
	if !request.Private {
		jobs.Add(1)
		go func() { defer jobs.Done(); _ = d.discoverDHT(ctx, request.Magnet.Identity, emitPeer) }()
	}
	jobs.Wait()
	return ctx.Err()
}

func (d *discovery) discoverDHT(ctx context.Context, identity Identity, emit func(string) bool) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	short, err := identity.ShortHash()
	if err != nil {
		return err
	}
	// Resolve before creating the DHT node, under the caller's DNS/address
	// policy. The library's global DNS/bootstrap helpers are never called.
	starts := make([]dht.Addr, 0, len(d.bootstrap))
	for _, host := range d.bootstrap {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		address, e := d.network.ResolveUDP(ctx, host)
		if e == nil && address != nil && address.Port > 0 && d.network.CheckIP(address.IP) == nil {
			starts = append(starts, dht.NewAddr(address))
		}
	}
	if len(starts) == 0 {
		return invalid("dht_bootstrap_unavailable", ErrUnavailable)
	}
	conn, err := d.network.ListenPacket(ctx)
	if err != nil {
		return invalid("dht_socket_unavailable", ErrUnavailable)
	}
	logger := log.NewLogger("resourcecontent")
	logger.Handlers = []log.Handler{log.DiscardHandler}
	guard := &discoveryPacketConn{PacketConn: conn, ctx: ctx, network: d.network, contacted: map[string]bool{}}
	server, err := dht.NewServer(&dht.ServerConfig{
		Conn: guard, Passive: true, NoSecurity: false, Logger: logger,
		OnQuery:          func(*krpc.Msg, net.Addr) bool { return false },
		StartingNodes:    func() ([]dht.Addr, error) { return starts, nil },
		DefaultWant:      []krpc.Want{krpc.WantNodes},
		QueryResendDelay: func() time.Duration { return time.Second },
		SendLimiter:      rate.NewLimiter(8, 8),
	})
	if err != nil {
		_ = conn.Close()
		return invalid("dht_start_failed", ErrUnavailable)
	}
	closeServer := func() { server.Close(); _ = conn.Close() }
	defer closeServer()
	stop := context.AfterFunc(ctx, closeServer)
	defer stop()
	// AnnounceTraversal without AnnouncePeer is get_peers only: it does not
	// announce this process as a media peer or upload any content.
	traversal, err := server.AnnounceTraversal(short)
	if err != nil {
		return invalid("dht_lookup_failed", ErrUnavailable)
	}
	defer func() { traversal.Close(); <-traversal.Finished() }()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case values, ok := <-traversal.Peers:
			if !ok {
				return nil
			}
			for _, peer := range values.Peers {
				if peer.Port > 0 && d.network.CheckIP(peer.IP) == nil && !emit(peer.String()) {
					return nil
				}
			}
		}
	}
}

type discoveryPacketConn struct {
	net.PacketConn
	ctx       context.Context
	network   Network
	writes    atomic.Int64
	reads     int
	mu        sync.Mutex
	contacted map[string]bool
}

func (c *discoveryPacketConn) WriteTo(raw []byte, address net.Addr) (int, error) {
	peer, ok := address.(*net.UDPAddr)
	if !ok || peer.Zone != "" || peer.Port <= 0 || len(raw) > 4096 || c.network.CheckIP(peer.IP) != nil {
		return 0, invalid("dht_address_denied", ErrUnsupported)
	}
	if c.writes.Add(1) > 128 || c.ctx.Err() != nil {
		return 0, invalid("dht_request_limit", ErrLimitExceeded)
	}
	c.mu.Lock()
	c.contacted[peer.String()] = true
	c.mu.Unlock()
	return c.PacketConn.WriteTo(raw, address)
}

func (c *discoveryPacketConn) ReadFrom(raw []byte) (int, net.Addr, error) {
	for {
		if c.reads >= 512 {
			// Stop reading an unsolicited flood. The context-driven Server.Close
			// marks the library closed before it closes the packet socket.
			<-c.ctx.Done()
		}
		n, address, err := c.PacketConn.ReadFrom(raw)
		if err != nil {
			return n, address, err
		}
		c.reads++
		peer, ok := address.(*net.UDPAddr)
		if !ok || peer.Zone != "" || n > 4096 || c.network.CheckIP(peer.IP) != nil {
			continue
		}
		c.mu.Lock()
		contacted := c.contacted[peer.String()]
		c.mu.Unlock()
		if contacted {
			return n, address, nil
		}
	}
}

func trackerURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || len(raw) > 2048 || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "udp" {
		return nil, invalid("tracker_invalid", ErrInvalidMetainfo)
	}
	return u, nil
}
