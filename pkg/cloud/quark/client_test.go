package quark

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(r *http.Request, status int, value any) *http.Response {
	b, _ := json.Marshal(value)
	return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(string(b))), Request: r}
}
func envelope(r *http.Request, value any) *http.Response {
	return response(r, 200, map[string]any{"status": 0, "data": value})
}
func fixtureCredentials() Credentials {
	return Credentials{Version: 1, Revision: 1, TV: &Authorization{AccessToken: "tv-access-secret", RefreshToken: "tv-refresh-secret", DeviceID: "tv-device", AccountID: "tv-account", Generation: 1, IdentityVerified: true}, Agent: &Authorization{AccessToken: "agent-access-secret", RefreshToken: "agent-refresh-secret", DeviceID: "agent-device", ClientDeviceID: "client-device", AccountID: "agent-account", Generation: 1, IdentityVerified: true}}
}
func fixtureClient(t *testing.T, transport transportFunc) *Client {
	t.Helper()
	raw, _ := json.Marshal(fixtureCredentials())
	d, e := NewClient(cloud.Config{Credentials: raw})
	if e != nil {
		t.Fatal(e)
	}
	d.api.http.Transport = transport
	return d
}
func errorCode(t *testing.T, e error, want string) {
	t.Helper()
	got, _ := cloud.ErrorInfo(e)
	if got != want {
		t.Fatalf("code %q want %q (%v)", got, want, e)
	}
}

func TestTVOriginalOnlyAndQualifiedIdentity(t *testing.T) {
	calls := 0
	d := fixtureClient(t, func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Host != "open-api-drive.quark.cn" || r.URL.Path != "/file" || r.URL.Query().Get("method") != "download" || r.URL.Query().Get("fid") != "raw-tv-file" {
			t.Fatal("wrong TV route")
		}
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Fatal("management credentials leaked")
		}
		return envelope(r, map[string]any{"fid": "raw-tv-file", "file_name": "movie.mkv", "size": 1234, "download_url": "https://cdn.example/movie?e=2000000000"}), nil
	})
	link, e := d.DirectURL(context.Background(), cloud.DirectURLRequest{FileID: QualifyID(RouteTV, "tv-account", "raw-tv-file")})
	if e != nil || len(link.Headers) != 0 || link.ExpiresAt.Unix() != 2000000000 {
		t.Fatalf("original result %v", e)
	}
	for _, id := range []string{"raw-tv-file", QualifyID(RouteAgent, "agent-account", "raw-tv-file"), QualifyID(RouteTV, "other", "raw-tv-file")} {
		_, e = d.DirectURL(context.Background(), cloud.DirectURLRequest{FileID: id})
		errorCode(t, e, CodeIdentity)
	}
	if calls != 1 {
		t.Fatalf("unexpected calls %d", calls)
	}
	d.api.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return response(r, 400, map[string]any{"status": -1, "errno": 23018, "message": "secret-response"}), nil
	})
	_, e = d.DirectURL(context.Background(), cloud.DirectURLRequest{FileID: QualifyID(RouteTV, "tv-account", "raw-tv-file")})
	if e == nil || strings.Contains(e.Error(), "secret") {
		t.Fatal("failure not safe")
	}
	if calls != 2 {
		t.Fatal("fallback performed")
	}
}

func TestListShapeAndQualifiedPagination(t *testing.T) {
	for _, mode := range []string{"valid", "duplicate", "missing_list", "empty_nonterminal", "wrong_parent", "wrong_total", "wrong_size", "wrong_type"} {
		t.Run(mode, func(t *testing.T) {
			d := fixtureClient(t, func(r *http.Request) (*http.Response, error) {
				if r.URL.Query().Get("page_size") != "2" || r.URL.Query().Get("page_index") != "1" || r.URL.Query().Get("parent_fid") != "p" {
					t.Fatal("wrong page")
				}
				f := map[string]any{"fid": "x", "parent_fid": "p", "filename": "x.mkv", "size": 4, "isdir": 0}
				files := []any{f}
				data := map[string]any{"total_count": 3, "files": files}
				switch mode {
				case "duplicate":
					data["files"] = []any{f, f}
					data["total_count"] = 4
				case "missing_list":
					delete(data, "files")
				case "empty_nonterminal":
					data["files"] = []any{}
				case "wrong_parent":
					f["parent_fid"] = "outside"
				case "wrong_total":
					data["total_count"] = 2
				case "wrong_size":
					delete(f, "size")
				case "wrong_type":
					f["isdir"] = true
				}
				return envelope(r, data), nil
			})
			p, e := d.List(context.Background(), QualifyID(RouteTV, "tv-account", "p"), cloud.PageRequest{Offset: 2, Limit: 2})
			if mode == "valid" {
				if e != nil || len(p.Items) != 1 || p.HasMore || p.Total == nil || *p.Total != 3 || p.Items[0].ID != QualifyID(RouteTV, "tv-account", "x") {
					t.Fatalf("invalid result: %+v %v", p, e)
				}
			} else if e == nil {
				t.Fatal("bad page accepted")
			}
		})
	}
}

func TestOriginalReadCarriesNoAccountHeadersAndValidatesRange(t *testing.T) {
	d := fixtureClient(t, func(r *http.Request) (*http.Response, error) {
		return envelope(r, map[string]any{"fid": "f", "file_name": "f.mkv", "size": 10, "download_url": "https://cdn.example/f"}), nil
	})
	d.mediaTransport = transportFunc(func(r *http.Request) (*http.Response, error) {
		for _, h := range []string{"Authorization", "Cookie", "Referer", "X-Pan-Token", "X-Agent-ID"} {
			if r.Header.Get(h) != "" {
				t.Fatalf("header leak %s", h)
			}
		}
		if r.Header.Get("Range") != "bytes=5-9" {
			t.Fatal("missing range")
		}
		return &http.Response{StatusCode: 206, ContentLength: 5, Header: http.Header{"Content-Range": {"bytes 5-9/10"}}, Body: io.NopCloser(strings.NewReader("12345"))}, nil
	})
	out, e := d.OpenRead(context.Background(), cloud.ReadRequest{FileID: QualifyID(RouteTV, "tv-account", "f"), Offset: 5})
	if e != nil || !out.OffsetAccepted || out.TotalSize == nil || *out.TotalSize != 10 {
		t.Fatal(e)
	}
	out.Body.Close()
	d.mediaTransport = transportFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 206, Header: http.Header{"Content-Range": {"bytes 0-9/10"}}, Body: io.NopCloser(strings.NewReader("1234567890"))}, nil
	})
	_, e = d.OpenRead(context.Background(), cloud.ReadRequest{FileID: QualifyID(RouteTV, "tv-account", "f"), Offset: 5})
	errorCode(t, e, CodeInvalid)
}

func TestRefreshCheckpointSurvivesIdentityFailureAndConfirmsWithoutRotation(t *testing.T) {
	original := fixtureCredentials()
	current := original
	writes := []Credentials{}
	persist := func(_ context.Context, expected uint64, raw json.RawMessage) error {
		if current.Revision != expected {
			t.Fatal("CAS revision")
		}
		var next Credentials
		if json.Unmarshal(raw, &next) != nil || next.Revision != expected+1 {
			t.Fatal("bad checkpoint")
		}
		writes = append(writes, next)
		current = next
		return nil
	}
	a := NewAuthenticator()
	calls := 0
	a.api.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Host == "api.extscreen.com" {
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["refresh_token"] != original.TV.RefreshToken || body["code"] != "" || strings.Contains(string(mustJSON(body)), original.Agent.AccessToken) {
				t.Fatal("wrong broker recipient")
			}
			return response(r, 200, map[string]any{"code": 200, "data": map[string]any{"status": 0, "access_token": "rotated-access", "refresh_token": "rotated-refresh"}}), nil
		}
		if len(writes) != 1 || current.TV.IdentityVerified || current.TV.AccountID != original.TV.AccountID || current.TV.AccessToken != "rotated-access" {
			t.Fatal("identity ran before durable checkpoint")
		}
		return nil, errors.New("network secret-url")
	})
	next, e := a.Refresh(context.Background(), RouteTV, original, persist)
	if e == nil || next.TV.IdentityVerified || current.TV.RefreshToken != "rotated-refresh" || calls != 2 {
		t.Fatal("lost refresh checkpoint")
	}
	a.api.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Path != "/user" {
			t.Fatal("confirm replayed exchange")
		}
		return envelope(r, map[string]any{"user_id": "tv-account"}), nil
	})
	next, e = a.Confirm(context.Background(), RouteTV, next, persist)
	if e != nil || !next.TV.IdentityVerified || next.TV.Generation != 2 || calls != 3 || len(writes) != 2 {
		t.Fatalf("confirm failed %v", e)
	}
}
func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

func TestConfirmationRejectsChangedIdentityWithoutDiscardingCheckpoint(t *testing.T) {
	c := fixtureCredentials()
	c.TV.IdentityVerified = false
	a := NewAuthenticator()
	a.api.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		return envelope(r, map[string]any{"user_id": "wrong-account"}), nil
	})
	next, e := a.Confirm(context.Background(), RouteTV, c, func(context.Context, uint64, json.RawMessage) error { t.Fatal("mismatch persisted"); return nil })
	errorCode(t, e, CodeIdentity)
	if !reflect.DeepEqual(next, c) {
		t.Fatal("checkpoint changed")
	}
}

func TestRateLimitCooldownSharedAcrossRoutes(t *testing.T) {
	d := fixtureClient(t, func(r *http.Request) (*http.Response, error) {
		out := response(r, 429, map[string]any{})
		out.Header.Set("Retry-After", "12")
		return out, nil
	})
	_, e := d.DirectURL(context.Background(), cloud.DirectURLRequest{FileID: QualifyID(RouteTV, "tv-account", "x")})
	errorCode(t, e, CodeLimited)
	d.api.http.Transport = transportFunc(func(*http.Request) (*http.Response, error) { t.Fatal("cooldown sent request"); return nil, nil })
	_, e = d.agentRead(context.Background(), http.MethodGet, "/open/v1/user/info", nil, nil)
	errorCode(t, e, CodeLimited)
}

func TestOptionalCapabilitiesAndProofIdentity(t *testing.T) {
	d := fixtureClient(t, func(*http.Request) (*http.Response, error) {
		t.Fatal("unsupported route sent request")
		return nil, nil
	})
	c := d.Capabilities()
	if c.NativeOfflineDownload || c.Copy || c.Recycle || c.Watch || c.ChangeCursor || c.SmallFileUpload || c.ShareReceive {
		t.Fatal("unverified ability advertised")
	}
	errorCode(t, d.Copy(context.Background(), "x", "y"), CodeUnsupported)
	errorCode(t, d.Recycle(context.Background(), "x"), CodeUnsupported)
}

type memoryOperations struct {
	mu       sync.Mutex
	receipts map[string]cloud.OperationReceipt
}

func (s *memoryOperations) Create(_ context.Context, r cloud.OperationReceipt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.receipts[r.ID]; ok {
		return errors.New("duplicate")
	}
	if r.State != "prepared" || r.Revision != 1 {
		return errors.New("bad create")
	}
	s.receipts[r.ID] = r
	return nil
}
func (s *memoryOperations) Load(_ context.Context, id string) (cloud.OperationReceipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.receipts[id]
	if !ok {
		return r, cloud.ErrOperationNotFound
	}
	return r, nil
}
func (s *memoryOperations) Update(_ context.Context, rev uint64, r cloud.OperationReceipt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, ok := s.receipts[r.ID]
	if !ok || old.Revision != rev || r.Revision != rev+1 || old.Route != r.Route || old.AccountID != r.AccountID || old.Kind != r.Kind {
		return errors.New("CAS")
	}
	s.receipts[r.ID] = r
	return nil
}
func managedFixture(t *testing.T, transport transportFunc) (*Client, *memoryOperations) {
	d := fixtureClient(t, transport)
	s := &memoryOperations{receipts: map[string]cloud.OperationReceipt{}}
	d.config.Operations = s
	d.credentials.Proofs = []SpaceProof{{Version: 1, Generation: 1, TVAccount: "tv-account", AgentAccount: "agent-account", TVRoot: QualifyID(RouteTV, "tv-account", "T"), AgentRoot: QualifyID(RouteAgent, "agent-account", "A"), Path: "/library", ReceiptID: "proof", ContentSHA256: strings.Repeat("a", 64), VerifiedAt: time.Now()}}
	return d, s
}
func agentFile(id, parent, name string, dir bool, size int64) map[string]any {
	kind := "1"
	if dir {
		kind = "0"
	}
	return map[string]any{"fid": id, "parent_fid": parent, "filename": name, "file_type": kind, "size": size}
}

func TestUnknownMutationNeverResubmitsAndRequestIsFrozen(t *testing.T) {
	writes := 0
	d, store := managedFixture(t, func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/open/v1/file/info":
			id := r.URL.Query().Get("fid")
			if id == "A" {
				return envelope(r, agentFile("A", "0", "library", true, 0)), nil
			}
			return envelope(r, agentFile("f", "A", "old.mkv", false, 9)), nil
		case "/open/v1/file/list":
			return envelope(r, map[string]any{"last_page": true, "file_list": []any{agentFile("f", "A", "old.mkv", false, 9)}}), nil
		case "/open/v1/file/rename/batch":
			writes++
			return nil, errors.New("lost acknowledgement")
		}
		t.Fatal("unexpected route")
		return nil, nil
	})
	req := cloud.OperationRequest{ID: "job/rename", Kind: "rename", ItemID: QualifyID(RouteAgent, "agent-account", "f"), Name: "new.mkv"}
	_, e := d.BeginOperation(context.Background(), req)
	errorCode(t, e, cloud.CodeMutationUnknown)
	if store.receipts[req.ID].State != "unknown" {
		t.Fatal("uncertainty lost")
	}
	_, e = d.BeginOperation(context.Background(), req)
	errorCode(t, e, cloud.CodeMutationUnknown)
	if writes != 1 {
		t.Fatal("mutation replayed")
	}
	req.Name = "different.mkv"
	_, e = d.BeginOperation(context.Background(), req)
	errorCode(t, e, CodeIdentity)
}

func TestAcknowledgedRenameConfirmsActualOutput(t *testing.T) {
	renamed := false
	writes := 0
	d, _ := managedFixture(t, func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/open/v1/file/info":
			id := r.URL.Query().Get("fid")
			if id == "A" {
				return envelope(r, agentFile("A", "0", "library", true, 0)), nil
			}
			name := "old.mkv"
			if renamed {
				name = "new.mkv"
			}
			return envelope(r, agentFile("f", "A", name, false, 9)), nil
		case "/open/v1/file/list":
			name := "old.mkv"
			if renamed {
				name = "new.mkv"
			}
			return envelope(r, map[string]any{"last_page": true, "file_list": []any{agentFile("f", "A", name, false, 9)}}), nil
		case "/open/v1/file/rename/batch":
			renamed = true
			writes++
			return envelope(r, map[string]any{"task_id": "t", "finish": false}), nil
		case "/open/v1/task/query":
			return envelope(r, map[string]any{"finish": true, "fail_count": 0}), nil
		}
		t.Fatal("unexpected route")
		return nil, nil
	})
	req := cloud.OperationRequest{ID: "rename", Kind: "rename", ItemID: QualifyID(RouteAgent, "agent-account", "f"), Name: "new.mkv"}
	result, e := d.BeginOperation(context.Background(), req)
	if e != nil || !result.Complete || len(result.Items) != 1 || result.Items[0].Name != "new.mkv" {
		t.Fatalf("rename not confirmed %v", e)
	}
	result, e = d.BeginOperation(context.Background(), req)
	if e != nil || !result.Complete || writes != 1 {
		t.Fatal("confirmed retry wrote again")
	}
}

func TestGuestPreviewNoAccountCredentialsAndExactSelection(t *testing.T) {
	d := fixtureClient(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "drive-pc.quark.cn" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("X-Pan-Token") != "" || r.URL.Query().Get("access_token") != "guest_placeholder" {
			t.Fatal("account credential crossed guest boundary")
		}
		return response(r, 200, map[string]any{"status": 200, "code": 0, "data": map[string]any{"token_info": map[string]string{"stoken": "guest-secret"}, "detail_info": map[string]any{"list": []any{map[string]any{"fid": "one", "file_name": "one.mkv", "file_type": 1, "size": 22, "share_fid_token": "file-secret"}, map[string]any{"fid": "two", "file_name": "two.mkv", "file_type": 1, "size": 44, "share_fid_token": "other-secret"}}}}}), nil
	})
	snap, e := d.InspectShare(context.Background(), "http://pan.quark.cn/s/abc123#/list/share")
	if e != nil || len(snap.Items) != 2 {
		t.Fatal(e)
	}
	snap.Items = snap.Items[:1]
	selected, e := selectedShare(&snap)
	if e != nil || len(selected.Files) != 1 || selected.Files[0].ID != "one" || selected.Files[0].Token != "file-secret" {
		t.Fatal("selection not exact")
	}
	snap.Items[0].Size++
	_, e = selectedShare(&snap)
	if e == nil {
		t.Fatal("tampered selection accepted")
	}
}

func TestAgentLoginRetainsExchangeIdentityBeforeVerification(t *testing.T) {
	a := NewAuthenticator()
	a.api.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/agent/v1/oauth/get_aac_by_pagecode":
			return envelope(r, map[string]any{"status": "success", "agent_auth_code": "single-use"}), nil
		case "/agent/v1/oauth/agent_auth_code":
			return envelope(r, map[string]any{"access_token": "issued-access", "refresh_token": "issued-refresh", "device_id": "device", "user_id": "exchange-account"}), nil
		case "/open/v1/user/info":
			return envelope(r, map[string]any{"user_id": "different-account"}), nil
		}
		t.Fatal("unexpected auth request")
		return nil, nil
	})
	var saved Credentials
	ch := Challenge{Route: RouteAgent, DeviceID: "device", ClientDeviceID: "client", PageCode: "page", ExpiresAt: time.Now().Add(time.Minute)}
	c, done, e := a.Poll(context.Background(), ch, NewCredentials(), func(_ context.Context, expected uint64, raw json.RawMessage) error {
		if expected != 0 {
			t.Fatal("unexpected verified save")
		}
		return json.Unmarshal(raw, &saved)
	})
	errorCode(t, e, CodeIdentity)
	if done || c.Agent == nil || saved.Agent == nil || saved.Agent.IdentityVerified || saved.Agent.AccountID != "exchange-account" || saved.Agent.RefreshToken != "issued-refresh" {
		t.Fatal("exchange identity or checkpoint lost")
	}
}

func TestAgentSecondConfirmationIdentityMismatchIsTerminal(t *testing.T) {
	a := NewAuthenticator()
	a.api.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/agent/v1/oauth/get_aac_by_pagecode":
			return envelope(r, map[string]any{"status": "success", "agent_auth_code": "single-use"}), nil
		case "/agent/v1/oauth/agent_auth_code":
			return envelope(r, map[string]any{"status": "second_confirmed", "user_id": "agent-account"}), nil
		case "/open/v1/user/info":
			return envelope(r, map[string]any{"user_id": "other-account"}), nil
		}
		t.Fatal("unexpected auth request")
		return nil, nil
	})
	ch := Challenge{Route: RouteAgent, Revision: 1, DeviceID: "device", ClientDeviceID: "client", PageCode: "page", ExpiresAt: time.Now().Add(time.Minute)}
	_, done, e := a.Poll(context.Background(), ch, fixtureCredentials(), func(context.Context, uint64, json.RawMessage) error { t.Fatal("identity mismatch saved"); return nil })
	errorCode(t, e, CodeIdentity)
	if done {
		t.Fatal("identity mismatch passed")
	}
}
