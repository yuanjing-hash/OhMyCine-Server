package hostapi

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/plugins/contract"
)

type offlineProjectionKey struct{}
type offlinePublicObservation struct {
	host *Host
	name string
}
type offlineProjectionCache struct {
	mu     sync.Mutex
	public map[offlinePublicObservation]time.Time
}

// WithOfflineProjection shares short public-DNS observations while expanding
// one bounded control graph. Actual HTTP dialing and redirects never use this
// cache and still resolve/check every request. All child domains are checked
// against current grants before this helper is used.
func WithOfflineProjection(ctx context.Context) context.Context {
	return context.WithValue(ctx, offlineProjectionKey{}, &offlineProjectionCache{public: map[offlinePublicObservation]time.Time{}})
}

func (host *Host) requireOfflineProjectionPublicHost(ctx context.Context, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	cache, ok := ctx.Value(offlineProjectionKey{}).(*offlineProjectionCache)
	if !ok {
		return host.requirePublicHost(ctx, name)
	}
	key := offlinePublicObservation{host: host, name: strings.ToLower(name)}
	cache.mu.Lock()
	observed, exists := cache.public[key]
	cache.mu.Unlock()
	if exists && host.now().UTC().Sub(observed) >= 0 && host.now().UTC().Sub(observed) < 5*time.Second {
		return nil
	}
	if err := host.requirePublicHost(ctx, name); err != nil {
		return err
	}
	cache.mu.Lock()
	if len(cache.public) < 64 || exists {
		cache.public[key] = host.now().UTC()
	}
	cache.mu.Unlock()
	return nil
}

// OfflineTransport is private native transport state, never a browse/playback
// DTO or a persisted task field. Sensitive headers always stay in the Host.
type OfflineTransport struct {
	URL       string
	Headers   map[string]string
	Gateway   bool
	ExpiresAt time.Time
}

func sameAssetOrigin(a, b *url.URL) bool {
	port := func(u *url.URL) string {
		if u.Port() == "" {
			return "443"
		}
		return u.Port()
	}
	return a != nil && b != nil && a.Scheme == b.Scheme && strings.EqualFold(a.Hostname(), b.Hostname()) && port(a) == port(b)
}

func (host *Host) offlineAsset(pluginID, connectionID, reference string) (Asset, pluginAuthorization, error) {
	asset, err := host.ResolveAsset(reference)
	if err != nil {
		return Asset{}, pluginAuthorization{}, err
	}
	if asset.Artwork || asset.PluginID != pluginID || asset.ConnectionID != connectionID {
		return Asset{}, pluginAuthorization{}, denied("plugin_asset_reference_denied", nil)
	}
	authorization, err := host.authorization(pluginID)
	if err != nil {
		return Asset{}, pluginAuthorization{}, err
	}
	allowed := false
	for _, permission := range authorization.Permissions {
		if permission.Kind == contract.PermissionDownloadPlan {
			allowed = true
		}
	}
	if !allowed {
		return Asset{}, pluginAuthorization{}, denied("plugin_offline_permission_denied", nil)
	}
	// References selected by an authorized offline operation cannot be
	// replayed through the ordinary read-only playback gateway.
	asset.OfflineOnly = true
	host.assetsMu.Lock()
	if _, exists := host.assets[reference]; exists {
		host.assets[reference] = asset
	} else if _, exists := host.offlineAssets[reference]; exists {
		host.offlineAssets[reference] = asset
	}
	host.assetsMu.Unlock()
	return asset, authorization, nil
}

// OfflineAssetTransport exports only credential-free CDN requirements. An
// inline body or any unknown/private header uses the authenticated gateway.
func (host *Host) OfflineAssetTransport(ctx context.Context, pluginID, connectionID, reference string) (OfflineTransport, error) {
	asset, authorization, err := host.offlineAsset(pluginID, connectionID, reference)
	if err != nil {
		return OfflineTransport{}, err
	}
	result := OfflineTransport{ExpiresAt: asset.ExpiresAt, Gateway: asset.URL == ""}
	if asset.URL != "" {
		target, err := url.Parse(asset.URL)
		if err != nil || !allowedAssetURL(target) || !domainAllowed(target.Hostname(), authorization.Permissions) {
			return OfflineTransport{}, denied("plugin_asset_url_denied", err)
		}
		if err := host.requireOfflineProjectionPublicHost(ctx, target.Hostname()); err != nil {
			return OfflineTransport{}, err
		}
	}
	for name, values := range asset.Headers {
		if len(values) != 1 {
			result.Gateway = true
			break
		}
		switch http.CanonicalHeaderKey(name) {
		case "Accept", "User-Agent":
		case "Referer":
			referer, parseErr := url.Parse(values[0])
			if parseErr != nil || !allowedAssetURL(referer) || referer.RawQuery != "" {
				result.Gateway = true
			}
		default:
			result.Gateway = true
		}
	}
	if !result.Gateway {
		result.URL = asset.URL
		result.Headers = make(map[string]string, len(asset.Headers))
		for name, values := range asset.Headers {
			result.Headers[name] = values[0]
		}
	}
	return result, nil
}

// ReadOfflineControl reads bounded manifests/keys, through the same validated
// transport as media assets. The final URL is in-process only for URI resolution.
func (host *Host) ReadOfflineControl(ctx context.Context, pluginID, connectionID, reference string, maximum int64) ([]byte, string, error) {
	if maximum < 1 || maximum > 2*1024*1024 {
		return nil, "", invalid("plugin_offline_control_invalid", nil)
	}
	asset, authorization, err := host.offlineAsset(pluginID, connectionID, reference)
	if err != nil {
		return nil, "", err
	}
	if asset.URL == "" {
		return nil, "", invalid("plugin_offline_control_invalid", nil)
	}
	target, err := url.Parse(asset.URL)
	if err != nil || !allowedAssetURL(target) || !domainAllowed(target.Hostname(), authorization.Permissions) {
		return nil, "", denied("plugin_asset_url_denied", err)
	}
	if err := host.requirePublicHost(ctx, target.Hostname()); err != nil {
		return nil, "", err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, "", invalid("plugin_offline_control_invalid", err)
	}
	request.Header = asset.Headers.Clone()
	client := host.clientForPermissions(authorization.Permissions, true)
	response, err := client.Do(request)
	if err != nil {
		return nil, "", invalid("plugin_offline_control_unavailable", nil)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, "", invalid("plugin_offline_control_unavailable", nil)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maximum+1))
	if err != nil || int64(len(body)) > maximum {
		return nil, "", invalid("plugin_offline_control_invalid", nil)
	}
	if err := host.validateAssetOwner(asset); err != nil {
		return nil, "", err
	}
	return body, response.Request.URL.String(), nil
}

// DeriveOfflineAsset resolves a manifest child and repeats current package,
// download grant, domain and public-address checks. Children cannot outlive the
// parent; cross-origin children cannot inherit provider credentials.
func (host *Host) DeriveOfflineAsset(ctx context.Context, pluginID, connectionID, parentRef, baseURL, childURI string) (string, error) {
	asset, authorization, err := host.offlineAsset(pluginID, connectionID, parentRef)
	if err != nil {
		return "", err
	}
	base, err := url.Parse(baseURL)
	if err != nil || !allowedAssetURL(base) || !domainAllowed(base.Hostname(), authorization.Permissions) {
		return "", denied("plugin_asset_url_denied", err)
	}
	child, err := url.Parse(childURI)
	if err != nil || childURI == "" || len(childURI) > 8192 {
		return "", invalid("plugin_offline_control_invalid", err)
	}
	target := base.ResolveReference(child)
	if !allowedAssetURL(target) || !domainAllowed(target.Hostname(), authorization.Permissions) {
		return "", denied("plugin_asset_url_denied", nil)
	}
	if err := host.requireOfflineProjectionPublicHost(ctx, target.Hostname()); err != nil {
		return "", err
	}
	initial, _ := url.Parse(asset.URL)
	if !sameAssetOrigin(initial, target) {
		asset.Headers.Del("Cookie")
		asset.Headers.Del("Authorization")
	}
	asset.URL, asset.Body, asset.ContentType = target.String(), nil, ""
	reference := uuid.NewString()
	host.assetsMu.Lock()
	defer host.assetsMu.Unlock()
	now := host.now().UTC()
	if len(host.offlineAssets) >= maxDerivedAssets {
		for key, current := range host.offlineAssets {
			if !current.ExpiresAt.After(now) {
				delete(host.offlineAssets, key)
			}
		}
	}
	if len(host.offlineAssets) >= maxDerivedAssets {
		return "", invalid("plugin_asset_capacity_exceeded", nil)
	}
	host.offlineAssets[reference] = asset
	return reference, nil
}

// ReleaseOfflineAsset removes used private control/transport references,
// including roots marked by the offline operation. Ordinary playback survives.
func (host *Host) ReleaseOfflineAsset(pluginID, connectionID, reference string) {
	host.assetsMu.Lock()
	defer host.assetsMu.Unlock()
	asset, exists := host.offlineAssets[reference]
	if exists && asset.OfflineOnly && asset.PluginID == pluginID && asset.ConnectionID == connectionID {
		delete(host.offlineAssets, reference)
	}
	asset, exists = host.assets[reference]
	if exists && asset.OfflineOnly && asset.PluginID == pluginID && asset.ConnectionID == connectionID {
		delete(host.assets, reference)
	}
}

func (host *Host) OpenOfflineAsset(ctx context.Context, pluginID, connectionID, reference, method, rangeHeader string) (*AssetStream, error) {
	if _, _, err := host.offlineAsset(pluginID, connectionID, reference); err != nil {
		return nil, err
	}
	return host.openAsset(ctx, reference, method, rangeHeader, false, true)
}
