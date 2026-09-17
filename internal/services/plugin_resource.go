package services

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/plugins/contract"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/plugins/hostapi"
	"gorm.io/gorm"
)

const (
	pluginResourceKind = "plugin_resource"
	pluginResourceTTL  = 15 * time.Minute
	maxResourceItems   = 200
)

type PluginResourceSearchInput struct {
	ConnectionID string
	Query        string
	Kind         string
	Year         *int
	Page         int
}

type PluginResourceSearchItem struct {
	ID        string
	Title     string
	SizeBytes int64
	Seeders   int
	UpdatedAt *time.Time
	Tags      []string
}

type PluginResourceSearchPage struct {
	Items   []PluginResourceSearchItem
	Page    int
	HasNext bool
}

type PluginResourceResolveInput struct{ ConnectionID, ResourceID string }
type PluginResourceResolveResult struct{ Magnet string }

type PluginResourceBridge interface {
	SearchResource(context.Context, PluginResourceSearchInput) (PluginResourceSearchPage, error)
	ResolveResource(context.Context, PluginResourceResolveInput) (PluginResourceResolveResult, error)
	ResourceProvenance(string) (string, string, error)
}

func (s *PluginRepositoryService) ResourceProvenance(connectionID string) (string, string, error) {
	var connection models.PluginConnection
	if err := s.db.Select("plugin_id").First(&connection, "id = ? AND enabled = ? AND resource_type = ?", connectionID, true, "bt_resource").Error; err != nil {
		return "", "", appError(CodeNotFound, "资源站连接不存在、已停用或类型无效", err)
	}
	var installation models.PluginInstallation
	if err := s.db.Select("active_package_id", "status").First(&installation, "plugin_id = ?", connection.PluginID).Error; err != nil || installation.Status != models.PluginInstallationEnabled {
		return "", "", appError(CodePluginRuntimeUnavailable, "插件当前不可用", err)
	}
	var packageRecord models.PluginPackage
	if err := s.db.Select("version").First(&packageRecord, "id = ? AND plugin_id = ?", installation.ActivePackageID, connection.PluginID).Error; err != nil {
		return "", "", appError(CodePluginRuntimeUnavailable, "插件版本不可用", err)
	}
	return connection.PluginID, packageRecord.Version, nil
}

func (s *PluginRepositoryService) SearchResource(ctx context.Context, input PluginResourceSearchInput) (PluginResourceSearchPage, error) {
	request := contract.ResourceSearchRequest{ConnectionID: input.ConnectionID, Query: input.Query, Kind: input.Kind, Year: input.Year, Page: input.Page}
	if !request.Validate() {
		return PluginResourceSearchPage{}, appError(CodeInvalidRequest, "资源站搜索请求无效", nil)
	}
	manifest, err := s.resourceManifest(input.ConnectionID, contract.CapabilityResourceSearch)
	if err != nil {
		return PluginResourceSearchPage{}, err
	}
	if !manifestHasCapability(manifest, contract.CapabilityResourceResolve) {
		return PluginResourceSearchPage{}, appError(CodePermissionDenied, "插件未声明资源站解析能力", nil)
	}
	if err := s.restoreResourceBrowser(ctx, input.ConnectionID); err != nil {
		return PluginResourceSearchPage{}, err
	}
	ctx = s.browserStateContext(ctx, input.ConnectionID)
	raw, err := s.InvokePlugin(ctx, input.ConnectionID, "resource.search", request)
	if err != nil {
		mapped := mapPluginResourceError(err)
		s.persistResourceFailure(input.ConnectionID, mapped)
		return PluginResourceSearchPage{}, mapped
	}
	var response contract.ResourceSearchResponse
	if err := decodePluginResourceResponse(raw, &response); err != nil {
		s.persistResourceFailure(input.ConnectionID, err)
		return PluginResourceSearchPage{}, err
	}
	wantedPage := input.Page
	if wantedPage == 0 {
		wantedPage = 1
	}
	if response.Page != wantedPage || len(response.Items) > maxResourceItems {
		return PluginResourceSearchPage{}, appError("resource_result_invalid", "插件资源站搜索结果无效", nil)
	}
	result := PluginResourceSearchPage{Page: response.Page, HasNext: response.HasNext, Items: make([]PluginResourceSearchItem, 0, len(response.Items))}
	for _, item := range response.Items {
		if !safeOnlineText(item.ID, 256) || !safeOnlineText(item.Title, 512) || item.SizeBytes < 0 || item.SizeBytes > 8<<40 || item.Seeders < 0 || item.Seeders > 1_000_000 || len(item.Tags) > 16 {
			return PluginResourceSearchPage{}, appError("resource_result_invalid", "插件资源站结果包含无效项目", nil)
		}
		for _, tag := range item.Tags {
			if !safeOnlineText(tag, 64) {
				return PluginResourceSearchPage{}, appError("resource_result_invalid", "插件资源站结果包含无效标签", nil)
			}
		}
		var updated *time.Time
		if parsed, ok := contract.ParseResourceUpdatedAt(item.UpdatedAt); ok {
			updated = &parsed
		}
		result.Items = append(result.Items, PluginResourceSearchItem{ID: item.ID, Title: item.Title, SizeBytes: item.SizeBytes, Seeders: item.Seeders, UpdatedAt: updated, Tags: append([]string(nil), item.Tags...)})
	}
	if err := s.saveBrowserState(ctx, input.ConnectionID); err != nil {
		return PluginResourceSearchPage{}, err
	}
	return result, nil
}

func (s *PluginRepositoryService) ResolveResource(ctx context.Context, input PluginResourceResolveInput) (PluginResourceResolveResult, error) {
	request := contract.ResourceResolveRequest{ConnectionID: input.ConnectionID, ResourceID: input.ResourceID}
	if !request.Validate() {
		return PluginResourceResolveResult{}, appError(CodeInvalidRequest, "资源站资源请求无效", nil)
	}
	if _, err := s.resourceManifest(input.ConnectionID, contract.CapabilityResourceResolve); err != nil {
		return PluginResourceResolveResult{}, err
	}
	if err := s.restoreResourceBrowser(ctx, input.ConnectionID); err != nil {
		return PluginResourceResolveResult{}, err
	}
	ctx = s.browserStateContext(ctx, input.ConnectionID)
	raw, err := s.InvokePlugin(ctx, input.ConnectionID, "resource.resolve", request)
	if err != nil {
		mapped := mapPluginResourceError(err)
		s.persistResourceFailure(input.ConnectionID, mapped)
		return PluginResourceResolveResult{}, mapped
	}
	var response contract.ResourceResolveResponse
	if err := decodePluginResourceResponse(raw, &response); err != nil {
		s.persistResourceFailure(input.ConnectionID, err)
		return PluginResourceResolveResult{}, err
	}
	magnet, ok := contract.NormalizeMagnet(response.Magnet)
	if !ok {
		return PluginResourceResolveResult{}, appError("resource_magnet_invalid", "插件返回的磁力链接无效", nil)
	}
	if err := s.saveBrowserState(ctx, input.ConnectionID); err != nil {
		return PluginResourceResolveResult{}, err
	}
	return PluginResourceResolveResult{Magnet: magnet}, nil
}

func (s *PluginRepositoryService) ResourceHealth(ctx context.Context, pluginID, connectionID string) (contract.ResourceHealthResponse, error) {
	if err := s.requireResourceRoute(pluginID, connectionID); err != nil {
		return contract.ResourceHealthResponse{}, err
	}
	return s.resourceHealth(ctx, connectionID)
}

func (s *PluginRepositoryService) resourceHealth(ctx context.Context, connectionID string) (contract.ResourceHealthResponse, error) {
	if _, err := s.resourceManifest(connectionID, contract.CapabilityResourceHealth); err != nil {
		return contract.ResourceHealthResponse{}, err
	}
	var connection models.PluginConnection
	if err := s.db.Select("credential_mode", "credential_ciphertext").First(&connection, "id = ? AND enabled = ?", connectionID, true).Error; err != nil {
		return contract.ResourceHealthResponse{}, appError(CodeNotFound, "资源站连接不存在或已停用", err)
	}
	var browserConnection models.PluginConnection
	_ = s.db.Select("plugin_id").First(&browserConnection, "id = ?", connectionID).Error
	if connection.CredentialMode != models.PluginCredentialModeNone && strings.TrimSpace(connection.CredentialCiphertext) == "" && !s.browserLive(ctx, browserConnection.PluginID, connectionID) && !s.hasBrowserState(connectionID) {
		response := contract.ResourceHealthResponse{Status: "auth_required"}
		if err := s.persistResourceHealth(connectionID, response); err != nil {
			return contract.ResourceHealthResponse{}, err
		}
		return response, nil
	}
	if err := s.restoreResourceBrowser(ctx, connectionID); err != nil {
		return contract.ResourceHealthResponse{}, err
	}
	ctx = s.browserStateContext(ctx, connectionID)
	diagnostic := &browserDiagnostic{}
	ctx = context.WithValue(ctx, browserDiagnosticKey{}, diagnostic)
	raw, err := s.InvokePlugin(ctx, connectionID, "resource.health", contract.ResourceHealthRequest{ConnectionID: connectionID})
	// A close/configuration edit while the guest was running must not publish
	// health for a different session or allow a stale confirmation to succeed.
	if expected, scoped := ctx.Value(browserAuthKey{}).(browserAuthScope); scoped {
		s.browserMu.Lock()
		valid := s.validBrowserLocked(ctx, expected.PluginID, connectionID) && s.browserSession.ID == expected.SessionID
		s.browserMu.Unlock()
		if !valid {
			return contract.ResourceHealthResponse{}, appError("resource_browser_session_expired", "浏览器会话已变化，请重新确认", nil)
		}
	}
	if err != nil {
		mapped := mapPluginResourceError(err)
		if failure := diagnostic.failure(); failure != nil {
			mapped = failure
		}
		s.persistResourceFailure(connectionID, mapped)
		return contract.ResourceHealthResponse{}, mapped
	}
	var response contract.ResourceHealthResponse
	if err := decodePluginResourceResponse(raw, &response); err != nil {
		if failure := diagnostic.failure(); failure != nil {
			err = failure
		}
		s.persistResourceFailure(connectionID, err)
		return contract.ResourceHealthResponse{}, err
	}
	if !map[string]bool{"healthy": true, "auth_required": true, "unavailable": true, "rate_limited": true}[response.Status] {
		return contract.ResourceHealthResponse{}, appError(CodePluginResponseInvalid, "插件资源站健康响应无效", nil)
	}
	if response.Status != "healthy" {
		if failure := diagnostic.failure(); failure != nil {
			s.persistResourceFailure(connectionID, failure)
			return contract.ResourceHealthResponse{}, failure
		}
	}
	if response.Status == "healthy" {
		if err := s.saveBrowserState(ctx, connectionID); err != nil {
			return contract.ResourceHealthResponse{}, err
		}
	}
	if err := s.persistResourceHealth(connectionID, response); err != nil {
		return contract.ResourceHealthResponse{}, err
	}
	return response, nil
}

func (s *PluginRepositoryService) persistResourceHealth(connectionID string, response contract.ResourceHealthResponse) error {
	status := response.Status
	checkedAt := time.Now().UTC()
	updates := map[string]any{"last_health_status": status, "last_health_error_code": "", "last_health_checked_at": checkedAt, "updated_at": checkedAt}
	if response.AccountName != "" {
		updates["login_account_label"] = response.AccountName
	}
	if status == "auth_required" {
		updates["last_health_error_code"] = "resource_auth_required"
		updates["login_account_label"] = ""
	}
	if status == "rate_limited" {
		updates["last_health_error_code"] = "resource_rate_limited"
	}
	if status == "browser_verification_required" {
		updates["last_health_error_code"] = "resource_browser_verification_required"
	}
	if status == "unavailable" {
		updates["last_health_error_code"] = "resource_entry_unavailable"
	}
	if err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&models.PluginConnection{}).Where("id = ?", connectionID).Updates(updates).Error; err != nil {
			return err
		}
		siteUpdates := map[string]any{"last_health_status": status, "last_health_error_code": updates["last_health_error_code"], "last_health_checked_at": checkedAt, "updated_at": checkedAt}
		if response.AccountName != "" {
			siteUpdates["last_health_username"] = response.AccountName
		} else if status == "auth_required" {
			siteUpdates["last_health_username"] = ""
		}
		return tx.Model(&models.Site{}).Where("plugin_connection_id = ?", connectionID).Updates(siteUpdates).Error
	}); err != nil {
		return appError(CodeInternalError, "资源站健康状态保存失败", err)
	}
	return nil
}

func (s *PluginRepositoryService) LoginResource(ctx context.Context, actor Actor, pluginID string, request contract.ResourceLoginRequest) (result contract.ResourceLoginResponse, resultErr error) {
	s.browserAuthMu.Lock()
	defer s.browserAuthMu.Unlock()
	return s.loginResourceLocked(ctx, actor, pluginID, request)
}

// Caller holds browserAuthMu, including confirmation's health-first continuation.
func (s *PluginRepositoryService) loginResourceLocked(ctx context.Context, actor Actor, pluginID string, request contract.ResourceLoginRequest) (result contract.ResourceLoginResponse, resultErr error) {
	if !actor.Can(authz.PermissionPluginsInstall) {
		return contract.ResourceLoginResponse{}, appError(CodePermissionDenied, "无权登录资源站", nil)
	}
	if !request.Validate() {
		return contract.ResourceLoginResponse{}, appError(CodeInvalidRequest, "资源站登录请求无效", nil)
	}
	if err := s.requireResourceRoute(pluginID, request.ConnectionID); err != nil {
		return contract.ResourceLoginResponse{}, err
	}
	if _, err := s.resourceManifest(request.ConnectionID, contract.CapabilityResourceLogin); err != nil {
		return contract.ResourceLoginResponse{}, err
	}
	if s.browser != nil {
		s.browserMu.Lock()
		if expected, continuing := ctx.Value(browserAuthKey{}).(browserAuthScope); continuing &&
			(!s.validBrowserLocked(ctx, pluginID, request.ConnectionID) || s.browserSession.ID != expected.SessionID || s.browserSession.Owner != actor.User.ID) {
			s.browserMu.Unlock()
			return contract.ResourceLoginResponse{}, appError("resource_browser_session_expired", "浏览器会话已变化，请重新确认", nil)
		}
		err := s.prepareBrowserLocked(ctx, actor.User.ID, pluginID, request.ConnectionID)
		if err != nil {
			s.browserMu.Unlock()
			return contract.ResourceLoginResponse{}, err
		}
		session := s.browserSession
		if session.Owner != actor.User.ID {
			s.browserMu.Unlock()
			return contract.ResourceLoginResponse{}, appError(CodeConflict, "浏览器正在被其他管理员使用", nil)
		}
		clearPending(session)
		session.Authenticating = true
		ctx = context.WithValue(ctx, browserAuthKey{}, browserAuthScope{pluginID, request.ConnectionID, session.ID})
		ctx = hostapi.WithBrowserAuthentication(ctx, pluginID, request.ConnectionID)
		s.browserMu.Unlock()
		defer func() {
			s.browserMu.Lock()
			defer s.browserMu.Unlock()
			if s.browserSession != session {
				return
			}
			session.Authenticating = false
			if ctx.Err() == nil && ErrorCode(resultErr) == "resource_browser_verification_required" {
				session.Pending = &pendingBrowserLogin{Username: request.Username, Password: []byte(request.Password), Expires: time.Now().Add(5 * time.Minute)}
				if session.LastGET != "" {
					_ = s.browser.Call(ctx, "session/navigate", map[string]any{"sessionId": session.ID, "identity": session.Identity, "url": session.LastGET}, nil)
				}
			}
		}()
	}
	raw, err := s.InvokePlugin(ctx, request.ConnectionID, "resource.auth.login", request)
	if err != nil {
		return contract.ResourceLoginResponse{}, mapPluginResourceError(err)
	}
	var response contract.ResourceLoginResponse
	if err := decodePluginResourceResponse(raw, &response); err != nil {
		return contract.ResourceLoginResponse{}, err
	}
	if err := s.validateResourceLoginResponse(response); err != nil {
		return contract.ResourceLoginResponse{}, err
	}
	return s.finalizeResourceLogin(ctx, request.ConnectionID, response)
}

func (s *PluginRepositoryService) SubmitResourceCookie(ctx context.Context, actor Actor, pluginID string, request contract.ResourceCookieRequest) (contract.ResourceLoginResponse, error) {
	s.browserAuthMu.Lock()
	defer s.browserAuthMu.Unlock()
	if !actor.Can(authz.PermissionPluginsInstall) {
		return contract.ResourceLoginResponse{}, appError(CodePermissionDenied, "无权登录资源站", nil)
	}
	if !request.Validate() {
		return contract.ResourceLoginResponse{}, appError(CodeInvalidRequest, "资源站 Cookie 无效", nil)
	}
	if err := s.requireResourceRoute(pluginID, request.ConnectionID); err != nil {
		return contract.ResourceLoginResponse{}, err
	}
	manifest, err := s.resourceManifest(request.ConnectionID, contract.CapabilityResourceCookie)
	if err != nil {
		return contract.ResourceLoginResponse{}, err
	}
	var connection models.PluginConnection
	if err := s.db.First(&connection, "id = ? AND enabled = ?", request.ConnectionID, true).Error; err != nil {
		return contract.ResourceLoginResponse{}, appError(CodeNotFound, "资源站连接不存在或已停用", err)
	}
	if connection.CredentialMode != models.PluginCredentialModeCookie || connection.CredentialScope == "" || !manifestHasCredentialScope(manifest, connection.CredentialScope) {
		return contract.ResourceLoginResponse{}, appError(CodePermissionDenied, "插件连接不支持 Cookie 登录", nil)
	}
	// Give the provider adapter its versioned resource.auth.cookie hook before
	// sealing the candidate. The Host still owns encryption and independently
	// verifies the resulting credential through resource.health below.
	raw, err := s.InvokePlugin(ctx, request.ConnectionID, "resource.auth.cookie", request)
	if err != nil {
		return contract.ResourceLoginResponse{}, mapPluginResourceError(err)
	}
	var cookieResponse contract.ResourceLoginResponse
	if err := decodePluginResourceResponse(raw, &cookieResponse); err != nil {
		return contract.ResourceLoginResponse{}, err
	}
	if err := s.validateResourceLoginResponse(cookieResponse); err != nil {
		return contract.ResourceLoginResponse{}, err
	}
	if cookieResponse.State != "authenticated" {
		return contract.ResourceLoginResponse{}, appError("resource_auth_failed", "资源站拒绝了 Cookie", nil)
	}
	// A pasted Cookie is already inside the trusted Server boundary. Seal it
	// directly after the plugin hook; the following health check is the
	// provider-specific validity test and is authoritative.
	ciphertext, err := s.credentials.Encrypt(hostapi.CredentialPurpose(connection.PluginID, connection.ID, connection.CredentialScope), strings.TrimSpace(request.Cookie))
	if err != nil {
		return contract.ResourceLoginResponse{}, err
	}
	var previousSite models.Site
	if err := s.db.Where("plugin_connection_id = ?", connection.ID).First(&previousSite).Error; err != nil {
		return contract.ResourceLoginResponse{}, appError(CodeNotFound, "资源站连接未注册搜索站点", err)
	}
	now := time.Now().UTC()
	result := s.db.Model(&models.PluginConnection{}).Where("id = ? AND revision = ?", connection.ID, connection.Revision).Updates(map[string]any{"credential_ciphertext": ciphertext, "credential_version": connection.CredentialVersion + 1, "last_health_status": "auth_pending", "last_health_error_code": "", "last_health_checked_at": now, "revision": connection.Revision + 1, "updated_at": now})
	if result.Error != nil {
		return contract.ResourceLoginResponse{}, result.Error
	}
	if result.RowsAffected != 1 {
		return contract.ResourceLoginResponse{}, appError(CodePluginRevisionConflict, "插件连接已变化，请刷新后重试", nil)
	}
	health, healthErr := s.resourceHealth(ctx, connection.ID)
	if healthErr == nil && health.Status == "healthy" {
		return contract.ResourceLoginResponse{State: "authenticated", AccountName: health.AccountName}, nil
	}
	if restoreErr := s.restoreResourceCredential(connection, previousSite); restoreErr != nil {
		return contract.ResourceLoginResponse{}, restoreErr
	}
	if healthErr != nil {
		return contract.ResourceLoginResponse{}, healthErr
	}
	switch health.Status {
	case "auth_required":
		return contract.ResourceLoginResponse{}, appError("resource_auth_failed", "Cookie 已失效或不属于当前入口", nil)
	case "rate_limited":
		return contract.ResourceLoginResponse{}, appError("resource_rate_limited", "资源站请求受到限流，请稍后重试", nil)
	default:
		return contract.ResourceLoginResponse{}, appError("resource_entry_unavailable", "当前资源站入口暂时不可用", nil)
	}
}

func (s *PluginRepositoryService) SubmitResourceCaptcha(ctx context.Context, actor Actor, pluginID string, request contract.ResourceCaptchaRequest) (contract.ResourceLoginResponse, error) {
	s.browserAuthMu.Lock()
	defer s.browserAuthMu.Unlock()
	if !actor.Can(authz.PermissionPluginsInstall) {
		return contract.ResourceLoginResponse{}, appError(CodePermissionDenied, "无权提交验证码", nil)
	}
	if !request.Validate() {
		return contract.ResourceLoginResponse{}, appError(CodeInvalidRequest, "验证码挑战无效", nil)
	}
	if err := s.requireResourceRoute(pluginID, request.ConnectionID); err != nil {
		return contract.ResourceLoginResponse{}, err
	}
	if _, err := s.resourceManifest(request.ConnectionID, contract.CapabilityResourceCaptcha); err != nil {
		return contract.ResourceLoginResponse{}, err
	}
	if s.browser != nil {
		s.browserMu.Lock()
		if !s.validBrowserLocked(ctx, pluginID, request.ConnectionID) || s.browserSession.Owner != actor.User.ID {
			s.browserMu.Unlock()
			return contract.ResourceLoginResponse{}, appError("resource_browser_login_expired", "本次登录已结束，请重新登录", nil)
		}
		ctx = context.WithValue(ctx, browserAuthKey{}, browserAuthScope{pluginID, request.ConnectionID, s.browserSession.ID})
		ctx = hostapi.WithBrowserAuthentication(ctx, pluginID, request.ConnectionID)
		session := s.browserSession
		session.Authenticating = true
		s.browserMu.Unlock()
		defer func() {
			s.browserMu.Lock()
			defer s.browserMu.Unlock()
			if s.browserSession == session {
				session.Authenticating = false
			}
		}()
	}
	raw, err := s.InvokePlugin(ctx, request.ConnectionID, "resource.auth.captcha", request)
	if err != nil {
		return contract.ResourceLoginResponse{}, mapPluginResourceError(err)
	}
	var response contract.ResourceLoginResponse
	if err := decodePluginResourceResponse(raw, &response); err != nil {
		return contract.ResourceLoginResponse{}, err
	}
	if err := s.validateResourceLoginResponse(response); err != nil {
		return contract.ResourceLoginResponse{}, err
	}
	return s.finalizeResourceLogin(ctx, request.ConnectionID, response)
}

func (s *PluginRepositoryService) finalizeResourceLogin(ctx context.Context, connectionID string, response contract.ResourceLoginResponse) (contract.ResourceLoginResponse, error) {
	if response.State != "authenticated" {
		if err := s.persistResourceLoginState(connectionID, response); err != nil {
			return contract.ResourceLoginResponse{}, err
		}
		return response, nil
	}
	health, err := s.resourceHealth(ctx, connectionID)
	if err != nil {
		return contract.ResourceLoginResponse{}, err
	}
	return resourceLoginHealthResult(response, health)
}

func resourceLoginHealthResult(response contract.ResourceLoginResponse, health contract.ResourceHealthResponse) (contract.ResourceLoginResponse, error) {
	switch health.Status {
	case "healthy":
		if health.AccountName != "" {
			response.AccountName = health.AccountName
		}
		return response, nil
	case "auth_required":
		return contract.ResourceLoginResponse{}, appError("resource_auth_failed", "资源站登录未生效，请重新登录", nil)
	case "rate_limited":
		return contract.ResourceLoginResponse{}, appError("resource_rate_limited", "资源站请求受到限流，请稍后重试", nil)
	default:
		return contract.ResourceLoginResponse{}, appError("resource_entry_unavailable", "当前资源站入口暂时不可用", nil)
	}
}

func (s *PluginRepositoryService) persistResourceLoginState(connectionID string, response contract.ResourceLoginResponse) error {
	if response.State != "captcha_required" {
		return nil
	}
	now := time.Now().UTC()
	return s.db.Transaction(func(tx *gorm.DB) error {
		updates := map[string]any{"last_health_status": "auth_pending", "last_health_error_code": "resource_captcha_required", "last_health_checked_at": now, "updated_at": now}
		if err := tx.Model(&models.PluginConnection{}).Where("id = ?", connectionID).Updates(updates).Error; err != nil {
			return err
		}
		return tx.Model(&models.Site{}).Where("plugin_connection_id = ?", connectionID).Updates(updates).Error
	})
}

func (s *PluginRepositoryService) requireResourceRoute(pluginID, connectionID string) error {
	pluginID, connectionID = strings.TrimSpace(pluginID), strings.TrimSpace(connectionID)
	if pluginID == "" || connectionID == "" {
		return appError(CodeInvalidRequest, "资源站连接地址无效", nil)
	}
	var count int64
	if err := s.db.Model(&models.PluginConnection{}).Where("id = ? AND plugin_id = ?", connectionID, pluginID).Count(&count).Error; err != nil {
		return err
	}
	if count != 1 {
		return appError(CodeNotFound, "资源站连接不存在", nil)
	}
	return nil
}

func (s *PluginRepositoryService) restoreResourceCredential(connection models.PluginConnection, site models.Site) error {
	now := time.Now().UTC()
	return s.db.Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&models.PluginConnection{}).
			Where("id = ? AND plugin_id = ? AND revision = ?", connection.ID, connection.PluginID, connection.Revision+1).
			Updates(map[string]any{
				"credential_ciphertext":  connection.CredentialCiphertext,
				"credential_version":     connection.CredentialVersion + 2,
				"last_health_status":     connection.LastHealthStatus,
				"last_health_error_code": connection.LastHealthErrorCode,
				"last_health_checked_at": connection.LastHealthCheckedAt,
				"login_account_label":    connection.LoginAccountLabel,
				"revision":               connection.Revision + 2,
				"updated_at":             now,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return appError(CodePluginRevisionConflict, "插件连接已变化，请刷新后重试", nil)
		}
		return tx.Model(&models.Site{}).Where("id = ? AND plugin_connection_id = ?", site.ID, connection.ID).Updates(map[string]any{
			"last_health_status":     site.LastHealthStatus,
			"last_health_error_code": site.LastHealthErrorCode,
			"last_health_username":   site.LastHealthUsername,
			"last_health_checked_at": site.LastHealthCheckedAt,
			"updated_at":             now,
		}).Error
	})
}

func (s *PluginRepositoryService) persistResourceFailure(connectionID string, err error) {
	status := ""
	switch ErrorCode(err) {
	case "resource_auth_required":
		status = "auth_required"
	case "resource_rate_limited":
		status = "rate_limited"
	case "resource_browser_verification_required":
		status = "browser_verification_required"
	case "resource_entry_unavailable":
		status = "unavailable"
	}
	if status != "" {
		_ = s.persistResourceHealth(connectionID, contract.ResourceHealthResponse{Status: status})
	}
}

func (s *PluginRepositoryService) resourceManifest(connectionID string, capability contract.Capability) (contract.Manifest, error) {
	var connection models.PluginConnection
	if err := s.db.First(&connection, "id = ? AND enabled = ?", connectionID, true).Error; err != nil {
		return contract.Manifest{}, appError(CodeNotFound, "资源站连接不存在或已停用", err)
	}
	if resourceOperationNeedsCredential(capability) && connection.CredentialMode != models.PluginCredentialModeNone && strings.TrimSpace(connection.CredentialCiphertext) == "" && !s.hasBrowserState(connectionID) {
		return contract.Manifest{}, appError("resource_auth_required", "资源站尚未登录，请先完成登录", nil)
	}
	manifest, err := s.installedManifest(connection.PluginID)
	if err != nil {
		return contract.Manifest{}, err
	}
	if !manifestHasCapability(manifest, capability) {
		return contract.Manifest{}, appError(CodePermissionDenied, "插件未声明资源站能力", nil)
	}
	return manifest, nil
}

func resourceOperationNeedsCredential(capability contract.Capability) bool {
	switch capability {
	case contract.CapabilityResourceSearch, contract.CapabilityResourceResolve:
		return true
	default:
		return false
	}
}

func (s *PluginRepositoryService) RegisterPluginResourceSite(tx *gorm.DB, connection models.PluginConnection, manifest contract.Manifest) error {
	if !manifestHasCapability(manifest, contract.CapabilityResourceSearch) || !manifestHasCapability(manifest, contract.CapabilityResourceResolve) {
		return nil
	}
	now := time.Now().UTC()
	status := firstNonEmpty(connection.LastHealthStatus, "unknown")
	siteRecord := models.Site{Name: connection.Name, NameNormalized: "plugin-" + strings.ToLower(connection.ID), Kind: pluginResourceKind, SourceType: "plugin", PluginID: connection.PluginID, PluginConnectionID: connection.ID, BaseURL: connection.EntryOrigin, CredentialCiphertext: "", Enabled: connection.Enabled, Priority: 100, TimeoutSeconds: 15, RateLimitPerMinute: 30, LastHealthStatus: status, LastHealthErrorCode: connection.LastHealthErrorCode, LastHealthCheckedAt: connection.LastHealthCheckedAt, Revision: 1, CreatedAt: now, UpdatedAt: now}
	return tx.Create(&siteRecord).Error
}

func mapPluginResourceError(err error) error {
	if err == nil {
		return nil
	}
	code := ErrorCode(err)
	switch code {
	case "plugin_browser_unavailable":
		return browserFailure()
	case "plugin_http_domain_denied", "plugin_http_private_address_denied", "plugin_http_upstream_unavailable", "plugin_runtime_unavailable", "plugin_runtime_start_timeout":
		return appError("resource_entry_unavailable", "资源站入口暂时不可用", nil)
	case "plugin_http_response_too_large":
		return appError("resource_result_invalid", "资源站响应过大", nil)
	case "plugin_http_timeout", "plugin_http_upstream_timeout":
		return appError("resource_entry_unavailable", "资源站入口暂时不可用", nil)
	default:
		return err
	}
}

// decodePluginResourceResponse handles the common plugin error envelope before
// strict DTO decoding. Plugin text is untrusted and is intentionally discarded;
// only the bounded provider error code selects a Server-owned stable message.
func decodePluginResourceResponse(raw []byte, destination any) error {
	trimmed := bytes.TrimSpace(raw)
	var envelope pluginErrorEnvelope
	if len(trimmed) > 0 && trimmed[0] == '{' && json.Unmarshal(trimmed, &envelope) == nil && envelope.PluginError != nil {
		code := strings.TrimSpace(envelope.PluginError.Code)
		switch code {
		case "not-authenticated":
			return appError("resource_auth_required", "资源站登录已失效，请重新登录", nil)
		case "rate-limited":
			return appError("resource_rate_limited", "资源站请求受到限流，请稍后重试", nil)
		case "browser-verification-required":
			return appError("resource_browser_verification_required", "资源站要求浏览器安全验证，暂时无法确认登录状态；请在当前镜像完成验证。若浏览器已通过但插件仍失败，需要检查站点对服务器请求的限制", nil)
		case "captcha-expired":
			return appError("resource_captcha_expired", "验证码已过期，请重新登录", nil)
		case "captcha-required":
			return appError("resource_captcha_required", "需要完成验证码挑战", nil)
		case "auth-failed":
			return appError("resource_auth_failed", "资源站账号或验证码不正确", nil)
		case "not-found":
			return appError("resource_result_invalid", "资源站资源不存在或已失效", nil)
		case "upstream-unavailable", "timeout":
			return appError("resource_entry_unavailable", "资源站入口暂时不可用", nil)
		default:
			return appError("resource_result_invalid", "插件资源站返回了无法识别的错误", nil)
		}
	}
	if err := strictPluginResponse(trimmed, destination); err != nil {
		return appError("resource_result_invalid", "插件资源站响应无效", err)
	}
	return nil
}

func (s *PluginRepositoryService) validateResourceLoginResponse(response contract.ResourceLoginResponse) error {
	if response.AccountName != "" && !safeOnlineText(response.AccountName, 128) {
		return appError("resource_result_invalid", "插件登录响应无效", nil)
	}
	switch response.State {
	case "authenticated":
		if response.Challenge != nil || response.ErrorCode != "" {
			return appError("resource_result_invalid", "插件登录响应无效", nil)
		}
	case "captcha_required":
		challenge := response.Challenge
		if response.ErrorCode != "" || challenge == nil || !safeOnlineText(challenge.ChallengeID, 256) || !safeOnlineText(challenge.ImageAssetRef, 128) || !safeOnlineText(challenge.Prompt, 256) || challenge.Width < 1 || challenge.Width > 4096 || challenge.Height < 1 || challenge.Height > 4096 || challenge.MaxPoints < 1 || challenge.MaxPoints > 16 {
			return appError("resource_result_invalid", "插件验证码挑战无效", nil)
		}
	case "failed":
		return appError("resource_auth_failed", "资源站登录失败，请检查账号、密码或验证码", nil)
	default:
		return appError("resource_result_invalid", "插件登录响应无效", nil)
	}
	return nil
}

func tokenDigest(token string) string {
	digest := sha256.Sum256([]byte(token))
	return hex.EncodeToString(digest[:])
}

var _ PluginResourceBridge = (*PluginRepositoryService)(nil)
