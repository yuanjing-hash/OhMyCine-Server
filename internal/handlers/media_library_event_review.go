package handlers

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/middleware"
)

func (a *API) MediaLibraryProviderEventReviews(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	page, err := strconv.Atoi(c.DefaultQuery("page", "1"))
	if err != nil {
		writeError(c, a.log, invalid("页码无效", nil))
		return
	}
	actor, _ := middleware.ActorFrom(c)
	result, err := a.libraries.ProviderEventReviews(c.Request.Context(), actor, id, page)
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, http.StatusOK, result)
}
