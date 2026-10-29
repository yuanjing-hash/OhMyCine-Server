package resourcecontent

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

type DiscoveryRequest struct {
	Magnet  Magnet
	Private bool
}

// PeerDiscoverer emits validated candidate addresses. It must stop promptly on
// ctx cancellation or when emit returns false. Private requests may only use
// their authorized private trackers; DHT/PEX/LSD are forbidden in that branch.
type PeerDiscoverer interface {
	Discover(context.Context, DiscoveryRequest, func(string) bool) error
}

type ResolveRequest struct {
	URI string
	// Private is set by trusted source provenance, never inferred solely from
	// a magnet query or delayed until the info dictionary has arrived.
	Private bool
	// AuthorizedTrackers comes from validated private-source provenance. A
	// private magnet cannot authorize its own arbitrary tracker destinations.
	// Only exact URI trackers also present in this set may be contacted.
	AuthorizedTrackers []string
}

type EngineConfig struct {
	Limits          Limits
	Network         Network
	Discoverer      PeerDiscoverer
	MaxConcurrent   int
	MaxPending      int
	MaxPeerAttempts int
	PeersPerSession int
	Timeout         time.Duration
	PeerTimeout     time.Duration
}

// Engine owns only ephemeral bounded metadata sessions. It has no downloader
// reference, filesystem storage, media-piece request API, or upload operation.
// Actor/configuration-bound cache and singleflight belong to the owning service.
type Engine struct {
	config  EngineConfig
	slots   chan struct{}
	pending atomic.Int64
}

func NewEngine(c EngineConfig) *Engine {
	c.Limits = normalizeLimits(c.Limits)
	if c.Network == nil {
		c.Network = NewPublicNetwork()
	}
	bounded := func(n, fallback, max int) int {
		if n <= 0 || n > max {
			return fallback
		}
		return n
	}
	c.MaxConcurrent = bounded(c.MaxConcurrent, 2, 8)
	c.MaxPending = bounded(c.MaxPending, 32, 128)
	c.MaxPeerAttempts = bounded(c.MaxPeerAttempts, 32, 64)
	c.PeersPerSession = bounded(c.PeersPerSession, 4, 8)
	if c.Timeout <= 0 || c.Timeout > time.Minute {
		c.Timeout = time.Minute
	}
	if c.PeerTimeout <= 0 || c.PeerTimeout > 20*time.Second {
		c.PeerTimeout = 15 * time.Second
	}
	if c.Discoverer == nil {
		c.Discoverer = newDiscovery(c.Network)
	}
	return &Engine{config: c, slots: make(chan struct{}, c.MaxConcurrent)}
}

func (e *Engine) Resolve(ctx context.Context, request ResolveRequest) (Manifest, error) {
	parentCtx := ctx
	if err := ctx.Err(); err != nil {
		return Manifest{}, err
	}
	m, err := ParseMagnet(request.URI)
	if err != nil {
		return Manifest{}, err
	}
	if request.Private {
		if len(request.AuthorizedTrackers) > 16 {
			return Manifest{}, invalid("private_tracker_limit", ErrLimitExceeded)
		}
		allowed := make(map[string]bool, len(request.AuthorizedTrackers))
		for _, tracker := range request.AuthorizedTrackers {
			if _, err := trackerURL(tracker); err != nil {
				return Manifest{}, err
			}
			allowed[tracker] = true
		}
		trackers := []string{}
		for _, tracker := range m.Trackers {
			if allowed[tracker] {
				trackers = append(trackers, tracker)
			}
		}
		if len(trackers) == 0 {
			return Manifest{}, invalid("private_tracker_authority_missing", ErrUnsupported)
		}
		m.Trackers = trackers
	}
	if e.pending.Add(1) > int64(e.config.MaxPending) {
		e.pending.Add(-1)
		return Manifest{}, invalid("metadata_queue_full", ErrLimitExceeded)
	}
	defer e.pending.Add(-1)
	ctx, cancel := context.WithTimeout(ctx, e.config.Timeout)
	defer cancel()
	select {
	case e.slots <- struct{}{}:
		defer func() { <-e.slots }()
	case <-ctx.Done():
		return Manifest{}, ctx.Err()
	}
	peers := make(chan string, e.config.MaxPeerAttempts)
	result := make(chan Manifest, 1)
	var seenMu sync.Mutex
	seen := map[string]bool{}
	emit := func(address string) bool {
		if !validHostPort(address) || ctx.Err() != nil {
			return false
		}
		seenMu.Lock()
		defer seenMu.Unlock()
		if seen[address] {
			return true
		}
		if len(seen) >= e.config.MaxPeerAttempts {
			return false
		}
		seen[address] = true
		select {
		case peers <- address:
			return true
		case <-ctx.Done():
			return false
		}
	}
	discoveryDone := make(chan struct{})
	go func() {
		defer close(discoveryDone)
		defer close(peers)
		if !request.Private {
			for _, address := range m.Peers {
				if !emit(address) {
					return
				}
			}
		}
		_ = e.config.Discoverer.Discover(ctx, DiscoveryRequest{Magnet: m, Private: request.Private}, emit)
	}()
	var workers sync.WaitGroup
	for i := 0; i < e.config.PeersPerSession; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for address := range peers {
				if ctx.Err() != nil {
					return
				}
				peerCtx, peerCancel := context.WithTimeout(ctx, e.config.PeerTimeout)
				manifest, err := fetchMetadata(peerCtx, e.config.Network, address, m.Identity, e.config.Limits)
				peerCancel()
				if err != nil || manifest.Private && !request.Private {
					continue
				}
				manifest.Trackers = [][]string{append([]string(nil), m.Trackers...)}
				select {
				case result <- manifest:
				case <-ctx.Done():
				}
				return
			}
		}()
	}
	done := make(chan struct{})
	go func() { workers.Wait(); close(done) }()
	select {
	case manifest := <-result:
		cancel()
		<-done
		<-discoveryDone
		if err := parentCtx.Err(); err != nil {
			return Manifest{}, err
		}
		return manifest, nil
	case <-done:
		sessionErr := ctx.Err()
		cancel()
		<-discoveryDone
		if err := parentCtx.Err(); err != nil {
			return Manifest{}, err
		}
		select {
		case manifest := <-result:
			return manifest, nil
		default:
		}
		if sessionErr != nil {
			return Manifest{}, sessionErr
		}
		return Manifest{}, invalid("metadata_unavailable", ErrUnavailable)
	case <-ctx.Done():
		cancel()
		<-done
		<-discoveryDone
		return Manifest{}, ctx.Err()
	}
}
