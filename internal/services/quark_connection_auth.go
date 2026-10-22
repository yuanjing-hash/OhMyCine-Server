package services

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
)

type quarkAuthenticator interface {
	Start(context.Context, quark.Route, quark.Credentials) (quark.Challenge, error)
	Poll(context.Context, quark.Challenge, quark.Credentials, quark.PersistFunc) (quark.Credentials, bool, error)
	Confirm(context.Context, quark.Route, quark.Credentials, quark.PersistFunc) (quark.Credentials, error)
}
type quarkAuthSession struct {
	mu                    sync.Mutex
	ownerID               uint
	connectionID          uint
	configurationRevision uint64
	challenge             quark.Challenge
	state                 string
	errorCode             string
	nextPoll              time.Time
	expires               time.Time
	cancel                context.CancelFunc
}
type QuarkAuthSessionSummary struct {
	SessionID        string    `json:"session_id"`
	Purpose          string    `json:"purpose"`
	State            string    `json:"state"`
	QRDataURL        string    `json:"qr_data_url,omitempty"`
	AuthorizationURL string    `json:"authorization_url,omitempty"`
	ExpiresAt        time.Time `json:"expires_at"`
	PollAfterSeconds int       `json:"poll_after_seconds"`
	ErrorCode        string    `json:"error_code,omitempty"`
	Message          string    `json:"message"`
}

func (s *ConnectionService) quarkAuthClient(id uint, revision uint64, accountReference string) quarkAuthenticator {
	s.quarkAuthMu.Lock()
	defer s.quarkAuthMu.Unlock()
	if s.quarkAuthenticator != nil {
		return s.quarkAuthenticator
	}
	return quark.NewAuthenticator(cloud.Config{ConnectionID: id,
		BeforeCall:   func(ctx context.Context) error { return s.guardQuarkRequest(ctx, id, revision) },
		OnCallResult: func(err error) { RecordQuarkAccountCooldown(s.db, id, accountReference, err) },
		LoadCredentials: func(ctx context.Context) (json.RawMessage, error) {
			record, bundle, err := s.quarkCredentials(ctx, id)
			if err != nil {
				return nil, err
			}
			if !record.Enabled || record.Revision != revision {
				return nil, cloud.Error(cloud.CodeUnavailable, false, nil)
			}
			return json.Marshal(bundle)
		},
	})
}

func quarkPurposeRoute(purpose string) (quark.Route, error) {
	switch purpose {
	case "playback":
		return quark.RouteTV, nil
	case "management":
		return quark.RouteAgent, nil
	default:
		return "", appError(CodeInvalidRequest, "请选择原画播放或文件管理授权", nil)
	}
}
func quarkRoutePurpose(route quark.Route) string {
	if route == quark.RouteTV {
		return "playback"
	}
	return "management"
}

func (s *ConnectionService) StartQuarkAuth(ctx context.Context, actor Actor, id uint, purpose string, request RequestContext) (QuarkAuthSessionSummary, error) {
	if !actor.Can(authz.PermissionConnectionsUpdate) {
		return QuarkAuthSessionSummary{}, appError(CodePermissionDenied, "无权更新连接授权", nil)
	}
	route, err := quarkPurposeRoute(purpose)
	if err != nil {
		return QuarkAuthSessionSummary{}, err
	}
	record, bundle, err := s.quarkCredentials(ctx, id)
	if err != nil {
		return QuarkAuthSessionSummary{}, err
	}
	if !record.Enabled {
		return QuarkAuthSessionSummary{}, appError(CodeConnectionUnavailable, "连接已停用", nil)
	}
	var entropy [24]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return QuarkAuthSessionSummary{}, err
	}
	key := hex.EncodeToString(entropy[:])
	// Reserve before network work so double clicks cannot create extra devices.
	session := &quarkAuthSession{ownerID: actor.User.ID, connectionID: id, configurationRevision: record.Revision, state: "starting", expires: time.Now().Add(5 * time.Minute)}
	s.quarkAuthMu.Lock()
	if s.quarkSessions == nil {
		s.quarkSessions = map[string]*quarkAuthSession{}
	}
	for k, existing := range s.quarkSessions {
		if time.Now().After(existing.expires) {
			delete(s.quarkSessions, k)
			continue
		}
		if existing.connectionID == id {
			existing.mu.Lock()
			finished := existing.state == "complete" || existing.state == "cancelled" || existing.state == "expired"
			existing.mu.Unlock()
			if finished {
				delete(s.quarkSessions, k)
				continue
			}
			s.quarkAuthMu.Unlock()
			return QuarkAuthSessionSummary{}, appError(CodeConflict, "此连接有正在进行的授权，请先完成或取消", nil)
		}
	}
	if len(s.quarkSessions) >= 64 {
		s.quarkAuthMu.Unlock()
		return QuarkAuthSessionSummary{}, appError(CodeConflict, "授权会话过多，请稍后重试", nil)
	}
	s.quarkSessions[key] = session
	s.quarkAuthMu.Unlock()
	callCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	var challenge quark.Challenge
	if saved := bundle.Route(route); bundle.Revision > 0 && saved != nil && saved.AccessToken != "" && !saved.IdentityVerified {
		// Resume a token checkpoint left by a previous process/session. A
		// successful exchange must not be repeated just to retry identity I/O.
		challenge = quark.Challenge{Route: route, DeviceID: saved.DeviceID, ClientDeviceID: saved.ClientDeviceID, Revision: bundle.Revision - 1, ExpiresAt: session.expires}
	} else {
		challenge, err = s.quarkAuthClient(id, record.Revision, record.AccountID).Start(callCtx, route, bundle)
	}
	if err != nil {
		s.removeQuarkSession(key)
		return QuarkAuthSessionSummary{}, quarkServiceError(err)
	}
	if challenge.ExpiresAt.IsZero() || challenge.ExpiresAt.After(session.expires) {
		challenge.ExpiresAt = session.expires
	}
	session.mu.Lock()
	session.challenge = challenge
	session.state = "waiting"
	summary := session.summary(key)
	session.mu.Unlock()
	_ = s.audit.Record(nil, &actor.User.ID, "connection.quark.authorization.start", "connection", uintID(id), "success", map[string]any{"purpose": purpose}, request)
	return summary, nil
}

func (s *ConnectionService) findQuarkSession(actor Actor, id uint, key string) (*quarkAuthSession, error) {
	if !actor.Can(authz.PermissionConnectionsUpdate) {
		return nil, appError(CodePermissionDenied, "无权更新连接授权", nil)
	}
	if len(key) != 48 {
		return nil, appError(CodeNotFound, "授权会话不存在或已结束", nil)
	}
	s.quarkAuthMu.Lock()
	session := s.quarkSessions[key]
	s.quarkAuthMu.Unlock()
	if session == nil || session.ownerID != actor.User.ID || session.connectionID != id {
		return nil, appError(CodeNotFound, "授权会话不存在或已结束", nil)
	}
	return session, nil
}
func (s *ConnectionService) removeQuarkSession(key string) {
	s.quarkAuthMu.Lock()
	delete(s.quarkSessions, key)
	s.quarkAuthMu.Unlock()
}

func (s *ConnectionService) PollQuarkAuth(ctx context.Context, actor Actor, id uint, key string, request RequestContext) (QuarkAuthSessionSummary, error) {
	session, err := s.findQuarkSession(actor, id, key)
	if err != nil {
		return QuarkAuthSessionSummary{}, err
	}
	session.mu.Lock()
	if session.state == "checking" || session.state == "complete" || session.state == "cancelled" {
		result := session.summary(key)
		session.mu.Unlock()
		return result, nil
	}
	if time.Now().After(session.expires) {
		session.state = "expired"
		result := session.summary(key)
		session.mu.Unlock()
		s.removeQuarkSession(key)
		return result, nil
	}
	if session.state == "starting" || time.Now().Before(session.nextPoll) {
		result := session.summary(key)
		session.mu.Unlock()
		return result, nil
	}
	session.state = "checking"
	callCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	session.cancel = cancel
	challenge := session.challenge
	session.mu.Unlock()
	defer cancel()
	record, bundle, err := s.quarkCredentials(callCtx, id)
	complete := false
	if err == nil && (record.Revision != session.configurationRevision || !record.Enabled) {
		err = appError(CodeConflict, "连接配置已变化，请重新授权", nil)
	}
	if err == nil {
		client := s.quarkAuthClient(id, session.configurationRevision, record.AccountID)
		persist := s.quarkCredentialPersister(id, session.configurationRevision)
		saved := bundle.Route(challenge.Route)
		if bundle.Revision > challenge.Revision && saved != nil && saved.AccessToken != "" && !saved.IdentityVerified {
			_, err = client.Confirm(callCtx, challenge.Route, bundle, persist)
			complete = err == nil
		} else {
			challenge.Revision = bundle.Revision
			_, complete, err = client.Poll(callCtx, challenge, bundle, persist)
		}
	}
	session.mu.Lock()
	if session.state != "cancelled" {
		session.state = "waiting"
		session.errorCode = ""
		session.nextPoll = time.Now().Add(3 * time.Second)
		if err != nil {
			session.state = "retryable"
			session.errorCode = quarkSafeErrorCode(err)
		}
		if complete {
			session.state = "complete"
			session.challenge.QR = nil
			session.challenge.AuthorizationURL = ""
		}
	}
	session.cancel = nil
	result := session.summary(key)
	session.mu.Unlock()
	if complete {
		_ = s.audit.Record(nil, &actor.User.ID, "connection.quark.authorization.complete", "connection", uintID(id), "success", map[string]any{"purpose": quarkRoutePurpose(challenge.Route)}, request)
	}
	return result, nil
}

func (s *ConnectionService) CancelQuarkAuth(actor Actor, id uint, key string) error {
	session, err := s.findQuarkSession(actor, id, key)
	if err != nil {
		return err
	}
	session.mu.Lock()
	session.state = "cancelled"
	if session.cancel != nil {
		session.cancel()
	}
	session.challenge.QR = nil
	session.challenge.AuthorizationURL = ""
	session.mu.Unlock()
	s.removeQuarkSession(key)
	return nil
}
func (s *quarkAuthSession) summary(key string) QuarkAuthSessionSummary {
	result := QuarkAuthSessionSummary{SessionID: key, Purpose: quarkRoutePurpose(s.challenge.Route), State: s.state, ExpiresAt: s.expires, PollAfterSeconds: 3, ErrorCode: s.errorCode}
	if s.state != "complete" && s.state != "cancelled" && s.state != "expired" {
		if len(s.challenge.QR) > 0 {
			mime := http.DetectContentType(s.challenge.QR)
			if mime == "image/png" || mime == "image/jpeg" {
				result.QRDataURL = "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(s.challenge.QR)
			}
		}
		u, err := url.Parse(s.challenge.AuthorizationURL)
		if err == nil && u.Scheme == "https" && (u.Hostname() == "quark.cn" || strings.HasSuffix(u.Hostname(), ".quark.cn")) && u.User == nil {
			result.AuthorizationURL = u.String()
		}
	}
	switch s.state {
	case "complete":
		result.Message = "授权已保存并完成账号身份验证"
	case "expired":
		result.Message = "本次授权等待已结束，请重新开始"
	case "cancelled":
		result.Message = "已取消本次授权"
	case "retryable":
		result.Message = quarkErrorMessage(s.errorCode)
	case "checking":
		result.Message = "正在确认授权并保存账号"
	default:
		if len(s.challenge.QR) == 0 {
			result.Message = "正在恢复已保存的授权，无需重新扫码"
		} else {
			result.Message = "请用夸克 App 扫码并确认授权"
		}
	}
	return result
}
func quarkSafeErrorCode(err error) string {
	code, _ := cloud.ErrorInfo(err)
	switch code {
	case quark.CodeAuth, quark.CodeInvalid, quark.CodeUnavailable, quark.CodeLimited, quark.CodeUnsupported, quark.CodeIdentity, quark.CodeProof, quark.CodePending:
		return code
	}
	if ErrorCode(err) == CodeConflict {
		return CodeConflict
	}
	return quark.CodeUnavailable
}
func quarkServiceError(err error) error {
	code := quarkSafeErrorCode(err)
	if code == CodeConflict {
		return appError(CodeConflict, "连接或授权已更新，请重新读取状态", nil)
	}
	return appError(CodeConnectionUnavailable, quarkErrorMessage(code), nil)
}
