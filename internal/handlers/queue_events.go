package handlers

import (
	"context"
	"maps"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/middleware"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/services"
)

func (a *API) QueueEvents(c *gin.Context) {
	if a.queueEvents == nil {
		c.AbortWithStatus(http.StatusServiceUnavailable)
		return
	}
	allowed := map[string]struct{}{}
	for _, origin := range a.config.AllowedOrigins() {
		allowed[origin] = struct{}{}
	}
	upgrader := websocket.Upgrader{CheckOrigin: func(request *http.Request) bool { _, ok := allowed[request.Header.Get("Origin")]; return ok }}
	actor, ok := middleware.ActorFrom(c)
	token := middleware.SessionTokenFrom(c)
	if !ok || token == "" || a.auth == nil {
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}
	events, unsubscribe := a.queueEvents.Subscribe(actor)
	defer unsubscribe()
	connection, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}
	connection.SetReadLimit(1024)
	_ = connection.SetReadDeadline(time.Now().Add(45 * time.Second))
	connection.SetPongHandler(func(string) error { return connection.SetReadDeadline(time.Now().Add(45 * time.Second)) })
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		// NextReader processes control frames (including pong/close). The stream
		// accepts no application messages or commands from the client.
		_, _, _ = connection.NextReader()
	}()
	defer func() { _ = connection.Close(); <-readDone }()
	validate := func() (services.Actor, bool) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
		defer cancel()
		current, err := a.auth.RevalidateSession(ctx, token)
		if err != nil || current.User.ID != actor.User.ID {
			_ = connection.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "session invalid"), time.Now().Add(time.Second))
			return services.Actor{}, false
		}
		if !maps.Equal(current.Permissions, actor.Permissions) || !maps.Equal(current.DeniedPermissions, actor.DeniedPermissions) || current.User.AuthzVersion != actor.User.AuthzVersion {
			// Reconnect under a fresh subscription filter for both grants and
			// revocations. No DB call happens under the hub's mutex.
			_ = connection.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseServiceRestart, "authorization changed"), time.Now().Add(time.Second))
			return services.Actor{}, false
		}
		return current, true
	}
	idle := time.NewTicker(5 * time.Second)
	ping := time.NewTicker(15 * time.Second)
	defer idle.Stop()
	defer ping.Stop()
	for {
		select {
		case <-c.Request.Context().Done():
			return
		case <-readDone:
			return
		case <-idle.C:
			if _, ok := validate(); !ok {
				return
			}
		case <-ping.C:
			if err := connection.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)); err != nil {
				return
			}
		case event, open := <-events:
			if !open {
				return
			}
			current, ok := validate()
			if !ok {
				return
			}
			if !services.CanReceiveJobEvent(current, event) {
				continue
			}
			_ = connection.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if err := connection.WriteJSON(services.JobEventEnvelope{Type: event.Type, Data: event}); err != nil {
				return
			}
		}
	}
}
