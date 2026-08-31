package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/middleware"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/services"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/updater"
)

type fakeUpdateHandlerService struct {
	status services.UpdateStatus
	err    error
	input  services.UpdateInstallInput
}

func (f *fakeUpdateHandlerService) Status(services.Actor) (services.UpdateStatus, error) {
	return f.status, f.err
}
func (f *fakeUpdateHandlerService) Check(context.Context, services.Actor, services.RequestContext) (services.UpdateStatus, error) {
	return f.status, f.err
}
func (f *fakeUpdateHandlerService) UpdateSettings(services.Actor, services.UpdateSettingsInput, services.RequestContext) (services.UpdateStatus, error) {
	return f.status, f.err
}
func (f *fakeUpdateHandlerService) Install(_ services.Actor, input services.UpdateInstallInput, _ services.RequestContext) (services.UpdateStatus, error) {
	f.input = input
	return f.status, f.err
}

func TestUpdateHandlersUseNoStoreAndAcceptedEnvelope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fake := &fakeUpdateHandlerService{status: services.UpdateStatus{CurrentVersion: "1.0.0", Channel: "beta", Revision: 1, Phase: updater.PhaseDownloading}}
	api := &API{update: fake, log: zerolog.Nop()}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/v1/system/update/install", strings.NewReader(`{"target_version":"1.1.0"}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Set(middleware.ContextActor, services.Actor{})
	api.InstallUpdate(ctx)
	if recorder.Code != http.StatusAccepted || recorder.Header().Get("Cache-Control") != "no-store" || fake.input.TargetVersion != "1.1.0" {
		t.Fatalf("status=%d cache=%q input=%+v body=%s", recorder.Code, recorder.Header().Get("Cache-Control"), fake.input, recorder.Body.String())
	}
	var body response
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil || body.Code != 0 {
		t.Fatalf("invalid envelope: %s err=%v", recorder.Body.String(), err)
	}
}

func TestUpdateHandlerMapsStableUpdaterErrorWithoutDetails(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fake := &fakeUpdateHandlerService{err: &services.AppError{Code: updater.CodeNetwork, Message: "官方更新服务暂时不可用"}}
	api := &API{update: fake, log: zerolog.Nop()}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/v1/system/update/check", nil)
	ctx.Set(middleware.ContextActor, services.Actor{})
	api.CheckUpdate(ctx)
	if recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get("Cache-Control") != "no-store" || strings.Contains(recorder.Body.String(), "github") || !strings.Contains(recorder.Body.String(), updater.CodeNetwork) {
		t.Fatalf("status=%d cache=%q body=%s", recorder.Code, recorder.Header().Get("Cache-Control"), recorder.Body.String())
	}
}

func TestUpdateSettingsRejectsUnknownJSONFields(t *testing.T) {
	gin.SetMode(gin.TestMode)
	api := &API{update: &fakeUpdateHandlerService{}, log: zerolog.Nop()}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPatch, "/api/v1/system/update/settings", strings.NewReader(`{"channel":"beta","revision":1,"url":"https://evil.example"}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Set(middleware.ContextActor, services.Actor{})
	api.UpdateUpdateSettings(ctx)
	if recorder.Code != http.StatusBadRequest || recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status=%d cache=%q body=%s", recorder.Code, recorder.Header().Get("Cache-Control"), recorder.Body.String())
	}
}
