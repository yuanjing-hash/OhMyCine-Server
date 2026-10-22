package quark

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/skip2/go-qrcode"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

// Challenge is private session state. Only QR, AuthorizationURL and ExpiresAt
// are projected through an authenticated no-store login DTO. Query/page tokens
// never leave the encrypted owner session. It does not contain account tokens.
type Challenge struct {
	Route            Route     `json:"route"`
	DeviceID         string    `json:"device_id"`
	ClientDeviceID   string    `json:"client_device_id,omitempty"`
	QueryToken       string    `json:"query_token,omitempty"`
	PageCode         string    `json:"page_code,omitempty"`
	QR               []byte    `json:"qr,omitempty"`
	AuthorizationURL string    `json:"authorization_url,omitempty"`
	ExpiresAt        time.Time `json:"expires_at"`
	Revision         uint64    `json:"revision"`
}
type Authenticator struct{ api *apiClient }

func NewAuthenticator(config ...cloud.Config) *Authenticator {
	var c cloud.Config
	if len(config) > 0 {
		c = config[0]
	}
	return &Authenticator{api: newAPI(c)}
}
func (a *Authenticator) Start(ctx context.Context, route Route, previous Credentials) (Challenge, error) {
	c := Challenge{Route: route, DeviceID: randomID(), ClientDeviceID: randomID(), ExpiresAt: a.api.clock().Add(5 * time.Minute), Revision: previous.Revision}
	if old := previous.Route(route); old != nil {
		c.DeviceID = old.DeviceID
		if old.ClientDeviceID != "" {
			c.ClientDeviceID = old.ClientDeviceID
		}
	}
	if route == RouteTV {
		raw, e := a.api.tv(ctx, Authorization{DeviceID: c.DeviceID}, "/oauth/authorize", url.Values{"auth_type": {"code"}, "client_id": {tvClientID}, "scope": {"netdisk"}, "qrcode": {"1"}, "qr_width": {"460"}, "qr_height": {"460"}})
		if e != nil {
			return Challenge{}, e
		}
		var out struct {
			QR    string `json:"qr_data"`
			Query string `json:"query_token"`
		}
		if json.Unmarshal(raw, &out) != nil || !scalar(out.Query, 32768) || len(out.QR) > 1024*1024 {
			return Challenge{}, failure(CodeInvalid)
		}
		b, e := base64.StdEncoding.Strict().DecodeString(out.QR)
		if e != nil || len(b) > 512*1024 {
			return Challenge{}, failure(CodeInvalid)
		}
		ic, format, e := image.DecodeConfig(bytes.NewReader(b))
		if e != nil || format != "png" && format != "jpeg" || ic.Width < 32 || ic.Height < 32 || ic.Width > 2048 || ic.Height > 2048 {
			return Challenge{}, failure(CodeInvalid)
		}
		if _, _, e = image.Decode(bytes.NewReader(b)); e != nil {
			return Challenge{}, failure(CodeInvalid)
		}
		c.QR, c.QueryToken = b, out.Query
		return c, nil
	}
	if route != RouteAgent {
		return Challenge{}, failure(CodeUnsupported)
	}
	body := map[string]string{"client_device_id": c.ClientDeviceID, "device_name": "OhMyCine Server", "agent_id": agentID, "client_id": agentClientID, "work_dir": "ohmycine", "is_cloud_agent": "false", "is_unsure_agent": "true"}
	if old := previous.Agent; old != nil && old.AccountID != "" {
		body["current_user_id"] = old.AccountID
	}
	raw, e := a.api.agent(ctx, Authorization{}, http.MethodPost, "/agent/v1/get_authorize_page_url", nil, body)
	if e != nil {
		return Challenge{}, e
	}
	var out struct {
		URL    string `json:"authorize_page_url"`
		Page   string `json:"page_code"`
		Device string `json:"device_id"`
	}
	if json.Unmarshal(raw, &out) != nil || !scalar(out.Page, 8192) || !scalar(out.Device, 4096) {
		return Challenge{}, failure(CodeInvalid)
	}
	u, e := url.Parse(out.URL)
	if e != nil || u.Scheme != "https" || u.User != nil || u.Opaque != "" || u.Port() != "" && u.Port() != "443" || u.Hostname() != "quark.cn" && !strings.HasSuffix(u.Hostname(), ".quark.cn") || !scalar(out.URL, 16384) || strings.Contains(out.URL, "\\") {
		return Challenge{}, failure(CodeInvalid)
	}
	q := u.Query()
	q.Set("client_id", agentClientID)
	q.Set("scope", "clouddrive.base clouddrive.netdisk")
	q.Set("redirect_uri", "")
	q.Set("client_device_id", c.ClientDeviceID)
	q.Set("device_name", "OhMyCine Server")
	q.Set("agent_id", agentID)
	if q.Get("device_id") == "" {
		q.Set("device_id", out.Device)
	}
	u.RawQuery = q.Encode()
	c.DeviceID, c.PageCode, c.AuthorizationURL = out.Device, out.Page, u.String()
	c.QR, e = qrcode.Encode(c.AuthorizationURL, qrcode.Medium, 512)
	if e != nil {
		return Challenge{}, failure(CodeInvalid)
	}
	return c, nil
}

func (a *Authenticator) Poll(ctx context.Context, ch Challenge, previous Credentials, persist PersistFunc) (Credentials, bool, error) {
	if ch.Revision != previous.Revision {
		return previous, false, failure(CodeAuth)
	}
	lock := credentialLock(a.api.config.ConnectionID)
	lock.Lock()
	defer lock.Unlock()
	current, err := a.latestCredentials(ctx, previous)
	if err != nil {
		return previous, false, err
	}
	before, after := previous.Route(ch.Route), current.Route(ch.Route)
	if before != nil && before.AccountID != "" && (after == nil || after.AccountID != before.AccountID) {
		return current, false, failure(CodeIdentity)
	}
	// Another route or a proof may have advanced the same encrypted bundle
	// while this login waited. Rebase before consuming its one-time code.
	ch.Revision = current.Revision
	return a.pollLocked(ctx, ch, current, persist)
}

func (a *Authenticator) latestCredentials(ctx context.Context, fallback Credentials) (Credentials, error) {
	if err := ctx.Err(); err != nil {
		return fallback, err
	}
	if a.api.config.LoadCredentials == nil {
		return fallback, nil
	}
	raw, err := a.api.config.LoadCredentials(ctx)
	if err != nil {
		return fallback, err
	}
	return ParseCredentials(raw)
}

func (a *Authenticator) pollLocked(ctx context.Context, ch Challenge, previous Credentials, persist PersistFunc) (Credentials, bool, error) {
	if !a.api.clock().Before(ch.ExpiresAt) || ch.Revision != previous.Revision || persist == nil {
		return previous, false, failure(CodeAuth)
	}
	auth := Authorization{DeviceID: ch.DeviceID, ClientDeviceID: ch.ClientDeviceID}
	if old := previous.Route(ch.Route); old != nil {
		auth.AccountID = old.AccountID
		auth.Generation = old.Generation
	}
	if ch.Route == RouteTV {
		raw, e := a.api.tv(ctx, auth, "/oauth/code", url.Values{"client_id": {tvClientID}, "scope": {"netdisk"}, "query_token": {ch.QueryToken}})
		if e != nil {
			return previous, false, e
		}
		var out struct {
			Code string `json:"code"`
		}
		if json.Unmarshal(raw, &out) != nil {
			return previous, false, failure(CodeInvalid)
		}
		if out.Code == "" {
			return previous, false, nil
		}
		if !scalar(out.Code, 32768) {
			return previous, false, failure(CodeInvalid)
		}
		auth.AccessToken, auth.RefreshToken, e = a.api.broker(ctx, auth, out.Code)
		if e != nil {
			return previous, false, e
		}
	} else if ch.Route == RouteAgent {
		raw, e := a.api.agent(ctx, Authorization{}, http.MethodGet, "/agent/v1/oauth/get_aac_by_pagecode", url.Values{"page_code": {ch.PageCode}}, nil)
		if e != nil {
			return previous, false, e
		}
		var out struct {
			Status string `json:"status"`
			Code   string `json:"agent_auth_code"`
		}
		if json.Unmarshal(raw, &out) != nil {
			return previous, false, failure(CodeInvalid)
		}
		if out.Status == "expired" || out.Status == "cancelled" || out.Status == "canceled" || out.Status == "rejected" {
			return previous, false, failure(CodeAuth)
		}
		if out.Status != "success" {
			return previous, false, nil
		}
		if !scalar(out.Code, 16384) {
			return previous, false, failure(CodeInvalid)
		}
		raw, e = a.api.agent(ctx, Authorization{}, http.MethodGet, "/agent/v1/oauth/agent_auth_code", url.Values{"agent_auth_code": {out.Code}, "client_device_id": {ch.ClientDeviceID}, "device_name": {"OhMyCine Server"}, "agent_id": {agentID}, "work_dir": {"ohmycine"}}, nil)
		if e != nil {
			return previous, false, e
		}
		var pair struct {
			Status  string    `json:"status"`
			Access  string    `json:"access_token"`
			Refresh string    `json:"refresh_token"`
			Device  string    `json:"device_id"`
			Account numericID `json:"user_id"`
		}
		if json.Unmarshal(raw, &pair) != nil {
			return previous, false, failure(CodeInvalid)
		}
		if pair.Status == "second_confirmed" {
			old := previous.Agent
			if old == nil || !old.IdentityVerified || old.AccountID != string(pair.Account) {
				return previous, false, failure(CodeAuth)
			}
			id, e := a.identity(ctx, RouteAgent, *old)
			if e != nil {
				return previous, false, e
			}
			if id != old.AccountID {
				return previous, false, failure(CodeIdentity)
			}
			return previous, true, nil
		}
		if !scalar(pair.Access, 32768) || !scalar(pair.Refresh, 32768) || !scalar(pair.Device, 4096) || !scalar(string(pair.Account), 4096) {
			return previous, false, failure(CodeInvalid)
		}
		if auth.AccountID != "" && auth.AccountID != string(pair.Account) {
			return previous, false, failure(CodeIdentity)
		}
		auth.AccessToken, auth.RefreshToken, auth.DeviceID = pair.Access, pair.Refresh, pair.Device
		// Preserve the exchange's independent identity claim through a failed
		// identity probe, including the very first login.
		auth.AccountID = string(pair.Account)
	} else {
		return previous, false, failure(CodeUnsupported)
	}
	next, e := a.checkpointVerify(ctx, ch.Route, previous, auth, persist)
	return next, e == nil, e
}

func (a *Authenticator) Refresh(ctx context.Context, route Route, previous Credentials, persist PersistFunc) (Credentials, error) {
	lock := credentialLock(a.api.config.ConnectionID)
	lock.Lock()
	defer lock.Unlock()
	current, err := a.latestCredentials(ctx, previous)
	if err != nil {
		return previous, err
	}
	old, saved := previous.Route(route), current.Route(route)
	if old != nil && saved != nil && old.Generation != saved.Generation && saved.IdentityVerified {
		if old.AccountID != saved.AccountID {
			return current, failure(CodeIdentity)
		}
		return current, nil
	}
	if saved != nil && !saved.IdentityVerified && scalar(saved.AccessToken, 32768) {
		return a.confirmLocked(ctx, route, current, persist)
	}
	return a.refreshLocked(ctx, route, current, persist)
}

func (a *Authenticator) refreshLocked(ctx context.Context, route Route, previous Credentials, persist PersistFunc) (Credentials, error) {
	old := previous.Route(route)
	if old == nil || !scalar(old.RefreshToken, 32768) || !scalar(old.AccountID, 4096) || persist == nil {
		return previous, failure(CodeAuth)
	}
	auth := *old
	var e error
	if route == RouteTV {
		auth.AccessToken, auth.RefreshToken, e = a.api.broker(ctx, auth, "")
	} else if route == RouteAgent {
		raw, err := a.api.agent(ctx, Authorization{}, http.MethodPost, "/agent/v1/oauth/access_token/rotate", nil, map[string]string{"refresh_token": auth.RefreshToken, "device_id": auth.DeviceID})
		e = err
		if e == nil {
			var pair struct {
				Access  string `json:"access_token"`
				Refresh string `json:"refresh_token"`
			}
			if json.Unmarshal(raw, &pair) != nil || !scalar(pair.Access, 32768) || !scalar(pair.Refresh, 32768) {
				e = failure(CodeInvalid)
			} else {
				auth.AccessToken, auth.RefreshToken = pair.Access, pair.Refresh
			}
		}
	} else {
		return previous, failure(CodeUnsupported)
	}
	if e != nil {
		return previous, e
	}
	return a.checkpointVerify(ctx, route, previous, auth, persist)
}
func (a *Authenticator) checkpointVerify(ctx context.Context, route Route, previous Credentials, auth Authorization, persist PersistFunc) (Credentials, error) {
	next := cloneCredentials(previous)
	auth.Generation++
	auth.IdentityVerified = false
	next.set(route, &auth)
	next.Revision++
	// Token exchange may consume the previous refresh token. The replacement
	// must survive caller cancellation and any later identity/network failure.
	checkpoint, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	raw, _ := json.Marshal(next)
	if e := persist(checkpoint, previous.Revision, raw); e != nil {
		return next, e
	}
	id, e := a.identity(ctx, route, auth)
	if e != nil {
		return next, e
	}
	if auth.AccountID != "" && auth.AccountID != id {
		return next, failure(CodeIdentity)
	}
	auth.AccountID, auth.IdentityVerified = id, true
	verified := cloneCredentials(next)
	verified.set(route, &auth)
	verified.Revision++
	raw, _ = json.Marshal(verified)
	if e = persist(ctx, next.Revision, raw); e != nil {
		return next, e
	}
	return verified, nil
}
func (a *Authenticator) identity(ctx context.Context, route Route, auth Authorization) (string, error) {
	var raw json.RawMessage
	var e error
	if route == RouteTV {
		raw, e = a.api.tv(ctx, auth, "/user", url.Values{"method": {"user_info"}})
		if e == nil {
			raw, e = unwrap(raw)
		}
	} else if route == RouteAgent {
		raw, e = a.api.agent(ctx, auth, http.MethodGet, "/open/v1/user/info", nil, nil)
	} else {
		return "", failure(CodeUnsupported)
	}
	if e != nil {
		return "", e
	}
	var user struct {
		ID numericID `json:"user_id"`
	}
	if json.Unmarshal(raw, &user) != nil || !scalar(string(user.ID), 4096) {
		return "", failure(CodeInvalid)
	}
	return string(user.ID), nil
}

func (a *Authenticator) Verify(ctx context.Context, route Route, c Credentials) error {
	auth := c.Route(route)
	if auth == nil || !auth.IdentityVerified {
		return failure(CodeAuth)
	}
	id, e := a.identity(ctx, route, *auth)
	if e != nil {
		return e
	}
	if id != auth.AccountID {
		return failure(CodeIdentity)
	}
	return nil
}

// Confirm resumes an already persisted token exchange/rotation checkpoint. It
// neither exchanges an authorization code again nor consumes a refresh token.
func (a *Authenticator) Confirm(ctx context.Context, route Route, previous Credentials, persist PersistFunc) (Credentials, error) {
	lock := credentialLock(a.api.config.ConnectionID)
	lock.Lock()
	defer lock.Unlock()
	current, err := a.latestCredentials(ctx, previous)
	if err != nil {
		return previous, err
	}
	return a.confirmLocked(ctx, route, current, persist)
}

func (a *Authenticator) confirmLocked(ctx context.Context, route Route, previous Credentials, persist PersistFunc) (Credentials, error) {
	old := previous.Route(route)
	if old == nil || !scalar(old.AccessToken, 32768) || persist == nil {
		return previous, failure(CodeAuth)
	}
	id, e := a.identity(ctx, route, *old)
	if e != nil {
		return previous, e
	}
	if old.AccountID != "" && old.AccountID != id {
		return previous, failure(CodeIdentity)
	}
	next := cloneCredentials(previous)
	auth := *old
	auth.AccountID, auth.IdentityVerified = id, true
	next.set(route, &auth)
	next.Revision++
	raw, _ := json.Marshal(next)
	if e = persist(ctx, previous.Revision, raw); e != nil {
		return previous, e
	}
	return next, nil
}
