package handlers

import (
	"context"
	"github.com/gin-gonic/gin"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/middleware"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/services"
	"net/http"
	"time"
)

// Browser cold start is bounded separately from the guest HTTP operation. Extend
// only these authenticated handlers, retaining the global deadline elsewhere.
func browserOperationDeadline(c *gin.Context) context.CancelFunc {
	_ = http.NewResponseController(c.Writer).SetWriteDeadline(time.Now().Add(175 * time.Second))
	ctx, cancel := context.WithTimeout(c.Request.Context(), 170*time.Second)
	c.Request = c.Request.WithContext(ctx)
	return cancel
}

func (a *API) BrowserComponent(c *gin.Context) {
	actor, _ := middleware.ActorFrom(c)
	c.Header("Cache-Control", "no-store")
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 8192)
	var input services.BrowserInput
	if c.Request.Method != http.MethodGet {
		if err := strictJSON(c, &input); err != nil {
			writeError(c, a.log, invalid("浏览器请求无效", nil))
			return
		}
	}
	output, err := a.pluginRepositories.BrowserComponent(c.Request.Context(), actor, c.Param("browser_operation"), input)
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, http.StatusOK, output)
}

func (a *API) PluginResourceBrowser(c *gin.Context) {
	defer browserOperationDeadline(c)()
	if c.Request.Method == http.MethodGet && c.Param("browser_operation") != "status" {
		c.Status(http.StatusMethodNotAllowed)
		return
	}
	actor, _ := middleware.ActorFrom(c)
	c.Header("Cache-Control", "no-store")
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 8192)
	var input services.BrowserInput
	if c.Request.Method != http.MethodGet {
		if err := strictJSON(c, &input); err != nil {
			writeError(c, a.log, invalid("浏览器请求无效", nil))
			return
		}
	}
	output, err := a.pluginRepositories.ResourceBrowser(c.Request.Context(), actor, c.Param("plugin_id"), c.Param("connection_id"), c.Param("browser_operation"), input)
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, http.StatusOK, output)
}
