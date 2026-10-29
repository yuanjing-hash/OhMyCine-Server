package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/middleware"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/services"
)

func (a *API) PreviewResourceContent(c *gin.Context) {
	actor, _ := middleware.ActorFrom(c)
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 8<<10)
	var input services.ContentPreviewInput
	if err := strictJSON(c, &input); err != nil {
		writeError(c, a.log, invalid("内容检查参数无效", err))
		return
	}
	value, err := a.sites.PreviewContent(c.Request.Context(), actor, input)
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	success(c, http.StatusOK, value)
}

func (a *API) SelectResourceContent(c *gin.Context) {
	actor, _ := middleware.ActorFrom(c)
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 32<<10)
	var input services.ContentSelectionInput
	if err := strictJSON(c, &input); err != nil {
		writeError(c, a.log, invalid("实际文件选择参数无效", err))
		return
	}
	value, err := a.sites.SelectContent(c.Request.Context(), actor, input)
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	success(c, http.StatusOK, value)
}
