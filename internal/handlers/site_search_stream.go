package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/services"
)

func parseSearchProgressVersion(c *gin.Context) (int, error) {
	values, provided := c.Request.URL.Query()["progress_version"]
	if !provided {
		return 1, nil
	}
	if len(values) != 1 {
		return 0, invalid("不支持的搜索进度版本", nil)
	}
	version, err := strconv.Atoi(values[0])
	if err != nil || (version != 1 && version != 2) {
		return 0, invalid("不支持的搜索进度版本", nil)
	}
	return version, nil
}

func searchOperationDeadline(c *gin.Context) context.CancelFunc {
	_ = http.NewResponseController(c.Writer).SetWriteDeadline(time.Now().Add(services.SearchResponseTimeout))
	ctx, cancel := context.WithTimeout(c.Request.Context(), services.SearchResponseTimeout)
	c.Request = c.Request.WithContext(ctx)
	return cancel
}

type siteSearchStream struct {
	c                            *gin.Context
	cancel                       context.CancelFunc
	mu                           sync.Mutex
	started                      bool
	version, bytes, interimBytes int
	last                         services.SiteSearchProgress
	failure                      error
	stop                         chan struct{}
	done                         chan struct{}
}

func newSiteSearchStream(c *gin.Context, version int) (*siteSearchStream, error) {
	if _, ok := c.Writer.(http.Flusher); !ok {
		return nil, &services.AppError{Code: services.CodeSiteUnavailable, Message: "当前 HTTP 服务不支持流式搜索"}
	}
	w := &siteSearchStream{c: c, cancel: searchOperationDeadline(c), version: version, stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(w.done)
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				w.heartbeat()
			case <-w.stop:
				return
			case <-c.Request.Context().Done():
				return
			}
		}
	}()
	return w, nil
}

func (w *siteSearchStream) close() { close(w.stop); <-w.done; w.cancel() }

func (w *siteSearchStream) start() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.started || w.failure != nil {
		return
	}
	w.c.Header("Content-Type", "text/event-stream; charset=utf-8")
	w.c.Header("Cache-Control", "no-store")
	w.c.Header("X-Accel-Buffering", "no")
	w.c.Status(http.StatusOK)
	w.started = true
	w.c.Writer.WriteHeaderNow()
	w.flushLocked()
}

func (w *siteSearchStream) flushLocked() {
	// Gin implements Flush without an error return. Unwrap it so net/http's
	// FlushError reaches us and a disconnected peer cancels provider work.
	var writer http.ResponseWriter = w.c.Writer
	for {
		unwrapper, ok := writer.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			break
		}
		writer = unwrapper.Unwrap()
	}
	if err := http.NewResponseController(writer).Flush(); err != nil {
		w.failure = err
		w.cancel()
	}
}

func (w *siteSearchStream) writeLocked(frame []byte, final bool) {
	if !w.started || w.failure != nil || w.c.Request.Context().Err() != nil {
		return
	}
	if w.version == 2 {
		if !final && w.interimBytes+len(frame) > services.SearchStreamInterimBytes {
			return
		}
		if len(frame) > services.SearchSnapshotEventBytes || w.bytes+len(frame) > services.SearchStreamBytes {
			w.failure = errors.New("search_stream_limit")
			w.cancel()
			return
		}
	}
	n, err := w.c.Writer.Write(frame)
	w.bytes += n
	if !final {
		w.interimBytes += n
	}
	if err != nil || n != len(frame) {
		if err == nil {
			err = errors.New("short_search_write")
		}
		w.failure = err
		w.cancel()
		return
	}
	w.flushLocked()
}

func (w *siteSearchStream) event(event string, value any) {
	w.mu.Lock()
	defer w.mu.Unlock()
	final := event == "done" || event == "error"
	if group, ok := value.(services.SiteSearchGroup); ok {
		final = group.Complete == nil || *group.Complete
	}
	if progress, ok := value.(services.SiteSearchProgress); ok && event == "progress" {
		w.last = progress
		final = progress.Completed == progress.Total && progress.Running == 0 && progress.Pending == 0
	}
	payload, err := json.Marshal(value)
	if err != nil {
		w.failure = err
		w.cancel()
		return
	}
	w.writeLocked([]byte(fmt.Sprintf("event: %s\ndata: %s\n\n", event, payload)), final)
}

func (w *siteSearchStream) site(group services.SiteSearchGroup) { w.event("site", group) }
func (w *siteSearchStream) progress(progress services.SiteSearchProgress) {
	w.event("progress", progress)
}
func (w *siteSearchStream) heartbeat() {
	w.mu.Lock()
	defer w.mu.Unlock()
	// A coalesced snapshot stream must remain alive while the next upstream
	// query runs. At most 39 tiny comments fit inside the reserved final tail
	// during the 390-second route budget; they still count against the total
	// stream limit, but cannot be suppressed by the interim snapshot limit.
	w.writeLocked([]byte(": keep-alive\n\n"), true)
}

func (w *siteSearchStream) finish(err error, restError func(error)) {
	w.mu.Lock()
	started, failed, progress := w.started, w.failure, w.last
	w.mu.Unlock()
	if failed != nil || w.c.Request.Context().Err() != nil {
		return
	}
	if err != nil {
		if !started {
			restError(err)
		} else {
			w.event("error", gin.H{"code": services.ErrorCode(err), "message": "本轮搜索未完整完成，已取得的结果予以保留"})
		}
		return
	}
	w.event("done", progress)
}
