package resourcecontent

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

func discoverTracker(ctx context.Context, network Network, raw string, identity Identity, emit func(string) bool) error {
	u, err := trackerURL(raw)
	if err != nil {
		return err
	}
	hash, err := identity.ShortHash()
	if err != nil {
		return err
	}
	var peerID [20]byte
	copy(peerID[:], "-OMC100-")
	if _, err = rand.Read(peerID[8:]); err != nil {
		return invalid("tracker_random_failed", ErrUnavailable)
	}
	if u.Scheme == "udp" {
		return discoverUDPTracker(ctx, network, u, hash, peerID, emit)
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return invalid("tracker_query_invalid", ErrInvalidMetainfo)
	}
	query.Set("info_hash", string(hash[:]))
	query.Set("peer_id", string(peerID[:]))
	query.Set("port", "6881")
	query.Set("uploaded", "0")
	query.Set("downloaded", "0")
	query.Set("left", "1")
	query.Set("compact", "1")
	query.Set("numwant", "32")
	u.RawQuery = query.Encode()
	transport := &http.Transport{
		Proxy: nil, DialContext: network.DialContext,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout: 3 * time.Second, ResponseHeaderTimeout: 5 * time.Second,
		MaxResponseHeaderBytes: 16 << 10, DisableCompression: true, DisableKeepAlives: true,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error {
		return invalid("tracker_redirect_denied", ErrUnsupported)
	}}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return invalid("tracker_request_invalid", ErrInvalidMetainfo)
	}
	request.Header.Set("User-Agent", "OhMyCine-Metadata/1")
	response, err := client.Do(request)
	if err != nil {
		return invalid("tracker_unavailable", ErrUnavailable)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.ContentLength > 1<<20 {
		return invalid("tracker_response_invalid", ErrInvalidMetainfo)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(body) > 1<<20 {
		return invalid("tracker_response_limit", ErrLimitExceeded)
	}
	return readTrackerPeers(body, network, emit)
}

func readTrackerPeers(raw []byte, network Network, emit func(string) bool) error {
	root, err := decode(raw, normalizeLimits(Limits{MaxDepth: 12, MaxValues: 4096}))
	if err != nil || root.kind != 'd' || root.get("failure reason") != nil {
		return invalid("tracker_response_invalid", ErrUnavailable)
	}
	peers := root.get("peers")
	if peers != nil {
		switch peers.kind {
		case 's':
			if len(peers.data)%6 != 0 {
				return invalid("tracker_peers_invalid", ErrInvalidMetainfo)
			}
			if !emitCompactPeers(peers.data, 4, network, emit) {
				return nil
			}
		case 'l':
			if len(peers.list) > 512 {
				return invalid("tracker_peer_limit", ErrLimitExceeded)
			}
			for _, peer := range peers.list {
				if peer.kind != 'd' {
					continue
				}
				ip, port := peer.get("ip"), peer.get("port")
				if ip == nil || port == nil || ip.kind != 's' || port.kind != 'i' || port.n <= 0 || port.n > 65535 {
					continue
				}
				address := net.ParseIP(string(ip.data))
				if address != nil && network.CheckIP(address) == nil && !emit(net.JoinHostPort(address.String(), strconv.FormatInt(port.n, 10))) {
					return nil
				}
			}
		default:
			return invalid("tracker_peers_invalid", ErrInvalidMetainfo)
		}
	}
	if peers6 := root.get("peers6"); peers6 != nil {
		if peers6.kind != 's' || len(peers6.data)%18 != 0 {
			return invalid("tracker_peers_invalid", ErrInvalidMetainfo)
		}
		emitCompactPeers(peers6.data, 16, network, emit)
	}
	return nil
}

func emitCompactPeers(raw []byte, ipSize int, network Network, emit func(string) bool) bool {
	if len(raw)/(ipSize+2) > 512 {
		return false
	}
	for len(raw) >= ipSize+2 {
		ip, port := net.IP(raw[:ipSize]), int(binary.BigEndian.Uint16(raw[ipSize:ipSize+2]))
		if port > 0 && network.CheckIP(ip) == nil && !emit(net.JoinHostPort(ip.String(), strconv.Itoa(port))) {
			return false
		}
		raw = raw[ipSize+2:]
	}
	return true
}

func discoverUDPTracker(ctx context.Context, network Network, u *url.URL, hash, peerID [20]byte, emit func(string) bool) error {
	// BEP41 URLData permits a bounded passkey/path to survive UDP announces.
	// Never strip a private tracker's suffix and contact a different resource.
	urlData := u.RequestURI()
	if len(urlData) > 255 {
		return invalid("udp_tracker_url_limit", ErrLimitExceeded)
	}
	address := u.Host
	if u.Port() == "" {
		return invalid("udp_tracker_port_missing", ErrInvalidMetainfo)
	}
	conn, err := network.DialContext(ctx, "udp", address)
	if err != nil {
		return invalid("tracker_unavailable", ErrUnavailable)
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	var tx [4]byte
	if _, err = rand.Read(tx[:]); err != nil {
		return invalid("tracker_random_failed", ErrUnavailable)
	}
	connect := make([]byte, 16)
	binary.BigEndian.PutUint64(connect, 0x41727101980)
	copy(connect[12:], tx[:])
	if _, err = conn.Write(connect); err != nil {
		return invalid("tracker_unavailable", ErrUnavailable)
	}
	buffer := make([]byte, 4096)
	n, err := conn.Read(buffer)
	if err != nil || n != 16 || binary.BigEndian.Uint32(buffer[:4]) != 0 || string(buffer[4:8]) != string(tx[:]) {
		return invalid("tracker_response_invalid", ErrInvalidMetainfo)
	}
	connectionID := binary.BigEndian.Uint64(buffer[8:16])
	announce := make([]byte, 98)
	binary.BigEndian.PutUint64(announce, connectionID)
	binary.BigEndian.PutUint32(announce[8:12], 1)
	if _, err = rand.Read(tx[:]); err != nil {
		return invalid("tracker_random_failed", ErrUnavailable)
	}
	copy(announce[12:16], tx[:])
	copy(announce[16:36], hash[:])
	copy(announce[36:56], peerID[:])
	binary.BigEndian.PutUint64(announce[64:72], 1)
	binary.BigEndian.PutUint32(announce[92:96], 32)
	binary.BigEndian.PutUint16(announce[96:98], 6881)
	if urlData != "" && urlData != "/" {
		announce = append(announce, 2, byte(len(urlData)))
		announce = append(announce, urlData...)
	}
	if _, err = conn.Write(announce); err != nil {
		return invalid("tracker_unavailable", ErrUnavailable)
	}
	n, err = conn.Read(buffer)
	if err != nil || n < 20 || binary.BigEndian.Uint32(buffer[:4]) != 1 || string(buffer[4:8]) != string(tx[:]) || (n-20)%6 != 0 {
		return invalid("tracker_response_invalid", ErrInvalidMetainfo)
	}
	emitCompactPeers(buffer[20:n], 4, network, emit)
	return nil
}
