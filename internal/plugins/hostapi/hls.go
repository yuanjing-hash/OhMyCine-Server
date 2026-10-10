package hostapi

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/google/uuid"
)

const maxDerivedAssets = 32768

var hlsURIAttribute = regexp.MustCompile(`URI="([^"\r\n]*)"`)

func isHLSAsset(target *url.URL, contentType string) bool {
	if target != nil && strings.HasSuffix(strings.ToLower(target.Path), ".m3u8") {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0])) {
	case "application/vnd.apple.mpegurl", "application/x-mpegurl", "audio/mpegurl", "audio/x-mpegurl":
		return true
	}
	return false
}

// rewriteHLSAsset preserves media streaming and reads only bounded control
// manifests. Every referenced resource belongs to this package generation and
// connection and remains behind Player device authentication.
func (host *Host) rewriteHLSAsset(ctx context.Context, parentRef string, parent Asset, authorization pluginAuthorization, response *http.Response, method, rangeHeader string) (*AssetStream, error) {
	defer response.Body.Close()
	if parent.HLSDepth >= 8 {
		return nil, invalid("plugin_hls_depth_exceeded", nil)
	}
	if response.StatusCode != http.StatusOK || response.Request == nil || response.Request.URL == nil {
		return nil, invalid("plugin_hls_manifest_invalid", nil)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 2*1024*1024+1))
	if err != nil || len(body) > 2*1024*1024 {
		return nil, invalid("plugin_hls_manifest_invalid", nil)
	}
	lines := strings.Split(strings.ReplaceAll(string(body), "\r\n", "\n"), "\n")
	if len(lines) > 50000 || len(lines) == 0 || strings.TrimSpace(strings.TrimPrefix(lines[0], "\ufeff")) != "#EXTM3U" {
		return nil, invalid("plugin_hls_manifest_invalid", nil)
	}
	base := response.Request.URL
	created := []string{}
	complete := false
	defer func() {
		if !complete {
			host.assetsMu.Lock()
			for _, ref := range created {
				delete(host.offlineAssets, ref)
			}
			host.assetsMu.Unlock()
		}
	}()
	publicHosts := map[string]bool{}
	derive := func(uri string) (string, error) {
		child, err := url.Parse(uri)
		if err != nil || uri == "" || len(uri) > 8192 {
			return "", invalid("plugin_hls_manifest_invalid", nil)
		}
		target := base.ResolveReference(child)
		if !allowedAssetURL(target) || !domainAllowed(target.Hostname(), authorization.Permissions) {
			return "", denied("plugin_asset_url_denied", nil)
		}
		if !publicHosts[target.Hostname()] {
			if err := host.requirePublicHost(ctx, target.Hostname()); err != nil {
				return "", err
			}
			publicHosts[target.Hostname()] = true
		}
		asset := parent
		asset.Body = nil
		asset.ContentType = ""
		asset.URL = target.String()
		asset.Headers = parent.Headers.Clone()
		asset.HLSDepth = parent.HLSDepth + 1
		initial, _ := url.Parse(parent.URL)
		if !sameAssetOrigin(initial, target) {
			asset.Headers.Del("Cookie")
			asset.Headers.Del("Authorization")
		}
		ref := uuid.NewSHA1(uuid.NameSpaceURL, []byte("ohmycine:hls:v1:"+parentRef+"|"+target.String())).String()
		host.assetsMu.Lock()
		defer host.assetsMu.Unlock()
		if existing, exists := host.offlineAssets[ref]; exists && existing.ExpiresAt.After(host.now().UTC()) {
			return "/api/v1/player/online-assets/" + ref, nil
		}
		if len(host.offlineAssets) >= maxDerivedAssets {
			for id, existing := range host.offlineAssets {
				if !existing.ExpiresAt.After(host.now().UTC()) {
					delete(host.offlineAssets, id)
				}
			}
		}
		if len(host.offlineAssets) >= maxDerivedAssets {
			return "", invalid("plugin_asset_capacity_exceeded", nil)
		}
		host.offlineAssets[ref] = asset
		created = append(created, ref)
		return "/api/v1/player/online-assets/" + ref, nil
	}
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if len(line) > 8192 || strings.ContainsAny(line, "\r\x00") {
			return nil, invalid("plugin_hls_manifest_invalid", nil)
		}
		if line == "" {
			continue
		}
		// Provider-private extension/comment payloads are not HLS transport
		// instructions. They may contain acquisition tickets or account data.
		if strings.HasPrefix(line, "#") && line != "#EXTM3U" && !strings.HasPrefix(line, "#EXT-X-") && !strings.HasPrefix(line, "#EXTINF:") {
			lines[i] = ""
			continue
		}
		if !strings.HasPrefix(line, "#") {
			value, err := derive(line)
			if err != nil {
				return nil, err
			}
			lines[i] = value
		} else if strings.Contains(line, "URI=") {
			matches := hlsURIAttribute.FindAllStringSubmatchIndex(line, -1)
			if len(matches) != 1 {
				return nil, invalid("plugin_hls_manifest_invalid", nil)
			}
			match := matches[0]
			value, err := derive(line[match[2]:match[3]])
			if err != nil {
				return nil, err
			}
			lines[i] = line[:match[2]] + value + line[match[3]:]
		}
	}
	if err := host.validateAssetOwner(parent); err != nil {
		return nil, err
	}
	result := Asset{Body: []byte(strings.Join(lines, "\n")), ContentType: "application/vnd.apple.mpegurl"}
	if len(result.Body) > 4*1024*1024 {
		return nil, invalid("plugin_hls_manifest_invalid", nil)
	}
	complete = true
	return openInlineAsset(result, method, rangeHeader)
}
