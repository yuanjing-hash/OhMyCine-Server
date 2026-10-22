//go:build quark_live

package quark

// Explicit, bounded acceptance of three new objects in the pre-existing random
// sandbox only. This is never part of ordinary builds. The journal is encrypted
// and every intent is durable before I/O; reruns reuse it instead of new IDs.

import (
	"bytes"
	"context"
	"crypto/rand"
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
	"github.com/yuanjing-hash/OhMyCine-Server/internal/database"
	storagefs "github.com/yuanjing-hash/OhMyCine-Server/internal/storage"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

const liveCreationPurpose = "quark-native-acceptance:creation-journal:v1"

type liveCreationState struct {
	Version                                                                              int
	RootName, RootACK, ProofID, UploadID, DirectoryID, UploadName, DirectoryName         string
	Content                                                                              []byte
	Bundle                                                                               Credentials
	Operations                                                                           map[string]cloud.OperationReceipt
	Requests, API, Reads, ReadBytes, PreWrites, FinishWrites, PutWrites, DirectoryWrites int
	Started                                                                              time.Time
}
type liveCreationStore struct {
	mu     sync.Mutex
	path   string
	cipher *credential.Store
	state  liveCreationState
}

func (s *liveCreationStore) save() error {
	b, e := json.Marshal(s.state)
	if e != nil {
		return errors.New("live_journal_encode")
	}
	env, e := s.cipher.Encrypt(liveCreationPurpose, string(b))
	if e != nil {
		return errors.New("live_journal_encrypt")
	}
	f, e := os.CreateTemp(filepath.Dir(s.path), ".journal-*")
	if e != nil {
		return errors.New("live_journal_create")
	}
	name := f.Name()
	defer os.Remove(name)
	if e = f.Chmod(0600); e == nil {
		_, e = f.WriteString(env)
	}
	if e == nil {
		e = f.Sync()
	}
	closeErr := f.Close()
	if e != nil || closeErr != nil {
		return errors.New("live_journal_write")
	}
	if os.Rename(name, s.path) != nil {
		return errors.New("live_journal_publish")
	}
	return nil
}
func (s *liveCreationStore) Load(_ context.Context, id string) (cloud.OperationReceipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.state.Operations[id]
	if !ok {
		return r, cloud.ErrOperationNotFound
	}
	r.Payload = append(json.RawMessage(nil), r.Payload...)
	return r, nil
}
func (s *liveCreationStore) Create(_ context.Context, r cloud.OperationReceipt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.state.Operations[r.ID]; ok {
		return errors.New("live_receipt_conflict")
	}
	if r.Revision != 1 {
		return errors.New("live_receipt_revision")
	}
	s.state.Operations[r.ID] = r
	return s.save()
}
func (s *liveCreationStore) Update(_ context.Context, expected uint64, r cloud.OperationReceipt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, ok := s.state.Operations[r.ID]
	if !ok || old.Revision != expected || r.Revision != expected+1 || r.Provider != old.Provider || r.Route != old.Route || r.AccountID != old.AccountID || r.Kind != old.Kind {
		return errors.New("live_receipt_conflict")
	}
	s.state.Operations[r.ID] = r
	return s.save()
}
func (s *liveCreationStore) credentials(context.Context) (json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return json.Marshal(s.state.Bundle)
}
func (s *liveCreationStore) persist(_ context.Context, expected uint64, raw json.RawMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, e := ParseCredentials(raw)
	old := s.state.Bundle
	if e != nil || old.Revision != expected || c.Revision != expected+1 || c.TV == nil || c.Agent == nil || old.TV == nil || old.Agent == nil || c.TV.AccessToken != old.TV.AccessToken || c.Agent.AccessToken != old.Agent.AccessToken || c.TV.AccountID != old.TV.AccountID || c.Agent.AccountID != old.Agent.AccountID || c.TV.RefreshToken != "" || c.Agent.RefreshToken != "" {
		return errors.New("live_bundle_conflict")
	}
	s.state.Bundle = c
	return s.save()
}

type liveCreationGuard struct {
	store                                *liveCreationStore
	rootName, rootACK, rootAgent, rootTV string
	agentIDs, tvFiles                    map[string]bool
	readURLs                             map[string]int64
	blocked                              string
	diagnostics                          []string
}
type liveCreationTransport struct {
	base  http.RoundTripper
	guard *liveCreationGuard
	media bool
}

func (g *liveCreationGuard) allowedName(name string) bool {
	s := g.store.state
	if name == s.UploadName || name == s.DirectoryName {
		return true
	}
	r, ok := s.Operations[s.ProofID]
	var p proofPayload
	return ok && json.Unmarshal(r.Payload, &p) == nil && name == p.Name && len(p.Bytes) == 32
}
func (g *liveCreationGuard) uploads() []uploadPayload {
	s := g.store.state
	var out []uploadPayload
	for _, id := range []string{s.ProofID + "/upload", s.UploadID} {
		r, ok := s.Operations[id]
		var p uploadPayload
		if ok && r.Kind == "upload" && json.Unmarshal(r.Payload, &p) == nil {
			out = append(out, p)
		}
	}
	return out
}
func (w liveCreationTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	g := w.guard
	g.store.mu.Lock()
	allowed, kind := false, "read"
	var body []byte
	if r.Body != nil {
		var e error
		body, e = io.ReadAll(io.LimitReader(r.Body, 32769))
		_ = r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(body))
		if e != nil || len(body) > 32768 {
			g.store.mu.Unlock()
			return nil, errors.New("live_body_bound")
		}
	}
	var input map[string]json.RawMessage
	_ = json.Unmarshal(body, &input)
	str := func(k string) string { var v string; _ = json.Unmarshal(input[k], &v); return v }
	if w.media {
		if r.Method == http.MethodGet {
			_, known := g.readURLs[r.URL.String()]
			allowed = known && g.store.state.ReadBytes < 1024 && r.Header.Get("Authorization") == "" && r.Header.Get("Cookie") == "" && r.Header.Get("x-pan-token") == ""
		} else if r.Method == http.MethodPut {
			for _, p := range g.uploads() {
				for _, dest := range p.Destinations {
					h := sha256.Sum256(body)
					if dest.URL == r.URL.String() && p.Phase == "put_inflight" && p.InFlightPart == 1 && p.Size <= 64 && p.Size > 0 && int64(len(body)) == p.Size && hex.EncodeToString(h[:]) == p.SHA256 && r.Header.Get("Authorization") == dest.Signature.Value {
						allowed = true
						kind = "put"
					}
				}
			}
		}
		for _, secret := range []string{g.store.state.Bundle.Agent.AccessToken, g.store.state.Bundle.TV.AccessToken} {
			if strings.Contains(r.URL.String(), secret) || strings.Contains(r.Header.Get("Authorization"), secret) {
				allowed = false
			}
		}
	} else if r.URL.Scheme == "https" && r.URL.Host == "open-api-drive.quark.cn" && r.URL.User == nil {
		switch r.URL.Path {
		case "/user":
			allowed = r.Method == "GET" && r.URL.Query().Get("method") == "user_info"
		case "/open/v1/user/info":
			allowed = r.Method == "GET"
		case "/open/v1/file/info":
			allowed = r.Method == "GET" && (g.agentIDs[r.URL.Query().Get("fid")] || r.URL.Query().Get("fid") == g.rootACK || sameAgent(r.URL.Query().Get("fid"), g.rootAgent))
		case "/open/v1/file/list":
			allowed = r.Method == "POST" && (str("parent_fid") == "0" || str("parent_fid") == g.rootACK || g.rootAgent != "" && sameAgent(str("parent_fid"), g.rootAgent)) && len(input["query_cursor"]) == 0
		case "/file":
			if r.Method == "GET" {
				switch r.URL.Query().Get("method") {
				case "list":
					p := r.URL.Query().Get("parent_fid")
					allowed = (p == "0" || g.rootTV != "" && p == g.rootTV) && r.URL.Query().Get("page_index") == "0" && r.URL.Query().Get("page_size") == "100"
				case "download":
					allowed = g.tvFiles[r.URL.Query().Get("fid")]
				}
			}
		case "/open/v1/file/upload_pre":
			for _, p := range g.uploads() {
				var size int64
				_ = json.Unmarshal(input["size"], &size)
				if r.Method == "POST" && p.Phase == "pre_inflight" && g.rootAgent != "" && sameAgent(p.Parent, g.rootAgent) && str("pdir_fid") == p.Parent && str("file_name") == p.Name && size == p.Size && size > 0 && size <= 64 && str("sha1") == p.SHA1 && str("md5") == p.MD5 && g.allowedName(p.Name) {
					allowed = true
					kind = "pre"
				}
			}
		case "/open/v1/file/upload_finish":
			for _, p := range g.uploads() {
				if r.Method == "POST" && p.Phase == "finish_inflight" && p.TaskID != "" && str("task_id") == p.TaskID {
					allowed = true
					kind = "finish"
				}
			}
		case "/open/v1/dir":
			s := g.store.state
			receipt, ok := s.Operations[s.DirectoryID]
			var p operationPayload
			allowed = r.Method == "POST" && ok && receipt.Kind == "mkdir" && receipt.State == "submitting" && json.Unmarshal(receipt.Payload, &p) == nil && p.Request.Name == s.DirectoryName && str("dir_path") == s.DirectoryName && g.rootAgent != "" && sameAgent(str("pdir_fid"), g.rootAgent)
			kind = "mkdir"
		}
	}
	s := &g.store.state
	if !allowed || s.Requests >= 100 || !s.Started.IsZero() && time.Since(s.Started) > 10*time.Minute || kind == "pre" && s.PreWrites >= 2 || kind == "put" && s.PutWrites >= 2 || kind == "finish" && s.FinishWrites >= 2 || kind == "mkdir" && s.DirectoryWrites >= 1 {
		g.blocked = r.Method + " " + r.URL.Path
		g.store.mu.Unlock()
		return nil, errors.New("live_creation_guard_rejected")
	}
	if s.Started.IsZero() {
		s.Started = time.Now().UTC()
	}
	s.Requests++
	if !w.media {
		s.API++
	} else if kind == "read" {
		s.Reads++
	}
	switch kind {
	case "pre":
		s.PreWrites++
	case "put":
		s.PutWrites++
	case "finish":
		s.FinishWrites++
	case "mkdir":
		s.DirectoryWrites++
	}
	if e := g.store.save(); e != nil {
		g.store.mu.Unlock()
		return nil, e
	}
	g.store.mu.Unlock()
	select {
	case <-r.Context().Done():
		return nil, r.Context().Err()
	case <-time.After(500 * time.Millisecond):
	}
	resp, e := w.base.RoundTrip(r)
	if e != nil {
		return nil, errors.New("live_transport_failed")
	}
	max := int64(4 << 20)
	if w.media && kind == "read" {
		max = 64
	}
	b, e := io.ReadAll(io.LimitReader(resp.Body, max+1))
	_ = resp.Body.Close()
	if e != nil || int64(len(b)) > max {
		return nil, errors.New("live_response_bound")
	}
	resp.Body = io.NopCloser(bytes.NewReader(b))
	g.store.mu.Lock()
	defer g.store.mu.Unlock()
	if w.media && kind == "read" {
		g.store.state.ReadBytes += len(b)
		if g.store.state.ReadBytes > 1024 {
			return nil, errors.New("live_read_budget")
		}
		if e = g.store.save(); e != nil {
			return nil, e
		}
	}
	if w.media {
		return resp, nil
	}
	var env struct{ Data json.RawMessage }
	_ = json.Unmarshal(b, &env)
	if r.URL.Path == "/open/v1/file/list" {
		var data struct {
			Files []struct {
				ID   string `json:"fid"`
				Name string `json:"filename"`
				Type string `json:"file_type"`
			} `json:"file_list"`
		}
		_ = json.Unmarshal(env.Data, &data)
		for _, f := range data.Files {
			if str("parent_fid") == "0" && f.Name == g.rootName && f.Type == "0" {
				if receipt, ok := g.store.state.Operations[g.store.state.ProofID]; ok {
					var p proofPayload
					if json.Unmarshal(receipt.Payload, &p) == nil {
						old, e := parseID(p.AgentRoot, RouteAgent, g.store.state.Bundle.Agent.AccountID, false)
						if e == nil {
							g.diagnostics = append(g.diagnostics, fmt.Sprintf("agent_root raw_equal=%t stable_equal=%t same_domain=%t", old == f.ID, sameAgent(old, f.ID), sameAgentDomain(old, f.ID)))
						}
					}
				}
				g.rootAgent = f.ID
				g.agentIDs[f.ID] = true
			} else if g.allowedName(f.Name) {
				g.agentIDs[f.ID] = true
			}
		}
	}
	if r.URL.Path == "/file" && r.URL.Query().Get("method") == "list" {
		var data struct {
			Files []struct {
				ID   string `json:"fid"`
				Name string `json:"filename"`
				Dir  int    `json:"isdir"`
			} `json:"files"`
		}
		_ = json.Unmarshal(env.Data, &data)
		for _, f := range data.Files {
			if r.URL.Query().Get("parent_fid") == "0" && f.Name == g.rootName && f.Dir == 1 {
				g.rootTV = f.ID
			} else if g.allowedName(f.Name) && f.Dir == 0 {
				g.tvFiles[f.ID] = true
			}
		}
	}
	if r.URL.Path == "/file" && r.URL.Query().Get("method") == "download" {
		var data struct {
			URL  string `json:"download_url"`
			Size int64  `json:"size"`
		}
		_ = json.Unmarshal(env.Data, &data)
		if data.Size > 0 && data.Size <= 64 {
			g.readURLs[data.URL] = data.Size
		}
	}
	if kind == "pre" || kind == "finish" || kind == "mkdir" {
		var data struct {
			ID string `json:"fid"`
		}
		_ = json.Unmarshal(env.Data, &data)
		if data.ID != "" {
			g.agentIDs[data.ID] = true
		}
	}
	return resp, nil
}

func TestQuarkLiveNativeCreationAndColdRecovery(t *testing.T) {
	agentDir, tvDir, stateDir := os.Getenv("OMC_QUARK_LIVE_AGENT_STATE"), os.Getenv("OMC_QUARK_LIVE_TV_STATE"), os.Getenv("OMC_QUARK_LIVE_CREATE_STATE")
	if agentDir == "" || tvDir == "" || stateDir == "" {
		t.Skip("explicit old session and new private acceptance directories required")
	}
	read := func(p string, max int64) []byte {
		t.Helper()
		for q := p; ; q = filepath.Dir(q) {
			i, e := os.Lstat(q)
			if e != nil || storagefs.IsReparsePoint(q, i) {
				t.Fatal("private path invalid")
			}
			if filepath.Dir(q) == q {
				break
			}
		}
		b, e := os.ReadFile(p)
		if e != nil || int64(len(b)) > max {
			t.Fatal("private file unavailable")
		}
		return b
	}
	old := map[string][]byte{}
	defer func() {
		for p, b := range old {
			if !bytes.Equal(b, read(p, 1<<20)) {
				t.Error("old state changed")
			}
		}
		t.Logf("old_private_files_unchanged=%d", len(old))
	}()
	load := func(dir, keyName, name, purpose string, out any) {
		t.Helper()
		kp, p := filepath.Join(dir, keyName), filepath.Join(dir, name)
		key, b := read(kp, 1024), read(p, 1<<20)
		old[kp], old[p] = key, b
		c, e := credential.Open("", string(key))
		if e != nil {
			t.Fatal("old key invalid")
		}
		var env struct {
			Version    int
			Ciphertext string
		}
		if json.Unmarshal(b, &env) != nil || env.Version != 1 {
			t.Fatal("old envelope invalid")
		}
		plain, e := c.Decrypt(purpose, env.Ciphertext)
		if e != nil || json.Unmarshal([]byte(plain), out) != nil {
			t.Fatal("old envelope unavailable")
		}
	}
	var agent, tv liveSession
	var ledger liveLedger
	load(agentDir, "credentials.key", "session.enc.json", "quark-original-302-probe:state:v1", &agent)
	load(agentDir, "credentials.key", "capability-ledger.enc.json", "quark-capability-validation:ownership-intent:v1", &ledger)
	load(tvDir, "tv-credentials.key", "tv-session.enc.json", "quark-original-302-probe:tv-session:v1", &tv)
	if !agent.FirstParty || !tv.Broker || !tv.IdentityVerified || agent.Access == "" || tv.Access == "" || agent.User == "" || tv.User == "" || ledger.User != agent.User || ledger.App != agent.App || ledger.Device != agent.Device || ledger.Client != agent.Client || ledger.Generation != agent.Generation || !ledger.SessionAt.Equal(agent.Updated) || len(ledger.Nonce) != 32 || ledger.RootName != "OMC-Quark-Validation-20261021-"+ledger.Nonce || ledger.Owned["root"].Name != ledger.RootName || ledger.Owned["root"].Parent != "0" {
		t.Fatal("old verified sandbox binding invalid")
	}
	if !filepath.IsAbs(stateDir) || strings.EqualFold(stateDir, agentDir) || strings.EqualFold(stateDir, tvDir) {
		t.Fatal("new private directory required")
	}
	for p := stateDir; ; p = filepath.Dir(p) {
		i, e := os.Lstat(p)
		if e != nil || !i.IsDir() || storagefs.IsReparsePoint(p, i) {
			t.Fatal("new private directory invalid")
		}
		if filepath.Dir(p) == p {
			break
		}
	}
	lock, e := database.AcquireExclusiveRuntime(filepath.Join(stateDir, "acceptance"))
	if e != nil {
		t.Fatal("acceptance lock unavailable")
	}
	defer lock.Close()
	cipher, e := credential.Open(filepath.Join(stateDir, "acceptance.key"), "")
	if e != nil {
		t.Fatal("new acceptance key unavailable")
	}
	s := &liveCreationStore{path: filepath.Join(stateDir, "creation.enc"), cipher: cipher}
	if b, e := os.ReadFile(s.path); e == nil {
		plain, e := cipher.Decrypt(liveCreationPurpose, string(b))
		if e != nil || json.Unmarshal([]byte(plain), &s.state) != nil {
			t.Fatal("acceptance journal invalid")
		}
	} else if errors.Is(e, os.ErrNotExist) {
		c := NewCredentials()
		c.Revision = 1
		c.TV = &Authorization{AccessToken: tv.Access, DeviceID: tv.Device, AccountID: tv.User, Generation: tv.Generation, IdentityVerified: true}
		c.Agent = &Authorization{AccessToken: agent.Access, DeviceID: agent.Device, ClientDeviceID: agent.Client, AccountID: agent.User, Generation: agent.Generation, IdentityVerified: true}
		nonce := make([]byte, 16)
		if _, e = rand.Read(nonce); e != nil {
			t.Fatal("acceptance nonce unavailable")
		}
		suffix := hex.EncodeToString(nonce)
		s.state = liveCreationState{Version: 1, RootName: ledger.RootName, RootACK: ledger.Owned["root"].ID, ProofID: "native-proof-" + suffix, UploadID: "native-txt-" + suffix, DirectoryID: "native-dir-" + suffix, UploadName: "native-" + suffix + ".txt", DirectoryName: "native-empty-" + suffix, Content: []byte("OhMyCine native acceptance " + suffix), Bundle: c, Operations: map[string]cloud.OperationReceipt{}}
		if s.save() != nil {
			t.Fatal("new acceptance journal unavailable")
		}
	} else {
		t.Fatal("acceptance journal unavailable")
	}
	if s.state.Version != 1 || s.state.RootName != ledger.RootName || s.state.RootACK != ledger.Owned["root"].ID || s.state.Bundle.Agent == nil || s.state.Bundle.TV == nil || s.state.Bundle.Agent.AccessToken != agent.Access || s.state.Bundle.TV.AccessToken != tv.Access || len(s.state.Content) > 64 {
		t.Fatal("acceptance manifest binding invalid")
	}
	g := &liveCreationGuard{store: s, rootName: s.state.RootName, rootACK: s.state.RootACK, agentIDs: map[string]bool{}, tvFiles: map[string]bool{}, readURLs: map[string]int64{}}
	for _, r := range s.state.Operations {
		var p uploadPayload
		if r.Kind == "upload" && json.Unmarshal(r.Payload, &p) == nil && p.Handle != "" {
			g.agentIDs[p.Handle] = true
		}
		var m operationPayload
		if r.Kind == "mkdir" && json.Unmarshal(r.Payload, &m) == nil && m.Handle != "" {
			g.agentIDs[m.Handle] = true
		}
	}
	defer func() {
		v := s.state
		t.Logf("requests=%d api=%d media_reads=%d media_bytes=%d upload_pre=%d upload_put=%d upload_finish=%d mkdir=%d", v.Requests, v.API, v.Reads, v.ReadBytes, v.PreWrites, v.PutWrites, v.FinishWrites, v.DirectoryWrites)
		if g.blocked != "" {
			t.Log("guard_blocked=" + g.blocked)
		}
		for _, line := range g.diagnostics {
			t.Log(line)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	client := func() *Client {
		raw, e := s.credentials(ctx)
		if e != nil {
			t.Fatal("acceptance credential read failed")
		}
		d, e := NewClient(cloud.Config{Credentials: raw, Operations: s, PersistCredentials: s.persist, LoadCredentials: s.credentials})
		if e != nil {
			t.Fatal("native initialization failed")
		}
		d.api.http.Transport = liveCreationTransport{base: d.api.http.Transport, guard: g}
		d.mediaTransport = liveCreationTransport{base: d.mediaTransport, guard: g, media: true}
		return d
	}
	check := func(stage string, e error) {
		t.Helper()
		if e != nil {
			var f *Failure
			if errors.As(e, &f) {
				t.Fatalf("stage=%s code=%s http=%d errno=%d", stage, f.Code, f.HTTPStatus, f.Errno)
			}
			code, _ := cloud.ErrorInfo(e)
			t.Fatalf("stage=%s code=%s", stage, code)
		}
		t.Logf("stage=%s passed requests=%d", stage, s.state.Requests)
	}
	d := client()
	check("agent_identity", (&Authenticator{api: d.api}).Verify(ctx, RouteAgent, d.Credentials()))
	check("tv_identity", (&Authenticator{api: d.api}).Verify(ctx, RouteTV, d.Credentials()))
	root, e := d.agentInfo(ctx, s.state.RootACK)
	check("original_sandbox_receipt", e)
	if !root.Dir || root.Name != s.state.RootName || root.Parent != "0" {
		t.Fatal("old sandbox changed")
	}
	proof, e := d.VerifyCommonSpace(ctx, s.state.ProofID, "/"+s.state.RootName)
	check("native_common_space", e)
	d = client()
	recovered, e := d.VerifyCommonSpace(ctx, s.state.ProofID, "/"+s.state.RootName)
	check("cold_common_space", e)
	if proof.ReceiptID != recovered.ReceiptID || proof.ContentSHA256 != recovered.ContentSHA256 {
		t.Fatal("proof recovery changed")
	}
	op := WithOperationID(ctx, s.state.UploadID)
	item, e := d.Upload(op, cloud.UploadRequest{ParentID: proof.AgentRoot, Name: s.state.UploadName, Size: int64(len(s.state.Content)), Reader: bytes.NewReader(s.state.Content)})
	check("native_txt_upload", e)
	mapped, e := d.ResolveReadItem(op, item.ID)
	check("native_txt_mapping", e)
	d = client()
	item2, e := d.Upload(op, cloud.UploadRequest{ParentID: proof.AgentRoot, Name: s.state.UploadName, Size: int64(len(s.state.Content)), Reader: bytes.NewReader(s.state.Content)})
	check("cold_txt_upload", e)
	mapped2, e := d.ResolveReadItem(op, item2.ID)
	check("cold_txt_mapping", e)
	if item.ID != item2.ID || mapped.ID != mapped2.ID {
		t.Fatal("upload identity changed")
	}
	dirctx := WithOperationID(ctx, s.state.DirectoryID)
	dir, e := d.CreateDirectory(dirctx, proof.AgentRoot, s.state.DirectoryName)
	check("native_mkdir", e)
	dirtv, e := d.ResolveReadItem(dirctx, dir.ID)
	check("native_mkdir_mapping", e)
	d = client()
	dir2, e := d.CreateDirectory(dirctx, proof.AgentRoot, s.state.DirectoryName)
	check("cold_mkdir", e)
	dirtv2, e := d.ResolveReadItem(dirctx, dir2.ID)
	check("cold_mkdir_mapping", e)
	if dir.ID != dir2.ID || dirtv.ID != dirtv2.ID {
		t.Fatal("directory identity changed")
	}
	if s.state.PreWrites != 2 || s.state.DirectoryWrites != 1 {
		t.Fatal("unexpected submission count")
	}
	t.Logf("new_files=2 new_file_bytes=%d new_empty_directories=1 cold_recovery_no_duplicate_writes=true", 32+len(s.state.Content))
}

func TestQuarkLiveCreationGuardDeniesUnrelatedWrites(t *testing.T) {
	s := &liveCreationStore{state: liveCreationState{Operations: map[string]cloud.OperationReceipt{}, Bundle: Credentials{TV: &Authorization{AccessToken: "tv-secret"}, Agent: &Authorization{AccessToken: "agent-secret"}}}}
	g := &liveCreationGuard{store: s, agentIDs: map[string]bool{}, tvFiles: map[string]bool{}, readURLs: map[string]int64{}}
	w := liveCreationTransport{guard: g}
	for _, p := range []string{"/agent/v1/oauth/access_token/rotate", "/open/v1/file/delete", "/open/v1/file/move", "/open/v1/file/rename/batch", "/open/v1/dir", "/open/v1/file/upload_pre", "/open/v1/file/upload_finish"} {
		r, _ := http.NewRequest("POST", apiOrigin+p, strings.NewReader(`{}`))
		if _, e := w.RoundTrip(r); e == nil {
			t.Fatal("unbound operation accepted")
		}
	}
	if s.state.Requests != 0 {
		t.Fatal("guard dispatched forbidden calls")
	}
}

// Offline evidence export: no client is constructed and no network transport
// is available. Only counters, object counts and terminal states are printed.
func TestQuarkLiveCreationJournalEvidence(t *testing.T) {
	dir := os.Getenv("OMC_QUARK_LIVE_CREATE_STATE")
	if dir == "" {
		t.Skip("explicit acceptance directory required")
	}
	key, e := os.ReadFile(filepath.Join(dir, "acceptance.key"))
	if e != nil || len(key) > 1024 {
		t.Fatal("acceptance key unavailable")
	}
	c, e := credential.Open("", string(key))
	if e != nil {
		t.Fatal("acceptance key invalid")
	}
	p := filepath.Join(dir, "creation.enc")
	b, e := os.ReadFile(p)
	if e != nil || len(b) > 1<<20 {
		t.Fatal("acceptance evidence unavailable")
	}
	plain, e := c.Decrypt(liveCreationPurpose, string(b))
	var s liveCreationState
	if e != nil || json.Unmarshal([]byte(plain), &s) != nil {
		t.Fatal("acceptance evidence invalid")
	}
	info, e := os.Stat(p)
	if e != nil {
		t.Fatal("acceptance timestamp unavailable")
	}
	complete := 0
	for _, id := range []string{s.ProofID, s.ProofID + "/upload", s.UploadID, s.DirectoryID} {
		if s.Operations[id].State == "confirmed" {
			complete++
		}
	}
	t.Logf("confirmed_receipts=%d requests=%d api=%d read_bytes=%d network_window_seconds=%.1f created_files=2 created_file_bytes=%d created_empty_directories=%d", complete, s.Requests, s.API, s.ReadBytes, info.ModTime().Sub(s.Started).Seconds(), 32+len(s.Content), s.DirectoryWrites)
	if complete != 4 || s.Requests > 100 || s.ReadBytes > 1024 || info.ModTime().Sub(s.Started) > 10*time.Minute {
		t.Fatal("acceptance completion or bound missing")
	}
}

func TestQuarkLiveCreationStoreEncryptsAndRestoresCAS(t *testing.T) {
	dir := t.TempDir()
	key, e := credential.Open(filepath.Join(dir, "key"), "")
	if e != nil {
		t.Fatal(e)
	}
	s := &liveCreationStore{path: filepath.Join(dir, "journal"), cipher: key, state: liveCreationState{Version: 1, Operations: map[string]cloud.OperationReceipt{}}}
	r := cloud.OperationReceipt{ID: "one", Provider: cloud.ProviderQuark, Route: string(RouteAgent), AccountID: "private-account", Kind: "upload", Revision: 1, Payload: json.RawMessage(`{"secret":"private-payload"}`)}
	if e = s.Create(context.Background(), r); e != nil {
		t.Fatal(e)
	}
	b, e := os.ReadFile(s.path)
	if e != nil || bytes.Contains(b, []byte("private-account")) || bytes.Contains(b, []byte("private-payload")) {
		t.Fatal("receipt not encrypted")
	}
	plain, e := key.Decrypt(liveCreationPurpose, string(b))
	if e != nil {
		t.Fatal(e)
	}
	cold := &liveCreationStore{path: s.path, cipher: key}
	if json.Unmarshal([]byte(plain), &cold.state) != nil {
		t.Fatal("recovery failed")
	}
	got, e := cold.Load(context.Background(), "one")
	if e != nil || got.AccountID != r.AccountID || !bytes.Equal(got.Payload, r.Payload) {
		t.Fatal("receipt recovery changed")
	}
	got.Revision = 2
	got.State = "accepted"
	if cold.Update(context.Background(), 1, got) != nil {
		t.Fatal("CAS failed")
	}
	if cold.Update(context.Background(), 1, got) == nil || cold.Create(context.Background(), r) == nil {
		t.Fatal("stale CAS or duplicate accepted")
	}
}
