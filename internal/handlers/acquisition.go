package handlers

import (
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/middleware"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/services"
)

func (a *API) MediaAcquisition(c *gin.Context) {
	tmdbID, err := strconv.ParseInt(c.Param("tmdbID"), 10, 64)
	if err != nil || tmdbID <= 0 || a.acquisition == nil {
		writeError(c, a.log, &services.AppError{Code: services.CodeInvalidRequest, Message: "媒体身份无效", Cause: err})
		return
	}
	actor, _ := middleware.ActorFrom(c)
	result, err := a.acquisition.Get(actor, c.Param("mediaType"), tmdbID)
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, 200, result)
}
