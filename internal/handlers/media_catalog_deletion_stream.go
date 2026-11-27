package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/middleware"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/services"
)

type catalogDeletionStream struct {
	c      *gin.Context
	writer http.ResponseWriter
	cancel context.CancelFunc
	mu     sync.Mutex
	stop   chan struct{}
	done   chan struct{}
}

func newCatalogDeletionStream(c *gin.Context) *catalogDeletionStream {
	ctx, cancel := context.WithCancel(c.Request.Context())
	c.Request = c.Request.WithContext(ctx)
	var writer http.ResponseWriter = c.Writer
	for {
		unwrapper, ok := writer.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			break
		}
		writer = unwrapper.Unwrap()
	}
	w := &catalogDeletionStream{c: c, writer: writer, cancel: cancel, stop: make(chan struct{}), done: make(chan struct{})}
	c.Header("Content-Type", "text/event-stream; charset=utf-8")
	c.Header("Cache-Control", "no-store")
	c.Header("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)
	w.write([]byte(": connected\n\n"))
	go func() {
		defer close(w.done)
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				w.write([]byte(": keep-alive\n\n"))
			case <-w.stop:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
	return w
}

func (w *catalogDeletionStream) close() { close(w.stop); <-w.done; w.cancel() }

func (w *catalogDeletionStream) write(frame []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.c.Request.Context().Err() != nil {
		return
	}
	controller := http.NewResponseController(w.writer)
	// Refresh only this active response's deadline. A healthy deletion stream
	// must not be cut off by the ordinary HTTP server's sixty-second deadline.
	_ = controller.SetWriteDeadline(time.Now().Add(60 * time.Second))
	if _, err := w.c.Writer.Write(frame); err != nil {
		w.cancel()
		return
	}
	if err := controller.Flush(); err != nil {
		w.cancel()
	}
}

func (w *catalogDeletionStream) event(name string, value any) {
	raw, err := json.Marshal(value)
	if err != nil {
		w.cancel()
		return
	}
	w.write([]byte(fmt.Sprintf("event: %s\ndata: %s\n\n", name, raw)))
}

func (w *catalogDeletionStream) progress(value services.MediaCatalogDeletionProgress) {
	w.event("progress", value)
}

func (w *catalogDeletionStream) finish(result any, err error) {
	if err != nil {
		w.event("error", gin.H{"code": services.ErrorCode(err), "message": services.ErrorMessage(err)})
		return
	}
	w.event("result", result)
}

func (a *API) MediaLibraryCatalogDeletionStatus(c *gin.Context) {
	actor, _ := middleware.ActorFrom(c)
	id, ok := pathID(c)
	if !ok {
		return
	}
	result, err := a.libraries.CatalogDeletionStatus(c.Request.Context(), actor, id, c.Param("work"), c.Param("deletion"))
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, http.StatusOK, result)
}
