//go:build quark_live

package handlers_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

func TestQuarkPlaybackDiagnosticIndependentContextBoundaries(t *testing.T) {
	d := &quarkPlaybackDiagnostic{active: true}
	foreign := &quarkPlaybackDiagnostic{active: true}
	for _, ctx := range []context.Context{
		context.Background(),
		context.WithValue(context.Background(), quarkPlaybackDiagnosticContextKey{}, "private-wrong-type"),
		context.WithValue(context.Background(), quarkPlaybackDiagnosticContextKey{}, quarkPlaybackDiagnosticContext{foreign, quarkPlaybackDiagnosticStream}),
		context.WithValue(context.Background(), quarkPlaybackDiagnosticContextKey{}, quarkPlaybackDiagnosticContext{d, 0}),
	} {
		d.direct(ctx, cloud.TemporaryURL{URL: "https://private.invalid/?secret=private", ExpiresAt: time.Now().Add(time.Hour)}, nil, time.Now())
		d.scoped(ctx, nil)
	}
	if d.report != (quarkPlaybackDiagnosticReport{}) {
		t.Fatal("unmarked, foreign or malformed context entered this observation")
	}
	d.active = false
	marked := context.WithValue(context.Background(), quarkPlaybackDiagnosticContextKey{}, quarkPlaybackDiagnosticContext{d, quarkPlaybackDiagnosticStream})
	d.direct(marked, cloud.TemporaryURL{}, errors.New("private-error"), time.Now())
	d.scoped(marked, nil)
	if d.report != (quarkPlaybackDiagnosticReport{}) {
		t.Fatal("completed observation accepted a late result")
	}
	t.Setenv("OMC_QUARK_LIBMPV_BRIDGE_ENABLED", "")
	if newQuarkPlaybackDiagnostic() != nil {
		t.Fatal("diagnostic enabled outside the explicit bridge mode")
	}
	var disabled *quarkPlaybackDiagnostic
	original := http.NewServeMux()
	if disabled.instrument(original) != original {
		t.Fatal("disabled diagnostic changed handler identity")
	}
	disabled.begin(t)()
}

func TestQuarkPlaybackDiagnosticIndependentRouteAndClientIsolation(t *testing.T) {
	d := &quarkPlaybackDiagnostic{}
	nextCalls := 0
	h := d.instrument(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalls++
		d.scoped(r.Context(), nil)
		w.Header().Set("X-Synthetic", "unchanged")
		w.WriteHeader(http.StatusAccepted)
	}))
	stop := d.begin(t)
	defer stop()
	for _, tc := range []struct{ method, endpoint string }{
		{"POST", "/api/v1/player/media-entries/1/stream"},
		{"GET", "/api/v1/player/media-entries/1/stream?check=1&private=1"},
		{"GET", "/api/v1/player/media-entries/1/stream/"},
		{"GET", "/api/v1/other/stream"},
	} {
		request := httptest.NewRequest(tc.method, tc.endpoint, nil)
		response := httptest.NewRecorder()
		h.ServeHTTP(response, request)
		if response.Code != http.StatusAccepted || response.Header().Get("X-Synthetic") != "unchanged" {
			t.Fatal("diagnostic changed the original response")
		}
	}
	if nextCalls != 4 || d.report != (quarkPlaybackDiagnosticReport{}) {
		t.Fatal("non-target route was counted or was not forwarded")
	}
	for _, port := range []string{"10001", "10002"} {
		request := httptest.NewRequest("GET", "/api/v1/player/media-entries/1/stream", nil)
		request.RemoteAddr = "[::1]:" + port
		request.Header.Set("User-Agent", "private-UA-synthetic")
		h.ServeHTTP(httptest.NewRecorder(), request)
	}
	if d.report.StreamRequests != 2 || !d.report.StreamUAStable || !d.report.StreamClientStable || d.report.Stream.ScopedCalls != 2 || d.report.Check.ScopedCalls != 0 {
		t.Fatal("IPv6 port churn changed stream identity or check counters")
	}
	encoded, _ := json.Marshal(d.report)
	if strings.Contains(string(encoded), "private-UA") || strings.Contains(string(encoded), "::1") {
		t.Fatal("client values escaped diagnostic")
	}
}

func TestQuarkPlaybackDiagnosticIndependentExpiryAggregationAndShape(t *testing.T) {
	d := &quarkPlaybackDiagnostic{active: true}
	ctx := context.WithValue(context.Background(), quarkPlaybackDiagnosticContextKey{}, quarkPlaybackDiagnosticContext{d, quarkPlaybackDiagnosticStream})
	now := time.Unix(1800000000, 0)
	secret := "PRIVATE_URL_TOKEN_QUERY"
	known := cloud.TemporaryURL{URL: "https://private.invalid/" + secret + "?e=&secret_unknown=" + secret, ExpiresAt: now.Add(time.Hour)}
	d.direct(ctx, known, nil, now)
	d.direct(ctx, cloud.TemporaryURL{URL: "https://private.invalid/?Expires=" + secret}, nil, now)
	d.direct(ctx, known, errors.New(secret), now)
	if got := d.report.Stream; got.DirectURLCalls != 3 || got.DirectURLSuccesses != 2 || got.KnownExpirySuccesses != 1 || got.CacheEligibleSuccesses != 1 || got.ExpiryKnown || got.RemainingEligible || !got.QueryE || !got.QueryExpiresUpper || got.QueryExpiresLower || got.QueryOSSExpires {
		t.Fatal("mixed expiry results were collapsed into a misleading positive")
	}
	encoded, err := json.Marshal(d.report)
	if err != nil || strings.Contains(string(encoded), secret) || strings.Contains(string(encoded), "private.invalid") || strings.Contains(string(encoded), "secret_unknown") {
		t.Fatal("arbitrary provider data escaped")
	}
	var decoded any
	if json.Unmarshal(encoded, &decoded) != nil {
		t.Fatal("diagnostic output is not JSON")
	}
	var allowed func(any) bool
	allowed = func(value any) bool {
		switch v := value.(type) {
		case bool, float64:
			return true
		case map[string]any:
			for _, child := range v {
				if !allowed(child) {
					return false
				}
			}
			return true
		default:
			return false
		}
	}
	if !allowed(decoded) {
		t.Fatal("diagnostic contains nonnumeric/nonboolean values")
	}
}
