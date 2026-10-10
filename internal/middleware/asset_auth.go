package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/services"
)

// ReadAssetAuth accepts browser sessions or Player devices only on read-only
// image routes. An invalid bearer never falls back to a browser session.
func ReadAssetAuth(auth *services.AuthService, cookieName string) gin.HandlerFunc {
	browserAuth, deviceAuth := Auth(auth, cookieName), DeviceAuth(auth)
	return func(c *gin.Context) {
		if c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead {
			abortJSON(c, http.StatusMethodNotAllowed, services.CodePermissionDenied, "仅允许读取图片")
			return
		}
		if _, supplied := c.Request.Header["Authorization"]; supplied {
			deviceAuth(c)
			return
		}
		browserAuth(c)
	}
}
