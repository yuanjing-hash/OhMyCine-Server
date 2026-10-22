package handlers

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/middleware"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/services"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/site/pansou"
)

func parseMixedDiscovery(c *gin.Context) (services.SiteSearchInput, error) {
	var result services.SiteSearchInput
	for _, value := range c.QueryArray("client_capabilities") {
		for _, part := range strings.Split(value, ",") {
			part = strings.TrimSpace(part)
			if part == "" || len(part) > 64 || len(result.ClientCapabilities) >= 16 {
				return result, invalid("搜索客户端能力无效", nil)
			}
			result.ClientCapabilities = append(result.ClientCapabilities, part)
		}
	}
	result.TypePolicy = strings.TrimSpace(c.Query("type_policy"))
	if result.TypePolicy != "" && result.TypePolicy != "auto" && result.TypePolicy != "all" && result.TypePolicy != "manual" {
		return result, invalid("搜索类型策略无效", nil)
	}
	if values, present := c.Request.URL.Query()["cloud_types"]; present {
		result.CloudTypes = []string{}
		for _, value := range values {
			result.CloudTypes = append(result.CloudTypes, strings.Split(value, ",")...)
		}
		var err error
		result.CloudTypes, err = pansou.NormalizeTypes(result.CloudTypes)
		if err != nil {
			return result, invalid("搜索结果类型无效", nil)
		}
	}
	if result.TypePolicy == "manual" && len(result.CloudTypes) == 0 {
		return result, invalid("请选择搜索结果类型", nil)
	}
	result.SnapshotID = strings.TrimSpace(c.Query("snapshot_id"))
	if result.SnapshotID != "" && (len(result.SnapshotID) != 48 || strings.ContainsAny(result.SnapshotID, "/\\\x00\r\n")) {
		return result, invalid("搜索快照无效", nil)
	}
	if raw, present := c.GetQuery("refresh"); present {
		if raw != "true" && raw != "false" {
			return result, invalid("搜索刷新参数无效", nil)
		}
		result.Refresh = raw == "true"
	}
	return result, nil
}

func (a *API) SavedPanSouScope(c *gin.Context) {
	actor, _ := middleware.ActorFrom(c)
	id, ok := pathID(c)
	if !ok {
		return
	}
	scope, err := a.sites.SavedPanSouScope(c.Request.Context(), actor, id, c.Query("refresh") == "true")
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	success(c, http.StatusOK, scope)
}

func (a *API) CandidatePanSouScope(c *gin.Context) {
	actor, _ := middleware.ActorFrom(c)
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 64<<10)
	var input services.PanSouScopeInput
	if err := strictJSON(c, &input); err != nil {
		writeError(c, a.log, invalid("PanSou频道检测参数无效", err))
		return
	}
	scope, err := a.sites.PanSouScope(c.Request.Context(), actor, input)
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	success(c, http.StatusOK, scope)
}

func (a *API) DiscoveryResultLink(c *gin.Context) {
	actor, _ := middleware.ActorFrom(c)
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4<<10)
	var input struct {
		ResultToken string `json:"result_token"`
	}
	if err := strictJSON(c, &input); err != nil {
		writeError(c, a.log, invalid("搜索结果链接参数无效", err))
		return
	}
	item, err := a.sites.ResultLink(c.Request.Context(), actor, input.ResultToken)
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	success(c, http.StatusOK, item)
}
