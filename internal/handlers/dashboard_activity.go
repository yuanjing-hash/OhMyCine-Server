package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/middleware"
)

func (a *API) DashboardActivity(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	data, err := a.admin.DashboardActivity(c.Request.Context(), mustActor(c))
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, http.StatusOK, data)
}

func (a *API) RefreshStorageCapacity(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	id, ok := pathID(c)
	if !ok {
		return
	}
	data, err := a.storage.RefreshCapacity(c.Request.Context(), mustActor(c), id, middleware.RequestContextFrom(c))
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, http.StatusOK, data)
}
