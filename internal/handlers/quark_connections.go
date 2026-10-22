package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/middleware"
)

func (a *API) QuarkConnectionStatus(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	actor, _ := middleware.ActorFrom(c)
	id, ok := pathID(c)
	if !ok {
		return
	}
	result, err := a.connections.QuarkStatus(c.Request.Context(), actor, id)
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, http.StatusOK, result)
}
func (a *API) StartQuarkConnectionAuth(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	actor, _ := middleware.ActorFrom(c)
	id, ok := pathID(c)
	if !ok {
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1024)
	var payload struct {
		Purpose string `json:"purpose"`
	}
	if err := strictJSON(c, &payload); err != nil {
		writeError(c, a.log, invalid("授权请求无效", nil))
		return
	}
	result, err := a.connections.StartQuarkAuth(c.Request.Context(), actor, id, payload.Purpose, middleware.RequestContextFrom(c))
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, http.StatusCreated, result)
}
func (a *API) PollQuarkConnectionAuth(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	actor, _ := middleware.ActorFrom(c)
	id, ok := pathID(c)
	if !ok {
		return
	}
	result, err := a.connections.PollQuarkAuth(c.Request.Context(), actor, id, c.Param("sessionID"), middleware.RequestContextFrom(c))
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, http.StatusOK, result)
}
func (a *API) CancelQuarkConnectionAuth(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	actor, _ := middleware.ActorFrom(c)
	id, ok := pathID(c)
	if !ok {
		return
	}
	if err := a.connections.CancelQuarkAuth(actor, id, c.Param("sessionID")); err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, http.StatusOK, gin.H{"cancelled": true})
}
func (a *API) VerifyQuarkConnectionRoot(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	actor, _ := middleware.ActorFrom(c)
	id, ok := pathID(c)
	if !ok {
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 16<<10)
	var payload struct {
		Token string `json:"provider_picker_token"`
	}
	if err := strictJSON(c, &payload); err != nil {
		writeError(c, a.log, invalid("目录验证请求无效", nil))
		return
	}
	selection, err := a.providerDirectory.ResolveSelection(c.Request.Context(), actor, id, payload.Token)
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	result, err := a.connections.VerifyQuarkRoot(c.Request.Context(), actor, id, selection.ProviderID, selection.DisplayPath, middleware.RequestContextFrom(c))
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, http.StatusOK, result)
}

func (a *API) RetryQuarkProofCleanup(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	actor, _ := middleware.ActorFrom(c)
	id, ok := pathID(c)
	if !ok {
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1024)
	var payload struct{}
	if err := strictJSON(c, &payload); err != nil {
		writeError(c, a.log, invalid("验证文件清理请求无效", nil))
		return
	}
	result, err := a.connections.RetryQuarkProofCleanup(c.Request.Context(), actor, id, middleware.RequestContextFrom(c))
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, http.StatusOK, result)
}
