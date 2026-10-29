package resourcecontent

import (
	"encoding/base32"
	"encoding/hex"
	"net"
	"net/url"
	"strconv"
	"strings"
)

type Magnet struct {
	Identity    Identity
	DisplayName string
	Trackers    []string
	Peers       []string
}

// ParseMagnet accepts identity and bounded peer-discovery hints only. Automatic
// sources/webseeds (xs/as/ws) are intentionally never requested by this module.
func ParseMagnet(raw string) (Magnet, error) {
	if len(raw) == 0 || len(raw) > 16<<10 {
		return Magnet{}, invalid("magnet_size", ErrLimitExceeded)
	}
	u, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(u.Scheme, "magnet") || u.Host != "" || u.Path != "" || u.User != nil || u.Fragment != "" || u.Opaque != "" {
		return Magnet{}, invalid("magnet_invalid", ErrInvalidMetainfo)
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil || len(q["xt"]) == 0 || len(q["xt"]) > 4 || len(q["dn"]) > 1 || len(q["tr"]) > 16 || len(q["x.pe"]) > 32 {
		return Magnet{}, invalid("magnet_invalid", ErrInvalidMetainfo)
	}
	m := Magnet{DisplayName: q.Get("dn"), Trackers: []string{}, Peers: []string{}}
	if len(m.DisplayName) > 1024 {
		return Magnet{}, invalid("magnet_name_limit", ErrLimitExceeded)
	}
	for _, topic := range q["xt"] {
		lower := strings.ToLower(topic)
		switch {
		case strings.HasPrefix(lower, "urn:btih:"):
			encoded := topic[len("urn:btih:"):]
			var b []byte
			if len(encoded) == 40 {
				b, err = hex.DecodeString(encoded)
			} else if len(encoded) == 32 {
				b, err = base32.StdEncoding.DecodeString(strings.ToUpper(encoded))
			} else {
				err = ErrInvalidMetainfo
			}
			if err != nil || len(b) != 20 || m.Identity.V1 != "" && m.Identity.V1 != hex.EncodeToString(b) {
				return Magnet{}, invalid("magnet_hash_invalid", ErrInvalidMetainfo)
			}
			m.Identity.V1 = hex.EncodeToString(b)
		case strings.HasPrefix(lower, "urn:btmh:1220"):
			b, e := hex.DecodeString(topic[len("urn:btmh:1220"):])
			if e != nil || len(b) != 32 || m.Identity.V2 != "" && m.Identity.V2 != hex.EncodeToString(b) {
				return Magnet{}, invalid("magnet_hash_invalid", ErrInvalidMetainfo)
			}
			m.Identity.V2 = hex.EncodeToString(b)
		default:
			return Magnet{}, invalid("magnet_hash_unsupported", ErrUnsupported)
		}
	}
	seen := map[string]bool{}
	for _, rawTracker := range q["tr"] {
		tracker, e := url.Parse(rawTracker)
		if e != nil || len(rawTracker) > 2048 || tracker.Host == "" || tracker.User != nil || tracker.Fragment != "" || tracker.Scheme != "https" && tracker.Scheme != "http" && tracker.Scheme != "udp" {
			return Magnet{}, invalid("tracker_invalid", ErrInvalidMetainfo)
		}
		if !seen[rawTracker] {
			seen[rawTracker] = true
			m.Trackers = append(m.Trackers, rawTracker)
		}
	}
	seen = map[string]bool{}
	for _, peer := range q["x.pe"] {
		if !validHostPort(peer) {
			return Magnet{}, invalid("peer_invalid", ErrInvalidMetainfo)
		}
		if !seen[peer] {
			seen[peer] = true
			m.Peers = append(m.Peers, peer)
		}
	}
	return m, nil
}

func validHostPort(address string) bool {
	if len(address) > 512 || strings.ContainsAny(address, "\x00\r\n\t/%@?#") {
		return false
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil || host == "" {
		return false
	}
	n, err := strconv.ParseUint(port, 10, 16)
	return err == nil && n != 0
}

func (i Identity) ShortHash() ([20]byte, error) {
	var out [20]byte
	full := i.V1
	expected := 20
	if full == "" {
		full = i.V2
		expected = 32
	}
	b, err := hex.DecodeString(full)
	if err != nil || len(b) != expected {
		return out, invalid("identity_invalid", ErrInvalidMetainfo)
	}
	copy(out[:], b)
	return out, nil
}
