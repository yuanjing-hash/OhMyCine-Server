//go:build quark_live

package handlers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/credential"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/database"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
)

type observationOfflineTransport func(*http.Request) (*http.Response, error)

func (f observationOfflineTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func observationOfflineStore(t *testing.T) *observationStore {
	t.Helper()
	dir := t.TempDir()
	c, e := credential.Open(filepath.Join(dir, "observation.key"), "")
	if e != nil {
		t.Fatal(e)
	}
	s := &observationStore{dir: dir, cipher: c, state: observationState{Version: 1, Nonce: strings.Repeat("a", 32), RootName: "owned-root", RootACK: "owned-ack", AgentAccount: "agent-account", TVAccount: "tv-account", TVDevice: strings.Repeat("b", 32), FixtureSHA: observationFixtureSHA, ProofID: "proof", DirectoryID: "directory", UploadID: "video", DirectoryName: "unique-child", UploadName: "unique.mp4", Operations: map[string]cloud.OperationReceipt{}, Writes: map[string]bool{}, Started: time.Now().UTC(), RootVerified: true, Bundle: quark.Credentials{Version: 1, Revision: 1, TV: &quark.Authorization{AccessToken: "private-tv-token", DeviceID: "device-tv", AccountID: "tv-account", Generation: 1, IdentityVerified: true}, Agent: &quark.Authorization{AccessToken: "private-agent-token", DeviceID: "device-agent", AccountID: "agent-account", Generation: 1, IdentityVerified: true}}}}
	if s.save() != nil {
		t.Fatal("offline store init")
	}
	return s
}
func observationOfflineResponse(body string) *http.Response {
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body))}
}
func TestQuarkObservationReceiptCASAndUnknownWrite(t *testing.T) {
	s := observationOfflineStore(t)
	p := observationMutation{Request: cloud.OperationRequest{ID: "directory", Kind: "mkdir", Name: s.state.DirectoryName, ParentID: "qualified-root"}}
	raw, _ := json.Marshal(p)
	r := cloud.OperationReceipt{ID: "directory", Kind: "mkdir", Provider: cloud.ProviderQuark, Route: "agent", AccountID: "agent-account", Revision: 1, State: "submitting", Payload: raw}
	if e := s.Create(context.Background(), r); e != nil {
		t.Fatal(e)
	}
	g := newObservationGuard(s)
	g.delay = 0
	g.rootAgent = "agent-parent"
	dispatched := 0
	transport := g.wrap(observationOfflineTransport(func(r *http.Request) (*http.Response, error) {
		dispatched++
		b, _ := os.ReadFile(filepath.Join(s.dir, "observation.enc"))
		plain, e := s.cipher.Decrypt(observationPurpose, string(b))
		var saved observationState
		if e != nil || json.Unmarshal([]byte(plain), &saved) != nil || saved.DirectoryWrites != 1 || !saved.Writes["directory:mkdir"] {
			t.Fatal("dispatch before durable intent and count")
		}
		return nil, errors.New("private upstream URL and token")
	}), false)
	request := func() *http.Request {
		r, _ := http.NewRequest("POST", "https://open-api-drive.quark.cn/open/v1/dir", strings.NewReader(`{"dir_path":"unique-child","pdir_fid":"agent-parent"}`))
		return r
	}
	_, e := transport.RoundTrip(request())
	if e == nil || e.Error() != observationError().Error() {
		t.Fatal("safe failure required")
	}
	if _, e = transport.RoundTrip(request()); e == nil || dispatched != 1 {
		t.Fatal("unknown creation repeated")
	}
	b, _ := os.ReadFile(filepath.Join(s.dir, "observation.enc"))
	if bytes.Contains(b, []byte("private-tv-token")) || bytes.Contains(b, []byte("agent-account")) {
		t.Fatal("secret journal plaintext")
	}
	plain, _ := s.cipher.Decrypt(observationPurpose, string(b))
	cold := &observationStore{dir: s.dir, cipher: s.cipher}
	if json.Unmarshal([]byte(plain), &cold.state) != nil {
		t.Fatal("cold journal")
	}
	g2 := newObservationGuard(cold)
	g2.delay = 0
	g2.rootAgent = "agent-parent"
	if _, e = g2.wrap(observationOfflineTransport(func(*http.Request) (*http.Response, error) { t.Fatal("cold duplicate dispatch"); return nil, nil }), false).RoundTrip(request()); e == nil {
		t.Fatal("cold duplicate allowed")
	}
	r.Revision = 2
	r.State = "unknown"
	if cold.Update(context.Background(), 1, r) != nil || cold.Update(context.Background(), 1, r) == nil || cold.Create(context.Background(), r) == nil {
		t.Fatal("receipt CAS")
	}
	r.ID = "other"
	r.Revision = 1
	if cold.Create(context.Background(), r) == nil {
		t.Fatal("foreign operation admitted")
	}
}

func TestQuarkObservationExactSyntheticUploadGuard(t *testing.T) {
	fixture, e := observationFixture(filepath.Join("..", "..", "..", "validation", "observation-fixture", "omc-observation-fixture.mp4"))
	if e != nil {
		t.Fatal("pinned synthetic fixture missing")
	}
	s := observationOfflineStore(t)
	g := newObservationGuard(s)
	g.delay = 0
	g.childAgent = "child-listed"
	p := map[string]any{"parent": "child-listed", "name": s.state.UploadName, "size": observationFixtureSize, "sha256": observationFixtureSHA, "sha1": "fixture-sha1", "md5": "fixture-md5", "phase": "pre_inflight"}
	writePayload := func(rev uint64, state string) {
		raw, _ := json.Marshal(p)
		r := cloud.OperationReceipt{ID: s.state.UploadID, Kind: "upload", Provider: cloud.ProviderQuark, Route: "agent", AccountID: s.state.AgentAccount, Revision: rev, State: state, Payload: raw}
		var e error
		if rev == 1 {
			e = s.Create(context.Background(), r)
		} else {
			e = s.Update(context.Background(), rev-1, r)
		}
		if e != nil {
			t.Fatal(e)
		}
	}
	writePayload(1, "submitting")
	calls := 0
	base := observationOfflineTransport(func(*http.Request) (*http.Response, error) {
		calls++
		return observationOfflineResponse(`{"data":{}}`), nil
	})
	api := g.wrap(base, false)
	request, _ := http.NewRequest("POST", "https://open-api-drive.quark.cn/open/v1/file/upload_pre", strings.NewReader(`{"pdir_fid":"child-listed","file_name":"unique.mp4","size":10998,"sha1":"fixture-sha1","md5":"fixture-md5"}`))
	resp, e := api.RoundTrip(request)
	if e != nil {
		t.Fatal("exact pre rejected")
	}
	resp.Body.Close()
	p["phase"] = "put_inflight"
	p["inflight_part"] = 1
	p["upload_urls"] = []any{map[string]any{"upload_url": "https://upload.example/exact", "signature_info": map[string]string{"signature": "upload-only-signature"}}}
	writePayload(2, "accepted")
	put, _ := http.NewRequest("PUT", "https://upload.example/exact", bytes.NewReader(fixture))
	put.Header.Set("Authorization", "upload-only-signature")
	media := g.wrap(base, true)
	bad, _ := http.NewRequest("PUT", "https://upload.example/exact", bytes.NewReader(append([]byte("x"), fixture[1:]...)))
	bad.Header.Set("Authorization", "upload-only-signature")
	if _, e = media.RoundTrip(bad); e == nil || calls != 1 || s.state.PutWrites != 0 {
		t.Fatal("changed content reached first PUT dispatch")
	}
	resp, e = media.RoundTrip(put)
	if e != nil {
		t.Fatal("exact body PUT rejected")
	}
	resp.Body.Close()
	p["phase"] = "finish_inflight"
	p["task_id"] = "one-task"
	writePayload(3, "accepted")
	finish, _ := http.NewRequest("POST", "https://open-api-drive.quark.cn/open/v1/file/upload_finish", strings.NewReader(`{"task_id":"one-task"}`))
	resp, e = api.RoundTrip(finish)
	if e != nil {
		t.Fatal("exact finish rejected")
	}
	resp.Body.Close()
	if calls != 3 || s.state.PreWrites != 1 || s.state.PutWrites != 1 || s.state.FinishWrites != 1 {
		t.Fatal("submission counts")
	}
	for _, phase := range []string{"pre", "put", "finish"} {
		if !s.state.Writes[s.state.UploadID+":"+phase] {
			t.Fatal("write phase not durable")
		}
	}
}
func TestQuarkObservationTransportBounds(t *testing.T) {
	s := observationOfflineStore(t)
	g := newObservationGuard(s)
	g.delay = 0
	calls := 0
	w := g.wrap(observationOfflineTransport(func(*http.Request) (*http.Response, error) {
		calls++
		return observationOfflineResponse(`{"data":{}}`), nil
	}), false)
	for _, path := range []string{"/open/v1/file/delete", "/open/v1/file/rename/batch", "/open/v1/file/move", "/agent/v1/oauth/access_token/rotate", "/open/v1/file/upload_pre", "/open/v1/file/upload_finish", "/open/v1/dir"} {
		r, _ := http.NewRequest("POST", "https://open-api-drive.quark.cn"+path, strings.NewReader(`{}`))
		if _, e := w.RoundTrip(r); e == nil {
			t.Fatal("unbound write allowed")
		}
	}
	if calls != 0 || s.state.Requests != 0 {
		t.Fatal("forbidden I/O")
	}
	r, _ := http.NewRequest("GET", "https://open-api-drive.quark.cn/user?method=user_info", nil)
	for _, set := range []func(){func() { s.state.API = 160 }, func() { s.state.API = 0; s.state.Requests = 180 }, func() { s.state.Requests = 0; s.state.Started = time.Now().Add(-13 * time.Minute) }, func() { s.state.Started = time.Now(); s.state.Completed = true }} {
		set()
		if _, e := w.RoundTrip(r); e == nil {
			t.Fatal("budget bypass")
		}
	}
	if calls != 0 {
		t.Fatal("budget request reached network")
	}
	s.state.Completed = false
	s.state.API = 0
	s.state.Requests = 0
	s.state.Started = time.Now()
	s.state.LastDispatch = time.Now()
	g.delay = time.Hour
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if _, e := w.RoundTrip(r.WithContext(ctx)); e == nil || calls != 0 {
		t.Fatal("deadline bypass")
	}
}
func TestQuarkObservationBrokerBoundAndRedaction(t *testing.T) {
	s := observationOfflineStore(t)
	g := newObservationGuard(s)
	g.delay = 0
	calls := 0
	w := g.wrap(observationOfflineTransport(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("https://upstream/?secret=bad")
	}), false)
	makeRequest := func() *http.Request {
		r, _ := http.NewRequest("POST", quark.TVTokenService, strings.NewReader(`{"refresh_token":"tv-only","device_id":"`+s.state.TVDevice+`"}`))
		return r
	}
	if _, e := w.RoundTrip(makeRequest()); e == nil || strings.Contains(e.Error(), "https:") {
		t.Fatal("not redacted")
	}
	if _, e := w.RoundTrip(makeRequest()); e == nil || calls != 1 || s.state.BrokerWrites != 1 {
		t.Fatal("broker retry allowed")
	}
	g.startup = false
	s.state.Writes = map[string]bool{}
	s.state.BrokerWrites = 0
	if _, e := w.RoundTrip(makeRequest()); e == nil || calls != 1 {
		t.Fatal("service renewed credentials")
	}
}
func TestQuarkObservationMediaScopeAndBodyBound(t *testing.T) {
	s := observationOfflineStore(t)
	g := newObservationGuard(s)
	g.delay = 0
	g.readURLs["https://media.example/exact"] = 32
	calls := 0
	w := g.wrap(observationOfflineTransport(func(*http.Request) (*http.Response, error) {
		calls++
		return observationOfflineResponse(strings.Repeat("x", 33)), nil
	}), true)
	for _, url := range []string{"https://media.example/other", "http://media.example/exact"} {
		r, _ := http.NewRequest("GET", url, nil)
		if _, e := w.RoundTrip(r); e == nil {
			t.Fatal("unowned link allowed")
		}
	}
	r, _ := http.NewRequest("GET", "https://media.example/exact", nil)
	r.Header.Set("Cookie", "private")
	if _, e := w.RoundTrip(r); e == nil {
		t.Fatal("credential forwarded")
	}
	r.Header.Del("Cookie")
	if _, e := w.RoundTrip(r); e == nil || calls != 1 || s.state.ReadBytes != 32 {
		t.Fatal("oversized body or reservation")
	}
}
func observationOfflineAgent(t *testing.T) (string, *observationAgentState) {
	t.Helper()
	dir := t.TempDir()
	c, e := credential.Open(filepath.Join(dir, "credentials.key"), "")
	if e != nil {
		t.Fatal(e)
	}
	a := observationAgentSession{Version: 1, App: quark.ObservationAgentProfileFingerprint(), Device: "old-device", Client: "old-client", User: "old-user", Access: "old-access", Generation: 2, FirstParty: true, Updated: time.Now().UTC()}
	l := observationLedger{Version: 1, App: a.App, User: a.User, Device: a.Device, Client: a.Client, Generation: a.Generation, SessionAt: a.Updated, Nonce: strings.Repeat("c", 32), RootName: "OMC-Quark-Validation-20261021-" + strings.Repeat("c", 32), Owned: map[string]observationOwned{}}
	l.Owned["root"] = observationOwned{ID: "owned-ack", Name: l.RootName, Parent: "0"}
	for _, v := range []struct {
		name, purpose string
		value         any
	}{{"session.enc.json", "quark-original-302-probe:state:v1", a}, {"capability-ledger.enc.json", "quark-capability-validation:ownership-intent:v1", l}} {
		raw, _ := json.Marshal(v.value)
		cipher, _ := c.Encrypt(v.purpose, string(raw))
		env, _ := json.Marshal(struct {
			Version    int
			Ciphertext string
		}{1, cipher})
		if os.WriteFile(filepath.Join(dir, v.name), env, 0600) != nil {
			t.Fatal("fixture write")
		}
	}
	s, e := openObservationAgent(dir)
	if e != nil {
		t.Fatal("fixture cannot open")
	}
	t.Cleanup(func() { s.lock.Close() })
	return dir, s
}
func TestQuarkObservationAgentOriginalLockAndBinding(t *testing.T) {
	dir, s := observationOfflineAgent(t)
	if _, e := openObservationAgent(dir); e == nil {
		t.Fatal("second original lock allowed")
	}
	if s.unchanged() != nil {
		t.Fatal("original fixture changed")
	}
	lock, e := database.AcquireExclusiveRuntime(filepath.Join(dir, "session.enc.json"))
	if e == nil {
		lock.Close()
		t.Fatal("wrong runtime lock")
	}
	s.lock.Close()
	key, _ := os.ReadFile(filepath.Join(dir, "credentials.key"))
	c, _ := credential.Open("", string(key))
	s.ledger.Generation++
	raw, _ := json.Marshal(s.ledger)
	sealed, _ := c.Encrypt("quark-capability-validation:ownership-intent:v1", string(raw))
	env, _ := json.Marshal(struct {
		Version    int
		Ciphertext string
	}{1, sealed})
	_ = os.WriteFile(filepath.Join(dir, "capability-ledger.enc.json"), env, 0600)
	if _, e = openObservationAgent(dir); e == nil {
		t.Fatal("stale ownership ledger admitted")
	}
}

func TestQuarkObservationFreshJournalAndFrozenBatch(t *testing.T) {
	_, agent := observationOfflineAgent(t)
	tv := &liveTVStore{dir: t.TempDir(), session: liveTVSession{UserID: "tv-account", DeviceID: strings.Repeat("e", 32), ProfileFingerprint: strings.Repeat("f", 64)}}
	dir := observationOfflinePrivateDir(t)
	s, e := openObservationStore(dir, agent, tv)
	if e != nil {
		t.Fatal("new batch unavailable")
	}
	first := s.state
	s.lock.Close()
	again, e := openObservationStore(dir, agent, tv)
	if e != nil {
		t.Fatal("batch recovery unavailable")
	}
	if again.state.Nonce != first.Nonce || !again.state.Started.Equal(first.Started) || again.state.UploadName != first.UploadName {
		t.Fatal("batch reset")
	}
	again.state.Completed = true
	again.state.Started = time.Now().Add(-time.Hour)
	if again.save() != nil {
		t.Fatal("completion persistence")
	}
	again.lock.Close()
	done, e := openObservationStore(dir, agent, tv)
	if e != nil || !done.state.Completed {
		t.Fatal("completed batch should report without calls")
	}
	done.lock.Close()
	tv.session.UserID = "changed-account"
	if altered, e := openObservationStore(dir, agent, tv); e == nil {
		altered.lock.Close()
		t.Fatal("account fence bypass")
	}
	orphan := observationOfflinePrivateDir(t)
	if os.WriteFile(filepath.Join(orphan, "old-file"), []byte("existing"), 0600) != nil {
		t.Fatal("fixture")
	}
	if s, e := openObservationStore(orphan, agent, tv); e == nil {
		s.lock.Close()
		t.Fatal("populated unjournaled directory reused")
	}
	if _, e = os.Stat(filepath.Join(orphan, "observation.key")); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("orphan generated a key")
	}
}

func TestQuarkObservationListingOwnershipNotNameOnly(t *testing.T) {
	s := observationOfflineStore(t)
	g := newObservationGuard(s)
	g.delay = 0
	w := g.wrap(observationOfflineTransport(func(*http.Request) (*http.Response, error) {
		return observationOfflineResponse(`{"data":{"files":[{"fid":"unowned","filename":"unique.mp4","isdir":0,"size":10998}]}}`), nil
	}), false)
	r, _ := http.NewRequest("GET", "https://open-api-drive.quark.cn/file?method=list&parent_fid=0&page_size=100&page_index=0", nil)
	resp, e := w.RoundTrip(r)
	if e != nil {
		t.Fatal(e)
	}
	resp.Body.Close()
	if g.tvFiles["unowned"] {
		t.Fatal("name outside owned child admitted")
	}
	g.childTV = "owned-child"
	g.tvParents["owned-child"] = true
	r, _ = http.NewRequest("GET", "https://open-api-drive.quark.cn/file?method=list&parent_fid=owned-child&page_size=100&page_index=0", nil)
	resp, e = w.RoundTrip(r)
	if e != nil {
		t.Fatal(e)
	}
	resp.Body.Close()
	if !g.tvFiles["unowned"] {
		t.Fatal("exact child fixture not allowed")
	}
	bad, _ := http.NewRequest("POST", "https://open-api-drive.quark.cn/open/v1/file/list", strings.NewReader(`{"parent_fid":"0","size":99999}`))
	if _, e = w.RoundTrip(bad); e == nil {
		t.Fatal("unbounded listing")
	}
}
