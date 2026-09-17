package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/browsercompanion"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/plugins/hostapi"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Only cookies may cross the persistence boundary. DOM, forms, localStorage,
// screenshots and entered passwords are never exported from the browser.
type browserCookie struct {
	Name     string  `json:"name"`
	Value    string  `json:"value"`
	Domain   string  `json:"domain"`
	Path     string  `json:"path"`
	Expires  float64 `json:"expires"`
	HTTPOnly bool    `json:"httpOnly"`
	Secure   bool    `json:"secure"`
	SameSite string  `json:"sameSite"`
}

type browserStateKey struct{}

func (s *PluginRepositoryService) browserStateContext(ctx context.Context, connectionID string) context.Context {
	s.browserMu.Lock()
	defer s.browserMu.Unlock()
	if session := s.browserSession; session != nil && session.ConnectionID == connectionID {
		return context.WithValue(ctx, browserStateKey{}, browserAuthScope{session.PluginID, connectionID, session.ID})
	}
	return ctx
}

func (c browserCookie) httpCookie() *http.Cookie {
	cookie := &http.Cookie{Name: c.Name, Value: c.Value, Domain: c.Domain, Path: c.Path, HttpOnly: c.HTTPOnly, Secure: c.Secure}
	if c.Expires > 0 {
		cookie.Expires = time.Unix(int64(c.Expires), 0)
	}
	return cookie
}

func validateBrowserCookies(origin string, cookies []browserCookie) error {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "https" || len(cookies) > 128 {
		return browserFailure()
	}
	raw, _ := json.Marshal(cookies)
	if len(raw) > 32768 {
		return browserFailure()
	}
	seen := map[string]bool{}
	for _, c := range cookies {
		if c.Name == "" || len(c.Name) > 256 || len(c.Value) > 4096 || c.Domain != u.Hostname() || !strings.HasPrefix(c.Path, "/") || len(c.Path) > 1024 || strings.ContainsAny(c.Path, "\r\n;") || c.httpCookie().Valid() != nil || math.IsNaN(c.Expires) || math.IsInf(c.Expires, 0) || (c.Expires != -1 && (c.Expires < 0 || c.Expires > 253402300799)) || (c.SameSite != "Lax" && c.SameSite != "Strict" && c.SameSite != "None") || seen[c.Name+"\n"+c.Path] {
			return browserFailure()
		}
		for _, b := range []byte(c.Path) {
			if b <= 32 || b == 127 {
				return browserFailure()
			}
		}
		for _, b := range []byte(c.Value) {
			if b < 0x21 || b > 0x7e || b == '"' || b == ',' || b == ';' || b == '\\' {
				return browserFailure()
			}
		}
		seen[c.Name+"\n"+c.Path] = true
	}
	return nil
}

func browserStatePurpose(pluginID, connectionID string) string {
	return "plugin-browser-state:" + pluginID + ":" + connectionID
}

func (s *PluginRepositoryService) browserStateFingerprint(connection models.PluginConnection) (string, error) {
	var installed models.PluginInstallation
	if err := s.db.First(&installed, "plugin_id = ? AND status = ?", connection.PluginID, models.PluginInstallationEnabled).Error; err != nil {
		return "", err
	}
	// Runtime process generations change on restart; the active package and
	// exact connection/credential revision remain the durable authority.
	raw, _ := json.Marshal([]any{connection.ID, connection.PluginID, connection.EntryOrigin, connection.ConfigJSON, connection.Revision, connection.CredentialVersion, connection.CredentialCiphertext, installed.ActivePackageID})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func (s *PluginRepositoryService) prepareBrowserLocked(ctx context.Context, owner uint, pluginID, connectionID string) error {
	if s.browser == nil {
		return browserFailure()
	}
	if s.validBrowserLocked(ctx, pluginID, connectionID) {
		if s.browserSession.Owner != owner && (s.browserSession.Pending != nil || s.browserSession.Authenticating) {
			return appError(CodeConflict, "其他管理员正在登录此连接", nil)
		}
		if s.browserSession.Owner == owner {
			return nil
		}
	}
	if current := s.browserSession; current != nil {
		if current.Authenticating || (current.Owner != owner && current.Pending != nil) {
			return appError(CodeConflict, "浏览器正在被其他管理员用于登录", nil)
		}
		_ = s.browser.Call(ctx, "session/close", map[string]any{"sessionId": current.ID, "identity": current.Identity}, nil)
		clearPending(current)
		s.browserSession = nil
	}
	connection, fingerprint, err := s.browserBinding(pluginID, connectionID)
	if err != nil {
		return err
	}
	cookies := []browserCookie{}
	var state models.PluginBrowserState
	if err := s.db.First(&state, "connection_id = ? AND plugin_id = ?", connectionID, pluginID).Error; err == nil {
		stable, err := s.browserStateFingerprint(connection)
		if err != nil {
			return err
		}
		if stable == state.Fingerprint {
			plaintext, err := s.credentials.Decrypt(browserStatePurpose(pluginID, connectionID), state.Ciphertext)
			if err != nil || json.Unmarshal([]byte(plaintext), &cookies) != nil || validateBrowserCookies(connection.EntryOrigin, cookies) != nil {
				return browserFailure()
			}
		} else {
			if err := s.db.Delete(&models.PluginBrowserState{}, "connection_id = ?", connectionID).Error; err != nil {
				return err
			}
		}
	}
	if len(cookies) == 0 && connection.CredentialCiphertext != "" {
		plain, err := s.credentials.Decrypt(hostapi.CredentialPurpose(pluginID, connectionID, connection.CredentialScope), connection.CredentialCiphertext)
		if err != nil {
			return err
		}
		u, _ := url.Parse(connection.EntryOrigin)
		req := &http.Request{Header: http.Header{"Cookie": {plain}}}
		for _, cookie := range req.Cookies() {
			cookies = append(cookies, browserCookie{Name: cookie.Name, Value: cookie.Value, Domain: u.Hostname(), Path: "/", Expires: -1, Secure: true, HTTPOnly: true, SameSite: "Lax"})
		}
	}
	identity := uuid.NewString()
	var created struct {
		SessionID string    `json:"sessionId"`
		ExpiresAt time.Time `json:"expiresAt"`
	}
	if err := s.browser.Call(ctx, "session/create", map[string]any{"identity": identity, "origin": strings.TrimRight(connection.EntryOrigin, "/"), "cookies": cookies}, &created); err != nil {
		return appError("resource_browser_start_failed", "浏览器启动失败，请在系统设置中查看运行状态", nil)
	}
	if created.SessionID == "" || !created.ExpiresAt.After(time.Now()) || created.ExpiresAt.After(time.Now().Add(31*time.Minute)) {
		return browserFailure()
	}
	s.browserSession = &resourceBrowserSession{ID: created.SessionID, Identity: identity, PluginID: pluginID, ConnectionID: connectionID, Origin: connection.EntryOrigin, Fingerprint: fingerprint, Owner: owner, Expires: created.ExpiresAt}
	return nil
}

// Called only after the Host commits a validated credential capture in this
// explicitly scoped authentication operation. Concurrent config edits cannot
// rebind a browser context by masquerading as a credential refresh.
func (s *PluginRepositoryService) BrowserCredentialCommitted(ctx context.Context, before models.PluginConnection) {
	s.browserMu.Lock()
	defer s.browserMu.Unlock()
	session := s.browserSession
	auth, ok := ctx.Value(browserAuthKey{}).(browserAuthScope)
	if session == nil || !ok || auth.SessionID != session.ID || auth.PluginID != before.PluginID || auth.ConnectionID != before.ID {
		return
	}
	var after models.PluginConnection
	var installation models.PluginInstallation
	if s.db.First(&installation, "plugin_id = ?", before.PluginID).Error != nil || browserBindingFingerprint(before, installation) != session.Fingerprint {
		return
	}
	if s.db.First(&after, "id = ?", before.ID).Error != nil || after.Revision != before.Revision+1 || after.CredentialVersion != before.CredentialVersion+1 || after.ConfigJSON != before.ConfigJSON || after.EntryOrigin != before.EntryOrigin {
		return
	}
	_, fingerprint, err := s.browserBinding(before.PluginID, before.ID)
	if err == nil {
		session.Fingerprint = fingerprint
	}
}

func (s *PluginRepositoryService) saveBrowserState(ctx context.Context, connectionID string) error {
	s.browserMu.Lock()
	defer s.browserMu.Unlock()
	session := s.browserSession
	if session == nil || session.ConnectionID != connectionID {
		return nil
	}
	auth, ok := ctx.Value(browserAuthKey{}).(browserAuthScope)
	if !ok {
		auth, ok = ctx.Value(browserStateKey{}).(browserAuthScope)
	}
	if !ok || auth.PluginID != session.PluginID || auth.ConnectionID != connectionID || auth.SessionID != session.ID {
		return browserFailure()
	}
	if !s.validBrowserLocked(ctx, session.PluginID, connectionID) {
		return browserFailure()
	}
	var output struct {
		Cookies []browserCookie `json:"cookies"`
	}
	if err := s.browser.Call(ctx, "session/cookies", map[string]any{"sessionId": session.ID, "identity": session.Identity}, &output); err != nil {
		return browserFailure()
	}
	if err := validateBrowserCookies(session.Origin, output.Cookies); err != nil {
		return err
	}
	if len(output.Cookies) == 0 {
		return appError("resource_auth_failed", "浏览器未返回可保存的登录 Cookie", nil)
	}
	connection, currentFingerprint, err := s.browserBinding(session.PluginID, connectionID)
	if err != nil {
		return err
	}
	if currentFingerprint != session.Fingerprint {
		return browserFailure()
	}
	fingerprint, err := s.browserStateFingerprint(connection)
	if err != nil {
		return err
	}
	raw, _ := json.Marshal(output.Cookies)
	ciphertext, err := s.credentials.Encrypt(browserStatePurpose(session.PluginID, connectionID), string(raw))
	clear(raw)
	if err != nil {
		return err
	}
	state := models.PluginBrowserState{ConnectionID: connectionID, PluginID: session.PluginID, OwnerID: session.Owner, Fingerprint: fingerprint, Ciphertext: ciphertext, UpdatedAt: time.Now().UTC()}
	return s.db.Transaction(func(tx *gorm.DB) error {
		var current models.PluginConnection
		var installed models.PluginInstallation
		if tx.First(&current, "id = ? AND enabled = ?", connectionID, true).Error != nil || tx.First(&installed, "plugin_id = ? AND status = ?", session.PluginID, models.PluginInstallationEnabled).Error != nil || browserBindingFingerprint(current, installed) != session.Fingerprint {
			return browserFailure()
		}
		return tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "connection_id"}}, DoUpdates: clause.AssignmentColumns([]string{"plugin_id", "owner_id", "fingerprint", "ciphertext", "updated_at"})}).Create(&state).Error
	})
}

func (s *PluginRepositoryService) hasBrowserState(connectionID string) bool {
	if s.browser == nil {
		return false
	}
	var connection models.PluginConnection
	var state models.PluginBrowserState
	if s.db.First(&connection, "id = ? AND enabled = ?", connectionID, true).Error != nil || s.db.First(&state, "connection_id = ?", connectionID).Error != nil {
		return false
	}
	fingerprint, err := s.browserStateFingerprint(connection)
	return err == nil && state.PluginID == connection.PluginID && state.Fingerprint == fingerprint
}

// Cold browser startup is outside the WASM/Host per-request time budget.
func (s *PluginRepositoryService) restoreResourceBrowser(ctx context.Context, connectionID string) error {
	if s.browser == nil {
		return nil
	}
	s.browserMu.Lock()
	defer s.browserMu.Unlock()
	var connection models.PluginConnection
	if s.db.First(&connection, "id = ? AND enabled = ?", connectionID, true).Error != nil {
		return appError(CodeNotFound, "连接不可用", nil)
	}
	if s.validBrowserLocked(ctx, connection.PluginID, connectionID) {
		return nil
	}
	var state models.PluginBrowserState
	if s.db.First(&state, "connection_id = ?", connectionID).Error != nil {
		return nil
	}
	return s.prepareBrowserLocked(ctx, state.OwnerID, connection.PluginID, connectionID)
}

func (s *PluginRepositoryService) BrowserComponent(ctx context.Context, actor Actor, operation string, input BrowserInput) (any, error) {
	permission := authz.PermissionSettingsRead
	if operation == "install" {
		permission = authz.PermissionSettingsUpdate
	}
	if !actor.Can(permission) {
		return nil, appError(CodePermissionDenied, "无权管理浏览器组件", nil)
	}
	if s.browser == nil {
		return map[string]any{"state": "unavailable", "installed": false, "protocol_version": 1}, nil
	}
	if operation == "install" && !input.LicenseAccepted {
		return nil, appError(CodeInvalidRequest, "请先确认浏览器组件上游许可", nil)
	}
	if operation != "status" && operation != "install" {
		return nil, appError(CodeInvalidRequest, "浏览器操作无效", nil)
	}
	var result struct {
		State            string `json:"state"`
		Supported        *bool  `json:"supported"`
		Installed        bool   `json:"installed"`
		RuntimeError     string `json:"runtimeError"`
		TUNFakeIPEnabled bool   `json:"tunFakeIPEnabled"`
		ProtocolVersion  int    `json:"protocolVersion"`
	}
	if err := s.browser.Call(ctx, operation, map[string]bool{"licenseAccepted": input.LicenseAccepted}, &result); err != nil {
		if operation == "status" {
			return map[string]any{"state": "unavailable", "installed": false, "runtime_error": browsercompanion.ErrorCode(err), "protocol_version": 1}, nil
		}
		return nil, browserFailure()
	}
	if result.Supported != nil && !*result.Supported {
		result.State = "platform_unsupported"
	}
	return map[string]any{"state": result.State, "installed": result.Installed, "runtime_error": result.RuntimeError, "protocol_version": result.ProtocolVersion, "supported": result.Supported, "tun_fake_ip_enabled": result.TUNFakeIPEnabled}, nil
}
