//go:build quark_live

package handlers_test

// Diagnostic observation only: no extra provider requests, cache changes, raw
// headers, URLs, expiry values, query values/names, identities or hashes escape.
// Native methods are promoted from *quark.Client, preserving optional driver
// interfaces. Only the two existing playback calls are counted.

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
)

type quarkPlaybackDiagnosticPhase uint8

const (
	quarkPlaybackDiagnosticCheck quarkPlaybackDiagnosticPhase = iota + 1
	quarkPlaybackDiagnosticStream
)

type quarkPlaybackDiagnosticContextKey struct{}
type quarkPlaybackDiagnosticContext struct {
	owner *quarkPlaybackDiagnostic
	phase quarkPlaybackDiagnosticPhase
}

type quarkPlaybackDiagnosticCalls struct {
	DirectURLCalls         int  `json:"direct_url_calls"`
	DirectURLSuccesses     int  `json:"direct_url_successes"`
	KnownExpirySuccesses   int  `json:"known_expiry_successes"`
	CacheEligibleSuccesses int  `json:"cache_eligible_successes"`
	ScopedCalls            int  `json:"scoped_calls"`
	ScopedSuccesses        int  `json:"scoped_successes"`
	ExpiryKnown            bool `json:"expiry_known"`
	RemainingEligible      bool `json:"remaining_eligible"`
	QueryExpiresUpper      bool `json:"query_Expires_present"`
	QueryExpiresLower      bool `json:"query_expires_present"`
	QueryOSSExpires        bool `json:"query_x_oss_expires_present"`
	QueryE                 bool `json:"query_e_present"`
}

type quarkPlaybackDiagnosticReport struct {
	Check              quarkPlaybackDiagnosticCalls `json:"check"`
	Stream             quarkPlaybackDiagnosticCalls `json:"stream"`
	StreamRequests     int                          `json:"stream_requests"`
	StreamUAStable     bool                         `json:"stream_ua_stable"`
	StreamClientStable bool                         `json:"stream_client_stable"`
}

type quarkPlaybackDiagnostic struct {
	mu          sync.Mutex
	active      bool
	report      quarkPlaybackDiagnosticReport
	firstUA     string
	firstClient string
}

func newQuarkPlaybackDiagnostic() *quarkPlaybackDiagnostic {
	if !quarkBridgeEnabled() {
		return nil
	}
	return &quarkPlaybackDiagnostic{}
}

// begin is called immediately around the native bridge hook. Its earlier
// automated head/tail requests and later permission tests do not enter these
// counters. The hook's check=1 and its raw stream requests stay separate.
func (d *quarkPlaybackDiagnostic) begin(t *testing.T) func() {
	if d == nil {
		return func() {}
	}
	d.mu.Lock()
	d.active = true
	d.report = quarkPlaybackDiagnosticReport{}
	d.firstUA, d.firstClient = "", ""
	d.mu.Unlock()
	return func() {
		d.mu.Lock()
		d.active = false
		report := d.report
		d.firstUA, d.firstClient = "", ""
		d.mu.Unlock()
		encoded, _ := json.Marshal(report)
		t.Logf("libmpv_bridge_cache_diagnostics %s", encoded)
	}
}

func (d *quarkPlaybackDiagnostic) instrument(next http.Handler) http.Handler {
	if d == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		phase := quarkPlaybackDiagnosticPhase(0)
		if strings.HasPrefix(r.URL.Path, "/api/v1/player/media-entries/") && strings.HasSuffix(r.URL.Path, "/stream") && r.Method == http.MethodGet {
			switch r.URL.RawQuery {
			case "":
				phase = quarkPlaybackDiagnosticStream
			case "check=1":
				phase = quarkPlaybackDiagnosticCheck
			}
		}
		d.mu.Lock()
		observed := d.active && phase != 0
		if observed && phase == quarkPlaybackDiagnosticStream {
			// Match the product's client normalization, not the changing TCP port.
			client := strings.TrimSpace(r.RemoteAddr)
			if host, _, err := net.SplitHostPort(client); err == nil {
				client = host
			}
			client = strings.ToLower(client)
			if client == "" {
				client = "direct"
			}
			ua := r.UserAgent() // raw UA is also an input to the product fence.
			d.report.StreamRequests++
			if d.report.StreamRequests == 1 {
				d.firstUA, d.firstClient = ua, client
				d.report.StreamUAStable, d.report.StreamClientStable = true, true
			} else {
				d.report.StreamUAStable = d.report.StreamUAStable && d.firstUA == ua
				d.report.StreamClientStable = d.report.StreamClientStable && d.firstClient == client
			}
		}
		d.mu.Unlock()
		if observed {
			r = r.WithContext(context.WithValue(r.Context(), quarkPlaybackDiagnosticContextKey{}, quarkPlaybackDiagnosticContext{d, phase}))
		}
		next.ServeHTTP(w, r)
	})
}

// Called under mu. A context from setup, a different recorder, or a completed
// observation cannot add facts to this run.
func (d *quarkPlaybackDiagnostic) calls(ctx context.Context) *quarkPlaybackDiagnosticCalls {
	marker, ok := ctx.Value(quarkPlaybackDiagnosticContextKey{}).(quarkPlaybackDiagnosticContext)
	if !d.active || !ok || marker.owner != d {
		return nil
	}
	switch marker.phase {
	case quarkPlaybackDiagnosticCheck:
		return &d.report.Check
	case quarkPlaybackDiagnosticStream:
		return &d.report.Stream
	default:
		return nil
	}
}

func (d *quarkPlaybackDiagnostic) direct(ctx context.Context, result cloud.TemporaryURL, err error, now time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	c := d.calls(ctx)
	if c == nil {
		return
	}
	c.DirectURLCalls++
	if err != nil {
		return // Even a populated error result contributes no URL/expiry evidence.
	}
	c.DirectURLSuccesses++
	known := !result.ExpiresAt.IsZero()
	eligible := known && result.ExpiresAt.After(now.Add(30*time.Second))
	if known {
		c.KnownExpirySuccesses++
	}
	if eligible {
		c.CacheEligibleSuccesses++
	}
	if c.DirectURLSuccesses == 1 {
		c.ExpiryKnown, c.RemainingEligible = known, eligible
	} else {
		c.ExpiryKnown = c.ExpiryKnown && known
		c.RemainingEligible = c.RemainingEligible && eligible
	}
	// Presence means any successful result contained this literal known key.
	// Do not report arbitrary query names, values, dates, hashes or URL strings.
	if len(result.URL) > 32768 {
		return
	}
	u, parseErr := url.Parse(result.URL)
	if parseErr != nil {
		return
	}
	q := u.Query()
	c.QueryExpiresUpper = c.QueryExpiresUpper || q.Has("Expires")
	c.QueryExpiresLower = c.QueryExpiresLower || q.Has("expires")
	c.QueryOSSExpires = c.QueryOSSExpires || q.Has("x-oss-expires")
	c.QueryE = c.QueryE || q.Has("e")
}

func (d *quarkPlaybackDiagnostic) scoped(ctx context.Context, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if c := d.calls(ctx); c != nil {
		c.ScopedCalls++
		if err == nil {
			c.ScopedSuccesses++
		}
	}
}

type quarkPlaybackDiagnosticDriver struct {
	*quark.Client
	diagnostic *quarkPlaybackDiagnostic
}

var _ cloud.Driver = (*quarkPlaybackDiagnosticDriver)(nil)

func (d *quarkPlaybackDiagnosticDriver) DirectURL(ctx context.Context, request cloud.DirectURLRequest) (cloud.TemporaryURL, error) {
	result, err := d.Client.DirectURL(ctx, request)
	d.diagnostic.direct(ctx, result, err, time.Now())
	return result, err
}

func (d *quarkPlaybackDiagnosticDriver) ResolveScopedItem(ctx context.Context, root, path, expectedID string) (cloud.Item, error) {
	result, err := d.Client.ResolveScopedItem(ctx, root, path, expectedID)
	d.diagnostic.scoped(ctx, err)
	return result, err
}

func TestQuarkPlaybackDiagnosticSafeResults(t *testing.T) {
	d := &quarkPlaybackDiagnostic{active: true}
	ctx := context.WithValue(context.Background(), quarkPlaybackDiagnosticContextKey{}, quarkPlaybackDiagnosticContext{d, quarkPlaybackDiagnosticStream})
	now := time.Unix(1800000000, 0)
	secret := "DO_NOT_PRINT_URL_TOKEN_OR_ERROR"
	link := cloud.TemporaryURL{URL: "https://secret.example/" + secret + "?personal_secret=" + secret + "&Expires=secret&e=&expires=secret&x-oss-expires=secret", ExpiresAt: now.Add(time.Hour)}
	d.direct(ctx, link, nil, now)
	d.scoped(ctx, nil)
	d.direct(ctx, link, errors.New(secret), now)
	d.scoped(ctx, errors.New(secret))
	c := d.report.Stream
	if c.DirectURLCalls != 2 || c.DirectURLSuccesses != 1 || c.KnownExpirySuccesses != 1 || c.CacheEligibleSuccesses != 1 || c.ScopedCalls != 2 || c.ScopedSuccesses != 1 || !c.ExpiryKnown || !c.RemainingEligible || !c.QueryExpiresUpper || !c.QueryExpiresLower || !c.QueryOSSExpires || !c.QueryE {
		t.Fatal("fixed diagnostic counters differ")
	}
	encoded, err := json.Marshal(d.report)
	if err != nil || strings.Contains(string(encoded), secret) || strings.Contains(string(encoded), "secret.example") || strings.Contains(string(encoded), "personal_secret") || strings.Contains(string(encoded), "180000") {
		t.Fatal("private diagnostic data escaped")
	}
	for _, tc := range []struct {
		name            string
		exp             time.Time
		known, eligible bool
	}{
		{"unknown", time.Time{}, false, false},
		{"expired", now.Add(-time.Second), true, false},
		{"margin", now.Add(30 * time.Second), true, false},
		{"eligible", now.Add(31 * time.Second), true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d.report.Stream = quarkPlaybackDiagnosticCalls{}
			d.direct(ctx, cloud.TemporaryURL{ExpiresAt: tc.exp}, nil, now)
			if got := d.report.Stream; got.ExpiryKnown != tc.known || got.RemainingEligible != tc.eligible {
				t.Fatal("expiry boolean differs")
			}
		})
	}
	t.Run("mixed_known_unknown", func(t *testing.T) {
		d.report.Stream = quarkPlaybackDiagnosticCalls{}
		d.direct(ctx, cloud.TemporaryURL{ExpiresAt: now.Add(time.Hour)}, nil, now)
		d.direct(ctx, cloud.TemporaryURL{ExpiresAt: now.Add(time.Second)}, nil, now)
		d.direct(ctx, cloud.TemporaryURL{}, nil, now)
		got := d.report.Stream
		if got.DirectURLSuccesses != 3 || got.KnownExpirySuccesses != 2 || got.CacheEligibleSuccesses != 1 || got.ExpiryKnown || got.RemainingEligible {
			t.Fatal("mixed expiry evidence was lost")
		}
	})
}

func TestQuarkPlaybackDiagnosticRequestIsolation(t *testing.T) {
	d := &quarkPlaybackDiagnostic{}
	handler := d.instrument(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.direct(r.Context(), cloud.TemporaryURL{}, nil, time.Now())
		d.scoped(r.Context(), nil)
		w.WriteHeader(http.StatusNoContent)
	}))
	send := func(query, ua, client string) {
		r := httptest.NewRequest(http.MethodGet, "http://localhost/api/v1/player/media-entries/1/stream"+query, nil)
		r.Header.Set("User-Agent", ua)
		r.RemoteAddr = client
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusNoContent {
			t.Fatal("diagnostic changed response")
		}
	}
	send("", "earlier-head-tail-client", "127.0.0.2:12")
	stop := d.begin(t)
	send("?check=1", "preflight-client", "127.0.0.2:13")
	send("?unrelated=1", "unrelated", "127.0.0.3:14")
	send("", "native-client", "127.0.0.1:15")
	send("", "native-client", "127.0.0.1:16")
	if d.report.StreamRequests != 2 || !d.report.StreamUAStable || !d.report.StreamClientStable || d.report.Check.DirectURLCalls != 1 || d.report.Stream.DirectURLCalls != 2 {
		t.Fatal("setup/check/port affected stream baseline")
	}
	send("", "changed-native-client", "127.0.0.4:16")
	if d.report.StreamUAStable || d.report.StreamClientStable {
		t.Fatal("client change was hidden")
	}
	stop()
	before := d.report
	send("", "later-permission-test", "127.0.0.1:17")
	if d.report != before {
		t.Fatal("closed diagnostic still changed")
	}
}

func TestQuarkPlaybackDiagnosticPreservesNativeInterfaces(t *testing.T) {
	native, wrapped := reflect.TypeOf((*quark.Client)(nil)), reflect.TypeOf((*quarkPlaybackDiagnosticDriver)(nil))
	for i := 0; i < native.NumMethod(); i++ {
		method := native.Method(i)
		promoted, ok := wrapped.MethodByName(method.Name)
		if !ok || method.Type.NumIn() != promoted.Type.NumIn() || method.Type.NumOut() != promoted.Type.NumOut() {
			t.Fatal("native method lost")
		}
		for arg := 1; arg < method.Type.NumIn(); arg++ {
			if method.Type.In(arg) != promoted.Type.In(arg) {
				t.Fatal("native input changed")
			}
		}
		for ret := 0; ret < method.Type.NumOut(); ret++ {
			if method.Type.Out(ret) != promoted.Type.Out(ret) {
				t.Fatal("native output changed")
			}
		}
	}
	raw, _ := json.Marshal(quark.NewCredentials())
	client, err := quark.NewClient(cloud.Config{Credentials: raw, BeforeCall: func(context.Context) error { t.Fatal("offline diagnostic reached network"); return nil }})
	if err != nil {
		t.Fatal("synthetic client setup failed")
	}
	d := &quarkPlaybackDiagnostic{active: true}
	driver := &quarkPlaybackDiagnosticDriver{client, d}
	ctx := context.WithValue(context.Background(), quarkPlaybackDiagnosticContextKey{}, quarkPlaybackDiagnosticContext{d, quarkPlaybackDiagnosticStream})
	_, directErr := driver.DirectURL(ctx, cloud.DirectURLRequest{})
	_, scopedErr := driver.ResolveScopedItem(ctx, "", "", "")
	directCode, _ := cloud.ErrorInfo(directErr)
	scopedCode, _ := cloud.ErrorInfo(scopedErr)
	if directCode != quark.CodeAuth || scopedCode != quark.CodeAuth || d.report.Stream.DirectURLCalls != 1 || d.report.Stream.ScopedCalls != 1 || d.report.Stream.DirectURLSuccesses != 0 || d.report.Stream.ScopedSuccesses != 0 {
		t.Fatal("native error semantics changed")
	}
}
