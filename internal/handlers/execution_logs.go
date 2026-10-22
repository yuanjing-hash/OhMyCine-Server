package handlers

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/middleware"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/services"
)

func (a *API) JobExecutionLogs(c *gin.Context) {
	filter, err := executionLogFilter(c)
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	actor, _ := middleware.ActorFrom(c)
	data, err := a.runtimeLogs.JobExecutionLogs(c.Request.Context(), actor, c.Param("id"), filter)
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, http.StatusOK, data)
}

func (a *API) FollowExecutionLogs(c *gin.Context) {
	filter, err := executionLogFilter(c)
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	actor, _ := middleware.ActorFrom(c)
	data, err := a.runtimeLogs.FollowExecutionLogs(c.Request.Context(), actor, c.Param("id"), c.Param("run_id"), filter)
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, http.StatusOK, data)
}

func executionLogFilter(c *gin.Context) (services.ExecutionLogFilter, error) {
	filter := services.ExecutionLogFilter{Level: c.Query("level"), Keyword: c.Query("keyword"), Cursor: c.Query("cursor")}
	var err error
	if raw := c.Query("attempt"); raw != "" {
		filter.Attempt, err = strconv.Atoi(raw)
		if err != nil || filter.Attempt < 1 {
			return filter, runtimeFilterError(err)
		}
	}
	if raw := c.Query("limit"); raw != "" {
		filter.Limit, err = strconv.Atoi(raw)
		if err != nil || filter.Limit < 1 || filter.Limit > 200 {
			return filter, runtimeFilterError(err)
		}
	}
	if raw := c.Query("snapshot_to"); raw != "" {
		filter.SnapshotTo, err = time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			return filter, runtimeFilterError(err)
		}
	}
	return filter, nil
}
