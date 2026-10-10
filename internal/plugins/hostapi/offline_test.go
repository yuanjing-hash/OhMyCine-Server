package hostapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/plugins/contract"
)

func TestOfflineActualHostProjectionPerformance(t *testing.T) {
	if os.Getenv("OMC_OFFLINE_PERFORMANCE") != "1" {
		t.Skip("opt-in bounded actual Host projection measurement")
	}
	fixture := newHostFixture(t, []contract.Permission{{Kind: contract.PermissionNetworkHTTP, Domains: []string{"cdn.example.test"}}, {Kind: contract.PermissionDownloadPlan}})
	resolutions := 0
	host := New(fixture.db, fixture.credentials, zerolog.Nop(), WithResolver(func(ctx context.Context, name string) ([]net.IPAddr, error) {
		resolutions++
		return publicResolver(ctx, name)
	}))
	payload, _ := json.Marshal(assetRegisterRequest{ConnectionID: fixture.connection.ID, URL: "https://cdn.example.test/v.m3u8", TTLSeconds: 120})
	response, err := host.Call(context.Background(), fixture.pluginID, OperationAssetRegister, payload)
	if err != nil {
		t.Fatal(err)
	}
	ref := assetReference(t, response)
	for _, count := range []int{758, 8192} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			ctx = WithOfflineProjection(ctx)
			started, before := time.Now(), resolutions
			for i := 0; i < count; i++ {
				child, err := host.DeriveOfflineAsset(ctx, fixture.pluginID, fixture.connection.ID, ref, "https://cdn.example.test/v.m3u8", fmt.Sprintf("segment%d.ts?short=opaque", i))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := host.OfflineAssetTransport(ctx, fixture.pluginID, fixture.connection.ID, child); err != nil {
					t.Fatal(err)
				}
				host.ReleaseOfflineAsset(fixture.pluginID, fixture.connection.ID, child)
				if ctx.Err() != nil {
					t.Fatal("actual Host projection exceeded online budget")
				}
			}
			t.Logf("actual Host SQLite ownership/grant/derive/transport: units=%d elapsed=%s public_resolver_calls=%d; no media/control network fetch", count, time.Since(started), resolutions-before)
		})
	}
}

func TestOfflineProjectionDNSCacheIsScopedShortAndNotUsedForReads(t *testing.T) {
	fixture := newHostFixture(t, []contract.Permission{{Kind: contract.PermissionNetworkHTTP, Domains: []string{"cdn.example.test"}}, {Kind: contract.PermissionDownloadPlan}})
	private := false
	calls := 0
	host := New(fixture.db, fixture.credentials, zerolog.Nop(), WithResolver(func(ctx context.Context, name string) ([]net.IPAddr, error) {
		calls++
		if private {
			return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
		}
		return publicResolver(ctx, name)
	}))
	payload, _ := json.Marshal(assetRegisterRequest{ConnectionID: fixture.connection.ID, URL: "https://cdn.example.test/v.m3u8", TTLSeconds: 120})
	response, err := host.Call(context.Background(), fixture.pluginID, OperationAssetRegister, payload)
	if err != nil {
		t.Fatal(err)
	}
	ref := assetReference(t, response)
	ctx := WithOfflineProjection(context.Background())
	if _, err := host.OfflineAssetTransport(ctx, fixture.pluginID, fixture.connection.ID, ref); err != nil {
		t.Fatal(err)
	}
	before := calls
	if _, err := host.DeriveOfflineAsset(ctx, fixture.pluginID, fixture.connection.ID, ref, "https://cdn.example.test/v.m3u8", "segment.ts"); err != nil || calls != before {
		t.Fatal("same-host metadata cache not reused")
	}
	private = true
	if _, _, err := host.ReadOfflineControl(ctx, fixture.pluginID, fixture.connection.ID, ref, 1024); err == nil {
		t.Fatal("control read reused public metadata cache after private rebinding")
	}
	if _, err := host.OfflineAssetTransport(context.Background(), fixture.pluginID, fixture.connection.ID, ref); err == nil {
		t.Fatal("cache escaped plan context")
	}
	clock := host.now().UTC().Add(6 * time.Second)
	host.now = func() time.Time { return clock }
	if _, err := host.OfflineAssetTransport(ctx, fixture.pluginID, fixture.connection.ID, ref); err == nil {
		t.Fatal("public metadata cache remained valid beyond five seconds")
	}
}

func TestOfflineTransportProtectsHeadersOwnershipAndGeneration(t *testing.T) {
	fixture := newHostFixture(t, []contract.Permission{{Kind: contract.PermissionNetworkHTTP, Domains: []string{"cdn.example.test", "api.example.test"}}, {Kind: contract.PermissionDownloadPlan}})
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("abcdefghijklmnop")), Request: request}, nil
	})}
	host := New(fixture.db, fixture.credentials, zerolog.Nop(), WithHTTPClient(client), WithResolver(publicResolver))
	register := func(headers map[string]string) string {
		payload, _ := json.Marshal(assetRegisterRequest{ConnectionID: fixture.connection.ID, URL: "https://cdn.example.test/v.m3u8?short=opaque", Headers: headers, TTLSeconds: 120})
		response, err := host.Call(context.Background(), fixture.pluginID, OperationAssetRegister, payload)
		if err != nil {
			t.Fatal(err)
		}
		return assetReference(t, response)
	}
	direct := register(map[string]string{"Referer": "https://api.example.test/watch/1", "User-Agent": "Fixture"})
	transport, err := host.OfflineAssetTransport(context.Background(), fixture.pluginID, fixture.connection.ID, direct)
	if err != nil || transport.Gateway || transport.URL == "" || transport.Headers["Referer"] == "" {
		t.Fatalf("credential-free transport invalid: %+v %v", transport, err)
	}
	private := register(map[string]string{"Referer": "https://api.example.test/?session=opaque"})
	// Current registration rejects Cookie/Authorization entirely. Defense in
	// depth also covers a future trusted Host-internal credential transport.
	host.assetsMu.Lock()
	privateAsset := host.assets[private]
	privateAsset.Headers.Set("Cookie", "session=sensitive")
	privateAsset.Headers.Set("Authorization", "Bearer sensitive")
	host.assets[private] = privateAsset
	host.assetsMu.Unlock()
	transport, err = host.OfflineAssetTransport(context.Background(), fixture.pluginID, fixture.connection.ID, private)
	if err != nil || !transport.Gateway || transport.URL != "" || len(transport.Headers) != 0 {
		t.Fatalf("private transport leaked: %+v %v", transport, err)
	}
	if _, err := host.OpenAsset(context.Background(), private, http.MethodGet, ""); ErrorCode(err) != "plugin_offline_reference_denied" {
		t.Fatal("known offline root bypassed download scope on generic playback route")
	}
	if _, err := host.OfflineAssetTransport(context.Background(), fixture.pluginID, "other", private); ErrorCode(err) != "plugin_asset_reference_denied" {
		t.Fatal("cross-connection transport accepted")
	}
	derived, err := host.DeriveOfflineAsset(context.Background(), fixture.pluginID, fixture.connection.ID, private, "https://cdn.example.test/v.m3u8", "https://api.example.test/segment.ts")
	if err != nil {
		t.Fatal(err)
	}
	child, err := host.ResolveAsset(derived)
	if err != nil || child.Headers.Get("Cookie") != "" || child.Headers.Get("Authorization") != "" {
		t.Fatal("credentials crossed derived origin")
	}
	if _, err := host.OpenAsset(context.Background(), derived, http.MethodGet, ""); ErrorCode(err) != "plugin_offline_reference_denied" {
		t.Fatal("known offline child bypassed download scope on generic playback route")
	}
	privateStream, err := host.OpenOfflineAsset(context.Background(), fixture.pluginID, fixture.connection.ID, derived, http.MethodGet, "")
	if err != nil {
		t.Fatal(err)
	}
	privateStream.Body.Close()
	parent, _ := host.ResolveAsset(private)
	if !child.ExpiresAt.Equal(parent.ExpiresAt) {
		t.Fatal("child outlives parent")
	}
	host.ReleaseOfflineAsset(fixture.pluginID, fixture.connection.ID, derived)
	host.ReleaseOfflineAsset(fixture.pluginID, fixture.connection.ID, private)
	if _, err := host.ResolveAsset(derived); err == nil {
		t.Fatal("offline child not released")
	}
	if _, err := host.ResolveAsset(private); err == nil {
		t.Fatal("used offline root not released")
	}
	ordinary := register(nil)
	host.ReleaseOfflineAsset(fixture.pluginID, fixture.connection.ID, ordinary)
	if _, err := host.ResolveAsset(ordinary); err != nil {
		t.Fatal("ordinary playback asset released")
	}
	if _, err := host.DeriveOfflineAsset(context.Background(), fixture.pluginID, fixture.connection.ID, direct, "https://cdn.example.test/v.m3u8", "https://outside.example.test/x.ts"); err == nil {
		t.Fatal("undeclared child accepted")
	}
	if _, _, err := host.ReadOfflineControl(context.Background(), fixture.pluginID, fixture.connection.ID, direct, 8); ErrorCode(err) != "plugin_offline_control_invalid" {
		t.Fatal("oversized control accepted")
	}
	host.resolve = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
	}
	if _, err := host.OfflineAssetTransport(context.Background(), fixture.pluginID, fixture.connection.ID, direct); err == nil {
		t.Fatal("rebinding transport accepted")
	}
	host.resolve = publicResolver
	if err := fixture.db.Model(&models.PluginInstallation{}).Where("plugin_id = ?", fixture.pluginID).Update("runtime_generation", 999).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := host.OfflineAssetTransport(context.Background(), fixture.pluginID, fixture.connection.ID, direct); ErrorCode(err) != "plugin_asset_expired" {
		t.Fatal("old generation transport accepted")
	}
}

func TestOfflineAssetNeedsCurrentDownloadGrantAndStripsPortRedirectCredentials(t *testing.T) {
	fixture := newHostFixture(t, []contract.Permission{{Kind: contract.PermissionNetworkHTTP, Domains: []string{"cdn.example.test"}}})
	host := New(fixture.db, fixture.credentials, zerolog.Nop(), WithResolver(publicResolver))
	payload, _ := json.Marshal(assetRegisterRequest{ConnectionID: fixture.connection.ID, URL: "https://cdn.example.test/media", TTLSeconds: 120})
	response, err := host.Call(context.Background(), fixture.pluginID, OperationAssetRegister, payload)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := host.OfflineAssetTransport(context.Background(), fixture.pluginID, fixture.connection.ID, assetReference(t, response)); ErrorCode(err) != "plugin_offline_permission_denied" {
		t.Fatal("playback grant implies download")
	}
	previous, _ := http.NewRequest(http.MethodGet, "https://cdn.example.test/media", nil)
	next, _ := http.NewRequest(http.MethodGet, "https://cdn.example.test:4483/media", nil)
	next.Header.Set("Cookie", "secret")
	next.Header.Set("Authorization", "secret")
	client := host.clientForPermissions([]contract.Permission{{Kind: contract.PermissionNetworkHTTP, Domains: []string{"cdn.example.test"}}}, true)
	if err := client.CheckRedirect(next, []*http.Request{previous}); err != nil {
		t.Fatal(err)
	}
	if next.Header.Get("Cookie") != "" || next.Header.Get("Authorization") != "" {
		t.Fatal("credentials crossed port origin")
	}
}
