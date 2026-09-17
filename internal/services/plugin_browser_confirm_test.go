package services

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/plugins/contract"
)

type confirmRuntime struct {
	*resourcePluginRuntime
	invoke func(context.Context, string)
}

func (r *confirmRuntime) Invoke(ctx context.Context, connection, operation string, input []byte) ([]byte, error) {
	if r.invoke != nil {
		r.invoke(ctx, operation)
	}
	return r.resourcePluginRuntime.Invoke(ctx, connection, operation, input)
}

func TestPluginBrowserConfirmHealthFirst(t *testing.T) {
	for _, tc := range []struct {
		name, health, code string
		expired, resume    bool
	}{
		{"manual", "healthy", "", false, false},
		{"manual_after_password_expiry", "healthy", "", true, false},
		{"unavailable", "unavailable", "resource_entry_unavailable", false, false},
		{"limited", "rate_limited", "resource_rate_limited", false, false},
		{"expired_not_logged_in", "auth_required", "resource_browser_login_expired", true, false},
		{"resume_only_proven_unauthenticated", "auth_required", "", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, actor, runtime, _, connection, _ := resourcePluginServiceFixture(t, "org.ohmycine.confirm")
			s.browser = &browserFixture{}
			runtime.responses["resource.auth.login"] = []byte(`{"pluginError":{"code":"browser-verification-required"}}`)
			_, _ = s.LoginResource(context.Background(), actor, connection.PluginID, contract.ResourceLoginRequest{ConnectionID: connection.ID, Username: "user", Password: "synthetic-secret"})
			pending, session := s.browserSession.Pending, s.browserSession
			if pending == nil {
				t.Fatal("missing pending")
			}
			if tc.expired {
				pending.Expires = time.Now().Add(-time.Minute)
			}
			runtime.operations = nil
			runtime.responses["resource.health"] = []byte(`{"status":"` + tc.health + `"}`)
			runtime.responses["resource.auth.login"] = []byte(`{"state":"authenticated"}`)
			s.runtime = &confirmRuntime{resourcePluginRuntime: runtime, invoke: func(_ context.Context, operation string) {
				if operation == "resource.auth.login" {
					runtime.responses["resource.health"] = []byte(`{"status":"healthy"}`)
				}
			}}
			_, err := s.ResourceBrowser(context.Background(), actor, connection.PluginID, connection.ID, "confirm", BrowserInput{SessionID: session.ID})
			if (tc.code == "" && err != nil) || (tc.code != "" && ErrorCode(err) != tc.code) {
				t.Fatalf("error=%v want=%s", err, tc.code)
			}
			want := "resource.health"
			if tc.resume {
				want += ",resource.auth.login,resource.health"
			}
			if strings.Join(runtime.operations, ",") != want {
				t.Fatalf("operations=%v", runtime.operations)
			}
			if s.browserSession != session || session.Authenticating {
				t.Fatal("session lost or busy")
			}
			if tc.code == "" || tc.expired {
				if session.Pending != nil || strings.Trim(string(pending.Password), "\x00") != "" {
					t.Fatal("secret retained")
				}
			} else if session.Pending != pending {
				t.Fatal("retryable pending discarded")
			}
			var count int64
			s.db.Model(&models.PluginBrowserState{}).Where("connection_id = ?", connection.ID).Count(&count)
			if (count == 1) != (tc.code == "") {
				t.Fatal("incorrect durable authentication")
			}
		})
	}
}

type failingRequestBrowser struct{ browserFixture }

func (b *failingRequestBrowser) Call(ctx context.Context, op string, in, out any) error {
	if op == "session/request" {
		return errors.New("secret-cookie=https://private.invalid")
	}
	return b.browserFixture.Call(ctx, op, in, out)
}

func TestPluginBrowserHealthRetainsSafeTransportFailure(t *testing.T) {
	s, actor, runtime, _, connection, _ := resourcePluginServiceFixture(t, "org.ohmycine.confirm-diagnostic")
	s.browser = &failingRequestBrowser{}
	if _, err := s.ResourceBrowser(context.Background(), actor, connection.PluginID, connection.ID, "start", BrowserInput{}); err != nil {
		t.Fatal(err)
	}
	runtime.responses["resource.health"] = []byte(`{"pluginError":{"code":"upstream-unavailable","message":"secret"}}`)
	s.runtime = &confirmRuntime{resourcePluginRuntime: runtime, invoke: func(ctx context.Context, _ string) {
		request, _ := http.NewRequest("GET", "https://mirror.example/search", nil)
		_, handled, err := s.BrowserRequest(ctx, connection.PluginID, connection.ID, "resource.session", request)
		if !handled || err == nil {
			t.Fatal("expected browser transport failure")
		}
	}}
	_, err := s.ResourceBrowser(context.Background(), actor, connection.PluginID, connection.ID, "confirm", BrowserInput{SessionID: s.browserSession.ID})
	if ErrorCode(err) != "resource_browser_request_failed" || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "private.invalid") {
		t.Fatalf("unsafe or lost transport classification: %v", err)
	}
}

func TestPluginBrowserConfirmRejectsRevokedSessionBeforeHealthPublication(t *testing.T) {
	for _, mode := range []string{"close", "config_revision"} {
		t.Run(mode, func(t *testing.T) {
			s, actor, runtime, _, connection, _ := resourcePluginServiceFixture(t, "org.ohmycine.confirm-revoke")
			if err := s.db.Model(&models.PluginConnection{}).Where("id = ?", connection.ID).Update("last_health_status", "unknown").Error; err != nil {
				t.Fatal(err)
			}
			s.browser = &browserFixture{}
			if _, err := s.ResourceBrowser(context.Background(), actor, connection.PluginID, connection.ID, "start", BrowserInput{}); err != nil {
				t.Fatal(err)
			}
			id := s.browserSession.ID
			runtime.responses["resource.health"] = []byte(`{"status":"healthy"}`)
			s.runtime = &confirmRuntime{resourcePluginRuntime: runtime, invoke: func(ctx context.Context, _ string) {
				if mode == "close" {
					if _, err := s.ResourceBrowser(ctx, actor, connection.PluginID, connection.ID, "close", BrowserInput{SessionID: id}); err != nil {
						t.Fatal(err)
					}
				} else if err := s.db.Model(&models.PluginConnection{}).Where("id = ?", connection.ID).Update("revision", 2).Error; err != nil {
					t.Fatal(err)
				}
			}}
			_, err := s.ResourceBrowser(context.Background(), actor, connection.PluginID, connection.ID, "confirm", BrowserInput{SessionID: id})
			if ErrorCode(err) != "resource_browser_session_expired" {
				t.Fatalf("accepted stale health: %v", err)
			}
			var count int64
			s.db.Model(&models.PluginBrowserState{}).Where("connection_id = ?", connection.ID).Count(&count)
			if count != 0 {
				t.Fatal("saved revoked cookie state")
			}
			var current models.PluginConnection
			s.db.First(&current, "id = ?", connection.ID)
			if current.LastHealthStatus == "healthy" {
				t.Fatal("saved stale healthy status")
			}
		})
	}
}
