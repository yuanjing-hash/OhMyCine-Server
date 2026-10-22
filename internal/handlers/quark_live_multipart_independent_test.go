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

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
)

// Reads only this test's generated temporary ciphertext; no original account
// state, credential source, real HTTP transport or live entry is used.
func multipartIndependentDisk(t *testing.T, s *multipartStore) multipartState {
	t.Helper()
	raw, e := os.ReadFile(filepath.Join(s.dir, "multipart.enc"))
	if e != nil {
		t.Fatal("synthetic journal read failed")
	}
	plain, e := s.cipher.Decrypt(multipartPurpose, string(raw))
	var persisted multipartState
	if e != nil || json.Unmarshal([]byte(plain), &persisted) != nil {
		t.Fatal("synthetic journal decode failed")
	}
	return persisted
}

func TestQuarkMultipartIndependentDurableRequestDeadline(t *testing.T) {
	s, _ := multipartFixture(t)
	g := newMultipartGuard(s)
	g.delay = 0
	s.state.Started = time.Now().UTC().Add(-multipartLifetime + 5*time.Second)
	frozen := s.state.Started.Add(multipartLifetime)
	for _, callerEarlier := range []bool{false, true} {
		ctx := context.Background()
		want := frozen
		if callerEarlier {
			want = time.Now().Add(time.Second)
			var cancel context.CancelFunc
			ctx, cancel = context.WithDeadline(ctx, want)
			defer cancel()
		}
		called := false
		base := multipartTestTransport(func(r *http.Request) (*http.Response, error) {
			called = true
			deadline, ok := r.Context().Deadline()
			if !ok || !deadline.Equal(want) {
				t.Fatal("transport did not retain the earlier durable/caller deadline")
			}
			disk := multipartIndependentDisk(t, s)
			if disk.Started != s.state.Started || disk.Requests != s.state.Requests || disk.API != s.state.API {
				t.Fatal("request escaped before durable budget commit")
			}
			body := `{"status":0,"data":{}}`
			return &http.Response{StatusCode: 200, Header: http.Header{}, ContentLength: int64(len(body)), Body: io.NopCloser(strings.NewReader(body))}, nil
		})
		r, _ := http.NewRequestWithContext(ctx, "GET", "https://open-api-drive.quark.cn/user?method=user_info", nil)
		resp, e := g.wrap(base, false).RoundTrip(r)
		if e != nil || !called {
			t.Fatal("synthetic deadline request failed")
		}
		resp.Body.Close()
	}
}

type multipartIndependentRead func([]byte) (int, error)

func (f multipartIndependentRead) Read(p []byte) (int, error) { return f(p) }

func TestQuarkMultipartIndependentMetadataSentinelReservation(t *testing.T) {
	s, _ := multipartFixture(t)
	g := newMultipartGuard(s)
	g.delay = 0
	s.state.Metadata = (8 << 20) - 1
	reads, calls := 0, 0
	base := multipartTestTransport(func(*http.Request) (*http.Response, error) {
		calls++
		body := multipartIndependentRead(func(p []byte) (int, error) {
			reads++
			disk := multipartIndependentDisk(t, s)
			if disk.Metadata != 8<<20 || len(p) != 1 {
				t.Fatal("metadata sentinel not durably reserved before consumption")
			}
			p[0] = 'X'
			return 1, nil
		})
		return &http.Response{StatusCode: 200, Header: http.Header{}, ContentLength: 0, Body: io.NopCloser(body)}, nil
	})
	for n := 0; n < 2; n++ {
		r, _ := http.NewRequest("GET", "https://open-api-drive.quark.cn/user?method=user_info", nil)
		if _, e := g.wrap(base, false).RoundTrip(r); e == nil || e.Error() != multipartError().Error() {
			t.Fatal("misdeclared/exhausted metadata accepted")
		}
	}
	if reads != 1 || calls > 2 || s.state.Metadata != 8<<20 {
		t.Fatal("metadata limit consumed an unreserved sentinel")
	}
}

func TestQuarkMultipartIndependentWriteAheadAndUnknownReplay(t *testing.T) {
	s, _ := multipartFixture(t)
	g := newMultipartGuard(s)
	g.delay = 0
	p := multipartPayload{Parent: s.state.ChildAgent, Name: s.state.UploadName, Size: multipartSize, SHA256: s.state.SourceSHA, PartSize: 16 << 20, TaskID: "frozen-task", Phase: "put_inflight", InFlightPart: 3}
	payload, _ := json.Marshal(p)
	s.state.Operations[s.state.UploadID] = cloud.OperationReceipt{ID: s.state.UploadID, Provider: cloud.ProviderQuark, AccountID: s.state.Identity.AgentAccount, Route: string(quark.RouteAgent), Kind: "upload", Revision: 1, State: "accepted", Payload: payload}
	s.state.Eligible = true
	dest := multipartDestination{Number: 3, URL: "https://objects.example/part/3"}
	dest.Signature.Value = "synthetic-upload-signature"
	s.state.Destinations[3] = dest
	f, e := s.source()
	if e != nil {
		t.Fatal("synthetic source unavailable")
	}
	last := make([]byte, 1)
	_, e = f.ReadAt(last, multipartSize-1)
	f.Close()
	if e != nil {
		t.Fatal("synthetic last byte unavailable")
	}
	started := s.state.Started
	calls := 0
	base := multipartTestTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		disk := multipartIndependentDisk(t, s)
		if disk.Put != 1 || disk.UploadReserved != 1 || disk.Requests != 1 || !disk.Writes["part:3"] || disk.Started != started {
			t.Fatal("PUT escaped before frozen write-key/budget commit")
		}
		_, _ = io.Copy(io.Discard, r.Body)
		return nil, errors.New("synthetic raw upstream secret")
	})
	put := func() error {
		r, _ := http.NewRequest("PUT", dest.URL, bytes.NewReader(last))
		r.Header.Set("Authorization", dest.Signature.Value)
		r.Header.Set("Accept-Encoding", "identity")
		_, e := g.wrap(base, true).RoundTrip(r)
		return e
	}
	if e := put(); e == nil || e.Error() != multipartError().Error() {
		t.Fatal("uncertain PUT did not return the fixed safe error")
	}
	s.state = multipartIndependentDisk(t, s)
	if e := put(); e == nil || calls != 1 || s.state.UploadActual != 1 || s.state.Put != 1 || s.state.Started != started {
		t.Fatal("uncertain PUT replayed or reset its original budget")
	}
}

func TestQuarkMultipartIndependentLegacyStopCannotBecomeSmall(t *testing.T) {
	s, parent := multipartFixture(t)
	// Simulate the exact legacy field shape, not a new explicit standard profile.
	s.state.FixtureProfile, s.state.FixtureBytes = "", 0
	s.state.Stopped, s.state.Requests, s.state.API, s.state.Pre = "multipart_not_exercised", 31, 31, 1
	s.state.Writes["pre"] = true
	started := s.state.Started
	if s.save() != nil {
		t.Fatal("legacy synthetic save failed")
	}
	read := func(name string) []byte {
		t.Helper()
		raw, e := os.ReadFile(filepath.Join(s.dir, name))
		if e != nil {
			t.Fatal("synthetic immutable file missing")
		}
		return raw
	}
	before, key := read("multipart.enc"), read("multipart.key")
	s.lock.Close()
	if changed, e := openMultipartStore(s.dir, parent, multipartSmallProfile); e == nil {
		changed.lock.Close()
		t.Fatal("legacy stopped batch became small")
	}
	if !bytes.Equal(before, read("multipart.enc")) || !bytes.Equal(key, read("multipart.key")) {
		t.Fatal("rejected profile changed legacy state/key")
	}
	reopened, e := openMultipartStore(s.dir, parent)
	if e != nil {
		t.Fatal("legacy standard no longer readable")
	}
	defer reopened.lock.Close()
	if reopened.state.Started != started || reopened.state.Stopped != "multipart_not_exercised" || reopened.state.Requests != 31 || reopened.state.Pre != 1 || !reopened.state.Writes["pre"] || reopened.state.FixtureProfile != "" || reopened.state.FixtureBytes != 0 {
		t.Fatal("legacy reopen migrated or reset a frozen field")
	}
	if !bytes.Equal(before, read("multipart.enc")) || !bytes.Equal(key, read("multipart.key")) {
		t.Fatal("legacy readonly reopen rewrote originals")
	}
}

func TestQuarkMultipartIndependentSmallReadBudgetIsFrozenN(t *testing.T) {
	s, _ := multipartFixture(t, multipartSmallProfile)
	if multipartStateSize(s.state) != 8388609 || !multipartManifest(s.state) {
		t.Fatal("small profile mismatch")
	}
	for _, bad := range []multipartState{
		{FixtureProfile: multipartSmallProfile, FixtureBytes: multipartSize},
		{FixtureProfile: multipartStandardProfile, FixtureBytes: multipartSmallSize},
		{FixtureProfile: "unapproved", FixtureBytes: multipartSmallSize},
		{FixtureProfile: "", FixtureBytes: multipartSmallSize},
	} {
		if _, n := multipartFixtureProfile(bad); n != 0 {
			t.Fatal("profile/size mismatch accepted")
		}
	}
	g := newMultipartGuard(s)
	g.delay = 0
	u := "https://objects.example/small"
	s.state.ReadURLs[u] = multipartSmallSize
	calls := 0
	base := multipartTestTransport(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Header: http.Header{}, ContentLength: multipartSmallSize, Body: io.NopCloser(strings.NewReader("x"))}, nil
	})
	for n := 0; n < 4; n++ {
		r, _ := http.NewRequest("GET", u, nil)
		r.Header.Set("Accept-Encoding", "identity")
		if _, e := g.wrap(base, true).RoundTrip(r); e == nil {
			t.Fatal("partial small read accepted")
		}
	}
	if calls != 3 || s.state.ReadReserved != 3*multipartSmallSize || s.state.ReadActual != 3 || !multipartManifest(s.state) {
		t.Fatal("small reads used former large budget or refunded partial reads")
	}
}
