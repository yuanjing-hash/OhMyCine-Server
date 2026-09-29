package torznab

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/site"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/site/btrss"
)

func TestJackettManualSearchUsesWebResultSetAndPaginatesLocally(t *testing.T) {
	const apiKey = "configured-secret-key"
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case jackettResultsPath:
			values := request.URL.Query()
			if values.Get("apikey") != apiKey || values.Get("query") != "名侦探柯南" || values.Get("t") != "" || values.Get("offset") != "" {
				t.Errorf("unexpected Jackett manual search: %s", request.URL.Path)
				http.Error(writer, "bad request", http.StatusBadRequest)
				return
			}
			rows := make([]map[string]any, 105)
			for index := range rows {
				rows[index] = map[string]any{
					"Title":       fmt.Sprintf("Unrelated.Release.%03d", index),
					"Link":        fmt.Sprintf("%s/download?id=%d&apikey=feed-secret&passkey=feed-passkey", server.URL, index),
					"PublishDate": "2026-09-29T10:00:00Z",
				}
			}
			rows[100] = map[string]any{"Title": "名侦探柯南.第30集", "Tracker": "Nyaa.si", "Link": server.URL + "/download?id=100&apikey=feed-secret&passkey=feed-passkey", "PublishDate": "2026-09-29T11:00:00Z", "Size": int64(123456789), "Seeders": 8, "Peers": 3, "Grabs": 5}
			writer.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(writer).Encode(map[string]any{"Results": rows, "Indexers": []any{}})
		case "/download":
			if request.URL.Query().Get("apikey") != apiKey || request.URL.Query().Get("passkey") != "" {
				t.Error("source resolution did not replace upstream credentials")
				http.Error(writer, "unauthorized", http.StatusUnauthorized)
				return
			}
			_, _ = writer.Write([]byte("d4:infod4:name5:movieee"))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	adapter := NewForTest(server.Client(), server.URL)
	config := site.Config{BaseURL: server.URL, APIKey: apiKey}
	first, err := adapter.Search(context.Background(), config, site.Query{Keyword: "名侦探柯南", Page: 1})
	if err != nil || len(first.Items) != 100 || !first.HasNext || first.Items[0].Title != "Unrelated.Release.000" {
		t.Fatalf("first Jackett page = %+v, err=%v", first, err)
	}
	second, err := adapter.Search(context.Background(), config, site.Query{Keyword: "名侦探柯南", Page: 2})
	if err != nil || len(second.Items) != 5 || second.HasNext || second.Items[0].Title != "名侦探柯南.第30集" {
		t.Fatalf("second Jackett page = %+v, err=%v", second, err)
	}
	explicit := config
	explicit.BaseURL += jackettResultsPath + "/torznab/api"
	if direct, err := adapter.Search(context.Background(), explicit, site.Query{Keyword: "名侦探柯南", Page: 2}); err != nil || len(direct.Items) != 5 || direct.Items[0].Title != second.Items[0].Title {
		t.Fatalf("explicit Jackett all-indexer search = %+v, err=%v", direct, err)
	}
	conan := second.Items[0]
	if len(conan.Tags) != 0 || conan.Subtitle != "" || conan.SizeBytes != 123456789 || conan.Published == nil || conan.Seeders == nil || *conan.Seeders != 8 || conan.Leechers == nil || *conan.Leechers != 3 || conan.Completed == nil || *conan.Completed != 5 {
		t.Fatalf("Jackett result facts were lost: %+v", conan)
	}
	_, raw, ok := btrss.DecodeIdentity(conan.TorrentID)
	if !ok || strings.Contains(raw, "feed-secret") || strings.Contains(raw, "feed-passkey") || strings.Contains(raw, apiKey) {
		t.Fatal("Jackett result retained an API credential")
	}
	if source, err := adapter.ResolveSource(context.Background(), config, conan.TorrentID); err != nil || len(source.Torrent) == 0 {
		t.Fatalf("Jackett torrent resolution failed: %+v, %v", source, err)
	}
	if _, err := adapter.Search(context.Background(), config, site.Query{Keyword: "名侦探柯南", Page: 3}); err != nil {
		t.Fatalf("out-of-range Jackett page failed: %v", err)
	}
}

func TestJackettManualSearchRejectsBadRepliesAndCrossOriginSources(t *testing.T) {
	const apiKey = "private-key"
	var response atomic.Value
	response.Store(`{"Results":[{"Title":"Invalid source","Link":"https://other.example.test/download"}]}`)
	var status atomic.Int32
	status.Store(http.StatusOK)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(int(status.Load()))
		_, _ = writer.Write([]byte(response.Load().(string)))
	}))
	defer server.Close()
	adapter := NewForTest(server.Client(), server.URL)
	config := site.Config{BaseURL: server.URL, APIKey: apiKey}
	query := site.Query{Keyword: "Test", Page: 1}
	page, err := adapter.Search(context.Background(), config, query)
	if err != nil || len(page.Items) != 0 || page.Skipped != 1 {
		t.Fatalf("cross-origin source accepted: %+v, err=%v", page, err)
	}
	response.Store(`{"error":"upstream failed"}`)
	if _, err := adapter.Search(context.Background(), config, query); err != site.ErrInvalidReply {
		t.Fatalf("malformed manual reply = %v", err)
	}
	status.Store(http.StatusUnauthorized)
	if _, err := adapter.Search(context.Background(), config, query); err != site.ErrAuthentication {
		t.Fatalf("manual endpoint authentication error = %v", err)
	}
}
