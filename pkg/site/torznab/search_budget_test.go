package torznab

import (
	"context"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/site"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestSearchBudgetDoesNotChangeProbeOrDownloadClient(t *testing.T) {
	config := site.Config{BaseURL: "http://127.0.0.1:9117", Timeout: 12 * time.Second}
	client, _, err := controlledClient(config)
	if err != nil || client.Timeout != 12*time.Second {
		t.Fatal("probe timeout changed", err)
	}
	config.SearchTimeout = 120 * time.Second
	client, _, err = controlledClient(config)
	if err != nil || client.Timeout != 120*time.Second {
		t.Fatal("search still capped at 30 seconds", err)
	}
}

func TestSlowManualTimeoutDoesNotFallbackOrRepeatRequest(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); <-r.Context().Done() }))
	defer server.Close()
	adapter := NewForTest(server.Client(), server.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := adapter.Search(ctx, site.Config{BaseURL: server.URL, APIKey: "fixture", Timeout: 12 * time.Second}, site.Query{Keyword: "七武士", Page: 1})
	if err == nil || calls.Load() != 1 {
		t.Fatalf("timeout retried/fell back: calls=%d err=%v", calls.Load(), err)
	}
}

func TestDocumentedFallbackSharesRemainingQueryDeadline(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path == "/api/v2.0/indexers/all/results" {
			time.Sleep(15 * time.Millisecond)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		<-r.Context().Done()
	}))
	defer server.Close()
	adapter := NewForTest(server.Client(), server.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := adapter.Search(ctx, site.Config{BaseURL: server.URL, APIKey: "fixture"}, site.Query{Keyword: "test", Page: 1})
	if err == nil || calls.Load() != 2 || time.Since(started) > 150*time.Millisecond {
		t.Fatalf("fallback reset/duplicated budget: calls=%d duration=%v err=%v", calls.Load(), time.Since(started), err)
	}
}
