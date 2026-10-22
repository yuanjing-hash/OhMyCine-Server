//go:build quark_live

package handlers_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestQuarkSpeedExactAnonymousBodyOnly(t *testing.T) {
	body := bytes.Repeat([]byte("synthetic-media"), 4096)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" || r.Header.Get("Referer") != "" || r.Header.Get("Accept-Encoding") != "identity" || r.Header.Get("Range") != fmt.Sprintf("bytes=100-%d", 99+len(body)) || r.Header.Get("User-Agent") != "Go-http-client/1.1" {
			t.Error("anonymous request headers differed")
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 100-%d/1000000", 99+len(body)))
		w.Header().Set("X-Synthetic-Header", strings.Repeat("h", 8192))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(body)
	}))
	defer server.Close()
	client := quarkSpeedClient("direct")
	defer client.CloseIdleConnections()
	result := quarkSpeedMeasure(context.Background(), client, server.URL, "go-head", "", "direct", 100, int64(len(body)), 1000000, time.Second)
	sum := sha256.Sum256(body)
	if !result.Complete || result.Bytes != int64(len(body)) || result.SHA256 != hex.EncodeToString(sum[:]) || result.TTFBMS <= 0 || result.FirstBodyMS <= 0 || result.KiBPerSecond <= 0 {
		t.Fatalf("bad complete sample: %+v", result)
	}
}

func TestQuarkSpeedTimeoutKeepsPartialMetrics(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", "bytes 0-65535/1000000")
		w.Header().Set("Content-Length", "65536")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(bytes.Repeat([]byte("x"), 4096))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	client := quarkSpeedClient("direct")
	defer client.CloseIdleConnections()
	result := quarkSpeedMeasure(context.Background(), client, server.URL, "go-head", "", "direct", 0, 65536, 1000000, 100*time.Millisecond)
	if result.Complete || result.Bytes <= 0 || result.Bytes > 4096 || result.SHA256 != "" || result.Outcome != "body_timeout" || result.ElapsedMS > 1000 || result.KiBPerSecond <= 0 {
		t.Fatalf("bad partial timeout: %+v", result)
	}
}

func TestQuarkSpeedOpenEndedRangeStillCapsBody(t *testing.T) {
	const size, start, cap = int64(10 << 20), int64(500), int64(64 << 10)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "bytes=500-" {
			t.Error("open-ended request range was changed")
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, size-1, size))
		w.Header().Set("Content-Length", fmt.Sprint(size-start))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(bytes.Repeat([]byte("x"), int(cap)))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	client := quarkSpeedClient("direct")
	defer client.CloseIdleConnections()
	result := quarkSpeedMeasure(context.Background(), client, server.URL, "go-open-middle", "", "direct", start, cap, size, time.Second)
	if !result.Complete || result.WholeRangeConsumed || result.Bytes != cap || result.RequestedBytes != cap || result.RangeSpanBytes != size-start || result.RangeEnd != size-1 || result.SHA256 == "" {
		t.Fatalf("open range cap or scope incorrect: %+v", result)
	}
}

func TestQuarkSpeedPrematureEOFIsNotComplete(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", "bytes 0-65535/1000000")
		w.Header().Set("Content-Length", "65536")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("partial"))
	}))
	defer server.Close()
	client := quarkSpeedClient("direct")
	defer client.CloseIdleConnections()
	result := quarkSpeedMeasure(context.Background(), client, server.URL, "go-head", "", "direct", 0, 65536, 1000000, time.Second)
	if result.Complete || result.Bytes != 7 || result.SHA256 != "" || result.Outcome != "body_incomplete" {
		t.Fatalf("bad EOF metrics: %+v", result)
	}
}

func TestQuarkSpeedExplicitEOFStillCapsBody(t *testing.T) {
	const size, sampleLimit = int64(10 << 20), int64(64 << 10)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Range") != fmt.Sprintf("bytes=0-%d", size-1) || r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" || r.Header.Get("Referer") != "" {
			t.Error("explicit EOF range or anonymous header contract changed")
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", size-1, size))
		w.Header().Set("Content-Length", fmt.Sprint(size))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(bytes.Repeat([]byte("x"), int(sampleLimit)))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	client := quarkSpeedClient("direct")
	defer client.CloseIdleConnections()
	result := quarkSpeedMeasure(context.Background(), client, server.URL, "go-explicit-eof-head", "", "direct", 0, sampleLimit, size, time.Second)
	if calls != 1 || !result.Complete || result.WholeRangeConsumed || result.Bytes != sampleLimit || result.RequestedBytes != sampleLimit || result.RangeSpanBytes != size || result.RangeEnd != size-1 {
		t.Fatalf("explicit EOF did not retain bounded single request: %+v calls=%d", result, calls)
	}
}

func TestQuarkSpeedRejectsWrongRangeAndStatusWithoutReadingBody(t *testing.T) {
	for _, tc := range []struct {
		name, rangeValue, outcome, encoding string
		status                              int
	}{
		{"ignored", "", "range_rejected", "", 200},
		{"redirect", "", "range_rejected", "", 302},
		{"wrong_offset", "bytes 1-64/1000", "range_metadata_invalid", "", 206},
		{"wrong_total", "bytes 0-63/2000", "range_metadata_invalid", "", 206},
		{"encoded", "bytes 0-63/1000", "range_metadata_invalid", "gzip", 206},
		{"auth", "", "access_rejected", "", 401},
		{"payment", "", "access_rejected", "", 402},
		{"denied", "", "access_rejected", "", 403},
		{"cooldown", "", "rate_limited", "", 429},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Content-Range", tc.rangeValue)
				w.Header().Set("Content-Encoding", tc.encoding)
				w.Header().Set("Location", "/must-not-follow?refresh_token=synthetic-secret")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte("refresh_token=synthetic-secret"))
			}))
			defer server.Close()
			client := quarkSpeedClient("direct")
			defer client.CloseIdleConnections()
			result := quarkSpeedMeasure(context.Background(), client, server.URL+"/?token=synthetic-secret", "go-head", "", "direct", 0, 64, 1000, time.Second)
			encoded, _ := json.Marshal(result)
			if result.Outcome != tc.outcome || result.Bytes != 0 || result.Complete || calls != 1 || bytes.Contains(encoded, []byte("synthetic-secret")) || bytes.Contains(encoded, []byte(server.URL)) {
				t.Fatalf("bad rejected response: %s", encoded)
			}
		})
	}
}

func TestQuarkSpeedTLSVerificationAndInvalidURL(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("untrusted certificate accepted") }))
	defer server.Close()
	client := quarkSpeedClient("direct")
	defer client.CloseIdleConnections()
	result := quarkSpeedMeasure(context.Background(), client, server.URL, "go-head", "", "direct", 0, 64, 1000, time.Second)
	if result.Outcome != "transport_failed" || result.Bytes != 0 {
		t.Fatalf("TLS verification bypassed: %+v", result)
	}
	for _, invalid := range []string{"file:///x", "https://user:password@example.test/x", "https://example.test/x#fragment", "https://example.test/\\bad", "https://example.test/\r\n"} {
		result = quarkSpeedMeasure(context.Background(), client, invalid, "go-head", "", "direct", 0, 64, 1000, time.Second)
		if result.Outcome != "request_invalid" {
			t.Fatal("unsafe URL was not rejected before request")
		}
	}
}

func TestQuarkSpeedOptionsBoundedAndExplicitUA(t *testing.T) {
	for _, name := range []string{"OMC_QUARK_SPEED_MIB", "OMC_QUARK_SPEED_SECONDS", "OMC_QUARK_SPEED_VARIANTS", "OMC_QUARK_SPEED_PLAYER_UA", "OMC_QUARK_SPEED_PROXY_MODE"} {
		t.Setenv(name, "")
	}
	options, err := quarkSpeedOptionsFromEnv()
	if err != nil || options.bytes != 32<<20 || options.timeout != 20*time.Second || options.proxyMode != "environment" || len(options.variants) != 1 || options.variants[0] != "go-head" {
		t.Fatal("unexpected defaults")
	}
	for name, value := range map[string]string{"OMC_QUARK_SPEED_MIB": "33", "OMC_QUARK_SPEED_SECONDS": "61", "OMC_QUARK_SPEED_VARIANTS": "go-head,go-head", "OMC_QUARK_SPEED_PROXY_MODE": "automatic-bypass"} {
		t.Setenv(name, value)
		if _, err := quarkSpeedOptionsFromEnv(); err == nil {
			t.Fatal("unbounded option accepted")
		}
		t.Setenv(name, "")
	}
	t.Setenv("OMC_QUARK_SPEED_VARIANTS", "player-head")
	if _, err := quarkSpeedOptionsFromEnv(); err == nil {
		t.Fatal("unprovided Player UA invented")
	}
	t.Setenv("OMC_QUARK_SPEED_PLAYER_UA", "mpv/synthetic\r\nAuthorization: invalid")
	if _, err := quarkSpeedOptionsFromEnv(); err == nil {
		t.Fatal("injected header accepted")
	}
	t.Setenv("OMC_QUARK_SPEED_PLAYER_UA", "mpv/synthetic-version")
	if _, err := quarkSpeedOptionsFromEnv(); err != nil {
		t.Fatal("explicit UA rejected")
	}
}
