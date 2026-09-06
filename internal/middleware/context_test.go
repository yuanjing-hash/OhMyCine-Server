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
