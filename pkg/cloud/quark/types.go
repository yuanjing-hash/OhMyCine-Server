// Package quark aggregates independently authenticated Quark routes. TV alone
// supplies original playback; management never substitutes a playback route.
package quark

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

type Route string

const (
	RouteTV    Route = "tv"
	RouteAgent Route = "agent"
	RouteWeb   Route = "web"
)
const (
	CodeAuth        = "quark_auth_expired"
	CodeInvalid     = "quark_response_invalid"
	CodeUnavailable = "quark_unavailable"
	CodeLimited     = "quark_rate_limited"
	CodeUnsupported = "quark_capability_unavailable"
	CodeIdentity    = "quark_identity_mismatch"
	CodeIncomplete  = "quark_directory_incomplete"
	CodeProof       = "quark_shared_space_unverified"
	CodePending     = "quark_operation_pending"
)

// Authorization and Credentials are encrypted storage contracts, NOT DTOs.
type Authorization struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	DeviceID         string `json:"device_id"`
	ClientDeviceID   string `json:"client_device_id,omitempty"`
	AccountID        string `json:"account_id,omitempty"`
	Generation       uint64 `json:"generation"`
	IdentityVerified bool   `json:"identity_verified"`
}
type WebAuthorization struct {
	Cookie     string `json:"cookie"`
	AccountID  string `json:"account_id"`
	Generation uint64 `json:"generation"`
}
type Credentials struct {
	Version  int               `json:"version"`
	Revision uint64            `json:"revision"`
	TV       *Authorization    `json:"tv,omitempty"`
	Agent    *Authorization    `json:"agent,omitempty"`
	Web      *WebAuthorization `json:"web,omitempty"`
	Proofs   []SpaceProof      `json:"proofs,omitempty"`
}
type SpaceProof struct {
	Version       int       `json:"version"`
	Generation    uint64    `json:"generation"`
	TVAccount     string    `json:"tv_account"`
	AgentAccount  string    `json:"agent_account"`
	TVRoot        string    `json:"tv_root"`
	AgentRoot     string    `json:"agent_root"`
	Path          string    `json:"path"`
	ReceiptID     string    `json:"receipt_id"`
	ContentSHA256 string    `json:"content_sha256"`
	VerifiedAt    time.Time `json:"verified_at"`
}
type PersistFunc = func(context.Context, uint64, json.RawMessage) error

func NewCredentials() Credentials { return Credentials{Version: 1} }
func ParseCredentials(raw json.RawMessage) (Credentials, error) {
	var c Credentials
	if len(raw) == 0 {
		return NewCredentials(), nil
	}
	if len(raw) > 256*1024 || json.Unmarshal(raw, &c) != nil || c.Version != 1 {
		return c, failure(CodeInvalid)
	}
	for _, a := range []*Authorization{c.TV, c.Agent} {
		if a != nil && (!scalar(a.DeviceID, 4096) || a.AccessToken != "" && !scalar(a.AccessToken, 32768) || a.RefreshToken != "" && !scalar(a.RefreshToken, 32768) || a.IdentityVerified && (!scalar(a.AccountID, 4096) || a.AccessToken == "" || a.Generation == 0)) {
			return Credentials{}, failure(CodeInvalid)
		}
	}
	return c, nil
}
func (c Credentials) Route(r Route) *Authorization {
	if r == RouteTV {
		return c.TV
	}
	if r == RouteAgent {
		return c.Agent
	}
	return nil
}
func (c *Credentials) set(r Route, a *Authorization) {
	if r == RouteTV {
		c.TV = a
	}
	if r == RouteAgent {
		c.Agent = a
	}
}
func cloneCredentials(c Credentials) Credentials {
	raw, _ := json.Marshal(c)
	var out Credentials
	_ = json.Unmarshal(raw, &out)
	return out
}
func (c Credentials) HasProof(tvRoot, agentRoot string) bool {
	if c.TV == nil || c.Agent == nil || !c.TV.IdentityVerified || !c.Agent.IdentityVerified {
		return false
	}
	for _, p := range c.Proofs {
		if p.Version == 1 && p.Generation > 0 && p.TVAccount == c.TV.AccountID && p.AgentAccount == c.Agent.AccountID && p.TVRoot == tvRoot && p.AgentRoot == agentRoot && !p.VerifiedAt.IsZero() && len(p.ContentSHA256) == 64 {
			return true
		}
	}
	return false
}
func scalar(s string, limit int) bool {
	return s != "" && len(s) <= limit && strings.IndexFunc(s, unicode.IsControl) < 0
}
func filename(s string) bool {
	return scalar(s, 4096) && s != "." && s != ".." && !strings.ContainsAny(s, "/\\")
}
func failure(code string) error {
	return cloud.Error(code, code == CodeUnavailable || code == CodeLimited || code == CodePending, nil)
}
func randomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("quark secure random unavailable")
	}
	return hex.EncodeToString(b[:])
}
func accountKey(s string) string {
	h := sha256.Sum256([]byte("quark-account-v1\x00" + s))
	return hex.EncodeToString(h[:12])
}

// QualifyID preserves the opaque route handle and adds an account namespace.
// It must never be decoded by generic consumers or converted between routes.
func QualifyID(route Route, account, raw string) string {
	return fmt.Sprintf("q1:%s:%s:%s", route, accountKey(account), base64.RawURLEncoding.EncodeToString([]byte(raw)))
}
func parseID(id string, route Route, account string, root bool) (string, error) {
	if root && (id == "" || id == "0") {
		return "0", nil
	}
	p := strings.Split(id, ":")
	if len(p) != 4 || p[0] != "q1" || p[1] != string(route) || p[2] != accountKey(account) {
		return "", failure(CodeIdentity)
	}
	b, e := base64.RawURLEncoding.DecodeString(p[3])
	if e != nil || !scalar(string(b), 4096) || base64.RawURLEncoding.EncodeToString(b) != p[3] {
		return "", failure(CodeIdentity)
	}
	return string(b), nil
}

type numericID string

func (s *numericID) UnmarshalJSON(b []byte) error {
	var v string
	if len(b) > 0 && b[0] == '"' {
		if json.Unmarshal(b, &v) != nil {
			return failure(CodeInvalid)
		}
	} else {
		var n json.Number
		if json.Unmarshal(b, &n) != nil {
			return failure(CodeInvalid)
		}
		v = n.String()
		for _, r := range v {
			if r < '0' || r > '9' {
				return failure(CodeInvalid)
			}
		}
	}
	if !scalar(v, 4096) {
		return failure(CodeInvalid)
	}
	*s = numericID(v)
	return nil
}

// Failure contains only allowlisted transport/provider status, never request
// URLs, response text, account handles or secrets. Unwrap yields a safe code.
type Failure struct {
	Code           string
	HTTPStatus     int
	Errno          int
	RetryAfter     time.Duration
	UnknownOutcome bool
	SubmitReason   string
}

func (e *Failure) Error() string { return e.Code }
func (e *Failure) Unwrap() error { return failure(e.Code) }
