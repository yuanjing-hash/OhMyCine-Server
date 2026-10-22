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

type agentRecoveryGuard struct {
	mu                                       sync.Mutex
	store                                    *agentRecoveryStore
	deadline                                 time.Time
	identities, rotations, httpStatus, errno int
}
type agentRecoveryTransport struct {
	guard *agentRecoveryGuard
	base  http.RoundTripper
	media bool
}

func (g *agentRecoveryGuard) wrap(base http.RoundTripper, media bool) http.RoundTripper {
	return &agentRecoveryTransport{g, base, media}
}

func (tr *agentRecoveryTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	g := tr.guard
	g.mu.Lock()
	defer g.mu.Unlock()
	reject := func() (*http.Response, error) { return nil, liveTVError("agent_refresh_scope_rejected") }
	if tr.media || tr.base == nil || r == nil || r.URL == nil || r.Context().Err() != nil || !time.Now().Before(g.deadline) || r.URL.Scheme != "https" || r.URL.Host != "open-api-drive.quark.cn" || r.URL.User != nil || r.URL.Fragment != "" || r.URL.RawPath != "" || r.URL.Opaque != "" || r.Host != "" && r.Host != r.URL.Host || r.Header.Get("Cookie") != "" || g.store.validate() != nil || g.store.original.unchanged() != nil {
		return reject()
	}
	a := g.store.state.Current.Agent
	q := r.URL.Query()
	for _, values := range q {
		if len(values) != 1 {
			return reject()
		}
	}
	if q.Get("platform") != "web" || q.Get("req_id") == "" {
		return reject()
	}
	if r.Method == http.MethodGet && r.URL.Path == "/open/v1/user/info" {
		if g.identities >= 3 || r.Body != nil && r.Body != http.NoBody || len(q) != 4 || q.Get("access_token") != a.AccessToken || q.Get("device_id") != a.DeviceID || r.Header.Get("Authorization") != "Bearer "+a.AccessToken {
			return reject()
		}
		g.identities++
	} else if r.Method == http.MethodPost && r.URL.Path == "/agent/v1/oauth/access_token/rotate" {
		if g.rotations != 0 || len(q) != 2 || r.Body == nil || r.Header.Get("Authorization") != "" || g.store.state.AttemptRevision != g.store.state.Current.Revision || !a.IdentityVerified || g.store.state.AttemptDenied || g.store.state.AttemptPairSHA != agentRecoveryPairSHA(g.store.state.Current) {
			return reject()
		}
		raw, err := io.ReadAll(io.LimitReader(r.Body, agentRecoveryDiagnosticLimit+1))
		_ = r.Body.Close()
		var body map[string]string
		if err != nil || int64(len(raw)) > agentRecoveryDiagnosticLimit || json.Unmarshal(raw, &body) != nil || len(body) != 2 || body["device_id"] != a.DeviceID || body["refresh_token"] != a.RefreshToken {
			return reject()
		}
		r = r.Clone(r.Context())
		r.Body = io.NopCloser(bytes.NewReader(raw))
		r.GetBody = nil
		g.rotations++
	} else {
		return reject()
	}
	ctx, cancel := context.WithDeadline(r.Context(), g.deadline)
	defer cancel()
	resp, err := tr.base.RoundTrip(r.Clone(ctx))
	if err != nil {
		return nil, liveTVError("agent_refresh_network_unavailable")
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

func TestQuarkLiveAgentRecover(t *testing.T) {
	if os.Getenv("OMC_QUARK_AGENT_RECOVER") != "1" {
		t.Skip("explicit first-party Agent recovery required")
	}
	dir := os.Getenv("OMC_QUARK_LIVE_AGENT_STATE")
	if observationCheckPrivateDirectory(dir) != nil {
		t.Fatal("agent_refresh_private_state_required")
	}
	original, err := openObservationAgent(dir)
	if err != nil {
		t.Fatal("agent_refresh_original_evidence_unavailable")
	}
	defer original.lock.Close()
	defer func() {
		if original.unchanged() != nil {
			t.Error("agent_refresh_original_evidence_changed")
		}
	}()
	s, err := openAgentRecoveryStore(dir, original)
	if err != nil {
		t.Fatal("agent_refresh_continuation_unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	deadline, _ := ctx.Deadline()
	g := &agentRecoveryGuard{store: s, deadline: deadline}
	a := quark.NewObservationValidationAuthenticator(cloud.Config{}, g.wrap)
	refreshed, err := s.ready(ctx, a)
	t.Logf("agent_recovery identities=%d rotations=%d last_http=%d last_errno=%d code=%s refreshed=%t old_evidence_unchanged=%t batch_opened=false", g.identities, g.rotations, g.httpStatus, g.errno, largefileRefreshCode(err), refreshed, original.unchanged() == nil)
	if err != nil {
		t.Fatal("agent_refresh_not_completed_preserve_continuation")
	}
	if _, err = agentRecoveryAccessSnapshot(dir, original); err != nil {
		t.Fatal("agent_refresh_access_snapshot_unavailable")
	}
	t.Log("agent_recovery_same_identity=true original_session_ledger_donor_batch_unchanged=true")
}
