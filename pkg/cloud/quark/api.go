package quark

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

const apiOrigin = "https://open-api-drive.quark.cn"
const guestOrigin = "https://drive-pc.quark.cn"
const TVTokenService = "https://api.extscreen.com/quarkdrive/token"
const apiLimit = 4 * 1024 * 1024
const tvClientID = "d3194e61504e493eb6222857bccfed94"
const tvSignKey = "kw2dvtd7p4t3pjl2d9ed9yc8yej8kw2d"
const agentClientID = "third_party_agent"
const agentSignKey = "cf134812e2de4032bd1cb7c3727e84b3"
const agentID = "ohmycine"
const tvUA = "Mozilla/5.0 (Linux; U; Android 13; zh-cn; M2004J7AC Build/UKQ1.231108.001) AppleWebKit/533.1 (KHTML, like Gecko) Mobile Safari/533.1"

// Public application profiles are pinned to the verified OpenList TV profile
// and official @quarkdrive/cli SDK1.0.20. They are not account credentials.
type apiClient struct {
	http   *http.Client
	clock  func() time.Time
	config cloud.Config
	gate   *accountGate
}
type accountGate struct {
	mu    sync.Mutex
	until time.Time
}

var gates sync.Map

func newAPI(config cloud.Config) *apiClient {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DisableCompression = true
	// Disable pooled stale-connection replay, including outcome-unknown writes.
	tr.DisableKeepAlives = true
	gate := &accountGate{}
	if config.ConnectionID != 0 {
		v, _ := gates.LoadOrStore(config.ConnectionID, gate)
		gate = v.(*accountGate)
	}
	return &apiClient{http: &http.Client{Transport: tr, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, clock: time.Now, config: config, gate: gate}
}
func (a *apiClient) call(ctx context.Context, r *http.Request) (raw json.RawMessage, err error) {
	if e := ctx.Err(); e != nil {
		return nil, &requestNotDispatchedError{Cause: e}
	}
	if a.config.BeforeCall != nil {
		if e := a.config.BeforeCall(ctx); e != nil {
			return nil, &requestNotDispatchedError{Cause: e}
		}
	}
	if a.config.OnCallResult != nil {
		defer func() { a.config.OnCallResult(err) }()
	}
	a.gate.mu.Lock()
	until := a.gate.until
	a.gate.mu.Unlock()
	if wait := until.Sub(a.clock()); wait > 0 {
		return nil, &requestNotDispatchedError{Cause: &Failure{Code: CodeLimited, RetryAfter: wait}}
	}
	if e := ctx.Err(); e != nil {
		return nil, &requestNotDispatchedError{Cause: e}
	}
	r.GetBody = nil
	response, e := a.http.Do(r.WithContext(ctx))
	if e != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, failure(CodeUnavailable)
	}
	defer response.Body.Close()
	body, e := io.ReadAll(io.LimitReader(response.Body, apiLimit+1))
	if e != nil {
		return nil, failure(CodeUnavailable)
	}
	if len(body) > apiLimit {
		return nil, failure(CodeInvalid)
	}
	var env struct {
		Status *int `json:"status"`
		Errno  int  `json:"errno"`
	}
	_ = json.Unmarshal(body, &env)
	if response.StatusCode == 429 {
		delay := 60 * time.Second
		if sec, e := strconv.Atoi(response.Header.Get("Retry-After")); e == nil && sec > 0 {
			delay = time.Duration(sec) * time.Second
		} else if t, e := http.ParseTime(response.Header.Get("Retry-After")); e == nil && t.After(a.clock()) {
			delay = t.Sub(a.clock())
		}
		if delay > time.Hour {
			delay = time.Hour
		}
		a.gate.mu.Lock()
		a.gate.until = a.clock().Add(delay)
		a.gate.mu.Unlock()
		return nil, &Failure{Code: CodeLimited, HTTPStatus: 429, RetryAfter: delay}
	}
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		return nil, &Failure{Code: CodeInvalid, HTTPStatus: response.StatusCode}
	}
	if response.StatusCode == 401 || env.Errno == 10001 || env.Errno == 11001 {
		return nil, &Failure{Code: CodeAuth, HTTPStatus: response.StatusCode, Errno: env.Errno}
	}
	if response.StatusCode != 200 {
		return nil, &Failure{Code: CodeUnavailable, HTTPStatus: response.StatusCode, Errno: env.Errno}
	}
	if enc := response.Header.Get("Content-Encoding"); enc != "" && enc != "identity" {
		return nil, failure(CodeInvalid)
	}
	if !json.Valid(body) || len(bytes.TrimSpace(body)) == 0 || bytes.TrimSpace(body)[0] != '{' {
		return nil, failure(CodeInvalid)
	}
	return body, nil
}
func unwrap(raw json.RawMessage) (json.RawMessage, error) {
	var env struct {
		Status *int            `json:"status"`
		Errno  int             `json:"errno"`
		Data   json.RawMessage `json:"data"`
	}
	if json.Unmarshal(raw, &env) != nil || env.Status == nil {
		return nil, failure(CodeInvalid)
	}
	if *env.Status != 0 || env.Errno != 0 {
		return nil, &Failure{Code: CodeUnavailable, HTTPStatus: 200, Errno: env.Errno}
	}
	if len(env.Data) == 0 || string(env.Data) == "null" {
		return nil, failure(CodeInvalid)
	}
	return env.Data, nil
}
func (a *apiClient) tv(ctx context.Context, auth Authorization, path string, extra url.Values) (json.RawMessage, error) {
	if path != "/oauth/authorize" && path != "/oauth/code" && path != "/user" && path != "/file" {
		return nil, failure(CodeInvalid)
	}
	timestamp := strconv.FormatInt(a.clock().UnixMilli(), 10)
	q := tvCommon(auth.DeviceID, timestamp)
	q.Set("access_token", auth.AccessToken)
	for k, v := range extra {
		q[k] = v
	}
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, apiOrigin+path+"?"+q.Encode(), nil)
	if e != nil {
		return nil, failure(CodeInvalid)
	}
	sign(req, tvClientID, tvSignKey, timestamp)
	req.Header.Set("User-Agent", tvUA)
	raw, e := a.call(ctx, req)
	if e != nil {
		return nil, e
	}
	var env struct {
		Status *int `json:"status"`
		Errno  int  `json:"errno"`
	}
	if json.Unmarshal(raw, &env) != nil || env.Status == nil {
		return nil, failure(CodeInvalid)
	}
	if *env.Status != 0 || env.Errno != 0 {
		return nil, &Failure{Code: CodeUnavailable, HTTPStatus: 200, Errno: env.Errno}
	}
	return raw, nil
}
func tvCommon(device, timestamp string) url.Values {
	sum := md5.Sum([]byte(device + timestamp))
	return url.Values{"req_id": {hex.EncodeToString(sum[:])}, "app_ver": {"1.8.2.2"}, "device_id": {device}, "device_brand": {"Xiaomi"}, "platform": {"tv"}, "device_name": {"M2004J7AC"}, "device_model": {"M2004J7AC"}, "build_device": {"M2004J7AC"}, "build_product": {"M2004J7AC"}, "device_gpu": {"Adreno (TM) 550"}, "activity_rect": {"{}"}, "channel": {"GENERAL"}}
}
func sign(r *http.Request, client, key, timestamp string) {
	sum := sha256.Sum256([]byte(r.Method + "&" + r.URL.Path + "&" + timestamp + "&" + key))
	r.Header.Set("x-pan-client-id", client)
	r.Header.Set("x-pan-tm", timestamp)
	r.Header.Set("x-pan-token", hex.EncodeToString(sum[:]))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json")
	r.Header.Set("Accept-Encoding", "identity")
}
func (a *apiClient) agent(ctx context.Context, auth Authorization, method, path string, q url.Values, body any) (json.RawMessage, error) {
	if !strings.HasPrefix(path, "/agent/v1/") && !strings.HasPrefix(path, "/open/v1/") {
		return nil, failure(CodeInvalid)
	}
	if q == nil {
		q = url.Values{}
	}
	q.Set("req_id", randomID())
	q.Set("platform", "web")
	if auth.AccessToken != "" {
		q.Set("access_token", auth.AccessToken)
	}
	if auth.DeviceID != "" {
		q.Set("device_id", auth.DeviceID)
	}
	var encoded []byte
	var e error
	if body != nil {
		encoded, e = json.Marshal(body)
		if e != nil || len(encoded) > 1024*1024 {
			return nil, failure(CodeInvalid)
		}
	}
	req, e := http.NewRequestWithContext(ctx, method, apiOrigin+path+"?"+q.Encode(), bytes.NewReader(encoded))
	if e != nil {
		return nil, failure(CodeInvalid)
	}
	sign(req, agentClientID, agentSignKey, strconv.FormatInt(a.clock().UnixMilli(), 10))
	req.Header.Set("User-Agent", "OhMyCine/Quark")
	if auth.AccessToken != "" {
		req.Header.Set("Authorization", "Bearer "+auth.AccessToken)
		req.Header.Set("X-Agent-ID", agentID)
	}
	raw, e := a.call(ctx, req)
	if e != nil {
		return nil, e
	}
	return unwrap(raw)
}
func (a *apiClient) broker(ctx context.Context, auth Authorization, code string) (string, string, error) {
	if code == "" && !scalar(auth.RefreshToken, 32768) {
		return "", "", failure(CodeAuth)
	}
	body := map[string]string{}
	for k, v := range tvCommon(auth.DeviceID, strconv.FormatInt(a.clock().UnixMilli(), 10)) {
		body[k] = v[0]
	}
	if code != "" {
		body["code"] = code
	} else {
		body["refresh_token"] = auth.RefreshToken
	}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, TVTokenService, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "OhMyCine/Quark")
	raw, e := a.call(ctx, req)
	if e != nil {
		return "", "", e
	}
	var out struct {
		Code *int `json:"code"`
		Data struct {
			Status  *int   `json:"status"`
			Access  string `json:"access_token"`
			Refresh string `json:"refresh_token"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &out) != nil || out.Code == nil || *out.Code != 200 || out.Data.Status == nil || *out.Data.Status != 0 || !scalar(out.Data.Access, 32768) || !scalar(out.Data.Refresh, 32768) {
		return "", "", failure(CodeInvalid)
	}
	return out.Data.Access, out.Data.Refresh, nil
}
func isAuth(err error) bool {
	code, _ := cloud.ErrorInfo(err)
	var f *Failure
	return code == CodeAuth || errors.As(err, &f) && f.Code == CodeAuth
}
