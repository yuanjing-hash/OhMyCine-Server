package handlers

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/services"
)

func TestSearchProgressVersionNegotiation(t *testing.T) {
	for _, test := range []struct {
		query   string
		version int
		bad     bool
	}{{"", 1, false}, {"?progress_version=1", 1, false}, {"?progress_version=2", 2, false}, {"?progress_version=3", 0, true}, {"?progress_version=", 0, true}, {"?progress_version=2&progress_version=1", 0, true}} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("GET", "/search"+test.query, nil)
		version, err := parseSearchProgressVersion(c)
		if (err != nil) != test.bad || !test.bad && version != test.version {
			t.Fatalf("%q version=%d err=%v", test.query, version, err)
		}
	}
}

func TestSearchDeadlineExtendsOnlySearchWriterAndRemainsCancellable(t *testing.T) {
	w := &browserDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/search", nil)
	cancel := searchOperationDeadline(c)
	if remaining := time.Until(w.deadline); remaining < 389*time.Second || remaining > 390*time.Second {
		t.Fatal("write budget missing", remaining)
	}
	if deadline, ok := c.Request.Context().Deadline(); !ok || time.Until(deadline) > 390*time.Second {
		t.Fatal("request budget missing")
	}
	cancel()
	if c.Request.Context().Err() != context.Canceled {
		t.Fatal("cancel did not terminate request")
	}
}

func TestSearchStreamHTTPHeadersHeartbeatSerializationAndOldDeadline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A short old write deadline models the ordinary 60-second server limit;
		// the search-specific handler must replace it before any long wait.
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(5 * time.Millisecond))
		c, _ := gin.CreateTestContext(w)
		c.Request = r
		stream, err := newSiteSearchStream(c, 2)
		if err != nil {
			t.Error(err)
			return
		}
		defer stream.close()
		stream.start()
		time.Sleep(15 * time.Millisecond)
		var concurrent sync.WaitGroup
		for i := 0; i < 4; i++ {
			concurrent.Add(1)
			go func() { defer concurrent.Done(); stream.heartbeat() }()
		}
		stream.progress(services.SiteSearchProgress{Total: 1, Pending: 1, ProgressVersion: 2, EventSequence: 1})
		concurrent.Wait()
		complete := true
		stream.site(services.SiteSearchGroup{SiteID: 1, Status: "success", Complete: &complete, Items: []services.SiteSearchResult{}})
		stream.progress(services.SiteSearchProgress{Total: 1, Completed: 1, Succeeded: 1, ProgressVersion: 2, EventSequence: 2})
		stream.finish(nil, func(err error) { t.Error(err) })
	}))
	defer server.Close()
	response, err := server.Client().Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.Header.Get("X-Accel-Buffering") != "no" || response.Header.Get("Cache-Control") != "no-store" || !strings.Contains(response.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatal("bad stream headers", response.Header)
	}
	if strings.Count(string(body), ": keep-alive\n\n") != 4 || strings.Count(string(body), "event: done\n") != 1 || strings.Count(string(body), "event: site\n") != 1 {
		t.Fatalf("frames interleaved/lost: %s", body)
	}
}

type failingSearchWriter struct{ *httptest.ResponseRecorder }

func (w *failingSearchWriter) Write([]byte) (int, error) { return 0, errors.New("peer disconnected") }

func TestSearchStreamWriteFailureCancelsUpstreamAndBudgetKeepsFinalFrames(t *testing.T) {
	c, _ := gin.CreateTestContext(&failingSearchWriter{httptest.NewRecorder()})
	c.Request = httptest.NewRequest("GET", "/search", nil)
	stream, err := newSiteSearchStream(c, 2)
	if err != nil {
		t.Fatal(err)
	}
	stream.start()
	stream.progress(services.SiteSearchProgress{Total: 1, Running: 1})
	stream.close()
	if c.Request.Context().Err() != context.Canceled || stream.failure == nil {
		t.Fatal("failed write did not cancel request")
	}
	recorder := httptest.NewRecorder()
	c, _ = gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest("GET", "/search", nil)
	stream, err = newSiteSearchStream(c, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.close()
	stream.start()
	stream.interimBytes = services.SearchStreamInterimBytes
	stream.heartbeat()
	if recorder.Body.String() != ": keep-alive\n\n" || stream.interimBytes != services.SearchStreamInterimBytes || stream.bytes != recorder.Body.Len() {
		t.Fatal("snapshot coalescing suppressed the idle heartbeat or skipped total accounting")
	}
	stream.progress(services.SiteSearchProgress{Total: 1, Running: 1})
	if strings.Contains(recorder.Body.String(), "event: progress") {
		t.Fatal("interim reserve exceeded")
	}
	stream.event("done", services.SiteSearchProgress{Total: 1, Completed: 1, Succeeded: 1})
	if !strings.Contains(recorder.Body.String(), "event: done") || stream.failure != nil {
		t.Fatal("final reserve unavailable", stream.failure)
	}
	stream.bytes = services.SearchStreamBytes
	before := recorder.Body.Len()
	stream.heartbeat()
	if stream.failure == nil || c.Request.Context().Err() != context.Canceled || recorder.Body.Len() != before {
		t.Fatal("heartbeat bypassed the overall stream hard limit")
	}
}
