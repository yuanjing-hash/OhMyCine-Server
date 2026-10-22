package handlers

import (
	"github.com/gin-gonic/gin"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/middleware"
	"net/http"
)

func (a *API) QuarkIntake(c *gin.Context) {
	actor, _ := middleware.ActorFrom(c)
	id, ok := pathID(c)
	if !ok {
		return
	}
	result, err := a.libraries.QuarkIntake(c.Request.Context(), actor, id)
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, http.StatusOK, result)
}

func (a *API) SetQuarkIntake(c *gin.Context) {
	actor, _ := middleware.ActorFrom(c)
	id, ok := pathID(c)
	if !ok {
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 2048)
	var payload struct {
		Enabled      bool   `json:"enabled"`
		DownloaderID string `json:"downloader_id"`
	}
	if err := strictJSON(c, &payload); err != nil {
		writeError(c, a.log, invalid("接收目录设置无效", err))
		return
	}
	result, err := a.libraries.SetQuarkIntake(c.Request.Context(), actor, id, payload.Enabled, payload.DownloaderID, middleware.RequestContextFrom(c))
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, http.StatusOK, result)
}
