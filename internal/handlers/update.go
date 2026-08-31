package handlers

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/middleware"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/services"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/updater"
)

type updateService interface {
	Status(services.Actor) (services.UpdateStatus, error)
	Check(context.Context, services.Actor, services.RequestContext) (services.UpdateStatus, error)
	UpdateSettings(services.Actor, services.UpdateSettingsInput, services.RequestContext) (services.UpdateStatus, error)
	Install(services.Actor, services.UpdateInstallInput, services.RequestContext) (services.UpdateStatus, error)
}

func (a *API) UpdateStatus(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	actor, _ := middleware.ActorFrom(c)
	item, err := a.update.Status(actor)
	if err != nil {
		writeUpdateError(c, a.log, err)
		return
	}
	success(c, http.StatusOK, item)
}

func (a *API) CheckUpdate(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	actor, _ := middleware.ActorFrom(c)
	item, err := a.update.Check(c.Request.Context(), actor, middleware.RequestContextFrom(c))
	if err != nil {
		writeUpdateError(c, a.log, err)
		return
	}
	success(c, http.StatusOK, item)
}

func (a *API) UpdateUpdateSettings(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 8<<10)
	var payload struct {
		Channel  string `json:"channel"`
		Revision uint64 `json:"revision"`
	}
	if err := strictJSON(c, &payload); err != nil {
		writeError(c, a.log, invalid("更新设置无效", err))
		return
	}
	actor, _ := middleware.ActorFrom(c)
	item, err := a.update.UpdateSettings(actor, services.UpdateSettingsInput{Channel: payload.Channel, Revision: payload.Revision}, middleware.RequestContextFrom(c))
	if err != nil {
		writeUpdateError(c, a.log, err)
		return
	}
	success(c, http.StatusOK, item)
}

func (a *API) InstallUpdate(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 8<<10)
	var payload struct {
		TargetVersion string `json:"target_version"`
	}
	if err := strictJSON(c, &payload); err != nil {
		writeError(c, a.log, invalid("更新版本无效", err))
		return
	}
	actor, _ := middleware.ActorFrom(c)
	item, err := a.update.Install(actor, services.UpdateInstallInput{TargetVersion: payload.TargetVersion}, middleware.RequestContextFrom(c))
	if err != nil {
		writeUpdateError(c, a.log, err)
		return
	}
	success(c, http.StatusAccepted, item)
}

func writeUpdateError(c *gin.Context, log zerolog.Logger, err error) {
	var appErr *services.AppError
	if errors.As(err, &appErr) && (appErr.Code == services.CodeInvalidRequest || appErr.Code == services.CodePermissionDenied || appErr.Code == services.CodeConflict) {
		writeError(c, log, err)
		return
	}
	code := services.ErrorCode(err)
	status := http.StatusInternalServerError
	switch code {
	case updater.CodeInvalidChannel, updater.CodeInvalidRelease:
		status = http.StatusBadRequest
	case updater.CodeUnsupportedPlatform:
		status = http.StatusConflict
	case updater.CodeNoRelease:
		status = http.StatusNotFound
	case updater.CodeNetwork:
		status = http.StatusServiceUnavailable
	case updater.CodeUntrustedSource, updater.CodeResponseTooLarge, updater.CodeChecksumInvalid, updater.CodeChecksumMismatch, updater.CodeArchiveInvalid, updater.CodeCandidateTooLarge:
		status = http.StatusBadGateway
	}
	if status == http.StatusInternalServerError {
		writeError(c, log, err)
		return
	}
	c.JSON(status, response{Code: status*100 + 1, Message: services.ErrorMessage(err), Data: gin.H{"error_code": code}})
}
