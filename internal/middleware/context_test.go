package middleware

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/services"
)

func TestAbortAuthenticationErrorPreservesAuthenticationSemantics(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, test := range []struct {
		name       string
		err        error
		status     int
		retryAfter string
	}{
		{name: "invalid session", err: &services.AppError{Code: services.CodeNotAuthenticated, Message: "请先登录"}, status: http.StatusUnauthorized},
		{name: "database busy", err: &services.AppError{Code: services.CodeDatabaseBusy, Message: "服务器数据库繁忙，请稍后重试"}, status: http.StatusServiceUnavailable, retryAfter: "1"},
		{name: "unknown infrastructure", err: errors.New("private database failure"), status: http.StatusInternalServerError},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			context, _ := gin.CreateTestContext(recorder)
			abortAuthenticationError(context, test.err)
			if recorder.Code != test.status || recorder.Header().Get("Retry-After") != test.retryAfter {
				t.Fatalf("status=%d retry-after=%q body=%s", recorder.Code, recorder.Header().Get("Retry-After"), recorder.Body.String())
			}
			if test.status == http.StatusInternalServerError && (!strings.Contains(recorder.Body.String(), `"message":"服务器内部错误"`) || strings.Contains(recorder.Body.String(), "private database failure")) {
				t.Fatalf("private error leaked or envelope changed: %s", recorder.Body.String())
			}
		})
	}
}

func TestBrowserMutationProtectionAllowsAutomaticSameOrigin(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(BrowserMutationProtection([]string{"http://localhost:3000", "http://localhost:5173"}))
	router.POST("/change", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	for _, test := range []struct {
		name    string
		host    string
		headers http.Header
		status  int
	}{
		{"HTTPS proxy rewrites Host", "server:3000", http.Header{"Origin": {"https://media.example.test"}, "Sec-Fetch-Site": {"same-origin"}}, http.StatusNoContent},
		{"same-origin metadata without Origin", "server:3000", http.Header{"Sec-Fetch-Site": {"same-origin"}}, http.StatusNoContent},
		{"LAN without fetch metadata", "192.168.1.10:3000", http.Header{"Origin": {"http://192.168.1.10:3000"}}, http.StatusNoContent},
		{"IPv6 without fetch metadata", "[::1]:3000", http.Header{"Origin": {"http://[::1]:3000"}}, http.StatusNoContent},
		{"domain Host is case insensitive", "MEDIA.EXAMPLE.TEST:8443", http.Header{"Origin": {"https://media.example.test:8443"}}, http.StatusNoContent},
		{"Host port must match", "192.168.1.10:3000", http.Header{"Origin": {"http://192.168.1.10:8080"}}, http.StatusForbidden},
		{"explicit development origin", "server:3000", http.Header{"Origin": {"http://localhost:5173"}, "Sec-Fetch-Site": {"same-site"}}, http.StatusNoContent},
		{"cross-site cannot use configured origin", "server:3000", http.Header{"Origin": {"http://localhost:5173"}, "Sec-Fetch-Site": {"cross-site"}}, http.StatusForbidden},
		{"same-site cannot use Host fallback", "media.example.test", http.Header{"Origin": {"http://media.example.test"}, "Sec-Fetch-Site": {"same-site"}}, http.StatusForbidden},
		{"foreign origin", "media.example.test", http.Header{"Origin": {"https://foreign.example.test"}}, http.StatusForbidden},
		{"unknown metadata falls back", "media.example.test", http.Header{"Origin": {"https://media.example.test"}, "Sec-Fetch-Site": {"unknown"}}, http.StatusNoContent},
		{"none metadata cannot bypass foreign origin", "media.example.test", http.Header{"Origin": {"https://foreign.example.test"}, "Sec-Fetch-Site": {"none"}}, http.StatusForbidden},
		{"missing browser evidence", "media.example.test", nil, http.StatusForbidden},
		{"null origin with same-origin metadata", "server:3000", http.Header{"Origin": {"null"}, "Sec-Fetch-Site": {"same-origin"}}, http.StatusForbidden},
		{"malformed origin with same-origin metadata", "server:3000", http.Header{"Origin": {"https://media.example.test/path"}, "Sec-Fetch-Site": {"same-origin"}}, http.StatusForbidden},
		{"invalid bracketed host with same-origin metadata", "server:3000", http.Header{"Origin": {"https://[not-an-ip]"}, "Sec-Fetch-Site": {"same-origin"}}, http.StatusForbidden},
		{"duplicate Origin with same-origin metadata", "server:3000", http.Header{"Origin": {"https://media.example.test", "https://foreign.example.test"}, "Sec-Fetch-Site": {"same-origin"}}, http.StatusForbidden},
		{"older browser Referer fallback", "192.168.1.10:3000", http.Header{"Referer": {"http://192.168.1.10:3000/system/settings?tab=media"}}, http.StatusNoContent},
		{"forwarded headers cannot grant trust", "server:3000", http.Header{"Origin": {"https://foreign.example.test"}, "X-Forwarded-Host": {"foreign.example.test"}, "X-Forwarded-Proto": {"https"}, "Forwarded": {"host=foreign.example.test;proto=https"}}, http.StatusForbidden},
		{"JSON gate remains enforced", "server:3000", http.Header{"Origin": {"https://media.example.test"}, "Sec-Fetch-Site": {"same-origin"}, "Content-Type": {"application/jsonp"}}, http.StatusUnsupportedMediaType},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "http://"+test.host+"/change", strings.NewReader(`{}`))
			for name, values := range test.headers {
				request.Header[name] = values
			}
			if request.Header.Get("Content-Type") == "" {
				request.Header.Set("Content-Type", "application/json; charset=utf-8")
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("status=%d want=%d body=%s", response.Code, test.status, response.Body.String())
			}
		})
	}
}
