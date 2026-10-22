//go:build quark_live

package handlers_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestQuarkLibmpvBridgeMeterOptInAndResponse(t *testing.T) {
	next := http.NewServeMux()
	next.HandleFunc("/api/v1/player/media-entries/1/stream", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("check") == "1" {
			_, _ = io.WriteString(w, `{"ready":true}`)
			return
		}
		if r.Header.Get("Authorization") == "" {
			http.Error(w, "denied", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Location", "https://synthetic.invalid/original?private=not-captured")
		w.WriteHeader(http.StatusFound)
	})
	t.Setenv("OMC_QUARK_LIBMPV_BRIDGE_ENABLED", "")
	h, m := quarkBridgeInstrument(next)
	if h != next || m != nil {
		t.Fatal("default handler changed")
	}
	t.Setenv("OMC_QUARK_LIBMPV_BRIDGE_ENABLED", "1")
	h, m = quarkBridgeInstrument(next)
	for _, endpoint := range []string{"/api/v1/player/media-entries/1/stream?check=1", "/api/v1/player/media-entries/1/stream"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", endpoint, nil))
	}
	before := m.snapshot()
	r := httptest.NewRequest("GET", "/api/v1/player/media-entries/1/stream", nil)
	r.Header.Set("Authorization", "Bearer synthetic")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	after := m.snapshot()
	if before.Requests != 1 || before.Other != 1 || before.BodyBytes != 7 || after.Requests-before.Requests != 1 || after.Redirects-before.Redirects != 1 || after.BodyBytes-before.BodyBytes != 0 || w.Code != 302 || w.Body.Len() != 0 {
		t.Fatal("meter changed response or included check/body data")
	}
	wrapped := &quarkBridgeResponse{ResponseWriter: httptest.NewRecorder()}
	n, e := wrapped.ReadFrom(strings.NewReader("fixture"))
	if e != nil || n != 7 || wrapped.bytes != 7 || wrapped.status != 200 {
		t.Fatal("ReaderFrom bypassed byte measurement")
	}
	if wrapped.Unwrap() == nil || http.NewResponseController(wrapped).Flush() != nil {
		t.Fatal("response control not forwarded")
	}
}

func TestQuarkLibmpvBridgePythonSchemaWithoutNetwork(t *testing.T) {
	home, e := os.UserHomeDir()
	if e != nil {
		t.Skip("bundled Python unavailable")
	}
	python := filepath.Join(home, ".cache", "codex-runtimes", "codex-primary-runtime", "dependencies", "python", "python.exe")
	probe, e := filepath.Abs("../../../validation/libmpv-http-probe/live_bridge_curl_probe.py")
	if e != nil {
		t.Fatal("helper path unavailable")
	}
	for _, p := range []string{python, probe} {
		if st, e := os.Stat(p); e != nil || !st.Mode().IsRegular() {
			t.Skip("local schema helper unavailable")
		}
	}
	result, e := quarkRunLibmpvCurlPayload(context.Background(), []byte(`{"synthetic_secret":"must-not-escape"}`), 35*time.Second, func(ctx context.Context) *exec.Cmd {
		return exec.CommandContext(ctx, python, "-I", "-B", probe, "--reject-without-input")
	})
	if e != nil || result.Outcome != "invalid_input" || result.FileLoaded || result.NativeDestroyed {
		t.Fatal("bridge schema mismatched or started native")
	}
}

func TestQuarkLibmpvBridgeDiagnosticsWhitelistAndCancellation(t *testing.T) {
	t.Setenv("OMC_QUARK_LIBMPV_BRIDGE_ENABLED", "1")
	for _, tc := range []struct {
		name, body, code string
		oversized        bool
	}{
		{"known", `{"message":"secret-url-and-token","data":{"error_code":"quark_unavailable","private":"secret-cookie"}}`, "quark_unavailable", false},
		{"unknown", `{"data":{"error_code":"secret-refresh-token"}}`, "unclassified_error", false},
		{"not_json", "secret-upstream-body", "unclassified_error", false},
		{"oversize", strings.Repeat("secret", 1000), "unclassified_error", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var captured *quarkBridgeResponse
			h, meter := quarkBridgeInstrument(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				captured = w.(*quarkBridgeResponse)
				w.Header().Set("Location", "https://synthetic.invalid/?secret=private")
				w.WriteHeader(503)
				_, _ = io.WriteString(w, tc.body[:len(tc.body)/2])
				_, _ = io.WriteString(w, tc.body[len(tc.body)/2:])
			}))
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			r := httptest.NewRequest("GET", "/api/v1/player/media-entries/1/stream", nil).WithContext(ctx)
			response := httptest.NewRecorder()
			h.ServeHTTP(response, r)
			counts := meter.snapshot()
			if counts.Requests != 1 || counts.HTTPStatuses[503] != 1 || counts.CancelledHTTPStatuses[503] != 1 || counts.ErrorCodes[tc.code] != 1 || counts.BodyBytes != int64(len(tc.body)) {
				t.Fatal("safe diagnostic did not match status/code/cancellation")
			}
			if response.Code != 503 || response.Body.String() != tc.body || captured.errorEnvelope != nil || captured.errorTooLarge != tc.oversized {
				t.Fatal("response altered or raw error retained")
			}
			encoded, _ := json.Marshal(counts)
			if strings.Contains(string(encoded), "secret") || strings.Contains(string(encoded), "synthetic.invalid") {
				t.Fatal("diagnostic retained an unapproved value")
			}
			counts.HTTPStatuses[503] = 999
			counts.ErrorCodes[tc.code] = 999
			if meter.snapshot().HTTPStatuses[503] != 1 || meter.snapshot().ErrorCodes[tc.code] != 1 {
				t.Fatal("snapshot mutated meter")
			}
		})
	}
	response := &quarkBridgeResponse{ResponseWriter: httptest.NewRecorder()}
	response.WriteHeader(302)
	_, _ = io.WriteString(response, "secret-redirect-body")
	if response.errorEnvelope != nil || response.safeErrorCode() != "" {
		t.Fatal("successful response body inspected")
	}
}

func TestQuarkLibmpvBridgeTimingKeepsDirectLimit(t *testing.T) {
	out := quarkLibmpvCurlOutput{Version: 1, DLLSHA256: quarkLibmpvDLLSHA, Outcome: "native_timeout", ElapsedMS: 28000}
	if quarkLibmpvOutputValid(out) || !quarkLibmpvOutputValidProfile(out, true) {
		t.Fatal("direct validation was widened or bridge timing rejected")
	}
	out.ElapsedMS = 35001
	if quarkLibmpvOutputValidProfile(out, true) {
		t.Fatal("bridge output elapsed bound exceeded")
	}
}

func TestQuarkLibmpvBridgeCancellationContract(t *testing.T) {
	t.Setenv("OMC_QUARK_LIBMPV_BRIDGE_ENABLED", "1")
	known := `{"message":"synthetic-private-message","data":{"error_code":"proxy_upstream_unavailable"}}`
	for _, tc := range []struct {
		name, cancellation, body string
		status                   int
		want                     bool
	}{
		{"only_redirects", "", "", 302, true},
		{"observed_cancelled_502", "cancel", known, 502, true},
		{"uncancelled_502", "", known, 502, false},
		{"cancelled_401", "cancel", known, 401, false},
		{"cancelled_permission_denial", "cancel", `{"data":{"error_code":"PERMISSION_DENIED"}}`, 403, false},
		{"cancelled_unknown_code", "cancel", `{"data":{"error_code":"secret-unrecognized-error"}}`, 502, false},
		{"cancelled_other_known_code", "cancel", `{"data":{"error_code":"quark_auth_expired"}}`, 502, false},
		{"cancelled_invalid_json", "cancel", "secret-response", 502, false},
		{"cancelled_oversized_error", "cancel", known + strings.Repeat(" ", 4096), 502, false},
		{"deadline_is_not_client_cancel", "deadline", known, 502, false},
		{"redirect_body", "", "redirect-body", 302, false},
		{"cancelled_redirect_body", "cancel", "redirect-body", 302, false},
		{"unexpected_success_body", "cancel", "video-like-body", 200, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, meter := quarkBridgeInstrument(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-Synthetic-First") == "1" {
					w.WriteHeader(302)
					return
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			first := httptest.NewRequest("GET", "/api/v1/player/media-entries/1/stream", nil)
			first.Header.Set("X-Synthetic-First", "1")
			h.ServeHTTP(httptest.NewRecorder(), first)
			ctx := context.Background()
			if tc.cancellation == "cancel" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			} else if tc.cancellation == "deadline" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer cancel()
			}
			request := httptest.NewRequest("GET", "/api/v1/player/media-entries/1/stream", nil).WithContext(ctx)
			response := httptest.NewRecorder()
			h.ServeHTTP(response, request)
			counts := meter.snapshot()
			if got := quarkBridgeResponseContract(counts); got != tc.want {
				t.Fatalf("response contract got=%v want=%v", got, tc.want)
			}
			if counts.Requests != 2 || counts.BodyBytes != int64(len(tc.body)) || response.Code != tc.status || response.Body.String() != tc.body {
				t.Fatal("meter changed response or discarded response bytes")
			}
			if tc.name == "observed_cancelled_502" && (counts.Other != 1 || counts.AcceptedCancelledErrors != 1 || counts.AcceptedCancelledBodyBytes != int64(len(known)) || counts.RedirectBodyBytes != 0) {
				t.Fatal("known cancellation was not separately accounted")
			}
			encoded, _ := json.Marshal(counts)
			if strings.Contains(string(encoded), "secret") || strings.Contains(string(encoded), "synthetic-private") {
				t.Fatal("cancellation diagnostics exposed raw content")
			}
		})
	}
	if quarkBridgeResponseContract(quarkBridgeCounts{Requests: 1, Other: 1, AcceptedCancelledErrors: 1}) {
		t.Fatal("only cancelled requests are not successful playback redirects")
	}
	// Status and code histograms may look similar despite belonging to different
	// requests. They must never substitute for the per-response exact tuple.
	if quarkBridgeResponseContract(quarkBridgeCounts{Requests: 3, Redirects: 1, Other: 2, UncancelledOther: 1, RejectedCancelledOther: 1,
		HTTPStatuses: map[int]int64{302: 1, 401: 1, 502: 1}, CancelledHTTPStatuses: map[int]int64{401: 1}, ErrorCodes: map[string]int64{"proxy_upstream_unavailable": 2}}) {
		t.Fatal("different request errors were combined into a valid cancellation")
	}
}
