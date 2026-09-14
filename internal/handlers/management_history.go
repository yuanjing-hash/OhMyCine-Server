package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/middleware"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/services"
)

func (a *API) PreviewManagementHistoryPurge(c *gin.Context) {
	var input services.HistoryPurgeInput
	if err := strictJSON(c, &input); err != nil {
		writeError(c, a.log, invalid("清理范围无效", err))
		return
	}
	actor, _ := middleware.ActorFrom(c)
	result, err := a.queue.PreviewHistoryPurge(actor, input)
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, http.StatusOK, result)
}

func (a *API) PurgeManagementHistory(c *gin.Context) {
	var input services.HistoryPurgeInput
	if err := strictJSON(c, &input); err != nil {
		writeError(c, a.log, invalid("清理范围无效", err))
		return
	}
	actor, _ := middleware.ActorFrom(c)
	result, err := a.queue.PurgeHistory(actor, input, middleware.RequestContextFrom(c))
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, http.StatusOK, result)
}
