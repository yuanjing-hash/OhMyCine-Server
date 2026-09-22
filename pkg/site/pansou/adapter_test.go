package pansou

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/site"
)

func TestSearchScopeAuthenticationAndLatestShare(t *testing.T) {
	logins, searches := 0, 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/auth/login" {
			logins++
			fmt.Fprintf(w, `{"token":"token-%d"}`, logins)
			return
		}
		searches++
		var body struct {
			Channels []string `json:"channels"`
			Clouds   []string `json:"cloud_types"`
			Source   string   `json:"src"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if strings.Join(body.Channels, ",") != "movies" || strings.Join(body.Clouds, ",") != "115" || body.Source != "tg" {
			t.Errorf("unexpected scope: %+v", body)
		}
		if r.Header.Get("Authorization") == "Bearer token-1" {
			w.WriteHeader(401)
			return
		}
		fmt.Fprint(w, `{"code":0,"data":{"total":5,"results":[
   {"channel":"movies","message_id":"1","title":"Show.S01E01","datetime":"2026-09-20T00:00:00Z","links":[{"type":"115","url":"https://115.com/s/shared","password":"old"}]},
   {"channel":"movies","message_id":"2","title":"Collection","datetime":"2026-09-21T00:00:00Z","links":[{"type":"115","url":"https://115cdn.com/s/shared?unrelated=secret","password":"new","datetime":"0001-01-01T00:00:00Z","work_title":"Show.S01E01-E02"},{"type":"aliyun","url":"https://aliyundrive.com/s/other"}]},
   {"channel":"other","title":"Wrong channel","links":[{"type":"115","url":"https://115.com/s/wrong?password=abcd"}]},
   {"channel":"movies","title":"Bad URL","links":[{"type":"115","url":"https://115.com.evil.test/s/invalid?password=abcd"}]},
   {"channel":"movies","title":"No password","links":[{"type":"115","url":"https://115.com/s/nopassword"}]}
  ]}}`)
	}))
	defer server.Close()
	cfg := site.Config{BaseURL: server.URL, Cloud: &site.CloudConfig{Provider: "115", Channels: []string{"@Movies", "https://t.me/s/movies"}, AuthEnabled: true}, Username: "user", Password: "secret"}
	adapter := NewForTest(server.Client())
	page, err := adapter.Search(context.Background(), cfg, site.Query{Keyword: "Show", Page: 1})
	if err != nil || len(page.Items) != 1 || logins != 2 || searches != 2 {
		t.Fatalf("page=%+v logins=%d searches=%d err=%v", page, logins, searches, err)
	}
	item := page.Items[0]
	if item.Title != "Show.S01E01-E02" || item.PostURL != "https://t.me/movies/2" || item.TorrentID != "https://115.com/s/shared?password=new" || item.SourceKind != "115_share" || item.Fingerprint == "" {
		t.Fatalf("item=%+v", item)
	}
	public, _ := json.Marshal(item)
	if strings.Contains(string(public), "password") || strings.Contains(string(public), "115.com") {
		t.Fatalf("share secret exposed: %s", public)
	}
	source, err := adapter.ResolveSource(context.Background(), cfg, item.TorrentID)
	if err != nil || source.ShareURL != item.TorrentID || source.Magnet != "" || len(source.Torrent) != 0 {
		t.Fatalf("source=%+v err=%v", source, err)
	}
}

func TestSearchRejectsInvalidResponsesAndCancellation(t *testing.T) {
	for _, body := range []string{`{}`, `{"code":0,"data":{}}`, strings.Repeat("x", (4<<20)+1)} {
		t.Run(fmt.Sprint(len(body)), func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
			defer server.Close()
			_, err := NewForTest(server.Client()).Search(context.Background(), site.Config{BaseURL: server.URL, Cloud: &site.CloudConfig{Provider: "115", Channels: []string{"movies"}}}, site.Query{Keyword: "Show", Page: 1})
			if err != site.ErrInvalidReply {
				t.Fatalf("err=%v", err)
			}
		})
	}
	requests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		http.Redirect(w, r, "/redirected", http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	adapter := NewForTest(server.Client())
	cfg := site.Config{BaseURL: server.URL, Cloud: &site.CloudConfig{Provider: "115", Channels: []string{"movies"}}}
	if _, err := adapter.Search(context.Background(), cfg, site.Query{Keyword: "Show", Page: 1}); err != site.ErrUnavailable || requests != 1 {
		t.Fatalf("followed redirect: requests=%d err=%v", requests, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := adapter.Search(ctx, cfg, site.Query{Keyword: "Show", Page: 1}); err == nil || requests != 1 {
		t.Fatalf("cancelled request sent: %d %v", requests, err)
	}
}
