package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

func (a *API) PutPlayerHistoryArtwork(c *gin.Context) {
	if a.playerHistory == nil {
		writeError(c, a.log, invalid("Server 暂不支持历史图片", nil))
		return
	}
	// The production HTTP server has a 30-second body read deadline. Gin's
	// wrapped writer does not implement ResponseController deadline support.
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 2<<20)
	result, err := a.playerHistory.PutArtwork(c.Request.Context(), mustActor(c), c.Param("key"), c.Param("slot"), c.GetHeader("Content-Type"), c.Request.Body)
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, http.StatusOK, result)
}

func (a *API) PlayerHistoryArtwork(c *gin.Context) {
	if a.playerHistory == nil {
		writeError(c, a.log, invalid("Server 暂不支持历史图片", nil))
		return
	}
	body, err := a.playerHistory.Artwork(c.Request.Context(), mustActor(c), c.Param("id"))
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Data(http.StatusOK, http.DetectContentType(body), body)
}
