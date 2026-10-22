//go:build quark_live

package handlers_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
)

type largefileZeroReader struct{}

func (largefileZeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }

func largefileOfflineStore(t *testing.T) (*largefileStore, *followParent) {
	t.Helper()
	p := quark.SpaceProof{Version: 1, Generation: 1, TVAccount: "tv-test", AgentAccount: "agent-test", TVRoot: quark.QualifyID(quark.RouteTV, "tv-test", "T"), AgentRoot: quark.QualifyID(quark.RouteAgent, "agent-test", "A"), Path: "/owned", ReceiptID: "old-proof", ContentSHA256: strings.Repeat("a", 64), VerifiedAt: time.Now().UTC().Add(-time.Hour)}
	b := quark.Credentials{Version: 1, Revision: 1, TV: &quark.Authorization{AccessToken: "tv-test-secret", DeviceID: "tv-device", AccountID: p.TVAccount, Generation: 1, IdentityVerified: true}, Agent: &quark.Authorization{AccessToken: "agent-test-secret", DeviceID: "agent-device", ClientDeviceID: "client-device", AccountID: p.AgentAccount, Generation: 1, IdentityVerified: true}, Proofs: []quark.SpaceProof{p}}
	parent := &followParent{identity: followIdentity{AgentAccount: p.AgentAccount, TVAccount: p.TVAccount, Proof: p, ProofRootACK: "A", JournalSHA: strings.Repeat("b", 64), KeySHA: strings.Repeat("c", 64)}, store: &observationStore{state: observationState{Bundle: b, Completed: true}}}
	s, e := openLargeFileStore(observationOfflinePrivateDir(t), parent)
	if e != nil {
		t.Fatal("synthetic store")
	}
	t.Cleanup(func() { s.lock.Close() })
	return s, parent
}

// The sparse synthetic source exercises offsets greater than signed 32-bit.
// This is not a provider upload or a real-account result.
func largefileSparseSource(t *testing.T, s *largefileStore) {
	t.Helper()
	f, e := os.OpenFile(filepath.Join(s.dir, "sample.bin"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		t.Fatal(e)
	}
	if f.Truncate(largefileSize) != nil || f.Close() != nil {
		t.Fatal("synthetic sparse source")
	}
	s.state.SourceReady = true
	s.state.SourceSHA = strings.Repeat("a", 64)
	s.state.RootVerified = true
	s.state.ChildAgent, s.state.ChildTV = "A", "T"
}

func TestQuarkLargeFileOfflineFrozenProfileAndPreflight(t *testing.T) {
	s, parent := largefileOfflineStore(t)
	if !largefileManifest(s.state) || s.state.SourceReady || s.state.SourceSHA != "" {
		t.Fatal("preflight must not create source")
	}
	if _, e := os.Stat(filepath.Join(s.dir, "sample.bin")); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("source exists before authorization")
	}
	initial := s.state.Started
	s.state.Requests, s.state.API = 2, 2
	if s.save() != nil {
		t.Fatal("save")
	}
	s.lock.Close()
	if _, e := openLargeFileStore(s.dir, parent, multipartFinalSmallProfile); e == nil {
		t.Fatal("small batch profile accepted")
	}
	reopened, e := openLargeFileStore(s.dir, parent)
	if e != nil {
		t.Fatal("reopen")
	}
	s.lock = reopened.lock
	if reopened.state.Started != initial || reopened.state.Requests != 2 || reopened.state.API != 2 || reopened.state.SourceReady {
		t.Fatal("preflight counters/deadline changed")
	}
	for _, size := range []int64{multipartSmallSize, 1 << 31, largefileSize + 1} {
		st := s.state
		st.FixtureBytes = size
		if largefileManifest(st) {
			t.Fatal("changed size accepted")
		}
	}
	for _, parts := range []struct {
		part  int64
		count int
		valid bool
	}{{4 << 20, 513, true}, {8 << 20, 257, true}, {2 << 20, 1025, false}, {1 << 30, 3, true}, {largefileSize, 0, false}, {63, 0, false}} {
		count, valid := largefilePlan(largefilePayload{Size: largefileSize, PartSize: parts.part})
		if valid != parts.valid || valid && count != parts.count {
			t.Fatalf("part geometry %d", parts.part)
		}
	}
	if largefileMaxReads != 771 || largefileSize <= 1<<31 {
		t.Fatal("large boundary or read budget")
	}
	// Never consume randomness again when partial source preparation is present.
	if os.WriteFile(filepath.Join(s.dir, "sample.bin"), []byte("partial"), 0600) != nil {
		t.Fatal("partial")
	}
	if s.prepareSource(context.Background()) == nil {
		t.Fatal("partial source regenerated")
	}
}

func TestQuarkLargeFileOfflinePartBeyondTwoGiBAndUnknownAck(t *testing.T) {
	s, _ := largefileOfflineStore(t)
	largefileSparseSource(t, s)
	g := newLargeFileGuard(s)
	g.delay = 0
	p := largefilePayload{Parent: "A", Name: s.state.UploadName, Size: largefileSize, SHA256: s.state.SourceSHA, PartSize: 4 << 20, TaskID: "original", Phase: "put_inflight", InFlightPart: 513}
	payload, _ := json.Marshal(p)
	s.state.Operations[s.state.UploadID] = cloud.OperationReceipt{ID: s.state.UploadID, Provider: cloud.ProviderQuark, Route: string(quark.RouteAgent), AccountID: s.state.Identity.AgentAccount, Kind: "upload", Revision: 1, State: "accepted", Payload: payload}
	s.state.Eligible = true
	destination := largefileDestination{Number: 513, URL: "https://objects.example/part513"}
	destination.Signature.Value = "object-only-signature"
	s.state.Destinations[513] = destination
	calls := 0
	base := multipartTestTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		buffer := make([]byte, 1<<20)
		n, e := io.CopyBuffer(io.Discard, r.Body, buffer)
		if e != nil || n != 1<<20 {
			return nil, largefileError()
		}
		return nil, errors.New("https://private.invalid/?token=SECRET")
	})
	put := func() error {
		r, _ := http.NewRequest("PUT", destination.URL, io.NopCloser(io.LimitReader(largefileZeroReader{}, 1<<20)))
		r.ContentLength = 1 << 20
		r.Header.Set("Authorization", destination.Signature.Value)
		r.Header.Set("Accept-Encoding", "identity")
		_, e := g.wrap(base, true).RoundTrip(r)
		return e
	}
	if e := put(); e == nil || strings.Contains(e.Error(), "SECRET") || calls != 1 || s.state.Put != 1 || s.state.UploadReserved != 1<<20 || s.state.UploadActual != 1<<20 {
		t.Fatal("unknown PUT accounting/redaction")
	}
	if put() == nil || calls != 1 {
		t.Fatal("unknown PUT replayed")
	}
}

func TestQuarkLargeFileOfflineStreamingReadFullDigest(t *testing.T) {
	s, _ := largefileOfflineStore(t)
	largefileSparseSource(t, s)
	// Resume the rolling hash immediately before the final >2GiB window.
	h := sha256.New()
	if n, e := io.CopyN(h, largefileZeroReader{}, 2<<30); e != nil || n != 2<<30 {
		t.Fatal("hash setup")
	}
	s.state.ReadHashState, _ = h.(encoding.BinaryMarshaler).MarshalBinary()
	s.state.ReadOffset, s.state.ReadReserved, s.state.ReadActual = 2<<30, 2<<30, 2<<30
	s.state.Reads = 256
	if _, e := io.CopyN(h, largefileZeroReader{}, 1<<20); e != nil {
		t.Fatal("hash tail")
	}
	s.state.SourceSHA = hex.EncodeToString(h.Sum(nil))
	u := "https://objects.example/original"
	s.state.ReadURLs[u] = largefileSize
	g := newLargeFileGuard(s)
	g.delay = 0
	calls := 0
	base := multipartTestTransport(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 206, ContentLength: 1 << 20, Header: http.Header{"Content-Range": {fmt.Sprintf("bytes %d-%d/%d", int64(2<<30), largefileSize-1, largefileSize)}}, Body: io.NopCloser(io.LimitReader(largefileZeroReader{}, 1<<20))}, nil
	})
	r, _ := http.NewRequest("GET", u, nil)
	r.Header.Set("Accept-Encoding", "identity")
	r.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", int64(2<<30), largefileSize-1))
	resp, e := g.wrap(base, true).RoundTrip(r)
	if e != nil {
		t.Fatal("bounded final read")
	}
	n, e := io.Copy(io.Discard, resp.Body)
	closeErr := resp.Body.Close()
	if e != nil || closeErr != nil || n != 1<<20 || s.state.ReadActual != largefileSize || s.state.ReadReserved != largefileSize || s.state.ReadPasses != 1 || s.state.ReadOffset != 0 || len(s.state.ReadHashState) != 0 || s.state.ReadInFlight {
		t.Fatal("full digest/stream counters")
	}
	if _, e = g.wrap(base, true).RoundTrip(r); e == nil || calls != 1 {
		t.Fatal("repeated tail accepted")
	}
}

func TestQuarkLargeFileOfflineReadRejectsCorruptionAndSecrets(t *testing.T) {
	for _, mode := range []string{"short", "corrupt", "overlong", "wrong_range", "cookie", "open_range"} {
		t.Run(mode, func(t *testing.T) {
			s, _ := largefileOfflineStore(t)
			largefileSparseSource(t, s)
			g := newLargeFileGuard(s)
			g.delay = 0
			u := "https://objects.example/original"
			s.state.ReadURLs[u] = largefileSize
			calls := 0
			base := multipartTestTransport(func(*http.Request) (*http.Response, error) {
				calls++
				body := io.Reader(io.LimitReader(largefileZeroReader{}, largefileReadChunk))
				if mode == "short" {
					body = strings.NewReader("short")
				}
				if mode == "corrupt" {
					body = io.MultiReader(bytes.NewReader([]byte{1}), io.LimitReader(largefileZeroReader{}, largefileReadChunk-1))
				}
				if mode == "overlong" {
					body = io.LimitReader(largefileZeroReader{}, largefileReadChunk+1)
				}
				cr := fmt.Sprintf("bytes 0-%d/%d", largefileReadChunk-1, largefileSize)
				if mode == "wrong_range" {
					cr = "bytes 1-8388608/2148532224"
				}
				return &http.Response{StatusCode: 206, ContentLength: largefileReadChunk, Header: http.Header{"Content-Range": {cr}}, Body: io.NopCloser(body)}, nil
			})
			r, _ := http.NewRequest("GET", u, nil)
			r.Header.Set("Accept-Encoding", "identity")
			r.Header.Set("Range", fmt.Sprintf("bytes=0-%d", largefileReadChunk-1))
			if mode == "cookie" {
				r.Header.Set("Cookie", "secret")
			}
			if mode == "open_range" {
				r.Header.Set("Range", "bytes=0-")
			}
			resp, e := g.wrap(base, true).RoundTrip(r)
			if mode == "short" || mode == "corrupt" || mode == "overlong" {
				if e != nil {
					t.Fatal("unexpected header failure")
				}
				_, readErr := io.Copy(io.Discard, resp.Body)
				closeErr := resp.Body.Close()
				if readErr == nil || closeErr == nil || s.state.ReadReserved != largefileReadChunk || s.state.ReadPasses != 0 || !s.state.ReadInFlight || s.state.Stopped == "" {
					t.Fatal("failed read refunded/confirmed")
				}
				if mode == "overlong" && (s.state.ReadDetectionActual != 1 || s.state.ReadActual != largefileReadChunk) {
					t.Fatal("overflow byte not separated")
				}
			} else if e == nil {
				resp.Body.Close()
				t.Fatal("invalid read admitted")
			}
			if (mode == "cookie" || mode == "open_range") && calls != 0 {
				t.Fatal("secret/open range dispatched")
			}
		})
	}
}

func TestQuarkLargeFileOfflineHardBudgetAndDeadline(t *testing.T) {
	s, _ := largefileOfflineStore(t)
	g := newLargeFileGuard(s)
	g.delay = 0
	calls := 0
	base := multipartTestTransport(func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("upstream SECRET") })
	request := func() error {
		r, _ := http.NewRequest("GET", "https://open-api-drive.quark.cn/user?method=user_info", nil)
		_, e := g.wrap(base, false).RoundTrip(r)
		return e
	}
	s.state.API = largefileMaxAPI
	if request() == nil || calls != 0 {
		t.Fatal("API cap")
	}
	s.state.API = 0
	s.state.Requests = largefileMaxRequests
	if request() == nil || calls != 0 {
		t.Fatal("request cap")
	}
	s.state.Requests = 0
	s.state.Started = time.Now().Add(-largefileLifetime)
	if request() == nil || calls != 0 {
		t.Fatal("deadline")
	}
	s.state.Started = time.Now()
	if e := request(); e == nil || strings.Contains(e.Error(), "SECRET") || calls != 1 {
		t.Fatal("redaction")
	}
}

func TestQuarkLargeFileOfflineIndependentPaths(t *testing.T) {
	if runtime.GOOS == "windows" {
		for _, c := range []struct {
			a, b string
			good bool
		}{{`C:\Users\test\state`, `D:\test\batch`, true}, {`C:\state`, `C:\state`, false}, {`C:\state`, `c:\STATE\child`, false}, {`C:\state\child`, `c:\STATE`, false}, {`C:\state`, `C:\state2`, true}} {
			if largefilePathScopesIndependent(c.a, c.b) != c.good {
				t.Fatalf("path geometry %q %q", c.a, c.b)
			}
		}
	}
	a, b := observationOfflinePrivateDir(t), observationOfflinePrivateDir(t)
	if !largefilePathScopesIndependent(a, b) || largefilePathScopesIndependent(a, filepath.Join(a, "child")) {
		t.Fatal("ordinary path overlap")
	}
	link := filepath.Join(b, "alias")
	if e := os.Symlink(a, link); e != nil {
		t.Skip("directory symlink creation unavailable for this Windows token")
	}
	if observationCheckPrivateDirectory(link) == nil {
		t.Fatal("reparse directory accepted")
	}
}

type largefileBlockingBody struct {
	entered chan struct{}
	closed  chan struct{}
	once    sync.Once
}

func (b *largefileBlockingBody) Read([]byte) (int, error) {
	b.once.Do(func() { close(b.entered) })
	<-b.closed
	return 0, io.ErrClosedPipe
}
func (b *largefileBlockingBody) Close() error {
	select {
	case <-b.closed:
	default:
		close(b.closed)
	}
	return nil
}

func TestQuarkLargeFileOfflineCloseInterruptsRead(t *testing.T) {
	s, _ := largefileOfflineStore(t)
	largefileSparseSource(t, s)
	g := newLargeFileGuard(s)
	g.delay = 0
	u := "https://objects.example/original"
	s.state.ReadURLs[u] = largefileSize
	b := &largefileBlockingBody{entered: make(chan struct{}), closed: make(chan struct{})}
	base := multipartTestTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 206, ContentLength: largefileReadChunk, Header: http.Header{"Content-Range": {fmt.Sprintf("bytes 0-%d/%d", largefileReadChunk-1, largefileSize)}}, Body: b}, nil
	})
	r, _ := http.NewRequest("GET", u, nil)
	r.Header.Set("Accept-Encoding", "identity")
	r.Header.Set("Range", fmt.Sprintf("bytes=0-%d", largefileReadChunk-1))
	resp, e := g.wrap(base, true).RoundTrip(r)
	if e != nil {
		t.Fatal("stream setup")
	}
	readDone, closeDone := make(chan error, 1), make(chan error, 1)
	go func() { var out [1]byte; _, e := resp.Body.Read(out[:]); readDone <- e }()
	select {
	case <-b.entered:
	case <-time.After(time.Second):
		t.Fatal("read did not block")
	}
	go func() { closeDone <- resp.Body.Close() }()
	select {
	case e := <-closeDone:
		if e == nil {
			t.Fatal("partial read closed successfully")
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not interrupt Read")
	}
	select {
	case e := <-readDone:
		if e == nil {
			t.Fatal("cancelled read succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("Read remained blocked")
	}
	if s.state.ReadReserved != largefileReadChunk || !s.state.ReadInFlight || s.state.Stopped == "" {
		t.Fatal("interrupted reservation erased")
	}
}
