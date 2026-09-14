package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/middleware"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/services"
)

type createTransferNodePayload struct {
	Name         string `json:"name"`
	APIURL       string `json:"api_url"`
	Platform     string `json:"platform"`
	Architecture string `json:"architecture"`
}

type updateTransferNodePayload struct {
	Name     *string `json:"name"`
	APIURL   *string `json:"api_url"`
	Enabled  *bool   `json:"enabled"`
	Revision uint64  `json:"revision"`
}

func (a *API) TransferNodes(c *gin.Context) {
	actor, _ := middleware.ActorFrom(c)
	items, err := a.transferNodes.List(actor)
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, http.StatusOK, gin.H{"list": items, "total": len(items)})
}

func (a *API) CreateTransferNode(c *gin.Context) {
	actor, _ := middleware.ActorFrom(c)
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 32<<10)
	var payload createTransferNodePayload
	if err := strictJSON(c, &payload); err != nil {
		writeError(c, a.log, invalid("传输节点配置无效", err))
		return
	}
	node, token, err := a.transferNodes.Create(c.Request.Context(), actor, services.CreateTransferNodeInput{
		Name:         payload.Name,
		APIURL:       payload.APIURL,
		Platform:     payload.Platform,
		Architecture: payload.Architecture,
	}, middleware.RequestContextFrom(c))
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	installation, err := a.transferNodes.Installation(node.ID, token)
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, http.StatusCreated, gin.H{"node": node, "enrollment_token": token, "expires_in_seconds": 600, "installation": installation})
}

func (a *API) UpdateTransferNode(c *gin.Context) {
	actor, _ := middleware.ActorFrom(c)
	id, ok := stringID(c)
	if !ok {
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 32<<10)
	var payload updateTransferNodePayload
	if err := strictJSON(c, &payload); err != nil {
		writeError(c, a.log, invalid("传输节点配置无效", err))
		return
	}
	node, err := a.transferNodes.Update(actor, id, services.UpdateTransferNodeInput{
		Name:     payload.Name,
		APIURL:   payload.APIURL,
		Enabled:  payload.Enabled,
		Revision: payload.Revision,
	}, middleware.RequestContextFrom(c))
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, http.StatusOK, node)
}

func (a *API) RegenerateTransferNodeEnrollment(c *gin.Context) {
	actor, _ := middleware.ActorFrom(c)
	id, ok := stringID(c)
	if !ok {
		return
	}
	token, expiresAt, err := a.transferNodes.RegenerateEnrollment(actor, id, middleware.RequestContextFrom(c))
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	installation, err := a.transferNodes.Installation(id, token)
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, http.StatusOK, gin.H{"enrollment_token": token, "expires_at": expiresAt, "installation": installation})
}

func (a *API) EnrollTransferNode(c *gin.Context) {
	actor, _ := middleware.ActorFrom(c)
	id, ok := stringID(c)
	if !ok {
		return
	}
	node, err := a.transferNodes.Enroll(c.Request.Context(), actor, id, middleware.RequestContextFrom(c))
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, http.StatusOK, node)
}

func (a *API) TestTransferNode(c *gin.Context) {
	actor, _ := middleware.ActorFrom(c)
	id, ok := stringID(c)
	if !ok {
		return
	}
	node, err := a.transferNodes.Test(c.Request.Context(), actor, id, middleware.RequestContextFrom(c))
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, http.StatusOK, node)
}

func (a *API) RevokeTransferNode(c *gin.Context) {
	actor, _ := middleware.ActorFrom(c)
	id, ok := stringID(c)
	if !ok {
		return
	}
	if err := a.transferNodes.Revoke(actor, id, middleware.RequestContextFrom(c)); err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, http.StatusOK, gin.H{"revoked": true})
}

func (a *API) DeleteTransferNode(c *gin.Context) {
	actor, _ := middleware.ActorFrom(c)
	id, ok := stringID(c)
	if !ok {
		return
	}
	if err := a.transferNodes.Delete(actor, id, middleware.RequestContextFrom(c)); err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, http.StatusOK, gin.H{"deleted": true})
}

func (a *API) TransferNodeSettings(c *gin.Context) {
	actor, _ := middleware.ActorFrom(c)
	settings, err := a.transferNodes.Settings(actor)
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, http.StatusOK, settings)
}

func (a *API) UpdateTransferNodeSettings(c *gin.Context) {
	actor, _ := middleware.ActorFrom(c)
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 16<<10)
	var payload struct {
		DefaultNodeID *string `json:"default_node_id"`
		Revision      uint64  `json:"revision"`
	}
	if err := strictJSON(c, &payload); err != nil {
		writeError(c, a.log, invalid("传输节点设置无效", err))
		return
	}
	settings, err := a.transferNodes.UpdateSettings(actor, payload.DefaultNodeID, payload.Revision, middleware.RequestContextFrom(c))
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, http.StatusOK, settings)
}
