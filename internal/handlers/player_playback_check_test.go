package handlers

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/services"
)

func TestPlayerPlaybackCheckRejectsMalformedModeBeforeResolver(t *testing.T) {
	a := &API{log: zerolog.Nop()}
	for _, tc := range []struct{ method, query string }{{"GET", "check="}, {"GET", "check=2"}, {"GET", "check=1&check=1"}, {"GET", "check=%ZZ"}, {"HEAD", "check=1"}} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(tc.method, "/api/v1/player/media-entries/1/stream?"+tc.query, nil)
		c.Params = gin.Params{{Key: "id", Value: "1"}}
		a.PlayerMediaEntryStream(c) // nil resolver would panic if mode validation fell through.
		if w.Code != http.StatusBadRequest || w.Header().Get("Location") != "" || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%+v status=%d", tc, w.Code)
		}
	}
}

func TestPlayerPlaybackCheckSafeErrorEnvelope(t *testing.T) {
	for code, status := range map[string]int{"quark_auth_expired": 503, "quark_rate_limited": 429, "quark_response_invalid": 502, "quark_unavailable": 503, "quark_capability_unavailable": 503, "quark_identity_mismatch": 409, "quark_shared_space_unverified": 409, "quark_directory_incomplete": 502} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("GET", "/", nil)
		writeError(c, zerolog.Nop(), &services.AppError{Code: code, Message: "夸克暂时不可用，请重试。", Cause: errors.New("https://private.invalid/?token=secret")})
		if w.Code != status || !strings.Contains(w.Body.String(), `"error_code":"`+code+`"`) || strings.Contains(w.Body.String(), "secret") || w.Header().Get("Location") != "" {
			t.Fatalf("unsafe envelope %s %d", code, w.Code)
		}
	}
}
