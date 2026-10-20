package services

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	serverlog "github.com/yuanjing-hash/OhMyCine-Server/internal/logging"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/plugins/contract"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/plugins/hostapi"
)

const (
	resourceBrowserRestoreTimeout = 70 * time.Second
	resourceSearchTimeout         = 35 * time.Second
	resourceRateCooldown          = 30 * time.Second
	resourceChallengeSettleDelay  = 300 * time.Millisecond
)

// Entries live only while an operation or waiter holds a reference. Inactive
// connections cannot accumulate an unbounded process-lifetime cache.
type resourceRecoveryState struct {
	mu      sync.Mutex
	entries map[string]*resourceRecoveryEntry
}

type resourceRecoveryEntry struct {
	gate     chan struct{}
	refs     int
	binding  string
	failedAt time.Time
	err      error
}

type resourceBrowserRetryKey struct{}
type resourceReadBindingKey struct{}
type resourceReadBinding struct{ connectionID, fingerprint string }
type resourceBrowserRetryBudget struct {
	mu   sync.Mutex
	used bool
}

func (b *resourceBrowserRetryBudget) take() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.used {
		return false
	}
	b.used = true
	return true
}

func (s *PluginRepositoryService) lockResourceRecovery(ctx context.Context, connectionID string) (*resourceRecoveryEntry, func(), error) {
	state := &s.recovery
	state.mu.Lock()
	if state.entries == nil {
		state.entries = make(map[string]*resourceRecoveryEntry)
	}
	entry := state.entries[connectionID]
	if entry == nil {
		entry = &resourceRecoveryEntry{gate: make(chan struct{}, 1)}
		state.entries[connectionID] = entry
	}
	entry.refs++
	state.mu.Unlock()
	drop := func() {
		state.mu.Lock()
		defer state.mu.Unlock()
		entry.refs--
		if entry.refs == 0 {
			delete(state.entries, connectionID)
		}
	}
	select {
	case entry.gate <- struct{}{}:
		return entry, func() { <-entry.gate; drop() }, nil
	case <-ctx.Done():
		drop()
		return nil, nil, ctx.Err()
	}
}

func resourceCooldown(connection models.PluginConnection) error {
	if connection.LastHealthStatus == "rate_limited" && connection.LastHealthCheckedAt != nil &&
		time.Since(*connection.LastHealthCheckedAt) < resourceRateCooldown {
		return appError("resource_rate_limited", "资源站仍在限流冷却中，请稍后重试", nil)
	}
	return nil
}

// PrepareResourceSearch is the provider-neutral recovery entrance used by Site.
// Historical health is an observation, not authority to permanently close the
// entrance. All lifecycle/capability and durable-cookie binding checks remain.
func (s *PluginRepositoryService) PrepareResourceSearch(ctx context.Context, connectionID string) (resultErr error) {
	if _, err := s.resourceManifest(connectionID, contract.CapabilityResourceSearch); err != nil {
		return err
	}
	entry, unlock, err := s.lockResourceRecovery(ctx, connectionID)
	if err != nil {
		return err
	}
	defer unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	manifest, err := s.resourceManifest(connectionID, contract.CapabilityResourceSearch)
	if err != nil {
		return err
	}
	if !manifestHasCapability(manifest, contract.CapabilityResourceResolve) || !manifestHasCapability(manifest, contract.CapabilityResourceHealth) {
		return appError(CodePermissionDenied, "插件未声明资源站搜索所需能力", nil)
	}
	var connection models.PluginConnection
	if err := s.db.First(&connection, "id = ? AND enabled = ?", connectionID, true).Error; err != nil {
		return appError(CodeNotFound, "资源站连接不存在或已停用", err)
	}
	if err := resourceCooldown(connection); err != nil {
		return err
	}
	_, binding, err := s.browserBinding(connection.PluginID, connectionID)
	if err != nil {
		return err
	}
	// Concurrent waiters observe one failed preflight instead of serially
	// hammering the same provider. Entries disappear after the last waiter; no
	// old health tag becomes a process-lifetime admission latch.
	if entry.err != nil && entry.binding == binding && time.Since(entry.failedAt) < 2*time.Second {
		return entry.err
	}
	defer func() {
		if resultErr != nil && ctx.Err() == nil {
			entry.binding, entry.failedAt, entry.err = binding, time.Now(), resultErr
		}
	}()

	s.browserMu.Lock()
	if current := s.browserSession; current != nil && (current.Pending != nil || current.Authenticating) {
		s.browserMu.Unlock()
		return appError("resource_browser_login_pending", "管理员正在完成浏览器登录，请完成或取消后再搜索", nil)
	}
	warm := s.validBrowserLocked(ctx, connection.PluginID, connectionID)
	confirmed := warm && s.browserSession.HealthChecked
	s.browserMu.Unlock()
	// A healthy live context has already passed a real health check. Concurrent
	// callers waiting behind the cold restore reuse that confirmation.
	if confirmed && connection.LastHealthStatus == "healthy" {
		return nil
	}
	if s.browser == nil && connection.LastHealthStatus == "healthy" {
		return nil
	}

	ReportResourceSearchPhase(ctx, "restoring_session")
	if err := s.restoreResourceBrowserBounded(ctx, connectionID); err != nil {
		return err
	}
	ReportResourceSearchPhase(ctx, "checking_session")
	for attempt := 0; attempt < 2; attempt++ {
		health, err := s.resourceHealth(ctx, connectionID)
		if err == nil {
			switch health.Status {
			case "healthy":
				return nil
			case "auth_required":
				return appError("resource_auth_required", "资源站登录已失效，请重新登录", nil)
			case "rate_limited":
				return appError("resource_rate_limited", "资源站请求受到限流，请稍后重试", nil)
			default:
				return appError("resource_entry_unavailable", "当前资源站入口暂时不可用", nil)
			}
		}
		// Initial page readiness is not authenticated evidence. A cold browser may
		// finish an automatic challenge shortly after DOMContentLoaded; confirm it
		// once more without reloading a page or submitting a login form.
		if warm || attempt != 0 || ErrorCode(err) != "resource_browser_verification_required" {
			return err
		}
		timer := time.NewTimer(resourceChallengeSettleDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return appError("resource_entry_unavailable", "当前资源站入口暂时不可用", nil)
}

func (s *PluginRepositoryService) restoreResourceBrowserBounded(ctx context.Context, connectionID string) error {
	started := time.Now()
	restore, cancel := context.WithTimeout(ctx, resourceBrowserRestoreTimeout)
	defer cancel()
	err := s.restoreResourceBrowser(restore, connectionID)
	if ctx.Err() != nil {
		err = ctx.Err()
	} else if errors.Is(restore.Err(), context.DeadlineExceeded) {
		err = appError("resource_browser_restore_timeout", "浏览器会话恢复超时，请稍后重试；尚未确认登录状态", nil)
	}
	if err != nil {
		code := ErrorCode(err)
		if errors.Is(err, context.Canceled) {
			code = "cancelled"
		}
		serverlog.OperationPluginRuntime.Event(s.log.Warn()).Str("plugin_connection_id", connectionID).
			Str("stage", "restoring_session").Str("error_code", code).
			Int64("duration_ms", time.Since(started).Milliseconds()).Msg("资源站浏览器会话恢复未完成")
	}
	return err
}

// Only the two read-only guest operations may retry a definitely lost browser
// context. A single observed POST, scoped login, cancellation, HTTP rejection or
// network failure forbids replay. Restoring happens outside the guest budget.
func (s *PluginRepositoryService) invokeReadOnlyResource(ctx context.Context, connectionID, operation string, request any) ([]byte, context.Context, error) {
	retry, ok := ctx.Value(resourceBrowserRetryKey{}).(*resourceBrowserRetryBudget)
	if !ok {
		retry = &resourceBrowserRetryBudget{}
		ctx = context.WithValue(ctx, resourceBrowserRetryKey{}, retry)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, ctx, err
		}
		if err := s.restoreResourceBrowserBounded(ctx, connectionID); err != nil {
			return nil, ctx, err
		}
		connection, expectedBinding, err := s.browserBindingForHealth(connectionID)
		if err != nil {
			return nil, ctx, err
		}
		scoped := s.browserStateContext(ctx, connectionID)
		scoped = context.WithValue(scoped, resourceReadBindingKey{}, resourceReadBinding{connectionID, expectedBinding})
		diagnostic := &browserDiagnostic{}
		s.browserMu.Lock()
		diagnostic.cold = s.browserSession != nil && s.browserSession.ConnectionID == connectionID && !s.browserSession.HealthChecked
		s.browserMu.Unlock()
		scoped = context.WithValue(scoped, browserDiagnosticKey{}, diagnostic)
		scoped = hostapi.WithHTTPMethodObserver(scoped, func(method string) {
			if method != http.MethodGet {
				diagnostic.mu.Lock()
				diagnostic.nonGET = true
				diagnostic.mu.Unlock()
			}
		})
		budget := 16 * time.Second
		if operation == "resource.search" {
			budget = resourceSearchTimeout
		}
		call, cancel := context.WithTimeout(scoped, budget)
		if operation == "resource.search" {
			ReportResourceSearchPhase(ctx, "querying")
		}
		raw, err := s.InvokePlugin(call, connectionID, operation, request)
		callErr := call.Err()
		cancel()
		if err := ctx.Err(); err != nil {
			return nil, scoped, err
		}
		if _, currentBinding, err := s.browserBinding(connection.PluginID, connectionID); err != nil || currentBinding != expectedBinding {
			return nil, scoped, appError("resource_browser_session_expired", "资源站配置已变化，请重新检查", nil)
		}
		diagnostic.mu.Lock()
		recoverable := diagnostic.sessionLost && !diagnostic.nonGET
		limited := diagnostic.rateLimited
		diagnostic.mu.Unlock()
		if limited {
			return nil, scoped, diagnostic.failure()
		}
		_, authenticating := ctx.Value(browserAuthKey{}).(browserAuthScope)
		if attempt == 0 && recoverable && !authenticating && (operation == "resource.search" || operation == "resource.health") && retry.take() {
			if operation == "resource.search" {
				ReportResourceSearchPhase(ctx, "restoring_session")
				if err := s.restoreResourceBrowserBounded(ctx, connectionID); err != nil {
					return nil, scoped, err
				}
				ReportResourceSearchPhase(ctx, "checking_session")
				health, err := s.resourceHealth(ctx, connectionID)
				if err != nil {
					return nil, scoped, err
				}
				if err := resourceHealthSearchError(health); err != nil {
					return nil, scoped, err
				}
			}
			continue
		}
		if callErr != nil {
			return nil, scoped, appError("resource_browser_request_timeout", "资源站请求超过操作等待时间，请稍后重试；不代表登录失效", nil)
		}
		if err != nil {
			if failure := diagnostic.failure(); failure != nil {
				err = failure
			}
		}
		return raw, scoped, err
	}
	return nil, ctx, browserFailure()
}

func resourceHealthSearchError(health contract.ResourceHealthResponse) error {
	switch health.Status {
	case "healthy":
		return nil
	case "auth_required":
		return appError("resource_auth_required", "资源站登录已失效，请重新登录", nil)
	case "rate_limited":
		return appError("resource_rate_limited", "资源站请求受到限流，请稍后重试", nil)
	default:
		return appError("resource_entry_unavailable", "当前资源站入口暂时不可用", nil)
	}
}

func (s *PluginRepositoryService) browserBindingForHealth(connectionID string) (models.PluginConnection, string, error) {
	var connection models.PluginConnection
	if err := s.db.First(&connection, "id = ? AND enabled = ?", connectionID, true).Error; err != nil {
		return connection, "", appError(CodeNotFound, "资源站连接不存在或已停用", err)
	}
	if err := resourceCooldown(connection); err != nil {
		return connection, "", err
	}
	return s.browserBinding(connection.PluginID, connectionID)
}

func (s *PluginRepositoryService) markBrowserHealthChecked(ctx context.Context, connectionID string) {
	s.browserMu.Lock()
	defer s.browserMu.Unlock()
	session := s.browserSession
	if session == nil || session.ConnectionID != connectionID {
		return
	}
	scope, ok := ctx.Value(browserAuthKey{}).(browserAuthScope)
	if !ok {
		scope, ok = ctx.Value(browserStateKey{}).(browserAuthScope)
	}
	if ok && scope.PluginID == session.PluginID && scope.SessionID == session.ID && s.validBrowserLocked(ctx, session.PluginID, connectionID) {
		session.HealthChecked = true
	}
}

func resourceDiagnosticFailure(diagnostic *browserDiagnostic) error {
	if diagnostic == nil {
		return nil
	}
	return diagnostic.failure()
}

func setResourceParseFailure(ctx context.Context, reason string) {
	if diagnostic, ok := ctx.Value(browserDiagnosticKey{}).(*browserDiagnostic); ok {
		diagnostic.mu.Lock()
		diagnostic.parseReason = reason // Only fixed call-site constants.
		diagnostic.mu.Unlock()
	}
}

func setResourceResultCode(ctx context.Context, err error) {
	if diagnostic, ok := ctx.Value(browserDiagnosticKey{}).(*browserDiagnostic); ok && err != nil {
		diagnostic.mu.Lock()
		diagnostic.resultCode = ErrorCode(err)
		diagnostic.mu.Unlock()
	}
}

func (s *PluginRepositoryService) recordResourceDiagnostic(ctx context.Context, connectionID, operation string, started time.Time) {
	diagnostic, ok := ctx.Value(browserDiagnosticKey{}).(*browserDiagnostic)
	if !ok {
		return
	}
	diagnostic.mu.Lock()
	defer diagnostic.mu.Unlock()
	code := diagnostic.resultCode
	if code == "" && diagnostic.err != nil {
		code = ErrorCode(diagnostic.err)
	}
	event := s.log.Debug()
	if code != "" || diagnostic.parseReason != "" {
		event = s.log.Warn()
	}
	serverlog.OperationPluginRuntime.Event(event).
		Str("plugin_connection_id", connectionID).
		Str("operation", operation).
		Str("error_code", code).
		Str("response_class", diagnostic.responseClass).
		Str("parse_reason", diagnostic.parseReason).
		Bool("cold_session", diagnostic.cold).
		Int("upstream_http_status", diagnostic.httpStatus).
		Int64("duration_ms", time.Since(started).Milliseconds()).
		Msg("资源站只读请求已结束；健康状态仅以实际检查结果为准")
}

// Cleanup alone may outlive caller cancellation; no provider operation or
// credential submission runs under this small independent cleanup allowance.
func (s *PluginRepositoryService) closeBrowserHandle(ctx context.Context, sessionID, identity string) {
	if s.browser == nil {
		return
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	_ = s.browser.Call(cleanup, "session/close", map[string]any{"sessionId": sessionID, "identity": identity}, nil)
}
