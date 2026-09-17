package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/services"
)

func TestPluginBrowserReloadHTTPBoundary(t *testing.T) {
	gin.SetMode(gin.TestMode)
	api := &API{pluginRepositories: &services.PluginRepositoryService{}, log: zerolog.Nop()}
	router := gin.New()
	router.GET("/plugins/:plugin_id/connections/:connection_id/resource/browser/:browser_operation", api.PluginResourceBrowser)
	router.POST("/plugins/:plugin_id/connections/:connection_id/resource/browser/:browser_operation", api.PluginResourceBrowser)
	path := "/plugins/test/connections/test/resource/browser/reload"
	for _, test := range []struct {
		method, body string
		status       int
	}{
		{http.MethodGet, "", http.StatusMethodNotAllowed},
		{http.MethodPost, `{"session_id":"opaque","url":"https://other.invalid"}`, http.StatusBadRequest},
		{http.MethodPost, `{"session_id":"opaque"}`, http.StatusForbidden},
	} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(test.method, path, strings.NewReader(test.body))
		request.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(response, request)
		if response.Code != test.status {
			t.Fatalf("%s: status=%d want=%d", test.method, response.Code, test.status)
		}
		if test.method == http.MethodPost && response.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("browser result can be cached")
		}
		if strings.Contains(response.Body.String(), "other.invalid") {
			t.Fatal("invalid navigation echoed")
		}
	}
}
