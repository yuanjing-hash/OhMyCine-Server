package hostapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/plugins/contract"
)

func TestHLSPlaybackRewritesRelativeMasterAudioMapKeyAndSegments(t *testing.T) {
	fixture := newHostFixture(t, []contract.Permission{{Kind: contract.PermissionNetworkHTTP, Domains: []string{"cdn.example.test"}}})
	master := "#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"a\",URI=\"audio.m3u8\"\n#EXT-X-STREAM-INF:BANDWIDTH=1,AUDIO=\"a\"\nmedia.m3u8?ticket=opaque\n"
	media := "#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"key.bin\"\n#EXT-X-MAP:URI=\"init.mp4\"\n#EXTINF:4,\nsegment.ts\n#EXT-X-ENDLIST\n"
	requests := 0
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		body := master
		if request.URL.Path == "/media.m3u8" {
			body = media
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/vnd.apple.mpegurl"}}, Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
	})}
	host := New(fixture.db, fixture.credentials, zerolog.Nop(), WithHTTPClient(client), WithResolver(publicResolver))
	payload, _ := json.Marshal(assetRegisterRequest{ConnectionID: fixture.connection.ID, URL: "https://cdn.example.test/master.m3u8", TTLSeconds: 120})
	response, err := host.Call(context.Background(), fixture.pluginID, OperationAssetRegister, payload)
	if err != nil {
		t.Fatal(err)
	}
	root := assetReference(t, response)
	stream, err := host.OpenAsset(context.Background(), root, http.MethodGet, "")
	if err != nil {
		t.Fatal(err)
	}
	content, _ := io.ReadAll(stream.Body)
	stream.Body.Close()
	if strings.Contains(string(content), "ticket=") || strings.Contains(string(content), "cdn.example") || strings.Count(string(content), "/api/v1/player/online-assets/") != 2 {
		t.Fatalf("unsafe HLS root: %s", content)
	}
	count := len(host.offlineAssets)
	second, err := host.OpenAsset(context.Background(), root, http.MethodHead, "")
	if err != nil {
		t.Fatal(err)
	}
	second.Body.Close()
	if len(host.offlineAssets) != count {
		t.Fatal("repeated control read leaked refs")
	}
	var mediaRef string
	for ref, asset := range host.offlineAssets {
		if strings.Contains(asset.URL, "media.m3u8") {
			mediaRef = ref
		}
	}
	stream, err = host.OpenAsset(context.Background(), mediaRef, http.MethodGet, "")
	if err != nil {
		t.Fatal(err)
	}
	content, _ = io.ReadAll(stream.Body)
	stream.Body.Close()
	if strings.Count(string(content), "/api/v1/player/online-assets/") != 3 || strings.Contains(string(content), "segment.ts") || strings.Contains(string(content), "key.bin") {
		t.Fatalf("HLS children not rewritten: %s", content)
	}
	if requests != 3 {
		t.Fatal("Server fetched media while rewriting control manifest")
	}
	if err := fixture.db.Model(&models.PluginConnection{}).Where("id = ?", fixture.connection.ID).Update("enabled", false).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := host.OpenAsset(context.Background(), mediaRef, http.MethodGet, ""); ErrorCode(err) != "plugin_asset_expired" {
		t.Fatal("child bypassed disabled owner")
	}
}

func TestHLSPlaybackRejectsUnsafeURIsAndCleansPartialChildren(t *testing.T) {
	fixture := newHostFixture(t, []contract.Permission{{Kind: contract.PermissionNetworkHTTP, Domains: []string{"cdn.example.test"}}})
	manifest := "#EXTM3U\n#EXTINF:4,\nsafe.ts\n#EXT-X-KEY:METHOD=SAMPLE-AES,URI=\"skd://private/license\"\n"
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(manifest)), Request: request}, nil
	})}
	host := New(fixture.db, fixture.credentials, zerolog.Nop(), WithHTTPClient(client), WithResolver(publicResolver))
	payload, _ := json.Marshal(assetRegisterRequest{ConnectionID: fixture.connection.ID, URL: "https://cdn.example.test/master.m3u8", TTLSeconds: 120})
	response, err := host.Call(context.Background(), fixture.pluginID, OperationAssetRegister, payload)
	if err != nil {
		t.Fatal(err)
	}
	root := assetReference(t, response)
	if _, err := host.OpenAsset(context.Background(), root, http.MethodGet, ""); err == nil {
		t.Fatal("nonHTTPS URI accepted")
	}
	if len(host.offlineAssets) != 0 {
		t.Fatal("failed manifest leaked derived children")
	}
	manifest = "#EXTM3U\n#EXTINF:4,\nhttps://outside.example.test/a.ts\n"
	if _, err := host.OpenAsset(context.Background(), root, http.MethodGet, ""); err == nil {
		t.Fatal("undeclared child domain accepted")
	}
	host.assetsMu.Lock()
	asset := host.assets[root]
	asset.HLSDepth = 8
	host.assets[root] = asset
	host.assetsMu.Unlock()
	if _, err := host.OpenAsset(context.Background(), root, http.MethodGet, ""); ErrorCode(err) != "plugin_hls_depth_exceeded" {
		t.Fatal("recursive controls unbounded")
	}
}
