package services

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/browsercompanion"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/plugins/contract"
)

type resourceBrowserSession struct {
	ID, Identity, PluginID, ConnectionID, Origin, Fingerprint string
	Owner                                                     uint
	Expires                                                   time.Time
	Pending                                                   *pendingBrowserLogin
	LastGET                                                   string
	Authenticating                                            bool
}

type browserAuthKey struct{}
type browserDiagnosticKey struct{}
type browserDiagnostic struct {
	mu  sync.Mutex
	err error
}

func (d *browserDiagnostic) failure() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.err
}

type browserAuthScope struct{ PluginID, ConnectionID, SessionID string }
type pendingBrowserLogin struct {
	Username string
	Password []byte
	Expires  time.Time
}

func clearPending(session *resourceBrowserSession) {
	if session != nil && session.Pending != nil {
		clear(session.Pending.Password)
		session.Pending = nil
	}
}

type BrowserInput struct {
	SessionID       string `json:"session_id"`
	LicenseAccepted bool   `json:"license_accepted"`
	Action          string `json:"action"`
	X               int    `json:"x"`
	Y               int    `json:"y"`
	Text            string `json:"text"`
	Key             string `json:"key"`
	DeltaY          int    `json:"delta_y"`
}

func WithPluginBrowser(browser browsercompanion.Caller) PluginServiceOption {
	return func(s *PluginRepositoryService) { s.browser = browser }
}

// MonitorBrowserSession closes idle contexts after configuration/runtime
// revocation, even when no user makes a subsequent browser request.
func (s *PluginRepositoryService) MonitorBrowserSession(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.revalidateBrowserSession(ctx)
		}
	}
}

func (s *PluginRepositoryService) revalidateBrowserSession(ctx context.Context) {
	s.browserMu.Lock()
	defer s.browserMu.Unlock()
	if current := s.browserSession; current != nil {
		if current.Pending != nil && !current.Pending.Expires.After(time.Now()) {
			clearPending(current)
		}
		s.validBrowserLocked(ctx, current.PluginID, current.ConnectionID)
	}
}

func (s *PluginRepositoryService) browserBinding(pluginID, connectionID string) (models.PluginConnection, string, error) {
	if err := s.requireResourceRoute(pluginID, connectionID); err != nil {
		return models.PluginConnection{}, "", err
	}
	if _, err := s.resourceManifest(connectionID, contract.CapabilityResourceHealth); err != nil {
		return models.PluginConnection{}, "", err
	}
	var connection models.PluginConnection
	if err := s.db.First(&connection, "id = ? AND plugin_id = ? AND enabled = ?", connectionID, pluginID, true).Error; err != nil {
		return connection, "", appError(CodeNotFound, "连接不可用", nil)
	}
	var installation models.PluginInstallation
	if err := s.db.First(&installation, "plugin_id = ? AND status = ?", pluginID, models.PluginInstallationEnabled).Error; err != nil {
		return connection, "", appError(CodeNotFound, "插件不可用", nil)
	}
	// Bind credentials, config and exact active runtime. Health writes deliberately
	// do not revoke the session, but mirror/package/credential edits do.
	return connection, browserBindingFingerprint(connection, installation), nil
}

func browserBindingFingerprint(connection models.PluginConnection, installation models.PluginInstallation) string {
	raw, _ := json.Marshal([]any{connection.ID, connection.PluginID, connection.EntryOrigin, connection.ConfigJSON, connection.CredentialCiphertext, connection.CredentialVersion, connection.Revision, installation.ActivePackageID, installation.RuntimeGeneration, installation.Revision})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func browserFailure() error {
	return appError("resource_browser_unavailable", "内置浏览器组件不可用，请检查组件安装状态", nil)
}

func (s *PluginRepositoryService) browserLive(ctx context.Context, pluginID, connectionID string) bool {
	s.browserMu.Lock()
	defer s.browserMu.Unlock()
	return s.validBrowserLocked(ctx, pluginID, connectionID)
}

func (s *PluginRepositoryService) validBrowserLocked(ctx context.Context, pluginID, connectionID string) bool {
	session := s.browserSession
	if session == nil || session.PluginID != pluginID || session.ConnectionID != connectionID {
		return false
	}
	_, fingerprint, err := s.browserBinding(pluginID, connectionID)
	if err == nil && fingerprint == session.Fingerprint && session.Expires.After(time.Now()) {
		return true
	}
	if s.browser != nil {
		_ = s.browser.Call(ctx, "session/close", map[string]any{"sessionId": session.ID, "identity": session.Identity}, nil)
	}
	s.browserSession = nil
	clearPending(session)
	return false
}

func (s *PluginRepositoryService) ResourceBrowser(ctx context.Context, actor Actor, pluginID, connectionID, operation string, input BrowserInput) (any, error) {
	if !actor.Can(authz.PermissionPluginsInstall) {
		return nil, appError(CodePermissionDenied, "无权管理浏览器登录", nil)
	}
	_, _, err := s.browserBinding(pluginID, connectionID)
	if err != nil {
		return nil, err
	}
	if s.browser == nil {
		return nil, browserFailure()
	}
	// Keep lock ordering consistent with LoginResource. Never wait for the auth
	// lock while holding browserMu: guest health re-enters BrowserRequest.
	if operation == "confirm" {
		s.browserAuthMu.Lock()
		defer s.browserAuthMu.Unlock()
	}
	s.browserMu.Lock()
	defer s.browserMu.Unlock()
	switch operation {
	case "status":
		var status struct {
			State           string `json:"state"`
			ProtocolVersion int    `json:"protocolVersion"`
		}
		if err := s.browser.Call(ctx, "status", struct{}{}, &status); err != nil {
			return map[string]any{"state": "unavailable", "protocol_version": 1}, nil
		}
		output := map[string]any{"state": status.State, "protocol_version": status.ProtocolVersion}
		if s.validBrowserLocked(ctx, pluginID, connectionID) && s.browserSession.Owner == actor.User.ID {
			output["session_id"] = s.browserSession.ID
			output["expires_at"] = s.browserSession.Expires
		}
		return output, nil
	case "install":
		return nil, appError(CodePermissionDenied, "请在系统设置中管理浏览器组件", nil)
	case "start":
		if err := s.prepareBrowserLocked(ctx, actor.User.ID, pluginID, connectionID); err != nil {
			return nil, err
		}
		return map[string]any{"session_id": s.browserSession.ID, "expires_at": s.browserSession.Expires}, nil
	}
	if !s.validBrowserLocked(ctx, pluginID, connectionID) || s.browserSession.Owner != actor.User.ID || input.SessionID != s.browserSession.ID {
		return nil, appError("resource_browser_session_expired", "浏览器登录会话不存在或已过期", nil)
	}
	session := s.browserSession
	payload := map[string]any{"sessionId": session.ID, "identity": session.Identity}
	switch operation {
	case "reload":
		// Reload the companion's current fixed-origin GET document, never replay
		// the pending credential request or accept an administrator-supplied URL.
		if err := s.browser.Call(ctx, "session/reload", payload, nil); err != nil {
			if browsercompanion.ErrorCode(err) == "browser_reload_post_denied" {
				return nil, appError("resource_browser_reload_post_denied", "当前页面来自表单提交，为避免重复提交，不能直接重新加载；可更新画面或继续手动验证", nil)
			}
			return nil, appError("resource_browser_reload_failed", "网页重新加载未完成，请检查网络或更新画面查看状态；不代表登录失效", nil)
		}
		return map[string]any{}, nil
	case "snapshot":
		var output struct {
			ImageBase64          string `json:"imageBase64"`
			MimeType             string `json:"mimeType"`
			Width                int    `json:"width"`
			Height               int    `json:"height"`
			NetworkErrorCode     string `json:"networkErrorCode"`
			BlockedResourceCount int    `json:"blockedResourceCount"`
		}
		if err := s.browser.Call(ctx, "session/snapshot", payload, &output); err != nil {
			return nil, browserFailure()
		}
		if output.MimeType != "image/png" || output.Width != 1280 || output.Height != 800 || len(output.ImageBase64) > 4<<20 {
			return nil, browserFailure()
		}
		switch output.NetworkErrorCode {
		case "", "resource_network_denied", "resource_network_timeout", "resource_network_failed", "resource_limit_exceeded", "tun_fake_ip_requires_opt_in":
		default:
			return nil, browserFailure()
		}
		if output.BlockedResourceCount < 0 || output.BlockedResourceCount > 10000 {
			return nil, browserFailure()
		}
		return map[string]any{"image_base64": output.ImageBase64, "mime_type": output.MimeType, "width": output.Width, "height": output.Height, "network_error_code": output.NetworkErrorCode, "blocked_resource_count": output.BlockedResourceCount}, nil
	case "input":
		if len(input.Text) > 4096 || input.X < 0 || input.X >= 1280 || input.Y < 0 || input.Y >= 800 || input.DeltaY < -2000 || input.DeltaY > 2000 {
			return nil, appError(CodeInvalidRequest, "浏览器输入超出范围", nil)
		}
		switch input.Action {
		case "click", "text", "key", "scroll":
		default:
			return nil, appError(CodeInvalidRequest, "浏览器操作无效", nil)
		}
		payload["action"] = input.Action
		payload["x"] = input.X
		payload["y"] = input.Y
		payload["text"] = input.Text
		payload["key"] = input.Key
		payload["deltaY"] = input.DeltaY
		if err := s.browser.Call(ctx, "session/input", payload, nil); err != nil {
			return nil, browserFailure()
		}
		return map[string]any{}, nil
	case "close":
		err := s.browser.Call(ctx, "session/close", payload, nil)
		clearPending(session)
		s.browserSession = nil
		if err != nil {
			return nil, browserFailure()
		}
		return map[string]any{}, nil
	case "confirm":
		// A user may already have signed in inside the page. Prove that identity
		// before considering a password replay, even when its pending TTL expired.
		session.Authenticating = true
		ctx = context.WithValue(ctx, browserAuthKey{}, browserAuthScope{pluginID, connectionID, session.ID})
		s.browserMu.Unlock()
		health, err := s.resourceHealth(ctx, connectionID)
		var output contract.ResourceLoginResponse
		if err == nil {
			output, err = resourceLoginHealthResult(contract.ResourceLoginResponse{State: "authenticated"}, health)
		}
		s.browserMu.Lock()
		session.Authenticating = false
		if !s.validBrowserLocked(ctx, pluginID, connectionID) || s.browserSession != session {
			return nil, appError("resource_browser_session_expired", "浏览器会话已变化，请重新确认", nil)
		}
		if err == nil {
			clearPending(session)
			return output, nil
		}
		pending := session.Pending
		if pending != nil && !pending.Expires.After(time.Now()) {
			clearPending(session)
			if health.Status == "auth_required" {
				return nil, appError("resource_browser_login_expired", "本次密码请求已超时；可在网页中登录后再次确认，或重新输入账号密码", nil)
			}
			return output, err
		}
		// Unknown health, transport errors and verification are not evidence that
		// credentials should be submitted again. Retain the current page/session.
		if health.Status != "auth_required" || pending == nil {
			return output, err
		}
		session.Pending = nil
		s.browserMu.Unlock()
		output, err = s.loginResourceLocked(ctx, actor, pluginID, contract.ResourceLoginRequest{ConnectionID: connectionID, Username: pending.Username, Password: string(pending.Password)})
		clear(pending.Password)
		s.browserMu.Lock()
		if !s.validBrowserLocked(ctx, pluginID, connectionID) || s.browserSession != session {
			return nil, appError("resource_browser_session_expired", "浏览器会话已变化，请重新确认", nil)
		}
		return output, err
	default:
		return nil, appError(CodeInvalidRequest, "浏览器操作无效", nil)
	}
}

// BrowserRequest is invoked only after Host has validated URL, DNS and manifest
// permissions. It never transfers stored Cookie to an external renderer.
func (s *PluginRepositoryService) BrowserRequest(ctx context.Context, pluginID, connectionID, scope string, request *http.Request) (response *http.Response, handled bool, resultErr error) {
	// Preserve only Host-created safe errors in this invocation; a guest may
	// otherwise flatten every transport failure into "upstream-unavailable".
	defer func() {
		if diagnostic, ok := ctx.Value(browserDiagnosticKey{}).(*browserDiagnostic); ok && resultErr != nil {
			var safe error
			switch ErrorCode(resultErr) {
			case "resource_browser_request_failed", "resource_browser_request_timeout", "resource_browser_request_denied", "resource_browser_tun_required":
				safe = resultErr // Created below without an upstream cause.
			default:
				safe = browserFailure()
			}
			diagnostic.mu.Lock()
			diagnostic.err = safe
			diagnostic.mu.Unlock()
		}
	}()
	s.browserMu.Lock()
	defer s.browserMu.Unlock()
	if !s.validBrowserLocked(ctx, pluginID, connectionID) {
		if scope == "" {
			return nil, false, nil
		}
		var state models.PluginBrowserState
		if s.browser == nil || s.db.First(&state, "connection_id = ? AND plugin_id = ?", connectionID, pluginID).Error != nil {
			return nil, false, nil
		}
		if err := s.prepareBrowserLocked(ctx, state.OwnerID, pluginID, connectionID); err != nil {
			return nil, true, err
		}
	}
	connection, _, err := s.browserBinding(pluginID, connectionID)
	if err != nil {
		return nil, true, err
	}
	auth, authOK := ctx.Value(browserAuthKey{}).(browserAuthScope)
	authOK = authOK && auth.PluginID == pluginID && auth.ConnectionID == connectionID && auth.SessionID == s.browserSession.ID
	if (scope == "" && !authOK) || (scope != "" && scope != connection.CredentialScope) {
		return nil, true, browserFailure()
	}
	session := s.browserSession
	if (session.Pending != nil || session.Authenticating) && !authOK {
		return nil, true, browserFailure()
	}
	origin, err := url.Parse(session.Origin)
	if err != nil || !strings.EqualFold(origin.Host, request.URL.Host) || origin.Scheme != request.URL.Scheme {
		return nil, true, browserFailure()
	}
	var reader io.Reader = strings.NewReader("")
	if request.Body != nil {
		reader = request.Body
	}
	body, err := io.ReadAll(io.LimitReader(reader, (64<<10)+1))
	if err != nil || len(body) > 64<<10 {
		return nil, true, browserFailure()
	}
	var output struct {
		Status     int               `json:"status"`
		Headers    map[string]string `json:"headers"`
		BodyBase64 *string           `json:"bodyBase64"`
		Cookies    []browserCookie   `json:"cookies"`
	}
	if authOK && request.Method == http.MethodGet {
		session.LastGET = request.URL.String()
	}
	err = s.browser.Call(ctx, "session/request", map[string]any{"sessionId": session.ID, "identity": session.Identity, "url": request.URL.String(), "method": request.Method, "body": string(body), "contentType": request.Header.Get("Content-Type")}, &output)
	if err != nil {
		switch browsercompanion.ErrorCode(err) {
		case "browser_request_timeout", "network_timeout":
			return nil, true, appError("resource_browser_request_timeout", "浏览器中的站点请求超时；未确认登录状态，请稍后重试", nil)
		case "network_denied", "resource_network_denied":
			return nil, true, appError("resource_browser_request_denied", "浏览器站点请求被网络安全策略拒绝；不代表密码错误", nil)
		case "tun_fake_ip_requires_opt_in":
			return nil, true, appError("resource_browser_tun_required", "浏览器检测到 TUN Fake-IP，请在 Server 部署设置中显式启用兼容选项", nil)
		}
		return nil, true, appError("resource_browser_request_failed", "浏览器中的站点请求失败（可能是网络或不支持的跳转）；不代表账号密码错误", nil)
	}
	if output.Status < 100 || output.Status > 599 || output.BodyBase64 == nil || len(*output.BodyBase64) > base64.StdEncoding.EncodedLen(2<<20) {
		return nil, true, browserFailure()
	}
	headers := http.Header{}
	if len(output.Headers["content-type"]) > 256 || strings.ContainsAny(output.Headers["content-type"], "\r\n") {
		return nil, true, browserFailure()
	}
	headers.Set("Content-Type", output.Headers["content-type"])
	if err := validateBrowserCookies(session.Origin, output.Cookies); err != nil {
		return nil, true, err
	}
	for _, cookie := range output.Cookies {
		headers.Add("Set-Cookie", cookie.httpCookie().String())
	}
	responseBody, err := base64.StdEncoding.Strict().DecodeString(*output.BodyBase64)
	if err != nil || len(responseBody) > 2<<20 || base64.StdEncoding.EncodeToString(responseBody) != *output.BodyBase64 {
		return nil, true, browserFailure()
	}
	return &http.Response{StatusCode: output.Status, Header: headers, Request: request, Body: io.NopCloser(bytes.NewReader(responseBody))}, true, nil
}
