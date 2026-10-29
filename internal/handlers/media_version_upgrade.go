package handlers

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/middleware"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/services"
)

func (a *API) PreviewMediaVersionUpgrade(c *gin.Context) {
	actor, _ := middleware.ActorFrom(c)
	libraryID, ok := pathID(c)
	if !ok {
		return
	}
	entry, err := strconv.ParseUint(c.Param("entry"), 10, 32)
	if err != nil || entry == 0 {
		writeError(c, a.log, invalid("媒体文件ID无效", nil))
		return
	}
	if a.versionUpgrades == nil {
		writeError(c, a.log, invalid("洗版服务尚未就绪", nil))
		return
	}
	result, err := a.versionUpgrades.Prepare(c.Request.Context(), actor, libraryID, c.Param("work"), uint(entry))
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, http.StatusOK, result)
}
func (a *API) MediaVersionUpgrade(c *gin.Context) {
	actor, _ := middleware.ActorFrom(c)
	if a.versionUpgrades == nil {
		writeError(c, a.log, invalid("洗版服务尚未就绪", nil))
		return
	}
	result, err := a.versionUpgrades.Get(c.Request.Context(), actor, c.Param("upgrade"))
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, http.StatusOK, result)
}
func (a *API) SubmitMediaVersionUpgrade(c *gin.Context) {
	actor, _ := middleware.ActorFrom(c)
	var input services.SubmitMediaVersionUpgradeInput
	if err := strictJSON(c, &input); err != nil {
		writeError(c, a.log, invalid("洗版提交参数无效", err))
		return
	}
	if a.versionUpgrades == nil {
		writeError(c, a.log, invalid("洗版服务尚未就绪", nil))
		return
	}
	result, err := a.versionUpgrades.Submit(c.Request.Context(), actor, c.Param("upgrade"), input, middleware.RequestContextFrom(c))
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, http.StatusAccepted, result)
}
