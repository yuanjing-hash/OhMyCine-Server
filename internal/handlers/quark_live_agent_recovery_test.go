//go:build quark_live

package handlers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
)

// This diagnostic does not rotate credentials or open any acceptance batch.
// The old session and ownership ledger remain byte-for-byte unchanged.
const agentRecoveryDiagnosticLimit int64 = 64 << 10

type agentRecoveryDiagnostic struct {
	mu                          sync.Mutex
	session                     observationAgentSession
	deadline                    time.Time
	requests, httpStatus, errno int
}

type agentRecoveryDiagnosticTransport struct {
	guard *agentRecoveryDiagnostic
	base  http.RoundTripper
	media bool
}

func (g *agentRecoveryDiagnostic) wrap(base http.RoundTripper, media bool) http.RoundTripper {
	return &agentRecoveryDiagnosticTransport{guard: g, base: base, media: media}
}

func (tr *agentRecoveryDiagnosticTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	g := tr.guard
	g.mu.Lock()
	defer g.mu.Unlock()
	reject := func() (*http.Response, error) { return nil, liveTVError("agent_identity_diagnostic_scope_rejected") }
	if tr.media || tr.base == nil || r == nil || r.URL == nil || r.Context().Err() != nil || g.requests != 0 || !time.Now().Before(g.deadline) || r.Method != http.MethodGet || r.URL.Scheme != "https" || r.URL.Host != "open-api-drive.quark.cn" || r.URL.Path != "/open/v1/user/info" || r.URL.User != nil || r.URL.Fragment != "" || r.URL.RawPath != "" || r.URL.Opaque != "" || r.Host != "" && r.Host != r.URL.Host || r.Body != nil && r.Body != http.NoBody || r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "Bearer "+g.session.Access {
		return reject()
	}
	q := r.URL.Query()
	if len(q) != 4 || q.Get("access_token") != g.session.Access || q.Get("device_id") != g.session.Device || q.Get("platform") != "web" || q.Get("req_id") == "" {
		return reject()
	}
	for _, v := range q {
		if len(v) != 1 {
			return reject()
		}
	}
	g.requests++
	ctx, cancel := context.WithDeadline(r.Context(), g.deadline)
	defer cancel()
	resp, err := tr.base.RoundTrip(r.Clone(ctx))
	if err != nil {
		return nil, liveTVError("agent_identity_diagnostic_network_unavailable")
	}
	if resp == nil || resp.Body == nil {
		return reject()
	}
	defer resp.Body.Close()
	g.httpStatus = resp.StatusCode
	if resp.Uncompressed || len(resp.Header.Values("Content-Encoding")) > 1 || resp.Header.Get("Content-Encoding") != "" && resp.Header.Get("Content-Encoding") != "identity" || resp.ContentLength > agentRecoveryDiagnosticLimit {
		return reject()
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, agentRecoveryDiagnosticLimit+1))
	if err != nil || int64(len(raw)) > agentRecoveryDiagnosticLimit {
		return reject()
	}
	var safe struct {
		Errno int `json:"errno"`
	}
	_ = json.Unmarshal(raw, &safe)
	g.errno = safe.Errno
	copyResponse := new(http.Response)
	*copyResponse = *resp
	copyResponse.Body = io.NopCloser(bytes.NewReader(raw))
	copyResponse.ContentLength = int64(len(raw))
	return copyResponse, nil
}

func agentRecoveryVerify(ctx context.Context, g *agentRecoveryDiagnostic, wrap func(http.RoundTripper, bool) http.RoundTripper) error {
	c := quark.NewCredentials()
	a := g.session
	c.Agent = &quark.Authorization{AccessToken: a.Access, DeviceID: a.Device, ClientDeviceID: a.Client, AccountID: a.User, Generation: a.Generation, IdentityVerified: true}
	return quark.NewObservationValidationAuthenticator(cloud.Config{}, wrap).Verify(ctx, quark.RouteAgent, c)
}

func TestQuarkLiveAgentIdentityDiagnostic(t *testing.T) {
	if os.Getenv("OMC_QUARK_AGENT_IDENTITY_DIAGNOSTIC") != "1" {
		t.Skip("explicit read-only Agent diagnostic required")
	}
	dir := os.Getenv("OMC_QUARK_LIVE_AGENT_STATE")
	if observationCheckPrivateDirectory(dir) != nil {
		t.Fatal("agent_identity_diagnostic_private_state_required")
	}
	s, err := openObservationAgent(dir)
	if err != nil {
		t.Fatal("agent_identity_diagnostic_original_state_unavailable")
	}
	defer s.lock.Close()
	defer func() {
		if s.unchanged() != nil {
			t.Error("agent_identity_diagnostic_original_state_changed")
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	deadline, _ := ctx.Deadline()
	g := &agentRecoveryDiagnostic{session: s.session, deadline: deadline}
	err = agentRecoveryVerify(ctx, g, g.wrap)
	t.Logf("agent_identity_diagnostic requests=%d http=%d errno=%d code=%s cloud_writes=0 token_refresh=0 batch_opened=false", g.requests, g.httpStatus, g.errno, largefileRefreshCode(err))
	if err != nil {
		t.Fatal("agent_identity_diagnostic_not_passed_preserve_original_state")
	}
}
