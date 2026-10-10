package services

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/plugins/contract"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/plugins/hostapi"
)

func TestHLSOfflineTwoHourDualTrackVODFitsBound(t *testing.T) {
	var media strings.Builder
	media.WriteString("#EXTM3U\n")
	for i := 0; i < 3600; i++ {
		fmt.Fprintf(&media, "#EXTINF:2,\nsegment%d.ts\n", i)
	}
	media.WriteString("#EXT-X-ENDLIST\n")
	master := "#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"a\",NAME=\"Audio\",URI=\"audio.m3u8\"\n#EXT-X-STREAM-INF:BANDWIDTH=1,AUDIO=\"a\"\nmedia.m3u8\n"
	b, asset, _ := hlsFixture(t, master, map[string][]byte{"media.m3u8": []byte(media.String()), "audio.m3u8": []byte(media.String())})
	tracks, err := b.hlsTracks(context.Background(), asset, 0)
	if err != nil || len(tracks) != 2 || len(tracks[0].Units) != 3600 || len(tracks[1].Units) != 3600 {
		t.Fatalf("long VOD rejected: tracks=%d error=%v", len(tracks), err)
	}
}

func TestHLSOfflineRealPublicSanitizedLongURIManifest(t *testing.T) {
	directory := os.Getenv("OMC_MGTV_FIXTURES")
	if directory == "" {
		t.Skip("optional sanitized official public control fixture")
	}
	body, err := os.ReadFile(filepath.Join(directory, "free-vod.m3u8"))
	if err != nil {
		t.Fatal(err)
	}
	// The parser fixture gateway deliberately allows only its generic test CDN;
	// changing the hostname preserves real URI paths, lengths and query shapes.
	manifest := strings.ReplaceAll(string(body), "pcvideomigu.titan.mgtv.com", "cdn.example.test")
	manifest = strings.ReplaceAll(manifest, "pcvideoks.titan.mgtv.com", "cdn.example.test")
	b, asset, _ := hlsFixture(t, manifest, nil)
	tracks, err := b.hlsTracks(context.Background(), asset, 0)
	if err != nil || len(tracks) != 1 || len(tracks[0].Units) != 758 {
		t.Fatalf("real public VOD control parse failed: tracks=%d err=%v", len(tracks), err)
	}
}

type offlineFixtureGateway struct {
	urls    map[string]string
	bodies  map[string][]byte
	derived []string
}

func (*offlineFixtureGateway) ReleaseOfflineAsset(string, string, string) {}

func (g *offlineFixtureGateway) OfflineAssetTransport(_ context.Context, _, _, ref string) (hostapi.OfflineTransport, error) {
	value, exists := g.urls[ref]
	if !exists {
		return hostapi.OfflineTransport{}, errors.New("missing asset")
	}
	return hostapi.OfflineTransport{URL: value, ExpiresAt: time.Now().Add(time.Minute)}, nil
}
func (g *offlineFixtureGateway) ReadOfflineControl(_ context.Context, _, _, ref string, max int64) ([]byte, string, error) {
	body, exists := g.bodies[g.urls[ref]]
	if !exists || int64(len(body)) > max {
		return nil, "", errors.New("missing/oversize control")
	}
	return body, g.urls[ref], nil
}
func (g *offlineFixtureGateway) DeriveOfflineAsset(_ context.Context, _, _, _, base, uri string) (string, error) {
	b, _ := url.Parse(base)
	child, err := url.Parse(uri)
	if err != nil {
		return "", err
	}
	target := b.ResolveReference(child)
	if target.Scheme != "https" || target.Hostname() != "cdn.example.test" {
		return "", errors.New("child denied")
	}
	ref := uuid.NewString()
	g.urls[ref] = target.String()
	g.derived = append(g.derived, target.String())
	return ref, nil
}
func (g *offlineFixtureGateway) OpenOfflineAsset(context.Context, string, string, string, string, string) (*hostapi.AssetStream, error) {
	return nil, errors.New("not required")
}

func hlsFixture(t *testing.T, manifest string, extras map[string][]byte) (*offlineBuilder, contract.DownloadAsset, *offlineFixtureGateway) {
	t.Helper()
	ref := uuid.NewString()
	root := "https://cdn.example.test/video.m3u8"
	expiry := time.Now().Add(5 * time.Minute).Unix()
	gateway := &offlineFixtureGateway{urls: map[string]string{ref: root}, bodies: map[string][]byte{root: []byte(manifest)}}
	for name, body := range extras {
		gateway.bodies["https://cdn.example.test/"+name] = body
	}
	return &offlineBuilder{gateway: gateway, pluginID: "fixture", connectionID: "connection", libraryID: "library", expiry: &expiry, keys: map[string][]byte{}}, contract.DownloadAsset{ID: "video", Kind: "video", URLRef: ref}, gateway
}

func TestHLSOfflineExpandsMapsRangesAESAndStableUnits(t *testing.T) {
	manifest := "#EXTM3U\n#EXT-X-MEDIA-SEQUENCE:42\n#EXT-X-KEY:METHOD=AES-128,URI=\"key.bin\",IV=0x01\n#EXT-X-MAP:URI=\"f.mp4\",BYTERANGE=\"64@0\"\n#EXTINF:4,\n#EXT-X-BYTERANGE:32@64\nf.mp4\n#EXT-X-KEY:METHOD=AES-128,URI=\"key.bin\"\n#EXTINF:4,\n#EXT-X-BYTERANGE:32\nf.mp4\n#EXT-X-ENDLIST\n"
	b, asset, _ := hlsFixture(t, manifest, map[string][]byte{"key.bin": []byte("1234567890123456")})
	tracks, err := b.hlsTracks(context.Background(), asset, 0)
	if err != nil || len(tracks) != 1 || len(tracks[0].Units) != 3 {
		t.Fatalf("tracks=%+v err=%v", tracks, err)
	}
	units := tracks[0].Units
	if notHasPrefix(units[0].ID, "video:map:0:") || notHasPrefix(units[1].ID, "video:segment:42:") || notHasPrefix(units[2].ID, "video:segment:43:") || units[2].ByteRange.Offset != 96 || units[2].ExpectedBytes != 32 {
		t.Fatalf("stable topology wrong: %+v", units)
	}
	if units[0].Encryption.IVHex != strings.Repeat("0", 31)+"1" || units[2].Encryption.IVHex != strings.Repeat("0", 30)+"2b" || units[1].Encryption.KeyBase64 != base64.StdEncoding.EncodeToString([]byte("1234567890123456")) {
		t.Fatal("AES plan incorrect")
	}
	refreshed, again, _ := hlsFixture(t, manifest, map[string][]byte{"key.bin": []byte("1234567890123456")})
	result, err := refreshed.hlsTracks(context.Background(), again, 0)
	if err != nil || result[0].Units[2].ID != units[2].ID {
		t.Fatal("unit IDs changed with asset references")
	}
}

func TestHLSOfflineMasterSelectsOnlyExplicitSingleRepresentation(t *testing.T) {
	master := "#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"a\",NAME=\"中文\",URI=\"audio.m3u8\"\n#EXT-X-STREAM-INF:BANDWIDTH=1000000,AUDIO=\"a\"\nmedia.m3u8\n"
	media := []byte("#EXTM3U\n#EXTINF:4,\nsegment.ts\n#EXT-X-ENDLIST\n")
	b, asset, _ := hlsFixture(t, master, map[string][]byte{"media.m3u8": media, "audio.m3u8": media})
	tracks, err := b.hlsTracks(context.Background(), asset, 0)
	if err != nil || len(tracks) != 2 || tracks[1].Kind != "audio" || tracks[1].ID != "video:audio" {
		t.Fatalf("tracks=%+v err=%v", tracks, err)
	}
}

func TestHLSOfflineRejectsLiveDRMAmbiguityAndMalformedRanges(t *testing.T) {
	for name, manifest := range map[string]string{
		"live":          "#EXTM3U\n#EXTINF:3,\na.ts\n",
		"drm":           "#EXTM3U\n#EXT-X-KEY:METHOD=SAMPLE-AES,URI=\"x\"\n#EXTINF:3,\na.ts\n#EXT-X-ENDLIST\n",
		"keyformat":     "#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,KEYFORMAT=\"com.apple.streamingkeydelivery\",URI=\"x\"\n#EXTINF:3,\na.ts\n#EXT-X-ENDLIST\n",
		"ambiguous":     "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1\na.m3u8\n#EXT-X-STREAM-INF:BANDWIDTH=2\nb.m3u8\n",
		"range":         "#EXTM3U\n#EXTINF:3,\n#EXT-X-BYTERANGE:10\na.ts\n#EXT-X-ENDLIST\n",
		"private-child": "#EXTM3U\n#EXTINF:3,\nhttp://127.0.0.1/a.ts\n#EXT-X-ENDLIST\n",
		"duplicate":     "#EXTM3U\n#EXT-X-MEDIA-SEQUENCE:1\n#EXT-X-MEDIA-SEQUENCE:2\n#EXTINF:3,\na.ts\n#EXT-X-ENDLIST\n",
		"discontinuity": "#EXTM3U\n#EXTINF:3,\na.ts\n#EXT-X-DISCONTINUITY\n#EXTINF:3,\nb.ts\n#EXT-X-ENDLIST\n",
		"gap":           "#EXTM3U\n#EXT-X-GAP\n#EXTINF:3,\na.ts\n#EXT-X-ENDLIST\n",
	} {
		t.Run(name, func(t *testing.T) {
			b, asset, _ := hlsFixture(t, manifest, nil)
			if _, err := b.hlsTracks(context.Background(), asset, 0); err == nil {
				t.Fatal("unsafe HLS accepted")
			}
		})
	}
	b, asset, _ := hlsFixture(t, "#EXTM3U\n#EXTINF:1,\na.ts\n#EXT-X-ENDLIST\n", nil)
	b.units = maxOfflineUnits
	if _, err := b.hlsTracks(context.Background(), asset, 0); err == nil {
		t.Fatal("unit bound ignored")
	}
}

func notHasPrefix(value, prefix string) bool { return !strings.HasPrefix(value, prefix) }
