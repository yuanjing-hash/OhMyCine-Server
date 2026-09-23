package hostapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"mime"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

const (
	maxPluginArtworkBytes = 5 << 20
	pluginArtworkTTL      = 15 * time.Minute
)

// RegisterArtwork turns an untrusted plugin image URL into a same-origin opaque
// reference. The active package grants, connection and public DNS destination
// are checked both here and again when the image is fetched.
func (host *Host) RegisterArtwork(ctx context.Context, pluginID, connectionID, upstream string) (string, error) {
	if _, err := uuid.Parse(connectionID); err != nil || len(upstream) == 0 || len(upstream) > 2048 {
		return "", denied("plugin_artwork_url_denied", err)
	}
	authorization, err := host.authorization(pluginID)
	if err != nil {
		return "", err
	}
	var connection models.PluginConnection
	if err := host.db.WithContext(ctx).First(&connection, "id = ? AND plugin_id = ? AND enabled = ?", connectionID, pluginID, true).Error; err != nil {
		return "", denied("plugin_artwork_connection_denied", err)
	}
	target, err := url.Parse(upstream)
	if err != nil || !allowedAssetURL(target) || !domainAllowed(target.Hostname(), authorization.Permissions) {
		return "", denied("plugin_artwork_url_denied", err)
	}
	if err := host.requirePublicHost(ctx, target.Hostname()); err != nil {
		return "", err
	}
	// Stable during this Server process, but neither the URL nor its query
	// string can be recovered from the reference sent to Player.
	identity := fmt.Sprintf("%s\x00%s\x00%d\x00%d\x00%s", pluginID, connectionID, authorization.PackageID, authorization.RuntimeGeneration, target.String())
	reference := uuid.NewSHA1(host.artworkNamespace, []byte(identity)).String()
	now := host.now().UTC()
	host.assetsMu.Lock()
	defer host.assetsMu.Unlock()
	for key, asset := range host.assets {
		if !asset.ExpiresAt.After(now) {
			delete(host.assets, key)
		}
	}
	if existing, ok := host.assets[reference]; !ok && len(host.assets) >= 4096 {
		return "", invalid("plugin_asset_capacity_exceeded", nil)
	} else if ok && (existing.PluginID != pluginID || existing.ConnectionID != connectionID || existing.URL != target.String()) {
		return "", denied("plugin_artwork_reference_denied", nil)
	}
	host.assets[reference] = Asset{
		PluginID: pluginID, ConnectionID: connectionID, PackageID: authorization.PackageID,
		RuntimeGeneration: authorization.RuntimeGeneration, URL: target.String(),
		ExpiresAt: now.Add(pluginArtworkTTL), Artwork: true,
	}
	return reference, nil
}

// OpenArtwork uses the existing plugin asset transport so all redirects,
// grants and resolved IPs are rechecked. Unlike video assets, image responses
// are fully bounded and their MIME type must agree with their signature.
func (host *Host) OpenArtwork(ctx context.Context, reference string) ([]byte, string, error) {
	asset, err := host.ResolveAsset(reference)
	if err != nil {
		return nil, "", err
	}
	if !asset.Artwork {
		return nil, "", denied("plugin_artwork_reference_denied", nil)
	}
	stream, err := host.openAsset(ctx, reference, "GET", "", true)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = stream.Body.Close() }()
	if stream.StatusCode != 200 {
		return nil, "", invalid("plugin_artwork_upstream_invalid", nil)
	}
	contentType, _, err := mime.ParseMediaType(stream.Header.Get("Content-Type"))
	if err != nil || !supportedArtworkMIME(contentType) {
		return nil, "", invalid("plugin_artwork_type_invalid", err)
	}
	if stream.Header.Get("Content-Length") != "" {
		// The read limit below is authoritative; this only avoids retaining
		// an obviously oversized upstream response.
		var length int64
		if _, err := fmt.Sscan(stream.Header.Get("Content-Length"), &length); err == nil && length > maxPluginArtworkBytes {
			return nil, "", invalid("plugin_artwork_size_invalid", nil)
		}
	}
	body, err := io.ReadAll(io.LimitReader(stream.Body, maxPluginArtworkBytes+1))
	if err != nil || len(body) == 0 || len(body) > maxPluginArtworkBytes || !artworkSignatureMatches(contentType, body) {
		return nil, "", invalid("plugin_artwork_body_invalid", err)
	}
	return body, contentType, nil
}

func supportedArtworkMIME(contentType string) bool {
	switch contentType {
	case "image/jpeg", "image/png", "image/webp", "image/avif":
		return true
	default:
		return false
	}
}

func artworkSignatureMatches(contentType string, body []byte) bool {
	switch contentType {
	case "image/jpeg":
		return len(body) >= 3 && bytes.Equal(body[:3], []byte{0xff, 0xd8, 0xff})
	case "image/png":
		return len(body) >= 8 && bytes.Equal(body[:8], []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a})
	case "image/webp":
		return len(body) >= 12 && string(body[:4]) == "RIFF" && string(body[8:12]) == "WEBP"
	case "image/avif":
		return len(body) >= 16 && string(body[4:8]) == "ftyp" && (strings.HasPrefix(string(body[8:12]), "avif") || strings.HasPrefix(string(body[8:12]), "avis") || avifCompatibleBrand(body))
	default:
		return false
	}
}

func avifCompatibleBrand(body []byte) bool {
	if len(body) < 24 {
		return false
	}
	size := binary.BigEndian.Uint32(body[:4])
	if size < 24 || int(size) > len(body) {
		return false
	}
	for offset := 16; offset+4 <= int(size); offset += 4 {
		if string(body[offset:offset+4]) == "avif" || string(body[offset:offset+4]) == "avis" {
			return true
		}
	}
	return false
}
