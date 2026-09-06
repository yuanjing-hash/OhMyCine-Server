package handlers

import (
	"github.com/gin-gonic/gin"
	"net/http"
)

func (a *API) Notifications(c *gin.Context) {
	page, size, err := historyPageParameters(c)
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	result, err := a.queue.Notifications(c.Request.Context(), mustActor(c), page, size)
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, http.StatusOK, result)
}

func (a *API) AcknowledgeNotification(c *gin.Context) {
	var input struct {
		Occurrence uint64 `json:"occurrence"`
	}
	if err := strictJSON(c, &input); err != nil {
		writeError(c, a.log, invalid("通知参数无效", err))
		return
	}
	if err := a.queue.AcknowledgeNotification(c.Request.Context(), mustActor(c), c.Param("id"), input.Occurrence); err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, http.StatusOK, gin.H{"read": true})
}
