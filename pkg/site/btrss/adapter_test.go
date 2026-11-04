package btrss

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/resourcecontent"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/site"
)

type magnetHintProbe struct {
	resourcecontent.Network
	request resourcecontent.DiscoveryRequest
	called  bool
}

func (p *magnetHintProbe) Discover(_ context.Context, request resourcecontent.DiscoveryRequest, _ func(string) bool) error {
	p.request, p.called = request, true
	return nil
}
func (*magnetHintProbe) DialContext(context.Context, string, string) (net.Conn, error) {
	return nil, resourcecontent.ErrUnavailable // No live peer/DNS traffic in this fixture.
}

func TestNyaaRSSSearchAndResolveTorrent(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/":
			if request.URL.Query().Get("page") != "rss" || request.URL.Query().Get("q") != "Samurai" || request.URL.Query().Get("c") != "0_0" || request.URL.Query().Get("f") != "0" {
				t.Errorf("unexpected Nyaa query: %s", request.URL.RawQuery)
			}
			writer.Header().Set("Content-Type", "application/rss+xml")
			_, _ = fmt.Fprintf(writer, `<?xml version="1.0"?><rss xmlns:nyaa="https://nyaa.si/xmlns/nyaa"><channel><item><title>Seven.Samurai.1954.1080p</title><link>%s/download/123.torrent</link><pubDate>Mon, 25 Aug 2025 08:00:00 +0000</pubDate><nyaa:size>1.5 GiB</nyaa:size><nyaa:seeders>21</nyaa:seeders><nyaa:leechers>3</nyaa:leechers><nyaa:downloads>99</nyaa:downloads></item></channel></rss>`, server.URL)
		case "/download/123.torrent":
			writer.Header().Set("Content-Type", "application/x-bittorrent")
			_, _ = writer.Write([]byte("d4:infod4:name7:samuraiee"))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	adapter := NewForTest("nyaa", NyaaProfile(), server.Client(), server.URL)
	page, err := adapter.Search(context.Background(), site.Config{BaseURL: server.URL}, site.Query{Keyword: "Samurai", Page: 1})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("search failed: items=%+v err=%v", page.Items, err)
	}
	item := page.Items[0]
	if item.SizeBytes != 1610612736 || item.Seeders == nil || *item.Seeders != 21 || item.Leechers == nil || *item.Leechers != 3 || item.Completed == nil || *item.Completed != 99 || item.Published == nil {
		t.Fatalf("Nyaa fields were not parsed: %+v", item)
	}
	source, err := adapter.ResolveSource(context.Background(), site.Config{BaseURL: server.URL}, item.TorrentID)
	if err != nil || len(source.Torrent) == 0 || source.Filename != "123.torrent" {
		t.Fatalf("torrent resolution failed: %+v err=%v", source, err)
	}
}

func TestRSSSearchDoesNotAppendTVFirstAirYear(t *testing.T) {
	year := 2005
	queries := make(chan string, 2)
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		queries <- request.URL.Query().Get("q")
		_, _ = writer.Write([]byte(`<rss><channel></channel></rss>`))
	}))
	defer server.Close()
	adapter := NewForTest("nyaa", NyaaProfile(), server.Client(), server.URL)
	if _, err := adapter.Search(context.Background(), site.Config{BaseURL: server.URL}, site.Query{Keyword: "Doraemon", MediaType: "tv", Year: &year, Page: 1}); err != nil {
		t.Fatal(err)
	}
	if got := <-queries; got != "Doraemon" {
		t.Fatalf("TV first-air year leaked into RSS query: %q", got)
	}
	if _, err := adapter.Search(context.Background(), site.Config{BaseURL: server.URL}, site.Query{Keyword: "Seven Samurai", MediaType: "movie", Year: &year, Page: 1}); err != nil {
		t.Fatal(err)
	}
	if got := <-queries; got != "Seven Samurai 2005" {
		t.Fatalf("movie year disambiguation missing: %q", got)
	}
}

func TestBuiltInProfilesGenerateExpectedQueryAndConstrainSources(t *testing.T) {
	tests := []struct {
		name, kind, path, parameter, prefix string
		profile                             Profile
	}{
		{"nyaa", "nyaa", "/", "q", "/download/", NyaaProfile()},
		{"animetosho", "animetosho", "/rss2", "q", "/storage/", AnimeToshoProfile()},
		{"tokyo", "tokyotoshokan", "/rss.php", "terms", "/download.php", TokyoToshokanProfile()},
		{"mikan", "mikan", "/RSS/Search", "searchstr", "/Download/", MikanProfile()},
		{"anidex", "anidex", "/rss/", "q", "/torrent/", AniDexProfile()},
		{"dmhy", "dmhy", "/topics/rss/rss.xml", "keyword", "/topics/download/", DMHYProfile()},
		{"acgrip", "acgrip", "/.xml", "term", "/torrent/", ACGRipProfile()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var server *httptest.Server
			server = httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path != test.path || request.URL.Query().Get(test.parameter) != "probe" {
					t.Errorf("unexpected request %s?%s", request.URL.Path, request.URL.RawQuery)
				}
				_, _ = fmt.Fprintf(writer, `<rss><channel><item><title>probe</title><enclosure url="%s%s123.torrent" type="application/x-bittorrent" /></item></channel></rss>`, server.URL, test.prefix)
			}))
			defer server.Close()
			adapter := NewForTest(test.kind, test.profile, server.Client(), server.URL)
			page, err := adapter.Search(context.Background(), site.Config{BaseURL: server.URL}, site.Query{Keyword: "probe", Page: 1})
			if err != nil || len(page.Items) != 1 {
				t.Fatalf("profile search failed: %+v err=%v", page, err)
			}
			foreign := "https://evil.example/download/123.torrent"
			if _, ok := adapter.sourceIdentity(server.URL, foreign); ok {
				t.Fatal("foreign torrent host was accepted")
			}
			if adapter.safeTorrentURL("https://nyaa.si", "https://nyaa.si:8443/download/123.torrent") {
				t.Fatal("allowlisted host with an unapproved port was accepted")
			}
		})
	}
}

func TestRSSCompatibilityElementsAndMagnetNormalization(t *testing.T) {
	bare := "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567&xt=urn:btmh:1220" + strings.Repeat("a", 64)
	magnet := bare + "&dn=Example+title&x.pe=8.8.8.8%3A6881&xs=http%3A%2F%2F127.0.0.1%2Fignored"
	// Exercise a valid URI larger than the former opaque envelope's 8 KiB cap.
	for i := 0; i < 6; i++ {
		magnet += "&tr=" + url.QueryEscape(fmt.Sprintf("https://tracker%d.example/", i)+strings.Repeat("a", 1500))
	}
	feed := `<?xml version="1.0"?><rss><channel><item><title>AnimeTosho torrent</title><torrent_url>https://animetosho.org/storage/a.torrent</torrent_url></item><item><title>AnimeTosho magnet</title><magnet_uri>` + strings.ReplaceAll(magnet, "&", "&amp;") + `</magnet_uri></item></channel></rss>`
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) { _, _ = writer.Write([]byte(feed)) }))
	defer server.Close()
	profile := AnimeToshoProfile()
	adapter := NewForTest("animetosho", profile, server.Client(), server.URL)
	page, err := adapter.Search(context.Background(), site.Config{BaseURL: server.URL, Timeout: 3 * time.Second}, site.Query{Keyword: "anime", Page: 1})
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("compat feed failed: %+v err=%v", page, err)
	}
	torrentKind, _, torrentOK := DecodeIdentity(page.Items[0].TorrentID)
	if !torrentOK || torrentKind != "torrent" || page.Items[0].PayloadKind != site.PayloadTorrent {
		t.Fatalf("torrent_url was not parsed: %+v", page.Items[0])
	}
	kind, value, ok := DecodeIdentity(page.Items[1].TorrentID)
	if !ok || kind != "magnet" || page.Items[1].PayloadKind != site.PayloadMagnet || page.Items[1].Fingerprint != EncodeIdentity("magnet", bare) || len(page.Items[1].TorrentID) <= 8192 {
		t.Fatal("magnet identity, full hints or bounded envelope were lost")
	}
	source, err := adapter.ResolveSource(context.Background(), site.Config{BaseURL: server.URL}, page.Items[1].TorrentID)
	if err != nil || source.Magnet != value || strings.Contains(source.Magnet, "xs=") {
		t.Fatal("resolved source lost or broadened transport hints", err)
	}
	probe := &magnetHintProbe{Network: resourcecontent.NewPublicNetwork()}
	engine := resourcecontent.NewEngine(resourcecontent.EngineConfig{Network: probe, Discoverer: probe, Timeout: time.Second})
	_, err = engine.Resolve(context.Background(), resourcecontent.ResolveRequest{URI: source.Magnet})
	m := probe.request.Magnet
	if !errors.Is(err, resourcecontent.ErrUnavailable) || !probe.called || probe.request.Private || m.Identity.V1 != "0123456789abcdef0123456789abcdef01234567" || m.Identity.V2 != strings.Repeat("a", 64) || m.DisplayName != "Example title" || len(m.Trackers) != 6 || len(m.Peers) != 1 || m.Peers[0] != "8.8.8.8:6881" {
		t.Fatal("actual Engine discovery did not receive the resolved full hints", err)
	}
	for _, raw := range []string{"magnet:?xt=urn:btih:bad", "https://example.test", bare + "&xt=urn:btih:" + strings.Repeat("f", 40), bare + "&tr=" + url.QueryEscape("https://user:secret@tracker.example/announce"), bare + "&dn=%0A", "magnet:?xt=urn:btmh:1220" + strings.Repeat("a", 64)} {
		if _, ok := NormalizeMagnet(raw); ok {
			t.Fatal("invalid, credential-bearing or unsupported source accepted")
		}
	}
	if normalized, ok := NormalizeMagnet("magnet:?xt=urn:btih:" + strings.Repeat("A", 32)); !ok || normalized != "magnet:?xt=urn:btih:"+strings.Repeat("0", 40) {
		t.Fatal("Base32 v1 compatibility lost")
	}
}
