package services

import (
	"context"
	"encoding/json"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/browsercompanion"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/plugins/contract"
	"net/http"
	"strings"
	"testing"
	"time"
)

type browserFixture struct{ calls []string }

type snapshotBrowserFixture struct {
	browserFixture
	code        string
	count       int
	reloadError error
}

func (b *snapshotBrowserFixture) Call(ctx context.Context, operation string, input, output any) error {
	if operation == "session/snapshot" {
		raw, _ := json.Marshal(map[string]any{"imageBase64": "YWJj", "mimeType": "image/png", "width": 1280, "height": 800, "networkErrorCode": b.code, "blockedResourceCount": b.count, "url": "https://private.invalid/?cookie=synthetic"})
		return json.Unmarshal(raw, output)
	}
	if operation == "session/reload" {
		payload := input.(map[string]any)
		if len(payload) != 2 || payload["sessionId"] == "" || payload["identity"] == "" {
			panic("reload leaked non-session fields")
		}
		if b.reloadError != nil {
			return b.reloadError
		}
	}
	return b.browserFixture.Call(ctx, operation, input, output)
}

func TestPluginBrowserReloadPreservesPendingLoginAndRejectsWrongOwnerAndRevocation(t *testing.T) {
	s, actor, runtime, _, connection, _ := resourcePluginServiceFixture(t, "org.ohmycine.browser-reload")
	browser := &snapshotBrowserFixture{}
	s.browser = browser
	runtime.responses["resource.auth.login"] = []byte(`{"pluginError":{"code":"browser-verification-required","message":"challenge"}}`)
	_, err := s.LoginResource(context.Background(), actor, connection.PluginID, contract.ResourceLoginRequest{ConnectionID: connection.ID, Username: "synthetic-user", Password: "synthetic-password"})
	if ErrorCode(err) != "resource_browser_verification_required" {
		t.Fatal(err)
	}
	session, pending := s.browserSession, s.browserSession.Pending
	before := len(runtime.operations)
	input := BrowserInput{SessionID: session.ID}
	if _, err := s.ResourceBrowser(context.Background(), actor, connection.PluginID, connection.ID, "reload", input); err != nil {
		t.Fatal(err)
	}
	if s.browserSession != session || session.Pending != pending || string(pending.Password) != "synthetic-password" || len(runtime.operations) != before {
		t.Fatal("reload recreated context, changed pending login, or replayed credential operation")
	}
	browser.reloadError = &browsercompanion.Failure{Code: "browser_reload_post_denied"}
	if _, err := s.ResourceBrowser(context.Background(), actor, connection.PluginID, connection.ID, "reload", input); ErrorCode(err) != "resource_browser_reload_post_denied" {
		t.Fatal("POST replay denial lost")
	}
	if s.browserSession != session || session.Pending != pending || len(runtime.operations) != before {
		t.Fatal("failed reload changed login")
	}
	browser.reloadError = nil
	other := actor
	other.User.ID++
	beforeCalls := len(browser.calls)
	if _, err := s.ResourceBrowser(context.Background(), other, connection.PluginID, connection.ID, "reload", input); ErrorCode(err) != "resource_browser_session_expired" {
		t.Fatal("foreign owner reload accepted")
	}
	if len(browser.calls) != beforeCalls {
		t.Fatal("foreign owner reached companion")
	}
	if err := s.db.Model(&models.PluginConnection{}).Where("id = ?", connection.ID).Update("credential_version", 2).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResourceBrowser(context.Background(), actor, connection.PluginID, connection.ID, "reload", input); ErrorCode(err) != "resource_browser_session_expired" {
		t.Fatal("revoked reload accepted")
	}
	if s.browserSession != nil || browser.calls[len(browser.calls)-1] != "session/close" {
		t.Fatal("revoked context remained live")
	}
	for _, value := range pending.Password {
		if value != 0 {
			t.Fatal("revoked pending password remained")
		}
	}
}

func TestPluginBrowserSnapshotProjectsOnlySafeResourceDiagnostics(t *testing.T) {
	s, actor, _, _, connection, _ := resourcePluginServiceFixture(t, "org.ohmycine.browser-snapshot-network")
	browser := &snapshotBrowserFixture{}
	s.browser = browser
	if _, err := s.ResourceBrowser(context.Background(), actor, connection.PluginID, connection.ID, "start", BrowserInput{}); err != nil {
		t.Fatal(err)
	}
	for _, code := range []string{"", "resource_network_denied", "resource_network_timeout", "resource_network_failed", "resource_limit_exceeded", "tun_fake_ip_requires_opt_in"} {
		browser.code, browser.count = code, 3
		output, err := s.ResourceBrowser(context.Background(), actor, connection.PluginID, connection.ID, "snapshot", BrowserInput{SessionID: s.browserSession.ID})
		if err != nil {
			t.Fatal(err)
		}
		data := output.(map[string]any)
		if data["network_error_code"] != code || data["blocked_resource_count"] != 3 {
			t.Fatal("resource summary lost")
		}
		raw, _ := json.Marshal(data)
		if strings.Contains(string(raw), "private.invalid") || strings.Contains(string(raw), "cookie") {
			t.Fatal("private resource details exposed")
		}
	}
	for _, invalid := range []struct {
		code  string
		count int
	}{{"https://secret.invalid", 0}, {"", -1}, {"", 10001}} {
		browser.code, browser.count = invalid.code, invalid.count
		if _, err := s.ResourceBrowser(context.Background(), actor, connection.PluginID, connection.ID, "snapshot", BrowserInput{SessionID: s.browserSession.ID}); err == nil {
			t.Fatal("invalid diagnostics accepted")
		}
	}
}

type tunStatusBrowserFixture struct{ enabled bool }

func (b tunStatusBrowserFixture) Call(_ context.Context, _ string, _, output any) error {
	raw, _ := json.Marshal(map[string]any{"state": "ready", "installed": true, "protocolVersion": 1, "tunFakeIPEnabled": b.enabled})
	return json.Unmarshal(raw, output)
}

func TestBrowserComponentProjectsDeploymentTUNStatus(t *testing.T) {
	s, actor, _, _, _, _ := resourcePluginServiceFixture(t, "org.ohmycine.browser-tun-status")
	actor.Permissions["settings.read"] = struct{}{}
	for _, enabled := range []bool{false, true} {
		s.browser = tunStatusBrowserFixture{enabled: enabled}
		output, err := s.BrowserComponent(context.Background(), actor, "status", BrowserInput{})
		if err != nil {
			t.Fatal(err)
		}
		if output.(map[string]any)["tun_fake_ip_enabled"] != enabled {
			t.Fatal("effective network mode missing from settings")
		}
	}
}

type hookBrowserFixture struct {
	browserFixture
	before func(string)
}

type responseBrowserFixture struct {
	browserFixture
	response map[string]any
}

func (b *responseBrowserFixture) Call(ctx context.Context, operation string, input, output any) error {
	if operation == "session/request" {
		raw, _ := json.Marshal(b.response)
		return json.Unmarshal(raw, output)
	}
	return b.browserFixture.Call(ctx, operation, input, output)
}

func TestPluginBrowserRequestRejectsMalformedBinaryProtocol(t *testing.T) {
	s, actor, _, _, connection, _ := resourcePluginServiceFixture(t, "org.ohmycine.browser-protocol")
	browser := &responseBrowserFixture{}
	s.browser = browser
	if _, err := s.ResourceBrowser(context.Background(), actor, connection.PluginID, connection.ID, "start", BrowserInput{}); err != nil {
		t.Fatal(err)
	}
	for _, response := range []map[string]any{
		{"status": 200, "body": "legacy text"},
		{"status": 200, "bodyBase64": "e30=\n"},
		{"status": 200, "bodyBase64": "!invalid"},
		{"status": 200, "bodyBase64": "e30=", "headers": map[string]string{"content-type": "text/plain\r\nX-Test: value"}},
	} {
		browser.response = response
		request, _ := http.NewRequest("GET", "https://mirror.example/search", nil)
		if _, _, err := s.BrowserRequest(context.Background(), connection.PluginID, connection.ID, "resource.session", request); err == nil {
			t.Fatal("invalid companion response accepted")
		}
	}
}

func (b *hookBrowserFixture) Call(ctx context.Context, operation string, input, output any) error {
	if b.before != nil {
		b.before(operation)
	}
	return b.browserFixture.Call(ctx, operation, input, output)
}

func (b *browserFixture) Call(_ context.Context, operation string, input, output any) error {
	b.calls = append(b.calls, operation)
	var response any = map[string]any{}
	switch operation {
	case "session/create":
		response = map[string]any{"sessionId": "opaque-session", "expiresAt": time.Now().Add(29 * time.Minute)}
	case "status":
		response = map[string]any{"state": "ready", "protocolVersion": 1}
	case "session/request":
		response = map[string]any{"status": 200, "headers": map[string]string{"content-type": "application/json"}, "bodyBase64": "e30="}
	case "session/cookies":
		response = map[string]any{"cookies": []browserCookie{{Name: "session", Value: "synthetic", Domain: "mirror.example", Path: "/", Expires: -1, Secure: true, HTTPOnly: true, SameSite: "Lax"}}}
	}
	if output == nil {
		return nil
	}
	raw, _ := json.Marshal(response)
	return json.Unmarshal(raw, output)
}

func TestPluginBrowserAutomaticLoginResumeAndEncryptedRestore(t *testing.T) {
	s, actor, runtime, store, connection, _ := resourcePluginServiceFixture(t, "org.ohmycine.browser-resume")
	browser := &browserFixture{}
	s.browser = browser
	runtime.responses["resource.auth.login"] = []byte(`{"pluginError":{"code":"browser-verification-required","message":"challenge"}}`)
	_, err := s.LoginResource(context.Background(), actor, connection.PluginID, contract.ResourceLoginRequest{ConnectionID: connection.ID, Username: "synthetic-user", Password: "synthetic-password"})
	if ErrorCode(err) != "resource_browser_verification_required" || s.browserSession == nil || s.browserSession.Pending == nil {
		t.Fatalf("verification not resumable: %v", err)
	}
	pending := s.browserSession.Pending
	if string(pending.Password) != "synthetic-password" {
		t.Fatal("pending login missing")
	}
	runtime.responses["resource.auth.login"] = []byte(`{"state":"authenticated"}`)
	runtime.responses["resource.health"] = []byte(`{"status":"healthy","accountName":"synthetic"}`)
	result, err := s.ResourceBrowser(context.Background(), actor, connection.PluginID, connection.ID, "confirm", BrowserInput{SessionID: s.browserSession.ID})
	if err != nil || result.(contract.ResourceLoginResponse).State != "authenticated" {
		t.Fatalf("resume: %v", err)
	}
	for _, b := range pending.Password {
		if b != 0 {
			t.Fatal("pending password not cleared")
		}
	}
	var state models.PluginBrowserState
	if err := s.db.First(&state, "connection_id = ?", connection.ID).Error; err != nil {
		t.Fatal(err)
	}
	if strings.Contains(state.Ciphertext, "synthetic") {
		t.Fatal("plaintext persisted")
	}
	plain, err := store.Decrypt(browserStatePurpose(connection.PluginID, connection.ID), state.Ciphertext)
	if err != nil || !strings.Contains(plain, "session") || strings.Contains(plain, "password") {
		t.Fatalf("cookie-only state invalid: %v", err)
	}
	s.browserSession.Expires = time.Now().Add(-time.Minute)
	s.revalidateBrowserSession(context.Background())
	if s.browserSession != nil {
		t.Fatal("idle browser remained")
	}
	request, _ := http.NewRequest("GET", "https://mirror.example/search", nil)
	response, handled, err := s.BrowserRequest(context.Background(), connection.PluginID, connection.ID, "resource.session", request)
	if err != nil || !handled {
		t.Fatalf("stored login not restored: %v", err)
	}
	_ = response.Body.Close()
	// A new service/process and Runtime generation must restore the durable
	// cookie, not treat resource idle/process lifetime as account expiration.
	if err := s.db.Model(&models.PluginInstallation{}).Where("plugin_id = ?", connection.PluginID).Update("runtime_generation", 2).Error; err != nil {
		t.Fatal(err)
	}
	restarted := &PluginRepositoryService{db: s.db, credentials: store, runtime: runtime, browser: &browserFixture{}}
	if err := restarted.restoreResourceBrowser(context.Background(), connection.ID); err != nil {
		t.Fatal(err)
	}
	if restarted.browserSession == nil {
		t.Fatal("server restart lost encrypted login")
	}
}

func TestPluginBrowserPendingIsolationAndExpiry(t *testing.T) {
	s, actor, runtime, _, connection, _ := resourcePluginServiceFixture(t, "org.ohmycine.browser-pending")
	s.browser = &browserFixture{}
	runtime.responses["resource.auth.login"] = []byte(`{"pluginError":{"code":"browser-verification-required"}}`)
	_, _ = s.LoginResource(context.Background(), actor, connection.PluginID, contract.ResourceLoginRequest{ConnectionID: connection.ID, Username: "test", Password: "synthetic"})
	if s.browserSession == nil || s.browserSession.Pending == nil {
		t.Fatal("pending missing")
	}
	request, _ := http.NewRequest("GET", "https://mirror.example/search", nil)
	if _, _, err := s.BrowserRequest(context.Background(), connection.PluginID, connection.ID, "resource.session", request); err == nil {
		t.Fatal("ordinary search borrowed pending identity")
	}
	pending := s.browserSession.Pending
	pending.Expires = time.Now().Add(-time.Minute)
	runtime.responses["resource.health"] = []byte(`{"status":"auth_required"}`)
	_, err := s.ResourceBrowser(context.Background(), actor, connection.PluginID, connection.ID, "confirm", BrowserInput{SessionID: s.browserSession.ID})
	if ErrorCode(err) != "resource_browser_login_expired" {
		t.Fatalf("expired pending: %v", err)
	}
	for _, b := range pending.Password {
		if b != 0 {
			t.Fatal("expired password retained")
		}
	}
}

func TestPluginBrowserCookieValidation(t *testing.T) {
	base := browserCookie{Name: "sid", Value: "synthetic", Domain: "mirror.example", Path: "/", Expires: -1, SameSite: "Lax", Secure: true}
	for _, change := range []func(*browserCookie){func(c *browserCookie) { c.Domain = "other.example" }, func(c *browserCookie) { c.Path = "/bad path" }, func(c *browserCookie) { c.Value = "a;b" }, func(c *browserCookie) { c.SameSite = "unknown" }, func(c *browserCookie) { c.Expires = -2 }} {
		cookie := base
		change(&cookie)
		if validateBrowserCookies("https://mirror.example", []browserCookie{cookie}) == nil {
			t.Fatal("invalid browser state accepted")
		}
	}
	if validateBrowserCookies("https://mirror.example", []browserCookie{base, base}) == nil {
		t.Fatal("duplicate cookie accepted")
	}
}

func TestPluginBrowserStateRejectsConcurrentConfigEdit(t *testing.T) {
	s, actor, runtime, _, connection, _ := resourcePluginServiceFixture(t, "org.ohmycine.browser-race")
	browser := &hookBrowserFixture{}
	s.browser = browser
	runtime.responses["resource.auth.login"] = []byte(`{"state":"authenticated"}`)
	runtime.responses["resource.health"] = []byte(`{"status":"healthy"}`)
	browser.before = func(operation string) {
		if operation == "session/cookies" {
			if err := s.db.Model(&models.PluginConnection{}).Where("id = ?", connection.ID).Update("revision", 2).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := s.LoginResource(context.Background(), actor, connection.PluginID, contract.ResourceLoginRequest{ConnectionID: connection.ID, Username: "test", Password: "synthetic"}); err == nil {
		t.Fatal("old browser state accepted for edited connection")
	}
	var count int64
	s.db.Model(&models.PluginBrowserState{}).Where("connection_id = ?", connection.ID).Count(&count)
	if count != 0 {
		t.Fatal("stale state persisted")
	}
}

func TestPluginBrowserFinishedSessionOwnerHandoff(t *testing.T) {
	s, actor, _, _, connection, _ := resourcePluginServiceFixture(t, "org.ohmycine.browser-handoff")
	s.browser = &browserFixture{}
	if _, err := s.ResourceBrowser(context.Background(), actor, connection.PluginID, connection.ID, "start", BrowserInput{}); err != nil {
		t.Fatal(err)
	}
	other := actor
	other.User.ID++
	if _, err := s.ResourceBrowser(context.Background(), other, connection.PluginID, connection.ID, "start", BrowserInput{}); err != nil {
		t.Fatal(err)
	}
	if s.browserSession.Owner != other.User.ID {
		t.Fatal("finished browser prevents owner handoff")
	}
	s.browserSession.Authenticating = true
	request, _ := http.NewRequest("GET", "https://mirror.example/search", nil)
	if _, _, err := s.BrowserRequest(context.Background(), connection.PluginID, connection.ID, "resource.session", request); err == nil {
		t.Fatal("search borrowed in-flight authentication")
	}
}

func TestPluginBrowserCredentialCommitRebindIsExplicitAndExact(t *testing.T) {
	s, actor, _, _, connection, _ := resourcePluginServiceFixture(t, "org.ohmycine.browser-commit")
	s.browser = &browserFixture{}
	if _, err := s.ResourceBrowser(context.Background(), actor, connection.PluginID, connection.ID, "start", BrowserInput{}); err != nil {
		t.Fatal(err)
	}
	session := s.browserSession
	original := session.Fingerprint
	if err := s.db.Model(&models.PluginConnection{}).Where("id = ?", connection.ID).Updates(map[string]any{"revision": connection.Revision + 1, "credential_version": connection.CredentialVersion + 1}).Error; err != nil {
		t.Fatal(err)
	}
	s.BrowserCredentialCommitted(context.Background(), connection)
	if session.Fingerprint != original {
		t.Fatal("unscoped commit rebind allowed")
	}
	ctx := context.WithValue(context.Background(), browserAuthKey{}, browserAuthScope{connection.PluginID, connection.ID, session.ID})
	s.BrowserCredentialCommitted(ctx, connection)
	if session.Fingerprint == original {
		t.Fatal("trusted commit did not preserve browser")
	}
	var before models.PluginConnection
	s.db.First(&before, "id = ?", connection.ID)
	if err := s.db.Model(&models.PluginConnection{}).Where("id = ?", connection.ID).Updates(map[string]any{"revision": before.Revision + 1, "credential_version": before.CredentialVersion + 1, "config_json": "{}"}).Error; err != nil {
		t.Fatal(err)
	}
	original = session.Fingerprint
	s.BrowserCredentialCommitted(ctx, before)
	if session.Fingerprint != original {
		t.Fatal("config edit disguised as commit")
	}
}

func TestPluginBrowserSessionIsolationAndRevocation(t *testing.T) {
	s, actor, _, _, connection, _ := resourcePluginServiceFixture(t, "org.ohmycine.browser")
	browser := &browserFixture{}
	s.browser = browser
	ctx := context.Background()
	if _, err := s.ResourceBrowser(ctx, actor, connection.PluginID, connection.ID, "install", BrowserInput{}); err == nil {
		t.Fatal("install accepted without license")
	}
	if len(browser.calls) != 0 {
		t.Fatal("unapproved install reached companion")
	}
	if _, err := s.ResourceBrowser(ctx, actor, connection.PluginID, connection.ID, "start", BrowserInput{}); err != nil {
		t.Fatal(err)
	}
	other := actor
	other.User.ID++
	if _, err := s.ResourceBrowser(ctx, other, connection.PluginID, connection.ID, "input", BrowserInput{SessionID: "opaque-session", Action: "text", Text: "synthetic"}); err == nil {
		t.Fatal("cross-owner input allowed")
	}
	request, _ := http.NewRequestWithContext(ctx, "GET", "https://mirror.example/search", nil)
	response, handled, err := s.BrowserRequest(ctx, connection.PluginID, connection.ID, "resource.session", request)
	if err != nil || !handled || response.StatusCode != 200 {
		t.Fatalf("request handled=%v err=%v", handled, err)
	}
	_ = response.Body.Close()
	request, _ = http.NewRequestWithContext(ctx, "GET", "https://other.example/search", nil)
	if _, _, err = s.BrowserRequest(ctx, connection.PluginID, connection.ID, "resource.session", request); err == nil {
		t.Fatal("cross-origin request allowed")
	}
	if err := s.db.Model(&models.PluginConnection{}).Where("id = ?", connection.ID).Update("credential_version", 2).Error; err != nil {
		t.Fatal(err)
	}
	s.revalidateBrowserSession(ctx)
	if s.browserSession != nil {
		t.Fatal("idle revoked context survived monitor tick")
	}
	if s.browserLive(ctx, connection.PluginID, connection.ID) {
		t.Fatal("credential edit kept session alive")
	}
	if browser.calls[len(browser.calls)-1] != "session/close" {
		t.Fatal("revoked context not closed")
	}
}

func TestPluginBrowserHealthWithoutPersistedCookie(t *testing.T) {
	s, actor, runtime, _, connection, _ := resourcePluginServiceFixture(t, "org.ohmycine.browser-health")
	if err := s.db.Model(&models.PluginConnection{}).Where("id = ?", connection.ID).Update("credential_ciphertext", "").Error; err != nil {
		t.Fatal(err)
	}
	s.browser = &browserFixture{}
	ctx := context.Background()
	if _, err := s.ResourceBrowser(ctx, actor, connection.PluginID, connection.ID, "start", BrowserInput{}); err != nil {
		t.Fatal(err)
	}
	runtime.responses["resource.health"] = []byte(`{"status":"healthy","accountName":"fixture"}`)
	if _, err := s.ResourceBrowser(ctx, actor, connection.PluginID, connection.ID, "confirm", BrowserInput{SessionID: "opaque-session"}); err != nil {
		t.Fatal(err)
	}
	if len(runtime.operations) != 1 || runtime.operations[0] != "resource.health" {
		t.Fatal("health not invoked")
	}
}
