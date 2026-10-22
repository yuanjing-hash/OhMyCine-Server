//go:build quark_live

package handlers_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func quarkBridgeEnabled() bool { return os.Getenv("OMC_QUARK_LIBMPV_BRIDGE_ENABLED") == "1" }

type quarkBridgeCounts struct {
	Requests, Redirects, Other, BodyBytes int64
	RedirectBodyBytes                     int64
	AcceptedCancelledErrors               int64
	AcceptedCancelledBodyBytes            int64
	UncancelledOther                      int64
	RejectedCancelledOther                int64
	HTTPStatuses                          map[int]int64
	CancelledHTTPStatuses                 map[int]int64
	ErrorCodes                            map[string]int64
}

// Only the observed, explicit client-cancellation tuple is permitted. Aggregate
// status/code maps alone cannot establish that both belonged to one request.
// Cancellation never excuses an authentication error, unknown error, or a body
// on a redirect. Error-envelope bytes remain reported separately from media.
func quarkBridgeResponseContract(c quarkBridgeCounts) bool {
	return c.Requests > 0 && c.Redirects > 0 && c.Requests == c.Redirects+c.Other &&
		c.Other == c.AcceptedCancelledErrors && c.UncancelledOther == 0 && c.RejectedCancelledOther == 0 &&
		c.RedirectBodyBytes == 0 && c.BodyBytes == c.AcceptedCancelledBodyBytes &&
		c.AcceptedCancelledBodyBytes >= 0 && c.AcceptedCancelledBodyBytes <= 4096*c.AcceptedCancelledErrors
}

type quarkBridgeMeter struct {
	mu     sync.Mutex
	counts quarkBridgeCounts
}

func (m *quarkBridgeMeter) snapshot() quarkBridgeCounts {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := m.counts
	result.HTTPStatuses = copyBridgeCounts(m.counts.HTTPStatuses)
	result.CancelledHTTPStatuses = copyBridgeCounts(m.counts.CancelledHTTPStatuses)
	result.ErrorCodes = copyBridgeCounts(m.counts.ErrorCodes)
	return result
}

func copyBridgeCounts[K comparable](source map[K]int64) map[K]int64 {
	result := make(map[K]int64, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func bridgeCountDelta[K comparable](after, before map[K]int64) map[K]int64 {
	result := make(map[K]int64)
	for key, value := range after {
		if count := value - before[key]; count > 0 {
			result[key] = count
		}
	}
	return result
}

type quarkBridgeResponse struct {
	http.ResponseWriter
	status int
	bytes  int64
	// Only a bounded error envelope is inspected in memory, never persisted or
	// logged. safeErrorCode discards it and returns a fixed vocabulary only.
	errorEnvelope []byte
	errorTooLarge bool
}

func (w *quarkBridgeResponse) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *quarkBridgeResponse) WriteHeader(code int) {
	if w.status == 0 && code >= 200 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}
func (w *quarkBridgeResponse) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	n, e := w.ResponseWriter.Write(p)
	w.bytes += int64(n)
	if w.status >= 400 && !w.errorTooLarge && n > 0 {
		if len(w.errorEnvelope)+n > 4096 {
			clear(w.errorEnvelope)
			w.errorEnvelope = nil
			w.errorTooLarge = true
		} else {
			w.errorEnvelope = append(w.errorEnvelope, p[:n]...)
		}
	}
	return n, e
}

func (w *quarkBridgeResponse) safeErrorCode() string {
	defer func() { clear(w.errorEnvelope); w.errorEnvelope = nil }()
	if w.status < 400 {
		return ""
	}
	var envelope struct {
		Data struct {
			ErrorCode string `json:"error_code"`
		} `json:"data"`
	}
	if w.errorTooLarge || json.Unmarshal(w.errorEnvelope, &envelope) != nil {
		return "unclassified_error"
	}
	switch envelope.Data.ErrorCode {
	case "NOT_AUTHENTICATED", "INVALID_CREDENTIALS", "PERMISSION_DENIED", "INVALID_REQUEST", "NOT_FOUND", "INTERNAL_ERROR", "DATABASE_BUSY",
		"proxy_signature_invalid", "proxy_signature_expired", "proxy_target_unavailable", "proxy_upstream_unavailable", "proxy_headers_unsupported", "proxy_unavailable", "proxy_device_limit",
		"quark_auth_expired", "quark_rate_limited", "quark_response_invalid", "quark_unavailable", "quark_capability_unavailable", "quark_identity_mismatch", "quark_shared_space_unverified", "quark_directory_incomplete":
		return envelope.Data.ErrorCode
	default:
		return "unclassified_error"
	}
}
func (w *quarkBridgeResponse) ReadFrom(r io.Reader) (int64, error) {
	return io.Copy(struct{ io.Writer }{w}, r)
}
func (w *quarkBridgeResponse) Flush() {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}
func (w *quarkBridgeResponse) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return http.NewResponseController(w.ResponseWriter).Hijack()
}
func (w *quarkBridgeResponse) Push(target string, options *http.PushOptions) error {
	if p, ok := w.ResponseWriter.(http.Pusher); ok {
		return p.Push(target, options)
	}
	return http.ErrNotSupported
}
func (w *quarkBridgeResponse) CloseNotify() <-chan bool {
	if c, ok := w.ResponseWriter.(http.CloseNotifier); ok {
		return c.CloseNotify()
	}
	return make(chan bool)
}

// Default execution retains the exact original handler. The enabled wrapper
// only meters stream responses and fixed error codes, never headers, raw bodies
// or URLs. Request cancellation is observed independently of the HTTP status.
func quarkBridgeInstrument(next http.Handler) (http.Handler, *quarkBridgeMeter) {
	if !quarkBridgeEnabled() {
		return next, nil
	}
	m := &quarkBridgeMeter{counts: quarkBridgeCounts{HTTPStatuses: map[int]int64{}, CancelledHTTPStatuses: map[int]int64{}, ErrorCodes: map[string]int64{}}}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/v1/player/media-entries/") || !strings.HasSuffix(r.URL.Path, "/stream") || r.URL.RawQuery != "" {
			next.ServeHTTP(w, r)
			return
		}
		rw := &quarkBridgeResponse{ResponseWriter: w}
		next.ServeHTTP(rw, r)
		code := rw.safeErrorCode()
		status := rw.status
		if status == 0 {
			status = http.StatusOK
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		m.counts.Requests++
		m.counts.BodyBytes += rw.bytes
		m.counts.HTTPStatuses[status]++
		if r.Context().Err() != nil {
			m.counts.CancelledHTTPStatuses[status]++
		}
		if code != "" {
			m.counts.ErrorCodes[code]++
		}
		if rw.status == http.StatusFound {
			m.counts.Redirects++
			m.counts.RedirectBodyBytes += rw.bytes
		} else {
			m.counts.Other++
			if r.Context().Err() == nil {
				m.counts.UncancelledOther++
			} else if errors.Is(r.Context().Err(), context.Canceled) && status == http.StatusBadGateway && code == "proxy_upstream_unavailable" {
				m.counts.AcceptedCancelledErrors++
				m.counts.AcceptedCancelledBodyBytes += rw.bytes
			} else {
				m.counts.RejectedCancelledOther++
			}
		}
	}), m
}

func runQuarkLiveBridgeHook(t *testing.T, ctx context.Context, origin, streamPath, token string, size int64, meter *quarkBridgeMeter, requestCount func() int) {
	t.Helper()
	if !quarkBridgeEnabled() {
		return
	}
	if size != quarkLibmpvExpectedSize || meter == nil || requestCount == nil || os.Getenv("OMC_QUARK_LIVE_MANUAL_SECONDS") != "0" {
		t.Fatal("bridge_validation_setup_invalid")
	}
	python, script := os.Getenv("OMC_QUARK_LIBMPV_PYTHON"), os.Getenv("OMC_QUARK_LIBMPV_BRIDGE_PROBE")
	for _, p := range []string{python, script} {
		st, e := os.Stat(p)
		if e != nil || !filepath.IsAbs(p) || !st.Mode().IsRegular() {
			t.Fatal("bridge_validation_helper_unavailable")
		}
	}
	if filepath.Base(script) != "live_bridge_curl_probe.py" {
		t.Fatal("bridge_validation_helper_invalid")
	}
	u, e := url.Parse(origin + streamPath)
	if e != nil || u.Scheme != "http" || !net.ParseIP(u.Hostname()).IsLoopback() {
		t.Fatal("bridge_validation_loopback_required")
	}
	request, e := http.NewRequestWithContext(ctx, http.MethodGet, origin+streamPath+"?check=1", nil)
	if e != nil {
		t.Fatal("bridge_check_invalid")
	}
	request.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, e := client.Do(request)
	if e != nil {
		t.Fatal("bridge_check_failed")
	}
	raw, e := io.ReadAll(io.LimitReader(response.Body, 4097))
	response.Body.Close()
	var check struct {
		Code int `json:"code"`
		Data struct {
			Ready   bool   `json:"ready"`
			Version int    `json:"version"`
			Profile string `json:"http_read_profile"`
		} `json:"data"`
	}
	if e != nil || len(raw) > 4096 || response.StatusCode != 200 || response.Header.Get("Location") != "" || len(response.Header.Values("Set-Cookie")) != 0 || json.Unmarshal(raw, &check) != nil || check.Code != 0 || !check.Data.Ready || check.Data.Version != 1 || check.Data.Profile != "bounded_ranges_v1" {
		t.Fatal("bridge_check_profile_unavailable")
	}
	input := struct {
		Version       int    `json:"version"`
		ServerURL     string `json:"server_url"`
		Authorization string `json:"authorization"`
		ExpectedSize  int64  `json:"expected_size"`
	}{1, origin + streamPath, "Bearer " + token, size}
	payload, _ := json.Marshal(input)
	before := meter.snapshot()
	apiBefore := requestCount()
	result, e := quarkRunLibmpvCurlPayload(ctx, payload, 50*time.Second, func(childContext context.Context) *exec.Cmd {
		return exec.CommandContext(childContext, python, "-I", "-B", script)
	})
	after := meter.snapshot()
	delta := quarkBridgeCounts{Requests: after.Requests - before.Requests, Redirects: after.Redirects - before.Redirects, Other: after.Other - before.Other, BodyBytes: after.BodyBytes - before.BodyBytes,
		RedirectBodyBytes: after.RedirectBodyBytes - before.RedirectBodyBytes, AcceptedCancelledErrors: after.AcceptedCancelledErrors - before.AcceptedCancelledErrors,
		AcceptedCancelledBodyBytes: after.AcceptedCancelledBodyBytes - before.AcceptedCancelledBodyBytes, UncancelledOther: after.UncancelledOther - before.UncancelledOther, RejectedCancelledOther: after.RejectedCancelledOther - before.RejectedCancelledOther,
		HTTPStatuses: bridgeCountDelta(after.HTTPStatuses, before.HTTPStatuses), CancelledHTTPStatuses: bridgeCountDelta(after.CancelledHTTPStatuses, before.CancelledHTTPStatuses), ErrorCodes: bridgeCountDelta(after.ErrorCodes, before.ErrorCodes)}
	t.Logf("libmpv_bridge_server requests=%d redirects=%d other=%d body_bytes=%d redirect_body_bytes=%d accepted_cancelled_errors=%d accepted_cancelled_body_bytes=%d uncancelled_other=%d rejected_cancelled_other=%d check_profile_bounded_ranges_v1=true", delta.Requests, delta.Redirects, delta.Other, delta.BodyBytes, delta.RedirectBodyBytes, delta.AcceptedCancelledErrors, delta.AcceptedCancelledBodyBytes, delta.UncancelledOther, delta.RejectedCancelledOther)
	diagnostics, _ := json.Marshal(struct {
		HTTPStatuses          map[int]int64    `json:"http_statuses"`
		CancelledHTTPStatuses map[int]int64    `json:"cancelled_http_statuses"`
		ErrorCodes            map[string]int64 `json:"error_codes"`
		GuardCallsBefore      int              `json:"guard_calls_before"`
		GuardCallsAfter       int              `json:"guard_calls_after"`
	}{delta.HTTPStatuses, delta.CancelledHTTPStatuses, delta.ErrorCodes, apiBefore, requestCount()})
	t.Logf("libmpv_bridge_diagnostics %s", diagnostics)
	t.Log("libmpv_bridge_timing profile=bridge_observation_v1 worker_observation_seconds=28 child_deadline_seconds=30 parent_deadline_seconds=45 go_deadline_seconds=50 pre_seek_observation_seconds=6 post_seek_observation_seconds=6")
	if e != nil {
		t.Fatal(e.Error())
	}
	encoded, _ := json.Marshal(result)
	t.Logf("libmpv_bridge_result %s", encoded)
	if !quarkBridgeResponseContract(delta) || result.Outcome != "completed" || !result.SizeMatchesExpected || !result.FileLoaded || !result.PlayedAny || !result.SeekRequested || result.SeekResult == nil || *result.SeekResult != 0 || result.TimePosAfterSeekMax == nil || *result.TimePosAfterSeekMax <= 120.1 {
		t.Fatal("libmpv_bridge_observation_incomplete")
	}
	t.Log(fmt.Sprintf("libmpv_bridge_completed=true media_request_count_known=false media_bytes_known=false native_deadline_seconds=30 parent_deadline_seconds=45 range_size=8388608"))
}
