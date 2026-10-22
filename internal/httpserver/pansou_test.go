package httpserver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/services"
	sitepkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/site"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/site/pansou"
)

func TestPanSouMixedHTTPRoutesSessionPlayerAndControlledLinks(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/health" {
			fmt.Fprint(w, `{"channels":["movies"]}`)
			return
		}
		fmt.Fprint(w, `{"code":0,"data":{"total":2,"results":[{"unique_id":"fixture-1","title":"Fixture Movie","links":[{"type":"baidu","url":"https://pan.baidu.com/s/fixture","password":"abcd"},{"type":"magnet","url":"magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567"}]}]}}`)
	}))
	defer upstream.Close()
	client := newTestClientWithSiteAdapters(t, []sitepkg.Adapter{routerSiteAdapter{}, pansou.NewForTest(upstream.Client())})
	for _, path := range []string{"/api/v1/discovery/results/link", "/api/v1/player/discovery/results/link"} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"result_token":"expired"}`))
		request.Header.Set("Origin", "http://localhost:3000")
		request.Header.Set("Content-Type", "application/json")
		client.router.ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("unauthenticated route %s status=%d cache=%q", path, response.Code, response.Header().Get("Cache-Control"))
		}
	}
	client.setup(t)
	status, envelope := client.request(t, http.MethodPost, "/api/v1/sites", map[string]any{"name": "Mixed PanSou", "kind": pansou.Kind, "base_url": upstream.URL, "cloud_config": map[string]any{"provider": "all", "source_scope": "plugin", "channel_mode": "defaults_plus_custom", "type_policy": "all"}, "enabled": true, "priority": 100, "timeout_seconds": 10, "rate_limit_per_minute": 120}, true)
	var created services.SiteSummary
	if status != http.StatusCreated || json.Unmarshal(envelope.Data, &created) != nil {
		t.Fatalf("create status=%d data=%s", status, envelope.Data)
	}
	searchPath := "/api/v1/discovery/torrent-search?keyword=Fixture&page=1&site_id=" + strconv.FormatUint(uint64(created.ID), 10) + "&client_capabilities=mixed_discovery_v1&type_policy=all"
	status, envelope = client.request(t, http.MethodGet, searchPath, nil, false)
	var result struct {
		Groups []services.SiteSearchGroup `json:"groups"`
	}
	if status != 200 || json.Unmarshal(envelope.Data, &result) != nil || len(result.Groups) != 1 || len(result.Groups[0].Items) != 2 || bytes.Contains(envelope.Data, []byte("pwd=")) {
		t.Fatalf("search status=%d data=%s", status, envelope.Data)
	}
	var token string
	for _, item := range result.Groups[0].Items {
		if item.ResultType == "baidu" {
			token = item.Token
		}
	}
	status, _ = client.request(t, http.MethodPost, "/api/v1/discovery/results/link", map[string]any{"result_token": token}, false)
	if status != 403 {
		t.Fatal("Session link bypassed CSRF", status)
	}
	status, envelope = client.request(t, http.MethodPost, "/api/v1/discovery/results/link", map[string]any{"result_token": token}, true)
	if status != 200 || client.lastHeader.Get("Cache-Control") != "no-store" || !bytes.Contains(envelope.Data, []byte("pan.baidu.com/s/fixture?pwd=abcd")) {
		t.Fatalf("controlled Session link status=%d data=%s", status, envelope.Data)
	}
	status, envelope = client.request(t, http.MethodGet, "/api/v1/sites/"+strconv.FormatUint(uint64(created.ID), 10)+"/pansou-scope", nil, false)
	if status != 200 || !bytes.Contains(envelope.Data, []byte(`"source_scope":"plugin"`)) {
		t.Fatalf("scope status=%d data=%s", status, envelope.Data)
	}
	status, loginEnvelope, _ := client.playerRequest(t, http.MethodPost, "/api/v1/player/auth/login", "", map[string]any{"username": "owner", "password": "strong-owner-password", "device_id": "mixed-http-device", "device_name": "Mixed HTTP fixture"})
	var login struct {
		AccessToken string `json:"access_token"`
	}
	if status != 200 || json.Unmarshal(loginEnvelope.Data, &login) != nil || login.AccessToken == "" {
		t.Fatalf("login status=%d", status)
	}
	status, bootstrap, _ := client.playerRequest(t, http.MethodGet, "/api/v1/player/bootstrap", login.AccessToken, nil)
	if status != 200 || !bytes.Contains(bootstrap.Data, []byte(services.MixedDiscoveryCapability)) {
		t.Fatal("missing mixed capability", status)
	}
	playerSearchPath := strings.Replace(searchPath, "/api/v1/", "/api/v1/player/", 1)
	status, envelope, _ = client.playerRequest(t, http.MethodGet, playerSearchPath, login.AccessToken, nil)
	result.Groups = nil
	if status != 200 || json.Unmarshal(envelope.Data, &result) != nil || len(result.Groups) != 1 || len(result.Groups[0].Items) != 2 {
		t.Fatalf("Player search status=%d data=%s", status, envelope.Data)
	}
	for _, item := range result.Groups[0].Items {
		if item.ResultType == "baidu" {
			token = item.Token
		}
	}
	status, envelope, headers := client.playerRequest(t, http.MethodPost, "/api/v1/player/discovery/results/link", login.AccessToken, map[string]any{"result_token": token})
	if status != 200 || headers.Get("Cache-Control") != "no-store" || !bytes.Contains(envelope.Data, []byte(`"result_type":"baidu"`)) {
		t.Fatalf("Player link status=%d data=%s", status, envelope.Data)
	}
	status, _, _ = client.playerRequest(t, http.MethodPost, "/api/v1/sites/pansou-scope", login.AccessToken, map[string]any{})
	if status != 401 && status != 403 {
		t.Fatal("Player entered management scope endpoint", status)
	}
}
