package handlers

import (
	"github.com/gin-gonic/gin"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/middleware"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/services"
	"net/http"
	"strconv"
)

func (a *API) SubscriptionCalendar(c *gin.Context) {
	if a.calendar == nil {
		writeError(c, a.log, &services.AppError{Code: services.CodeQueueWorkerUnavailable, Message: "订阅日历暂不可用"})
		return
	}
	actor, _ := middleware.ActorFrom(c)
	size := 0
	if raw, ok := c.GetQuery("page_size"); ok {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 500 {
			writeError(c, a.log, &services.AppError{Code: services.CodeInvalidRequest, Message: "日历分页大小无效"})
			return
		}
		size = parsed
	}
	data, err := a.calendar.Read(c.Request.Context(), actor, services.CalendarQuery{Start: c.Query("start"), End: c.Query("end"), Timezone: c.Query("timezone"), Status: c.Query("status"), SubscriptionID: c.Query("subscription_id"), Cursor: c.Query("cursor"), PageSize: size})
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, http.StatusOK, data)
}
func (a *API) RefreshSubscriptionCalendar(c *gin.Context) {
	if a.calendar == nil {
		writeError(c, a.log, &services.AppError{Code: services.CodeQueueWorkerUnavailable, Message: "订阅日历暂不可用"})
		return
	}
	var input struct {
		SubscriptionID string `json:"subscription_id"`
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1024)
	if err := strictJSON(c, &input); err != nil {
		writeError(c, a.log, &services.AppError{Code: services.CodeInvalidRequest, Message: "排期更新参数无效"})
		return
	}
	actor, _ := middleware.ActorFrom(c)
	data, err := a.calendar.RequestRefresh(c.Request.Context(), actor, input.SubscriptionID)
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, http.StatusAccepted, data)
}
