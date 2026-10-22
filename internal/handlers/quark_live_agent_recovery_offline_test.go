//go:build quark_live

package handlers_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
)

func TestQuarkAgentRecoveryDiagnosticOffline(t *testing.T) {
	for _, tc := range []struct {
		name          string
		status, errno int
		want          string
	}{
		{"valid", 200, 0, "ok"}, {"auth", 400, 11001, quark.CodeAuth}, {"unknown", 400, 777, quark.CodeUnavailable}, {"limited", 429, 0, quark.CodeLimited}, {"redirect", 302, 0, quark.CodeInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, s := observationOfflineAgent(t)
			g := &agentRecoveryDiagnostic{session: s.session, deadline: time.Now().Add(2 * time.Second)}
			calls := 0
			wrap := func(_ http.RoundTripper, media bool) http.RoundTripper {
				return g.wrap(observationOfflineTransport(func(r *http.Request) (*http.Response, error) {
					calls++
					if r.URL.Query().Get("access_token") != s.session.Access || r.Header.Get("Authorization") != "Bearer "+s.session.Access {
						t.Fatal("native identity token mismatch")
					}
					raw := fmt.Sprintf(`{"status":0,"errno":%d,"message":"never-print-private","data":{"user_id":%q}}`, tc.errno, s.session.User)
					return &http.Response{StatusCode: tc.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(raw)), ContentLength: int64(len(raw))}, nil
				}), media)
			}
			err := agentRecoveryVerify(context.Background(), g, wrap)
			if largefileRefreshCode(err) != tc.want || g.requests != 1 || calls != 1 || g.httpStatus != tc.status || g.errno != tc.errno || s.unchanged() != nil {
				t.Fatal("bounded native diagnostic mismatch", largefileRefreshCode(err), g.requests, calls, g.httpStatus, g.errno)
			}
			_ = agentRecoveryVerify(context.Background(), g, wrap)
			if calls != 1 {
				t.Fatal("second request escaped single-request budget")
			}
		})
	}
}

func TestQuarkAgentRecoveryDiagnosticScopeOffline(t *testing.T) {
	_, s := observationOfflineAgent(t)
	for _, mode := range []string{"media", "refresh", "other_host", "duplicate_query", "wrong_token", "oversize", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			g := &agentRecoveryDiagnostic{session: s.session, deadline: time.Now().Add(time.Second)}
			r, _ := http.NewRequest(http.MethodGet, "https://open-api-drive.quark.cn/open/v1/user/info", nil)
			q := r.URL.Query()
			q.Set("access_token", s.session.Access)
			q.Set("device_id", s.session.Device)
			q.Set("req_id", "one")
			q.Set("platform", "web")
			r.URL.RawQuery = q.Encode()
			r.Header.Set("Authorization", "Bearer "+s.session.Access)
			switch mode {
			case "refresh":
				r.Method = http.MethodPost
				r.URL.Path = "/agent/v1/oauth/access_token/rotate"
			case "other_host":
				r.URL.Host = "foreign.example"
			case "duplicate_query":
				q.Add("platform", "web")
				r.URL.RawQuery = q.Encode()
			case "wrong_token":
				r.Header.Set("Authorization", "Bearer wrong")
			case "deadline":
				g.deadline = time.Now().Add(-time.Second)
			}
			calls := 0
			tr := g.wrap(observationOfflineTransport(func(*http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: 200, Header: make(http.Header), ContentLength: -1, Body: io.NopCloser(strings.NewReader(strings.Repeat("x", int(agentRecoveryDiagnosticLimit)+1)))}, nil
			}), mode == "media")
			resp, err := tr.RoundTrip(r)
			if resp != nil {
				resp.Body.Close()
			}
			if err == nil || mode != "oversize" && calls != 0 || mode == "oversize" && calls != 1 {
				t.Fatal("scope or response guard failed")
			}
		})
	}
}
