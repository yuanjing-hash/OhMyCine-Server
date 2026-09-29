package torznab

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/site"
)

func TestTorznabCapsSearchAndTorrentResolution(t *testing.T) {
	const apiKey = "server-only-api-key"
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("apikey") != apiKey {
			t.Error("API key was not supplied to Torznab")
			http.Error(writer, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch request.URL.Query().Get("t") {
		case "caps":
			_, _ = writer.Write([]byte(`<caps><searching><search available="yes" /></searching></caps>`))
		case "search":
			if request.URL.Query().Get("q") != "Seven Samurai 1954" || request.URL.Query().Get("cat") != "2000" {
				t.Errorf("unexpected search query: %s", request.URL.RawQuery)
			}
			_, _ = fmt.Fprintf(writer, `<?xml version="1.0"?><rss xmlns:torznab="http://torznab.com/schemas/2015/feed"><channel><item><title>Seven.Samurai.1954.1080p</title><enclosure url="%s/api?t=download&amp;id=42&amp;apikey=feed-supplied" type="application/x-bittorrent" length="2048"/><torznab:attr name="seeders" value="8"/><torznab:attr name="peers" value="2"/><torznab:attr name="grabs" value="5"/></item></channel></rss>`, server.URL)
		case "download":
			_, _ = writer.Write([]byte("d4:infod4:name7:samuraiee"))
		default:
			http.Error(writer, "bad request", http.StatusBadRequest)
		}
	}))
	defer server.Close()
	adapter := NewForTest(server.Client(), server.URL)
	config := site.Config{BaseURL: server.URL, APIKey: apiKey}
	if health, err := adapter.Test(context.Background(), config); err != nil || health.Status != "online" {
		t.Fatalf("caps failed: %+v err=%v", health, err)
	}
	year := 1954
	page, err := adapter.Search(context.Background(), config, site.Query{Keyword: "Seven Samurai", MediaType: "movie", Year: &year, Page: 1})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("search failed: %+v err=%v", page, err)
	}
	item := page.Items[0]
	if item.Seeders == nil || *item.Seeders != 8 || item.Leechers == nil || *item.Leechers != 2 || item.Completed == nil || *item.Completed != 5 || item.SizeBytes != 2048 {
		t.Fatalf("Torznab attrs were not parsed: %+v", item)
	}
	source, err := adapter.ResolveSource(context.Background(), config, item.TorrentID)
	if err != nil || len(source.Torrent) == 0 {
		t.Fatalf("resolve failed: %+v err=%v", source, err)
	}
	encoded := fmt.Sprintf("%+v", page)
	if strings.Contains(encoded, apiKey) {
		t.Fatal("API key leaked into parsed result")
	}
}

func TestTorznabRejectsCrossOriginTorrentAndMissingAPIKey(t *testing.T) {
	adapter := New()
	if _, err := adapter.Test(context.Background(), site.Config{BaseURL: "https://indexer.example.test"}); err != site.ErrAuthentication {
		t.Fatalf("missing API key error = %v", err)
	}
	if safeTorznabURL("https://indexer.example.test", "https://other.example.test/api?t=download") {
		t.Fatal("cross-origin torrent accepted")
	}
}

func TestTorznabHTTPJackettEndToEnd(t *testing.T) {
	const apiKey = "local-jackett-key"
	const apiPath = "/api/v2.0/indexers/all/results/torznab/api"
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api" {
			http.NotFound(writer, request)
			return
		}
		if request.URL.Path != apiPath || request.URL.Query().Get("apikey") != apiKey {
			http.Error(writer, "unexpected request", http.StatusBadRequest)
			return
		}
		switch request.URL.Query().Get("t") {
		case "caps":
			_, _ = writer.Write([]byte("<caps/>"))
		case "search":
			_, _ = fmt.Fprintf(writer, `<rss><channel><item><title>Movie.2026</title><enclosure url="%s%s?t=download&amp;id=1" type="application/x-bittorrent" /></item></channel></rss>`, server.URL, apiPath)
		case "download":
			_, _ = writer.Write([]byte("d4:infod4:name5:movieee"))
		default:
			http.Error(writer, "unsupported", http.StatusBadRequest)
		}
	}))
	defer server.Close()
	config := site.Config{BaseURL: server.URL + "/api/v2.0/indexers/all/results/torznab", APIKey: apiKey}
	adapter := New()
	if _, err := adapter.Test(context.Background(), config); err != nil {
		t.Fatalf("HTTP Jackett caps failed: %v", err)
	}
	page, err := adapter.Search(context.Background(), config, site.Query{Keyword: "Movie", Page: 1})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("HTTP Jackett search failed: %+v, %v", page, err)
	}
	if source, err := adapter.ResolveSource(context.Background(), config, page.Items[0].TorrentID); err != nil || len(source.Torrent) == 0 {
		t.Fatalf("HTTP Jackett torrent fetch failed: %+v, %v", source, err)
	}
	rootConfig := site.Config{BaseURL: server.URL, APIKey: apiKey}
	if _, err := adapter.Test(context.Background(), rootConfig); err != nil {
		t.Fatalf("Jackett root caps fallback failed: %v", err)
	}
	if rootPage, err := adapter.Search(context.Background(), rootConfig, site.Query{Keyword: "Movie", Page: 1}); err != nil || len(rootPage.Items) != 1 {
		t.Fatalf("Jackett root search fallback failed: %+v, %v", rootPage, err)
	}
	if safeTorznabURL(config.BaseURL, "https"+strings.TrimPrefix(server.URL, "http")+apiPath+"?t=download") {
		t.Fatal("HTTP Torznab accepted a cross-scheme torrent URL")
	}
	client, _, err := controlledClient(config)
	if err != nil {
		t.Fatal(err)
	}
	redirect, _ := http.NewRequest(http.MethodGet, "https"+strings.TrimPrefix(server.URL, "http")+apiPath, nil)
	previous, _ := http.NewRequest(http.MethodGet, server.URL+apiPath, nil)
	if err := client.CheckRedirect(redirect, []*http.Request{previous}); err != http.ErrUseLastResponse {
		t.Fatalf("cross-scheme redirect was not stopped: %v", err)
	}
}
