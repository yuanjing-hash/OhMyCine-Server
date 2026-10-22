//go:build quark_live

package handlers_test

// Explicit TV-only recovery is separate from the immutable large-file run.
// It never opens Agent/Web/donor/batch state. The normal TV store owns the
// original runtime lock and persists no-replay attempts/checkpoints before
// native token rotation; this wrapper only narrows its network budget.
import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
)

const largefileRefreshLimit int64 = 64 << 10

type largefileRefreshGuard struct {
	mu                                       sync.Mutex
	store                                    *liveTVStore
	deadline                                 time.Time
	identities, brokers, lastHTTP, lastErrno int
	initialHTTP, initialErrno                int
	initialCode                              string
}

type largefileRefreshTransport struct {
	guard *largefileRefreshGuard
	base  http.RoundTripper
	media bool
}

func (g *largefileRefreshGuard) wrap(base http.RoundTripper, media bool) http.RoundTripper {
	return &largefileRefreshTransport{guard: g, base: base, media: media}
}

func (tr *largefileRefreshTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	g := tr.guard
	g.mu.Lock()
	defer g.mu.Unlock()
	reject := func() (*http.Response, error) { return nil, liveTVError("tv_refresh_scope_rejected") }
	if tr.media || tr.base == nil || r == nil || r.URL == nil || !time.Now().Before(g.deadline) || r.URL.Scheme != "https" || r.URL.User != nil || r.URL.Fragment != "" || r.URL.Opaque != "" || r.URL.RawPath != "" || r.Host != "" && r.Host != r.URL.Host || r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
		return reject()
	}
	s := g.store
	if r.URL.Host == "open-api-drive.quark.cn" && r.URL.Path == "/user" && r.Method == http.MethodGet {
		q := r.URL.Query()
		if g.identities >= 3 || r.Body != nil || q.Get("method") != "user_info" || q.Get("device_id") != s.session.DeviceID || q.Get("platform") != "tv" || q.Get("access_token") == "" {
			return reject()
		}
		access := q.Get("access_token")
		if access != s.session.AccessToken && (s.checkpoint == nil || access != s.checkpoint.AccessToken) {
			return reject()
		}
		allowed := map[string]bool{"method": true, "access_token": true, "req_id": true, "app_ver": true, "device_id": true, "device_brand": true, "platform": true, "device_name": true, "device_model": true, "build_device": true, "build_product": true, "device_gpu": true, "activity_rect": true, "channel": true}
		for k, v := range q {
			if !allowed[k] || len(v) != 1 {
				return reject()
			}
		}
		g.identities++
	} else if r.URL.Host == "api.extscreen.com" && r.URL.Path == "/quarkdrive/token" && r.Method == http.MethodPost {
		if g.brokers >= 1 || r.URL.RawQuery != "" || r.Body == nil || s.attempt == nil || s.attempt.Generation != s.session.Generation || s.checkpoint != nil {
			return reject()
		}
		raw, e := io.ReadAll(io.LimitReader(r.Body, largefileRefreshLimit+1))
		_ = r.Body.Close()
		if e != nil || int64(len(raw)) > largefileRefreshLimit {
			return reject()
		}
		var p map[string]string
		if json.Unmarshal(raw, &p) != nil || p["refresh_token"] == "" || p["refresh_token"] != s.session.RefreshToken || p["device_id"] != s.session.DeviceID || p["platform"] != "tv" {
			return reject()
		}
		allowed := map[string]bool{"refresh_token": true, "req_id": true, "app_ver": true, "device_id": true, "device_brand": true, "platform": true, "device_name": true, "device_model": true, "build_device": true, "build_product": true, "device_gpu": true, "activity_rect": true, "channel": true}
		for k := range p {
			if !allowed[k] {
				return reject()
			}
		}
		r = r.Clone(r.Context())
		r.Body = io.NopCloser(bytes.NewReader(raw))
		r.GetBody = nil
		g.brokers++
	} else {
		return reject()
	}
	ctx, cancel := context.WithDeadline(r.Context(), g.deadline)
	defer cancel()
	resp, e := tr.base.RoundTrip(r.Clone(ctx))
	if e != nil {
		return nil, liveTVError("tv_refresh_network_unavailable")
	}
	if resp == nil || resp.Body == nil {
		return reject()
	}
	defer resp.Body.Close()
	g.lastHTTP = resp.StatusCode
	if enc := resp.Header.Get("Content-Encoding"); enc != "" && enc != "identity" {
		return reject()
	}
	if resp.ContentLength > largefileRefreshLimit {
		return reject()
	}
	raw, e := io.ReadAll(io.LimitReader(resp.Body, largefileRefreshLimit+1))
	if e != nil || int64(len(raw)) > largefileRefreshLimit {
		return reject()
	}
	var safe struct {
		Errno int `json:"errno"`
	}
	_ = json.Unmarshal(raw, &safe)
	g.lastErrno = safe.Errno
	copyResponse := new(http.Response)
	*copyResponse = *resp
	copyResponse.Body = io.NopCloser(bytes.NewReader(raw))
	copyResponse.ContentLength = int64(len(raw))
	return copyResponse, nil
}

type largefileRefreshAuthenticator struct {
	native *quark.Authenticator
	guard  *largefileRefreshGuard
}

func (a *largefileRefreshAuthenticator) Verify(ctx context.Context, r quark.Route, c quark.Credentials) error {
	e := a.native.Verify(ctx, r, c)
	a.guard.initialCode = largefileRefreshCode(e)
	a.guard.initialHTTP, a.guard.initialErrno = a.guard.lastHTTP, a.guard.lastErrno
	return e
}
func (a *largefileRefreshAuthenticator) Refresh(ctx context.Context, r quark.Route, c quark.Credentials, p quark.PersistFunc) (quark.Credentials, error) {
	return a.native.Refresh(ctx, r, c, p)
}
func (a *largefileRefreshAuthenticator) Confirm(ctx context.Context, r quark.Route, c quark.Credentials, p quark.PersistFunc) (quark.Credentials, error) {
	return a.native.Confirm(ctx, r, c, p)
}
func largefileRefreshCode(e error) string {
	if e == nil {
		return "ok"
	}
	code, _ := cloud.ErrorInfo(e)
	var f *quark.Failure
	if errors.As(e, &f) {
		code = f.Code
	}
	switch code {
	case quark.CodeAuth, quark.CodeIdentity, quark.CodeInvalid, quark.CodeUnavailable, quark.CodeLimited:
		return code
	}
	return "local_or_unclassified"
}
func largefileRefreshReady(ctx context.Context, s *liveTVStore, g *largefileRefreshGuard) (bool, error) {
	initial := s.session
	a := &largefileRefreshAuthenticator{native: quark.NewObservationValidationAuthenticator(cloud.Config{}, g.wrap), guard: g}
	_, refreshed, e := s.ready(ctx, a)
	if !sameLiveTVIdentity(initial, s.session) {
		return false, liveTVError("tv_identity_mismatch")
	}
	return refreshed, e
}

func TestQuarkLiveLargeFileRefreshTV(t *testing.T) {
	if os.Getenv("OMC_QUARK_LARGEFILE_REFRESH_TV") != "1" {
		t.Skip("explicit TV-only recovery required")
	}
	dir := os.Getenv("OMC_QUARK_LIVE_TV_STATE")
	if dir == "" || observationCheckPrivateDirectory(dir) != nil {
		t.Fatal("tv_recovery_private_state_required")
	}
	s, e := openLiveTVStore(dir)
	if e != nil {
		t.Fatal("tv_recovery_original_state_unavailable")
	}
	defer s.lock.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	deadline, _ := ctx.Deadline()
	g := &largefileRefreshGuard{store: s, deadline: deadline, initialCode: "not_checked"}
	renewed, e := largefileRefreshReady(ctx, s, g)
	t.Logf("tv_recovery identities=%d broker_requests=%d initial_identity_code=%s initial_http=%d initial_errno=%d last_http=%d last_errno=%d renewed=%t largefile_batch_opened=false", g.identities, g.brokers, g.initialCode, g.initialHTTP, g.initialErrno, g.lastHTTP, g.lastErrno, renewed)
	if e != nil {
		t.Fatal(liveTVMessage(e))
	}
	t.Log("tv_recovery_same_identity=true original_batch_deadline_and_budgets_unchanged=true")
}
