//go:build quark_live

package handlers_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
)

type largefileRefreshFixtureTransport func(*http.Request) (*http.Response, error)

func (f largefileRefreshFixtureTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func offlineLargefileTVReady(t *testing.T, s *liveTVStore) (bool, *largefileRefreshGuard, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	d, _ := ctx.Deadline()
	g := &largefileRefreshGuard{store: s, deadline: d}
	b, e := largefileRefreshReady(ctx, s, g)
	return b, g, e
}

func TestQuarkLargeFileOfflineTVRecoveryNative(t *testing.T) {
	for _, scenario := range []string{"expired", "valid", "ambiguous", "denied", "checkpoint"} {
		t.Run(scenario, func(t *testing.T) {
			f := newLiveTVFixture(t)
			s := f.open(t)
			before := append([]byte(nil), s.saved...)
			key := append([]byte(nil), s.key...)
			f.oldValid.Store(scenario == "valid")
			f.brokerFailure.Store(scenario == "ambiguous")
			f.brokerDenied.Store(scenario == "denied")
			f.identityFailure.Store(scenario == "checkpoint")
			renewed, g, e := offlineLargefileTVReady(t, s)
			if scenario == "valid" {
				if e != nil || renewed || g.brokers != 0 || g.identities != 1 || !bytes.Equal(before, s.saved) {
					t.Fatal("valid recovery changed original state or contacted broker")
				}
			} else if scenario == "expired" {
				if e != nil || !renewed || g.brokers != 1 || g.identities != 2 || s.session.Generation != 8 || s.session.AccessToken != "fixture-new-access" || s.checkpoint != nil || s.attempt != nil {
					t.Fatal("native bounded refresh failed")
				}
			} else {
				if e == nil || renewed || g.brokers != 1 || !bytes.Equal(before, s.saved) {
					t.Fatal("failed refresh changed original session")
				}
				_ = s.lock.Close()
				s = f.open(t)
				f.identityFailure.Store(false)
				renewed, g, e = offlineLargefileTVReady(t, s)
				if g.brokers != 0 || f.brokerCalls.Load() != 1 {
					t.Fatal("saved ambiguous rotation was replayed")
				}
				if scenario == "checkpoint" {
					if e != nil || !renewed || s.session.Generation != 8 {
						t.Fatal("saved checkpoint was not confirmed")
					}
				} else if e == nil || renewed {
					t.Fatal("unknown or denied rotation was accepted")
				}
			}
			if !bytes.Equal(key, s.key) {
				t.Fatal("original encryption key changed")
			}
		})
	}
}

func TestQuarkLargeFileOfflineTVRecoveryHTTP400Classification(t *testing.T) {
	for _, authError := range []bool{false, true} {
		t.Run(map[bool]string{false: "unknown", true: "explicit_auth_errno"}[authError], func(t *testing.T) {
			f := newLiveTVFixture(t)
			s := f.open(t)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			d, _ := ctx.Deadline()
			g := &largefileRefreshGuard{store: s, deadline: d}
			wrap := func(base http.RoundTripper, media bool) http.RoundTripper {
				alter := largefileRefreshFixtureTransport(func(r *http.Request) (*http.Response, error) {
					resp, e := base.RoundTrip(r)
					if e != nil {
						return resp, e
					}
					if r.URL.Path == "/user" && r.URL.Query().Get("access_token") == "fixture-old-access" {
						_ = resp.Body.Close()
						resp.StatusCode = 400
						b := `{"errno":777,"message":"private-upstream-message"}`
						if authError {
							b = `{"errno":10001,"message":"private-upstream-message"}`
						}
						resp.Body = io.NopCloser(strings.NewReader(b))
						resp.ContentLength = int64(len(b))
					}
					return resp, nil
				})
				return g.wrap(alter, media)
			}
			a := &largefileRefreshAuthenticator{native: quark.NewObservationValidationAuthenticator(cloud.Config{}, wrap), guard: g}
			_, renewed, e := s.ready(ctx, a)
			if g.initialHTTP != 400 {
				t.Fatal("HTTP diagnostic was lost")
			}
			if authError {
				if e != nil || !renewed || g.initialCode != quark.CodeAuth || g.initialErrno != 10001 || g.brokers != 1 {
					t.Fatal("explicit authentication error did not renew")
				}
			} else {
				if e == nil || renewed || g.initialCode != quark.CodeUnavailable || g.initialErrno != 777 || g.brokers != 0 {
					t.Fatal("generic HTTP400 triggered token consumption")
				}
			}
		})
	}
}

func TestQuarkLargeFileOfflineTVRecoveryLimits(t *testing.T) {
	f := newLiveTVFixture(t)
	s := f.open(t)
	for _, scenario := range []string{"agent", "media", "broker_query", "redirect_host", "oversize", "quota", "deadline"} {
		t.Run(scenario, func(t *testing.T) {
			g := &largefileRefreshGuard{store: s, deadline: time.Now().Add(time.Second)}
			calls := 0
			base := largefileRefreshFixtureTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				body := `{"status":0}`
				if scenario == "oversize" {
					body = strings.Repeat("x", int(largefileRefreshLimit)+1)
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), ContentLength: -1}, nil
			})
			r, _ := http.NewRequest(http.MethodGet, "https://open-api-drive.quark.cn/user?method=user_info&device_id="+s.session.DeviceID+"&platform=tv&access_token="+s.session.AccessToken, nil)
			if scenario == "agent" {
				r.URL.Path = "/open/v1/user/info"
			}
			if scenario == "redirect_host" {
				r.URL.Host = "other.example"
			}
			if scenario == "broker_query" {
				r.URL.Host = "api.extscreen.com"
				r.URL.Path = "/quarkdrive/token"
				r.Method = http.MethodPost
			}
			if scenario == "quota" {
				g.identities = 3
			}
			if scenario == "deadline" {
				g.deadline = time.Now().Add(-time.Second)
			}
			resp, e := g.wrap(base, scenario == "media").RoundTrip(r)
			if resp != nil {
				resp.Body.Close()
			}
			if e == nil {
				t.Fatal("recovery guard accepted out-of-scope request or response")
			}
			if scenario == "oversize" {
				if calls != 1 {
					t.Fatal("oversize fixture not read")
				}
			} else if calls != 0 {
				t.Fatal("invalid request was dispatched")
			}
		})
	}
}
