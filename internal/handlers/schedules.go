package handlers

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/middleware"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/services"
)

func (a *API) ScheduleActions(c *gin.Context) {
	success(c, http.StatusOK, gin.H{"list": a.schedules.Actions()})
}
func (a *API) Schedules(c *gin.Context) {
	rows, err := a.schedules.List(mustActor(c))
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, http.StatusOK, gin.H{"list": rows, "total": len(rows)})
}
func (a *API) ScheduleRuns(c *gin.Context) {
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
	rows, err := a.schedules.Runs(mustActor(c), c.Param("id"), limit)
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, http.StatusOK, gin.H{"list": rows, "total": len(rows)})
}
func (a *API) PreviewSchedule(c *gin.Context) {
	var input struct {
		Cron     string `json:"cron_expression"`
		Timezone string `json:"timezone"`
		Count    int    `json:"count"`
	}
	if err := strictJSON(c, &input); err != nil {
		writeError(c, a.log, invalid("Cron 预览参数无效", err))
		return
	}
	items, err := services.PreviewSchedule(input.Cron, input.Timezone, input.Count, time.Now())
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, http.StatusOK, gin.H{"list": items})
}
func (a *API) CreateSchedule(c *gin.Context) {
	var input services.ScheduleInput
	if err := strictJSON(c, &input); err != nil {
		writeError(c, a.log, invalid("计划任务参数无效", err))
		return
	}
	item, err := a.schedules.Create(mustActor(c), input, middleware.RequestContextFrom(c))
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, http.StatusCreated, item)
}
func (a *API) UpdateSchedule(c *gin.Context) {
	var input services.ScheduleInput
	if err := strictJSON(c, &input); err != nil {
		writeError(c, a.log, invalid("计划任务参数无效", err))
		return
	}
	item, err := a.schedules.Update(mustActor(c), c.Param("id"), input, middleware.RequestContextFrom(c))
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, http.StatusOK, item)
}
func (a *API) DeleteSchedule(c *gin.Context) {
	if err := a.schedules.Delete(mustActor(c), c.Param("id")); err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, http.StatusOK, gin.H{"deleted": true})
}
