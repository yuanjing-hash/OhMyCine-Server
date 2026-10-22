//go:build quark_live

package handlers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/credential"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
)

type agentRecoveryFixture struct {
	dir                                       string
	original                                  *observationAgentState
	store                                     *agentRecoveryStore
	valid, unknown, denied, pending, mismatch bool
	rotations, identities                     int
	cancelAfterRotate                         bool
	cancel                                    context.CancelFunc
	blockCheckpoint                           bool
}

func newAgentRecoveryFixture(t *testing.T) *agentRecoveryFixture {
	t.Helper()
	dir, original := observationOfflineAgent(t)
	cipher, err := credential.Open("", string(original.files[filepath.Join(dir, "credentials.key")]))
	if err != nil {
		t.Fatal(err)
	}
	full := struct {
		observationAgentSession
		Refresh string `json:"refresh_token"`
	}{original.session, "fixture-old-refresh"}
	plain, _ := json.Marshal(full)
	sealed, _ := cipher.Encrypt("quark-original-302-probe:state:v1", string(plain))
	raw, _ := json.Marshal(struct {
		Version    int
		Ciphertext string
	}{1, sealed})
	path := filepath.Join(dir, "session.enc.json")
	if os.WriteFile(path, raw, 0600) != nil {
		t.Fatal("fixture write")
	}
	original.files[path] = raw
	s, err := openAgentRecoveryStore(dir, original)
	if err != nil {
		t.Fatal("fixture open", err)
	}
	return &agentRecoveryFixture{dir: dir, original: original, store: s}
}

func (f *agentRecoveryFixture) response(status int, raw string) (*http.Response, error) {
	return &http.Response{StatusCode: status, Header: make(http.Header), ContentLength: int64(len(raw)), Body: io.NopCloser(strings.NewReader(raw))}, nil
}
func (f *agentRecoveryFixture) transport(r *http.Request) (*http.Response, error) {
	switch r.URL.Path {
	case "/agent/v1/oauth/access_token/rotate":
		f.rotations++
		// Durable original-pair marker must exist before the first token call.
		s, err := openAgentRecoveryStore(f.dir, f.original)
		if err != nil || s.state.AttemptRevision == 0 {
			return nil, errors.New("fixture_rotation_without_durable_attempt")
		}
		if f.unknown {
			return nil, errors.New("fixture_private_transport_failure")
		}
		if f.denied {
			return f.response(400, `{"errno":11001,"message":"never-print"}`)
		}
		if f.blockCheckpoint {
			path := filepath.Join(f.dir, agentRecoveryName)
			if os.Rename(path, path+".fixture-attempt") != nil || os.Mkdir(path, 0700) != nil {
				return nil, errors.New("fixture_checkpoint_block_failed")
			}
		}
		if f.cancelAfterRotate {
			f.cancel()
		}
		return f.response(200, `{"status":0,"data":{"access_token":"fixture-new-access","refresh_token":"fixture-new-refresh"}}`)
	case "/open/v1/user/info":
		f.identities++
		if r.URL.Query().Get("access_token") == f.original.session.Access && !f.valid {
			return f.response(400, `{"errno":11001,"message":"never-print"}`)
		}
		if f.pending {
			return f.response(500, `{"errno":777,"message":"never-print"}`)
		}
		user := f.original.session.User
		if f.mismatch {
			user = "another-account"
		}
		raw, _ := json.Marshal(map[string]any{"status": 0, "data": map[string]string{"user_id": user}})
		return f.response(200, string(raw))
	}
	return nil, errors.New("fixture_unexpected_endpoint")
}

func (f *agentRecoveryFixture) ready(t *testing.T) (bool, *agentRecoveryGuard, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	f.cancel = cancel
	deadline, _ := ctx.Deadline()
	g := &agentRecoveryGuard{store: f.store, deadline: deadline}
	wrap := func(_ http.RoundTripper, media bool) http.RoundTripper {
		return g.wrap(observationOfflineTransport(f.transport), media)
	}
	a := quark.NewObservationValidationAuthenticator(cloud.Config{}, wrap)
	r, err := f.store.ready(ctx, a)
	return r, g, err
}

func TestQuarkAgentRecoverySidecarNativeOffline(t *testing.T) {
	for _, mode := range []string{"valid", "expired", "unknown", "denied", "pending", "mismatch", "cancel_after_rotate", "checkpoint_save_failure"} {
		t.Run(mode, func(t *testing.T) {
			f := newAgentRecoveryFixture(t)
			before := map[string][]byte{}
			for p, raw := range f.original.files {
				before[p] = append([]byte(nil), raw...)
			}
			f.valid = mode == "valid"
			f.unknown = mode == "unknown"
			f.denied = mode == "denied"
			f.pending = mode == "pending"
			f.mismatch = mode == "mismatch"
			f.cancelAfterRotate = mode == "cancel_after_rotate"
			f.blockCheckpoint = mode == "checkpoint_save_failure"
			renewed, g, err := f.ready(t)
			if mode == "valid" {
				if err != nil || renewed || g.rotations != 0 || g.identities != 1 {
					t.Fatal("valid state changed")
				}
			} else if mode == "expired" {
				if err != nil || !renewed || g.rotations != 1 || g.identities != 2 {
					t.Fatal("native refresh failed", err, g.rotations, g.identities)
				}
				a, e := agentRecoveryAccessSnapshot(f.dir, f.original)
				if e != nil || a.RefreshToken != "" || a.AccessToken != "fixture-new-access" || a.Generation != f.original.session.Generation+1 {
					t.Fatal("access snapshot invalid")
				}
			} else {
				if err == nil || renewed || g.rotations != 1 {
					t.Fatal("failure accepted", err)
				}
				if _, e := agentRecoveryAccessSnapshot(f.dir, f.original); e == nil {
					t.Fatal("incomplete recovery exposed access snapshot")
				}
				f.pending = false
				f.cancelAfterRotate = false
				if f.blockCheckpoint {
					path := filepath.Join(f.dir, agentRecoveryName)
					if os.Remove(path) != nil || os.Rename(path+".fixture-attempt", path) != nil {
						t.Fatal("fixture attempt restore failed")
					}
					f.blockCheckpoint = false
				}
				f.store, err = openAgentRecoveryStore(f.dir, f.original)
				if err != nil {
					t.Fatal("reopen sidecar", err)
				}
				renewed, g, err = f.ready(t)
				if g.rotations != 0 || f.rotations != 1 {
					t.Fatal("consuming refresh replayed")
				}
				if mode == "pending" || mode == "cancel_after_rotate" {
					if err != nil || !renewed {
						t.Fatal("pending identity could not resume", err)
					}
				} else if err == nil || renewed {
					t.Fatal("failed identity/rotation promoted")
				}
			}
			for path, raw := range before {
				now, e := os.ReadFile(path)
				if e != nil || !bytes.Equal(now, raw) {
					t.Fatal("original ledger/session/key changed")
				}
			}
		})
	}
}

func TestQuarkAgentRecoveryAttemptPersistenceFailureDoesNotRotateOffline(t *testing.T) {
	f := newAgentRecoveryFixture(t)
	path := filepath.Join(f.dir, agentRecoveryName)
	if os.Mkdir(path, 0700) != nil {
		t.Fatal("fixture blocker failed")
	}
	_, g, err := f.ready(t)
	if err == nil || g.rotations != 0 || f.rotations != 0 {
		t.Fatal("rotation preceded durable intent")
	}
	if os.Remove(path) != nil {
		t.Fatal("fixture blocker cleanup failed")
	}
	f.store, err = openAgentRecoveryStore(f.dir, f.original)
	if err != nil {
		t.Fatal(err)
	}
	if renewed, _, err := f.ready(t); err != nil || !renewed || f.rotations != 1 {
		t.Fatal("unsubmitted refresh could not recover safely", err)
	}
}

func TestQuarkAgentRecoverySidecarRejectsScopeAndOriginalChangesOffline(t *testing.T) {
	f := newAgentRecoveryFixture(t)
	_, _, err := f.ready(t)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"account", "device", "client", "proof", "web", "revision"} {
		t.Run(mode, func(t *testing.T) {
			st := f.store.state
			raw, _ := json.Marshal(st.Current)
			copyBundle, _ := quark.ParseCredentials(raw)
			switch mode {
			case "account":
				copyBundle.Agent.AccountID = "other"
			case "device":
				copyBundle.Agent.DeviceID = "other"
			case "client":
				copyBundle.Agent.ClientDeviceID = "other"
			case "proof":
				copyBundle.Proofs = []quark.SpaceProof{{Version: 1}}
			case "web":
				copyBundle.Web = &quark.WebAuthorization{}
			case "revision":
				copyBundle.Revision = 0
			}
			f.store.state.Current = copyBundle
			if f.store.save() == nil {
				t.Fatal("changed scope persisted")
			}
			f.store.state = st
		})
	}
	keyPath := filepath.Join(f.dir, "credentials.key")
	f.original.files[keyPath] = append([]byte(nil), f.original.files[keyPath]...)
	f.original.files[keyPath][0] ^= 1
	if _, err = openAgentRecoveryStore(f.dir, f.original); err == nil {
		t.Fatal("changed original fingerprint accepted")
	}
}

func TestQuarkAgentRecoveryRefreshGuardLimitsOffline(t *testing.T) {
	f := newAgentRecoveryFixture(t)
	for _, mode := range []string{"media", "broker", "quota", "deadline", "oversize", "rotation_without_attempt"} {
		t.Run(mode, func(t *testing.T) {
			g := &agentRecoveryGuard{store: f.store, deadline: time.Now().Add(time.Second)}
			r, _ := http.NewRequest("GET", "https://open-api-drive.quark.cn/open/v1/user/info", nil)
			q := r.URL.Query()
			q.Set("req_id", "fixture")
			q.Set("platform", "web")
			q.Set("access_token", f.store.state.Current.Agent.AccessToken)
			q.Set("device_id", f.store.state.Current.Agent.DeviceID)
			r.URL.RawQuery = q.Encode()
			r.Header.Set("Authorization", "Bearer "+f.store.state.Current.Agent.AccessToken)
			switch mode {
			case "broker":
				r.URL.Host = "api.extscreen.com"
			case "quota":
				g.identities = 3
			case "deadline":
				g.deadline = time.Now().Add(-time.Second)
			case "rotation_without_attempt":
				r.Method = "POST"
				r.URL.Path = "/agent/v1/oauth/access_token/rotate"
				q.Del("access_token")
				q.Del("device_id")
				r.URL.RawQuery = q.Encode()
				r.Header.Del("Authorization")
				r.Body = io.NopCloser(strings.NewReader(`{}`))
			}
			calls := 0
			tr := g.wrap(observationOfflineTransport(func(*http.Request) (*http.Response, error) {
				calls++
				return f.response(200, strings.Repeat("x", int(agentRecoveryDiagnosticLimit)+1))
			}), mode == "media")
			resp, err := tr.RoundTrip(r)
			if resp != nil {
				resp.Body.Close()
			}
			if err == nil || mode == "oversize" && calls != 1 || mode != "oversize" && calls != 0 {
				t.Fatal("scope limits failed")
			}
		})
	}
}
