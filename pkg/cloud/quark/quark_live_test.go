//go:build quark_live

package quark

// This explicit acceptance helper imports an existing diagnostic session only
// in memory. It never loads refresh tokens, persists state, or submits writes.
// Its transport permits only reads of the ledger's pre-existing small fixtures.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/credential"
	storagefs "github.com/yuanjing-hash/OhMyCine-Server/internal/storage"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

type liveSession struct {
	Version          int       `json:"version"`
	App              string    `json:"app_fingerprint"`
	Device           string    `json:"device_id"`
	Client           string    `json:"client_device_id"`
	User             string    `json:"user_id"`
	Access           string    `json:"access_token"`
	Generation       uint64    `json:"generation"`
	FirstParty       bool      `json:"first_party_authorization"`
	IdentityVerified bool      `json:"identity_verified"`
	Broker           bool      `json:"broker_authorization"`
	Updated          time.Time `json:"updated_at"`
}
type liveOwned struct {
	ID     string `json:"fid"`
	Name   string `json:"name"`
	Parent string `json:"parent_fid"`
}
type liveUpload struct {
	ID, Name, Parent, SHA256 string
	Size                     int64
}
type liveLedger struct {
	Version       int                   `json:"version"`
	App           string                `json:"app_fingerprint"`
	User          string                `json:"user_id"`
	Device        string                `json:"device_id"`
	Client        string                `json:"client_device_id"`
	Generation    uint64                `json:"generation"`
	SessionAt     time.Time             `json:"session_updated_at"`
	Nonce         string                `json:"nonce"`
	RootName      string                `json:"root_name"`
	Owned         map[string]liveOwned  `json:"owned"`
	Uploads       map[string]liveUpload `json:"uploads"`
	Pending       json.RawMessage       `json:"pending"`
	UploadPending json.RawMessage       `json:"upload_pending"`
}
type liveReadBudget struct {
	mu        sync.Mutex
	count     int
	api       int
	media     int
	agentIDs  map[string]bool
	tvParents map[string]bool
	tvFiles   map[string]bool
	rootPages int
	shapes    []string
	secrets   []string
}
type liveReadTransport struct {
	base   http.RoundTripper
	budget *liveReadBudget
	media  bool
}

func (g liveReadTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	g.budget.mu.Lock()
	allowed := false
	if g.media {
		allowed = r.Method == http.MethodGet && r.URL.Scheme == "https" && r.URL.User == nil && r.Header.Get("Authorization") == "" && r.Header.Get("Cookie") == "" && r.Header.Get("x-pan-token") == "" && g.budget.media < 2
		for _, secret := range g.budget.secrets {
			if secret != "" && (strings.Contains(r.URL.String(), secret) || strings.Contains(r.URL.RawQuery, secret)) {
				allowed = false
			}
		}
	} else if r.URL.Scheme == "https" && r.URL.Host == "open-api-drive.quark.cn" && r.URL.User == nil {
		switch r.URL.Path {
		case "/user":
			allowed = r.Method == http.MethodGet && r.URL.Query().Get("method") == "user_info"
		case "/open/v1/user/info":
			allowed = r.Method == http.MethodGet
		case "/open/v1/file/info":
			allowed = r.Method == http.MethodGet && g.budget.agentIDs[r.URL.Query().Get("fid")]
		case "/open/v1/file/list":
			if r.Method == http.MethodPost && r.Body != nil {
				body, e := io.ReadAll(io.LimitReader(r.Body, 32769))
				_ = r.Body.Close()
				r.Body = io.NopCloser(bytes.NewReader(body))
				var in struct {
					Parent string `json:"parent_fid"`
				}
				allowed = e == nil && len(body) <= 32768 && json.Unmarshal(body, &in) == nil && g.budget.agentIDs[in.Parent] && in.Parent != "0"
			}
		case "/file":
			if r.Method == http.MethodGet {
				switch r.URL.Query().Get("method") {
				case "list":
					p := r.URL.Query().Get("parent_fid")
					allowed = g.budget.tvParents[p]
					if p == "0" {
						allowed = allowed && g.budget.rootPages < 2
						if allowed {
							g.budget.rootPages++
						}
					}
				case "download":
					allowed = g.budget.tvFiles[r.URL.Query().Get("fid")]
				}
			}
		}
	}
	if !allowed || g.budget.count >= 40 {
		g.budget.mu.Unlock()
		return nil, errors.New("live_read_guard_rejected")
	}
	g.budget.count++
	if g.media {
		g.budget.media++
	} else {
		g.budget.api++
	}
	g.budget.mu.Unlock()
	select {
	case <-r.Context().Done():
		return nil, r.Context().Err()
	case <-time.After(500 * time.Millisecond):
	}
	resp, e := g.base.RoundTrip(r)
	if e != nil || g.media {
		return resp, e
	}
	// Only field types and public numeric error codes are retained. Never log
	// field values, URL, IDs, names, tokens, or raw provider response bodies.
	b, e := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
	_ = resp.Body.Close()
	if e != nil || len(b) > 4<<20 {
		return nil, errors.New("live_read_response_invalid")
	}
	resp.Body = io.NopCloser(bytes.NewReader(b))
	var env map[string]json.RawMessage
	_ = json.Unmarshal(b, &env)
	var data map[string]json.RawMessage
	_ = json.Unmarshal(env["data"], &data)
	shape := fmt.Sprintf("path=%s http=%d data=%s", r.URL.Path, resp.StatusCode, liveJSONType(env["data"]))
	for _, k := range []string{"fid", "filename", "file_name", "parent_fid", "file_type", "size", "file_list", "last_page"} {
		shape += " " + k + "=" + liveJSONType(data[k])
	}
	g.budget.mu.Lock()
	g.budget.shapes = append(g.budget.shapes, shape)
	g.budget.mu.Unlock()
	return resp, nil
}
func liveJSONType(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "absent"
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return "invalid"
	}
	switch v.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "bool"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return "invalid"
}

func TestQuarkLiveExistingGeneratedReadOnly(t *testing.T) {
	agentDir, tvDir := os.Getenv("OMC_QUARK_LIVE_AGENT_STATE"), os.Getenv("OMC_QUARK_LIVE_TV_STATE")
	if agentDir == "" || tvDir == "" {
		t.Skip("explicit existing Agent and TV diagnostic state directories required")
	}
	private := map[string][]byte{}
	read := func(p string, max int64) []byte {
		t.Helper()
		for q := p; ; q = filepath.Dir(q) {
			info, e := os.Lstat(q)
			if e != nil || storagefs.IsReparsePoint(q, info) {
				t.Fatal("private path invalid")
			}
			if filepath.Dir(q) == q {
				break
			}
		}
		f, e := os.Open(p)
		if e != nil {
			t.Fatal("private state unavailable")
		}
		defer f.Close()
		info, e := f.Stat()
		if e != nil || !info.Mode().IsRegular() || info.Size() > max {
			t.Fatal("private state invalid")
		}
		b, e := io.ReadAll(io.LimitReader(f, max+1))
		if e != nil || int64(len(b)) > max {
			t.Fatal("private state read failed")
		}
		return b
	}
	defer func() {
		for p, b := range private {
			if !bytes.Equal(b, read(p, 1<<20)) {
				t.Error("existing private state bytes changed")
			}
		}
		t.Logf("private_files_unchanged=%d", len(private))
	}()
	load := func(dir, keyName, name, purpose string, out any) {
		t.Helper()
		kp, p := filepath.Join(dir, keyName), filepath.Join(dir, name)
		key, b := read(kp, 1024), read(p, 1<<20)
		if old, ok := private[kp]; ok && !bytes.Equal(old, key) {
			t.Fatal("private key changed during read")
		}
		private[kp], private[p] = key, b
		cipher, e := credential.Open("", string(key))
		if e != nil {
			t.Fatal("existing private key invalid")
		}
		var env struct {
			Version    int
			Ciphertext string
		}
		if json.Unmarshal(b, &env) != nil || env.Version != 1 {
			t.Fatal("private envelope invalid")
		}
		plain, e := cipher.Decrypt(purpose, env.Ciphertext)
		if e != nil || json.Unmarshal([]byte(plain), out) != nil {
			t.Fatal("private envelope unavailable")
		}
	}
	var agent, tv liveSession
	var ledger liveLedger
	load(agentDir, "credentials.key", "session.enc.json", "quark-original-302-probe:state:v1", &agent)
	load(agentDir, "credentials.key", "capability-ledger.enc.json", "quark-capability-validation:ownership-intent:v1", &ledger)
	load(tvDir, "tv-credentials.key", "tv-session.enc.json", "quark-original-302-probe:tv-session:v1", &tv)
	if agent.Version != 1 || tv.Version != 1 || !agent.FirstParty || !tv.Broker || !tv.IdentityVerified || agent.Access == "" || tv.Access == "" || agent.User == "" || tv.User == "" || agent.Generation == 0 || tv.Generation == 0 {
		t.Fatal("verified existing sessions required")
	}
	if ledger.Version != 1 || ledger.App != agent.App || ledger.User != agent.User || ledger.Device != agent.Device || ledger.Client != agent.Client || ledger.Generation != agent.Generation || !ledger.SessionAt.Equal(agent.Updated) || len(ledger.Nonce) != 32 || ledger.RootName != "OMC-Quark-Validation-20261021-"+ledger.Nonce || ledger.Owned["root"].Name != ledger.RootName || ledger.Owned["source"].Name != "generated-source" || ledger.Owned["source"].Parent != ledger.Owned["root"].ID {
		t.Fatal("private ownership ledger binding invalid")
	}
	if len(ledger.Uploads) != 2 || len(ledger.Pending) > 0 && string(ledger.Pending) != "null" || len(ledger.UploadPending) > 0 && string(ledger.UploadPending) != "null" {
		t.Fatal("known uploads must be confirmed")
	}
	for _, label := range []string{"txt", "srt"} {
		f := ledger.Uploads[label]
		if f.Name != "generated-upload."+label || f.Parent != ledger.Owned["source"].ID || f.Size <= 0 || f.Size > 1024 || len(f.SHA256) != 64 || f.ID == "" {
			t.Fatal("small owned fixture invalid")
		}
	}
	bundle := NewCredentials()
	bundle.Revision = 1
	bundle.TV = &Authorization{AccessToken: tv.Access, DeviceID: tv.Device, AccountID: tv.User, Generation: tv.Generation, IdentityVerified: true}
	bundle.Agent = &Authorization{AccessToken: agent.Access, DeviceID: agent.Device, ClientDeviceID: agent.Client, AccountID: agent.User, Generation: agent.Generation, IdentityVerified: true}
	raw, _ := json.Marshal(bundle)
	d, e := NewClient(cloud.Config{Credentials: raw})
	if e != nil {
		t.Fatal("native client initialization failed")
	}
	budget := &liveReadBudget{agentIDs: map[string]bool{}, tvParents: map[string]bool{"0": true}, tvFiles: map[string]bool{}, secrets: []string{agent.Access, tv.Access}}
	for _, v := range ledger.Owned {
		budget.agentIDs[v.ID] = true
	}
	for _, v := range ledger.Uploads {
		budget.agentIDs[v.ID] = true
	}
	d.api.http.Transport = liveReadTransport{base: d.api.http.Transport, budget: budget}
	d.mediaTransport = liveReadTransport{base: d.mediaTransport, budget: budget, media: true}
	defer func() {
		t.Logf("requests=%d api=%d anonymous=%d", budget.count, budget.api, budget.media)
		if t.Failed() {
			for _, s := range budget.shapes {
				t.Log(s)
			}
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	check := func(stage string, err error) bool {
		t.Helper()
		if err == nil {
			t.Log(stage + "=passed")
			return true
		}
		code, _ := cloud.ErrorInfo(err)
		var f *Failure
		if errors.As(err, &f) {
			t.Errorf("%s code=%s http=%d errno=%d", stage, f.Code, f.HTTPStatus, f.Errno)
		} else {
			t.Errorf("%s code=%s", stage, code)
		}
		return false
	}
	agentOK := check("agent_identity", (&Authenticator{api: d.api}).Verify(ctx, RouteAgent, bundle))
	tvOK := check("tv_identity", (&Authenticator{api: d.api}).Verify(ctx, RouteTV, bundle))
	if agentOK {
		for _, label := range []string{"root", "source"} {
			item, err := d.agentInfo(ctx, ledger.Owned[label].ID)
			if check("agent_"+label+"_info", err) && (item.Name != ledger.Owned[label].Name || !item.Dir) {
				t.Error("agent owned directory metadata mismatch")
			}
		}
		entries, err := d.agentDirectory(ctx, ledger.Owned["source"].ID)
		if check("agent_source_list", err) {
			for _, label := range []string{"txt", "srt"} {
				u := ledger.Uploads[label]
				confirmed, infoErr := d.agentInfo(ctx, u.ID)
				if !check("agent_"+label+"_ack_info", infoErr) {
					continue
				}
				if confirmed.Name != u.Name || confirmed.Dir || confirmed.Size != u.Size || !sameAgent(confirmed.Parent, u.Parent) {
					t.Error("agent authoritative upload info mismatch")
					continue
				}
				n := 0
				for _, f := range entries {
					if f.Name == u.Name {
						n++
						sameDomain := strings.HasPrefix(agentKey(f.ID), "fingerprint:") == strings.HasPrefix(agentKey(u.ID), "fingerprint:")
						t.Logf("fixture=%s list_name_match=true list_type_match=%t list_size_match=%t ack_list_same_domain=%t ack_list_same_identity=%t", label, !f.Dir, f.Size == u.Size, sameDomain, sameAgent(f.ID, u.ID))
						if f.Dir || f.Size != u.Size || sameDomain && !sameAgent(f.ID, u.ID) {
							t.Error("agent confirmed upload membership conflict")
						}
					}
				}
				if n != 1 {
					t.Error("agent confirmed fixture membership mismatch")
				}
			}
		}
	}
	if os.Getenv("OMC_QUARK_LIVE_AGENT_ONLY") == "1" {
		return
	}
	if !tvOK {
		return
	}
	parent := "0"
	for _, name := range []string{ledger.RootName, ledger.Owned["source"].Name} {
		var matched cloud.Item
		n := 0
		complete := false
		for page := int64(0); page < 2; page++ {
			p, err := d.List(ctx, parent, cloud.PageRequest{Limit: 100, Offset: page * 100})
			if !check("tv_known_directory_list", err) {
				return
			}
			for _, f := range p.Items {
				if f.Name == name && f.IsDir {
					matched = f
					n++
				}
			}
			if !p.HasMore {
				complete = true
				break
			}
		}
		if !complete || n != 1 {
			t.Fatal("known TV directory not uniquely confirmed within read budget")
		}
		parent = matched.ID
		id, err := parseID(parent, RouteTV, tv.User, false)
		if err != nil {
			t.Fatal("TV directory namespace invalid")
		}
		budget.tvParents[id] = true
	}
	p, err := d.List(ctx, parent, cloud.PageRequest{Limit: 100})
	if !check("tv_source_list", err) || p.HasMore {
		return
	}
	for _, label := range []string{"txt", "srt"} {
		u := ledger.Uploads[label]
		var matched cloud.Item
		n := 0
		for _, f := range p.Items {
			if f.Name == u.Name && !f.IsDir && f.Size == u.Size {
				matched = f
				n++
			}
		}
		if n != 1 {
			t.Error("small TV fixture metadata mismatch")
			continue
		}
		id, err := parseID(matched.ID, RouteTV, tv.User, false)
		if err != nil {
			t.Fatal("TV file namespace invalid")
		}
		budget.tvFiles[id] = true
		stream, err := d.OpenRead(ctx, cloud.ReadRequest{FileID: matched.ID})
		if !check("tv_"+label+"_original_read", err) {
			continue
		}
		content, err := io.ReadAll(io.LimitReader(stream.Body, 1024))
		_ = stream.Body.Close()
		sum := sha256.Sum256(content)
		if err != nil || int64(len(content)) != u.Size || hex.EncodeToString(sum[:]) != u.SHA256 || stream.TotalSize != nil && *stream.TotalSize != u.Size {
			t.Error("original content did not match the confirmed small upload")
			continue
		}
		t.Logf("fixture=%s bytes=%d exact_content_match=true", label, len(content))
	}
}

func TestQuarkLiveGuardRejectsMutationAndRefresh(t *testing.T) {
	b := &liveReadBudget{agentIDs: map[string]bool{}, tvParents: map[string]bool{}, tvFiles: map[string]bool{}}
	g := liveReadTransport{budget: b}
	for _, path := range []string{"/agent/v1/oauth/access_token/rotate", "/open/v1/file/move", "/oauth/authorize", "/open/v1/file/delete"} {
		r, _ := http.NewRequest(http.MethodPost, apiOrigin+path, nil)
		if _, e := g.RoundTrip(r); e == nil {
			t.Fatal("unsafe request allowed")
		}
	}
	r, _ := http.NewRequest(http.MethodPost, TVTokenService, nil)
	if _, e := g.RoundTrip(r); e == nil {
		t.Fatal("broker allowed")
	}
	if b.count != 0 {
		t.Fatal("rejected calls consumed network budget")
	}
}
