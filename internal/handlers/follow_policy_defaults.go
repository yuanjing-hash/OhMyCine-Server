package handlers

import (
	"github.com/gin-gonic/gin"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/middleware"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/services"
	"net/http"
)

func (a *API) FollowPolicyDefaults(c *gin.Context) {
	actor, _ := middleware.ActorFrom(c)
	result, err := a.follows.PolicyDefaults(c.Request.Context(), actor)
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, http.StatusOK, result)
}
func (a *API) UpdateFollowPolicyDefaults(c *gin.Context) {
	actor, _ := middleware.ActorFrom(c)
	var input services.UpdateFollowPolicyDefaultsInput
	if err := strictJSON(c, &input); err != nil {
		writeError(c, a.log, invalid("默认订阅策略无效", err))
		return
	}
	result, err := a.follows.UpdatePolicyDefaults(c.Request.Context(), actor, input, middleware.RequestContextFrom(c))
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, http.StatusOK, result)
}
