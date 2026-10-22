package pansou

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/site"
)

const fixtureMagnet = "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567"

func TestMixedConfigLegacyAndChannelModes(t *testing.T) {
	for _, tt := range []struct {
		name                 string
		config               site.CloudConfig
		mode, source, policy string
		invalid              bool
	}{
		{"new", site.CloudConfig{}, "defaults_plus_custom", "all", "auto", false},
		{"legacy", site.CloudConfig{Provider: "115", Channels: []string{"@Movies", "https://t.me/s/MOVIES"}}, "custom_only", "all", "auto", false},
		{"plugin-only", site.CloudConfig{SourceScope: "plugin", ChannelMode: "custom_only"}, "custom_only", "plugin", "auto", false},
		{"empty-custom", site.CloudConfig{SourceScope: "tg", ChannelMode: "custom_only"}, "", "", "", true},
		{"bad-source", site.CloudConfig{SourceScope: "all-the-web"}, "", "", "", true},
		{"manual-empty", site.CloudConfig{TypePolicy: "manual"}, "", "", "", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := NormalizeConfig(&tt.config)
			if tt.invalid {
				if err == nil {
					t.Fatal("invalid config accepted")
				}
				return
			}
			if err != nil || cfg.ChannelMode != tt.mode || cfg.SourceScope != tt.source || cfg.TypePolicy != tt.policy {
				t.Fatalf("cfg=%+v err=%v", cfg, err)
			}
			if tt.name == "legacy" && strings.Join(cfg.Channels, ",") != "movies" {
				t.Fatal(cfg.Channels)
			}
		})
	}
	channels := []string{}
	for i := 0; i < 111; i++ {
		channels = append(channels, fmt.Sprintf("channel%03d", i))
	}
	if values, err := NormalizeChannels(channels); err != nil || len(values) != 111 {
		t.Fatal("default111 rejected", err)
	}
	for len(channels) <= 1024 {
		channels = append(channels, fmt.Sprintf("channel%04d", len(channels)))
	}
	if _, err := NormalizeChannels(channels); err == nil {
		t.Fatal("overlimit accepted")
	}
}

func TestMixedSourcesTypesOriginsAndExtractionCode(t *testing.T) {
	var payload map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/health" {
			fmt.Fprint(w, `{"channels":["movies","@Shows"]}`)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		fmt.Fprintf(w, `{"code":0,"data":{"total":4,"results":[{"channel":"movies","message_id":"19","title":"new title","datetime":"2026-10-22T00:00:00Z","links":[{"type":"quark","url":"https://pan.quark.cn/s/aabb"}]},{"unique_id":"gying-122","title":"older title","datetime":"2026-10-21T00:00:00Z","links":[{"type":"quark","url":"https://pan.quark.cn/s/aabb","password":"abcd"},{"type":"magnet","url":%q},{"type":"115","url":"https://pan.quark.cn/s/wrong"},{"type":"ed2k","url":"ed2k://|file|sample.mkv|1234|0123456789abcdef0123456789abcdef|/"}]}],"merged_by_type":{"baidu":[{"url":"https://pan.baidu.com/s/abc","password":"test","note":"Baidu title","source":"plugin:pansearch"}]}}}`, fixtureMagnet)
	}))
	defer server.Close()
	adapter := NewForTest(server.Client())
	cfg := site.Config{BaseURL: server.URL, Cloud: &site.CloudConfig{Channels: []string{"@movies", "custom"}, ChannelMode: "defaults_plus_custom"}}
	page, err := adapter.Search(context.Background(), cfg, site.Query{Keyword: "Show", Page: 1, TypePolicy: "all", ScopeKey: "actor1"})
	if err != nil || len(page.Items) != 4 {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	if payload["src"] != "all" || payload["res"] != "all" || payload["cloud_types"] != nil || payload["refresh"] != false {
		t.Fatal(payload)
	}
	if page.Scope.DefaultCount != 2 || page.Scope.CustomCount != 2 || page.Scope.EffectiveCount != 3 {
		t.Fatal(page.Scope)
	}
	byType := map[string]site.Result{}
	for _, item := range page.Items {
		byType[item.ResultType] = item
	}
	quark := byType["quark"]
	if quark.Title != "new title" || len(quark.Origins) != 2 || !strings.Contains(quark.TorrentID, "pwd=abcd") || quark.PostURL != "https://t.me/movies/19" {
		t.Fatal(quark)
	}
	if byType["magnet"].SourceKind != "bt" || byType["magnet"].PostURL != "" || byType["baidu"].SourceKind != "external_link" || byType["ed2k"].SourceKind != "external_link" {
		t.Fatal(byType)
	}
	encoded, _ := json.Marshal(page.Items)
	if strings.Contains(string(encoded), "pwd=") || strings.Contains(string(encoded), fixtureMagnet) {
		t.Fatalf("raw link leaked: %s", encoded)
	}
	if _, err := adapter.ResolveSource(context.Background(), cfg, byType["ed2k"].TorrentID); err != site.ErrNotFound {
		t.Fatal("ed2k became download", err)
	}
}

func TestMixedDefaultFailurePreservesPluginAndPluginOnlySkipsDefaults(t *testing.T) {
	for _, source := range []string{"all", "plugin", "tg"} {
		t.Run(source, func(t *testing.T) {
			health, search := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/health" {
					health++
					w.WriteHeader(503)
					return
				}
				search++
				var payload map[string]any
				_ = json.NewDecoder(r.Body).Decode(&payload)
				if payload["src"] != "plugin" {
					t.Error(payload)
				}
				fmt.Fprintf(w, `{"code":0,"data":{"total":1,"results":[{"unique_id":"gying-1","title":"magnet","links":[{"type":"magnet","url":%q}]}]}}`, fixtureMagnet)
			}))
			defer server.Close()
			cfg := site.Config{BaseURL: server.URL, Cloud: &site.CloudConfig{SourceScope: source}}
			page, err := NewForTest(server.Client()).Search(context.Background(), cfg, site.Query{Keyword: "Show", Page: 1, TypePolicy: "auto", CloudTypes: []string{"magnet"}})
			if source == "tg" {
				if err == nil || search != 0 || health != 1 {
					t.Fatalf("page=%+v err=%v health=%d search=%d", page, err, health, search)
				}
				return
			}
			if err != nil || len(page.Items) != 1 || search != 1 {
				t.Fatalf("page=%+v err=%v", page, err)
			}
			if source == "plugin" && health != 0 {
				t.Fatal("plugin-only inspected TG")
			}
			if source == "all" && (!page.Partial || page.Scope.DefaultErrorCode == "") {
				t.Fatal("TG failure hidden", page)
			}
		})
	}
}

func TestMixedSnapshotPagingIsStableBoundedAndActorScoped(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		items := []any{}
		for i := 0; i < 61; i++ {
			items = append(items, map[string]any{"unique_id": fmt.Sprintf("fixture-%d", i), "title": fmt.Sprintf("sample%d", i), "links": []any{map[string]any{"type": "magnet", "url": fmt.Sprintf("magnet:?xt=urn:btih:%040x", i+1)}}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"total": len(items), "results": items}})
	}))
	defer server.Close()
	a := NewForTest(server.Client())
	cfg := site.Config{BaseURL: server.URL, Cloud: &site.CloudConfig{SourceScope: "plugin"}}
	query := site.Query{Keyword: "Show", Page: 1, ScopeKey: "actor1", TypePolicy: "auto", CloudTypes: []string{"magnet"}}
	p1, err := a.Search(context.Background(), cfg, query)
	if err != nil || len(p1.Items) != 50 || !p1.HasNext {
		t.Fatalf("page=%+v err=%v", p1, err)
	}
	query.Page = 2
	query.SnapshotID = p1.SnapshotID
	p2, err := a.Search(context.Background(), cfg, query)
	if err != nil || len(p2.Items) != 11 || p2.HasNext || requests != 1 {
		t.Fatalf("page=%+v requests=%d err=%v", p2, requests, err)
	}
	query.Page = 1
	back, err := a.Search(context.Background(), cfg, query)
	if err != nil || back.SnapshotID != p1.SnapshotID || len(back.Items) != 50 || requests != 1 {
		t.Fatal("return to page1 refreshed generation", err, requests)
	}
	query.Page = 2
	query.ScopeKey = "actor2"
	if _, err := a.Search(context.Background(), cfg, query); err != site.ErrSnapshotExpired || requests != 1 {
		t.Fatal("snapshot crossed actor", err, requests)
	}
	query.ScopeKey = "actor1"
	query.TypePolicy = "all"
	if _, err := a.Search(context.Background(), cfg, query); err != site.ErrSnapshotExpired {
		t.Fatal("snapshot crossed types", err)
	}
	query.TypePolicy = "auto"
	query.Page = 1
	query.Refresh = true
	query.SnapshotID = ""
	p3, err := a.Search(context.Background(), cfg, query)
	if err != nil || requests != 2 || p3.SnapshotID == p1.SnapshotID {
		t.Fatal("refresh did not replace generation", err, requests)
	}
	query.Refresh = false
	query.SnapshotID = p1.SnapshotID
	if _, err := a.Search(context.Background(), cfg, query); err != site.ErrSnapshotExpired {
		t.Fatal("obsolete generation survived", err)
	}
	a.mu.Lock()
	entry := a.snapshots[p3.SnapshotID]
	entry.expires = time.Now().Add(-time.Second)
	a.snapshots[p3.SnapshotID] = entry
	a.mu.Unlock()
	query.SnapshotID = p3.SnapshotID
	if _, err := a.Search(context.Background(), cfg, query); err != site.ErrSnapshotExpired {
		t.Fatal("expired snapshot survived", err)
	}
}

func TestMixedResourceNormalizationRejectsProviderSpoofAndNonHTTP(t *testing.T) {
	for _, tt := range []struct{ kind, url string }{{"quark", "https://115.com/s/test?password=abcd"}, {"magnet", "ed2k://|file|sample|12|0123456789abcdef0123456789abcdef|/"}, {"others", "javascript:alert(1)"}, {"baidu", "https://pan.baidu.com.evil.test/s/x"}, {"others", "https://user:secret@example.test/s/x"}} {
		if _, _, _, err := NormalizeResource(tt.kind, tt.url, ""); err == nil {
			t.Fatalf("accepted %+v", tt)
		}
	}
	if kind, url, _, err := NormalizeResource("others", "http://downloads.example.test/share/abc", ""); err != nil || kind != "others" || !strings.HasPrefix(url, "http://") {
		t.Fatalf("view-only http refused: %s %s %v", kind, url, err)
	}
}

func TestMixedTianyiQueryShareIdentityPreservesDistinctResources(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"code":0,"data":{"total":3,"results":[{"unique_id":"fixture-1","title":"first","links":[{"type":"tianyi","url":"https://cloud.189.cn/web/share?code=first-share"},{"type":"tianyi","url":"https://cloud.189.cn/web/share?code=first-share","password":"1234"}]},{"unique_id":"fixture-2","title":"second","links":[{"type":"tianyi","url":"https://cloud.189.cn/web/share?code=second-share","password":"5678"}]}]}}`)
	}))
	defer server.Close()
	page, err := NewForTest(server.Client()).Search(context.Background(), site.Config{BaseURL: server.URL, Cloud: &site.CloudConfig{SourceScope: "plugin"}}, site.Query{Keyword: "Show", Page: 1, TypePolicy: "all"})
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("distinct shares collapsed: page=%+v err=%v", page, err)
	}
	if page.Items[0].Fingerprint == page.Items[1].Fingerprint || !strings.Contains(page.Items[0].TorrentID, "code=first-share") || !strings.Contains(page.Items[0].TorrentID, "password=1234") || !strings.Contains(page.Items[1].TorrentID, "code=second-share") || !strings.Contains(page.Items[1].TorrentID, "password=5678") {
		t.Fatalf("share identity or extraction code crossed resources: %+v", page.Items)
	}
	if _, _, _, err := NormalizeResource("tianyi", "https://cloud.189.cn/web/share?password=1234", ""); err == nil {
		t.Fatal("query share without identity accepted")
	}
}
