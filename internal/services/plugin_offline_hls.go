package services

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/plugins/contract"
)

type offlineBuilder struct {
	gateway                           PluginOfflineGateway
	pluginID, connectionID, libraryID string
	expiry                            *int64
	units, controls                   int
	keys                              map[string][]byte
	derived                           map[string]bool
}

func (b *offlineBuilder) derive(ctx context.Context, parent, base, uri string) (string, error) {
	ref, err := b.gateway.DeriveOfflineAsset(ctx, b.pluginID, b.connectionID, parent, base, uri)
	if err == nil {
		if b.derived == nil {
			b.derived = map[string]bool{}
		}
		b.derived[ref] = false
	}
	return ref, err
}

func (b *offlineBuilder) cleanup(success bool) {
	for ref, retain := range b.derived {
		if !success || !retain {
			b.gateway.ReleaseOfflineAsset(b.pluginID, b.connectionID, ref)
		}
	}
}

func hlsUnitID(track, kind string, index uint64, base, uri string, byteRange *OfflineByteRange) string {
	baseURL, _ := url.Parse(base)
	child, _ := url.Parse(uri)
	path := baseURL.ResolveReference(child).EscapedPath()
	if byteRange != nil {
		path += fmt.Sprintf("|%d|%d", byteRange.Offset, byteRange.Length)
	}
	digest := sha256.Sum256([]byte(path))
	return fmt.Sprintf("%s:%s:%d:%x", track, kind, index, digest[:8])
}

func (b *offlineBuilder) unit(ctx context.Context, reference, id string, expected int64, byteRange *OfflineByteRange, encryption *OfflineEncryption) (OfflineUnit, error) {
	b.units++
	if b.units > maxOfflineUnits {
		return OfflineUnit{}, errors.New("offline unit limit exceeded")
	}
	transport, err := b.gateway.OfflineAssetTransport(ctx, b.pluginID, b.connectionID, reference)
	if err != nil {
		return OfflineUnit{}, err
	}
	if expiry := transport.ExpiresAt.Unix(); expiry < *b.expiry {
		*b.expiry = expiry
	}
	url := transport.URL
	if transport.Gateway {
		url = "/api/v1/player/online-libraries/" + b.libraryID + "/offline-assets/" + reference
		if b.derived != nil {
			b.derived[reference] = true
		}
	} else {
		b.gateway.ReleaseOfflineAsset(b.pluginID, b.connectionID, reference)
	}
	if byteRange != nil {
		expected = byteRange.Length
	}
	return OfflineUnit{ID: id, URL: url, Headers: transport.Headers, ExpectedBytes: expected, ByteRange: byteRange, Encryption: encryption}, nil
}

func (b *offlineBuilder) control(ctx context.Context, reference string, max int64) ([]byte, string, error) {
	b.controls++
	if b.controls > 48 {
		return nil, "", errors.New("offline control limit exceeded")
	}
	// Controls also constrain the whole plan to the key/manifest lifetime.
	transport, err := b.gateway.OfflineAssetTransport(ctx, b.pluginID, b.connectionID, reference)
	if err != nil {
		return nil, "", err
	}
	if expiry := transport.ExpiresAt.Unix(); expiry < *b.expiry {
		*b.expiry = expiry
	}
	return b.gateway.ReadOfflineControl(ctx, b.pluginID, b.connectionID, reference, max)
}

func (b *offlineBuilder) hlsTracks(ctx context.Context, asset contract.DownloadAsset, depth int) ([]OfflineTrack, error) {
	if depth > 2 {
		return nil, errors.New("HLS nesting limit exceeded")
	}
	body, base, err := b.control(ctx, asset.URLRef, 2*1024*1024)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.ReplaceAll(string(body), "\r\n", "\n"), "\n")
	if len(lines) > 50000 || len(lines) == 0 || strings.TrimSpace(strings.TrimPrefix(lines[0], "\ufeff")) != "#EXTM3U" {
		return nil, errors.New("HLS header invalid")
	}
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
		if len(lines[i]) > 8192 || strings.ContainsAny(lines[i], "\r\x00") {
			return nil, errors.New("HLS line invalid")
		}
	}
	master := false
	for _, line := range lines {
		if strings.HasPrefix(line, "#EXT-X-STREAM-INF:") {
			master = true
		}
	}
	if master {
		return b.hlsMaster(ctx, asset, depth, lines, base)
	}
	track, err := b.hlsMedia(ctx, asset, lines, base)
	if err != nil {
		return nil, err
	}
	return []OfflineTrack{track}, nil
}

func (b *offlineBuilder) hlsMaster(ctx context.Context, asset contract.DownloadAsset, depth int, lines []string, base string) ([]OfflineTrack, error) {
	var stream map[string]string
	streamURI := ""
	var audios []map[string]string
	pending := false
	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, "#EXT-X-STREAM-INF:"):
			if stream != nil || pending {
				return nil, errors.New("HLS representation is ambiguous")
			}
			var err error
			stream, err = hlsAttributes(strings.TrimPrefix(line, "#EXT-X-STREAM-INF:"))
			if err != nil {
				return nil, err
			}
			pending = true
		case strings.HasPrefix(line, "#EXT-X-MEDIA:"):
			attrs, err := hlsAttributes(strings.TrimPrefix(line, "#EXT-X-MEDIA:"))
			if err != nil {
				return nil, err
			}
			if attrs["TYPE"] == "AUDIO" {
				audios = append(audios, attrs)
			}
		case strings.HasPrefix(line, "#EXT-X-SESSION-KEY:"):
			return nil, errors.New("HLS session encryption unsupported")
		case strings.HasPrefix(line, "#EXTINF:") || strings.HasPrefix(line, "#EXT-X-MAP:"):
			return nil, errors.New("mixed HLS master/media playlist")
		case line != "" && !strings.HasPrefix(line, "#"):
			if !pending || streamURI != "" {
				return nil, errors.New("HLS master URI invalid")
			}
			streamURI, pending = line, false
		}
	}
	if streamURI == "" || pending || asset.Kind != "video" {
		return nil, errors.New("HLS master invalid")
	}
	ref, err := b.derive(ctx, asset.URLRef, base, streamURI)
	if err != nil {
		return nil, err
	}
	child := asset
	child.URLRef = ref
	tracks, err := b.hlsTracks(ctx, child, depth+1)
	if err != nil {
		return nil, err
	}
	if group := stream["AUDIO"]; group != "" {
		var matches []map[string]string
		for _, audio := range audios {
			if audio["GROUP-ID"] == group {
				matches = append(matches, audio)
			}
		}
		if len(matches) != 1 {
			return nil, errors.New("HLS audio is ambiguous")
		}
		if uri := matches[0]["URI"]; uri != "" {
			audioRef, err := b.derive(ctx, asset.URLRef, base, uri)
			if err != nil {
				return nil, err
			}
			audio, err := b.hlsTracks(ctx, contract.DownloadAsset{ID: asset.ID + ":audio", Kind: "audio", URLRef: audioRef}, depth+1)
			if err != nil {
				return nil, err
			}
			tracks = append(tracks, audio...)
		}
	}
	return tracks, nil
}

type hlsEncryptionState struct {
	key []byte
	iv  string
}

func (b *offlineBuilder) hlsMedia(ctx context.Context, asset contract.DownloadAsset, lines []string, base string) (OfflineTrack, error) {
	track := OfflineTrack{ID: asset.ID, Kind: asset.Kind, Units: []OfflineUnit{}}
	sequence := uint64(0)
	seenSequence, seenMedia, end, pendingMedia := false, false, false, false
	mapIndex := 0
	mapURI, previousURI := "", ""
	previousEnd := int64(0)
	var pendingRange string
	var encryption *hlsEncryptionState
	for _, line := range lines[1:] {
		if line == "" {
			continue
		}
		if end && !strings.HasPrefix(line, "#") {
			return OfflineTrack{}, errors.New("HLS content after ENDLIST")
		}
		switch {
		case line == "#EXT-X-ENDLIST":
			if end || pendingMedia || pendingRange != "" {
				return OfflineTrack{}, errors.New("HLS ENDLIST invalid")
			}
			end = true
		case strings.HasPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"):
			if seenSequence || seenMedia {
				return OfflineTrack{}, errors.New("HLS sequence invalid")
			}
			value, err := strconv.ParseUint(strings.TrimPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"), 10, 64)
			if err != nil {
				return OfflineTrack{}, errors.New("HLS sequence invalid")
			}
			sequence, seenSequence = value, true
		case strings.HasPrefix(line, "#EXTINF:"):
			if pendingMedia || end {
				return OfflineTrack{}, errors.New("HLS duration invalid")
			}
			value := strings.SplitN(strings.TrimPrefix(line, "#EXTINF:"), ",", 2)[0]
			duration, err := strconv.ParseFloat(value, 64)
			if err != nil || math.IsNaN(duration) || math.IsInf(duration, 0) || duration <= 0 || duration > 86400 {
				return OfflineTrack{}, errors.New("HLS duration invalid")
			}
			pendingMedia = true
		case strings.HasPrefix(line, "#EXT-X-BYTERANGE:"):
			if pendingRange != "" {
				return OfflineTrack{}, errors.New("HLS range duplicated")
			}
			pendingRange = strings.TrimPrefix(line, "#EXT-X-BYTERANGE:")
		case strings.HasPrefix(line, "#EXT-X-KEY:"):
			attrs, err := hlsAttributes(strings.TrimPrefix(line, "#EXT-X-KEY:"))
			if err != nil {
				return OfflineTrack{}, err
			}
			if attrs["KEYFORMAT"] != "" && attrs["KEYFORMAT"] != "identity" {
				return OfflineTrack{}, errors.New("HLS DRM unsupported")
			}
			switch attrs["METHOD"] {
			case "NONE":
				encryption = nil
			case "AES-128":
				if attrs["URI"] == "" || (attrs["KEYFORMATVERSIONS"] != "" && attrs["KEYFORMATVERSIONS"] != "1") {
					return OfflineTrack{}, errors.New("HLS encryption invalid")
				}
				iv, err := normalizeHLSIV(attrs["IV"])
				if err != nil {
					return OfflineTrack{}, err
				}
				key, exists := b.keys[base+"\x00"+attrs["URI"]]
				if !exists {
					if len(b.keys) >= 32 {
						return OfflineTrack{}, errors.New("HLS key limit exceeded")
					}
					ref, err := b.derive(ctx, asset.URLRef, base, attrs["URI"])
					if err != nil {
						return OfflineTrack{}, err
					}
					key, _, err = b.control(ctx, ref, 16)
					if err != nil || len(key) != 16 {
						return OfflineTrack{}, errors.New("HLS key invalid")
					}
					b.keys[base+"\x00"+attrs["URI"]] = key
				}
				encryption = &hlsEncryptionState{key: key, iv: iv}
			default:
				return OfflineTrack{}, errors.New("HLS encryption unsupported")
			}
		case strings.HasPrefix(line, "#EXT-X-MAP:"):
			attrs, err := hlsAttributes(strings.TrimPrefix(line, "#EXT-X-MAP:"))
			if err != nil || attrs["URI"] == "" {
				return OfflineTrack{}, errors.New("HLS map invalid")
			}
			var byteRange *OfflineByteRange
			if value := attrs["BYTERANGE"]; value != "" {
				// A map has no implicit previous media range; require its offset.
				byteRange, err = hlsByteRange(value, 0, false)
				if err != nil {
					return OfflineTrack{}, err
				}
			}
			crypto, err := hlsUnitEncryption(encryption, sequence, true)
			if err != nil {
				return OfflineTrack{}, err
			}
			identity := attrs["URI"] + "|" + attrs["BYTERANGE"] + "|"
			if crypto != nil {
				identity += crypto.IVHex + "|" + crypto.KeyBase64
			}
			if identity != mapURI {
				ref, err := b.derive(ctx, asset.URLRef, base, attrs["URI"])
				if err != nil {
					return OfflineTrack{}, err
				}
				unit, err := b.unit(ctx, ref, hlsUnitID(asset.ID, "map", uint64(mapIndex), base, attrs["URI"], byteRange), 0, byteRange, crypto)
				if err != nil {
					return OfflineTrack{}, err
				}
				track.Units = append(track.Units, unit)
				mapIndex++
				mapURI = identity
			}
		case strings.HasPrefix(line, "#EXT-X-GAP") || strings.HasPrefix(line, "#EXT-X-PART") || strings.HasPrefix(line, "#EXT-X-PRELOAD-HINT") || strings.HasPrefix(line, "#EXT-X-SKIP") || strings.HasPrefix(line, "#EXT-X-SESSION-KEY") || strings.HasPrefix(line, "#EXT-X-STREAM-INF"):
			return OfflineTrack{}, errors.New("HLS incomplete or unsupported playlist")
		case line == "#EXT-X-DISCONTINUITY":
			return OfflineTrack{}, errors.New("HLS discontinuity requires unsupported timeline remux")
		case !strings.HasPrefix(line, "#"):
			if !pendingMedia || sequence == math.MaxUint64 {
				return OfflineTrack{}, errors.New("HLS segment invalid")
			}
			var byteRange *OfflineByteRange
			var err error
			if pendingRange != "" {
				byteRange, err = hlsByteRange(pendingRange, previousEnd, previousURI == line && previousEnd > 0)
				if err != nil {
					return OfflineTrack{}, err
				}
			}
			crypto, err := hlsUnitEncryption(encryption, sequence, false)
			if err != nil {
				return OfflineTrack{}, err
			}
			ref, err := b.derive(ctx, asset.URLRef, base, line)
			if err != nil {
				return OfflineTrack{}, err
			}
			unit, err := b.unit(ctx, ref, hlsUnitID(asset.ID, "segment", sequence, base, line, byteRange), 0, byteRange, crypto)
			if err != nil {
				return OfflineTrack{}, err
			}
			track.Units = append(track.Units, unit)
			previousURI, previousEnd = line, 0
			if byteRange != nil {
				previousEnd = byteRange.Offset + byteRange.Length
			}
			sequence++
			pendingMedia, pendingRange, seenMedia = false, "", true
		}
	}
	if !end || !seenMedia || pendingMedia || len(track.Units) == 0 {
		return OfflineTrack{}, errors.New("HLS requires complete VOD playlist")
	}
	return track, nil
}

func hlsUnitEncryption(state *hlsEncryptionState, sequence uint64, initialization bool) (*OfflineEncryption, error) {
	if state == nil {
		return nil, nil
	}
	iv := state.iv
	if iv == "" {
		if initialization {
			return nil, errors.New("encrypted HLS map requires explicit IV")
		}
		iv = fmt.Sprintf("%032x", sequence)
	}
	return &OfflineEncryption{Method: "aes-128", KeyBase64: base64.StdEncoding.EncodeToString(state.key), IVHex: iv}, nil
}

func normalizeHLSIV(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if !strings.HasPrefix(value, "0x") || len(value) < 3 || len(value) > 34 {
		return "", errors.New("HLS IV invalid")
	}
	value = strings.Repeat("0", 32-len(value[2:])) + value[2:]
	if _, err := hex.DecodeString(value); err != nil {
		return "", errors.New("HLS IV invalid")
	}
	return strings.ToLower(value), nil
}

func hlsByteRange(value string, previousEnd int64, allowImplicit bool) (*OfflineByteRange, error) {
	parts := strings.Split(value, "@")
	if len(parts) > 2 {
		return nil, errors.New("HLS range invalid")
	}
	length, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || length <= 0 || length > 2*1024*1024*1024 {
		return nil, errors.New("HLS range invalid")
	}
	offset := previousEnd
	if len(parts) == 2 {
		offset, err = strconv.ParseInt(parts[1], 10, 64)
	} else if !allowImplicit {
		return nil, errors.New("HLS range offset missing")
	}
	if err != nil || offset < 0 || offset > math.MaxInt64-length {
		return nil, errors.New("HLS range invalid")
	}
	return &OfflineByteRange{Offset: offset, Length: length}, nil
}

func hlsAttributes(value string) (map[string]string, error) {
	result := map[string]string{}
	for value != "" {
		index := strings.IndexByte(value, '=')
		if index < 1 {
			return nil, errors.New("HLS attributes invalid")
		}
		key := value[:index]
		value = value[index+1:]
		if _, exists := result[key]; exists {
			return nil, errors.New("HLS attribute duplicated")
		}
		var item string
		if strings.HasPrefix(value, "\"") {
			index = strings.IndexByte(value[1:], '"')
			if index < 0 {
				return nil, errors.New("HLS quoted attribute invalid")
			}
			item, value = value[1:index+1], value[index+2:]
			if value != "" {
				if value[0] != ',' {
					return nil, errors.New("HLS attributes invalid")
				}
				value = value[1:]
			}
		} else {
			index = strings.IndexByte(value, ',')
			if index < 0 {
				item, value = value, ""
			} else {
				item, value = value[:index], value[index+1:]
			}
		}
		if key == "" || item == "" || len(result) > 32 {
			return nil, errors.New("HLS attributes invalid")
		}
		result[key] = item
	}
	return result, nil
}
