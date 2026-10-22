//go:build quark_live && windows

package handlers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

func nodeProbeOfflineGuard(t *testing.T) *nodeProbeGuard {
	t.Helper()
	root := observationOfflinePrivateDir(t)
	p := quark.SpaceProof{Version: 1, Generation: 1, TVAccount: "guard-tv", AgentAccount: "guard-agent", TVRoot: quark.QualifyID(quark.RouteTV, "guard-tv", "T"), AgentRoot: quark.QualifyID(quark.RouteAgent, "guard-agent", "A"), Path: "/owned", ContentSHA256: strings.Repeat("a", 64), VerifiedAt: time.Now().UTC()}
	i := followIdentity{TVAccount: p.TVAccount, AgentAccount: p.AgentAccount, Proof: p, ProofRootACK: "A"}
	store, c, err := nodeProbePrepareController(root, i)
	if err != nil {
		t.Fatal("synthetic controller")
	}
	t.Cleanup(func() { store.lock.Close() })
	g, err := openNodeProbeGuard(root, c.Spec)
	if err != nil {
		t.Fatal("synthetic guard")
	}
	t.Cleanup(func() { g.journal.lock.Close() })
	g.delay = 0
	g.bundle = quark.Credentials{Version: 1, Revision: 1, TV: &quark.Authorization{AccessToken: "guard-tv-secret"}, Agent: &quark.Authorization{AccessToken: "guard-agent-secret"}}
	return g
}

type nodeProbeTrackBody struct {
	io.Reader
	closed bool
	n      int64
}

func (b *nodeProbeTrackBody) Read(p []byte) (int, error) {
	n, e := b.Reader.Read(p)
	b.n += int64(n)
	return n, e
}
func (b *nodeProbeTrackBody) Close() error { b.closed = true; return nil }

func nodeProbeIdentityRequest() *http.Request {
	r, _ := http.NewRequest("GET", "https://open-api-drive.quark.cn/user?method=user_info&access_token=guard-tv-secret", nil)
	return r
}

func TestQuarkNodeProbeOfflineMetadataBodyClosedAndBudgetPersisted(t *testing.T) {
	g := nodeProbeOfflineGuard(t)
	body := &nodeProbeTrackBody{Reader: strings.NewReader(`{"status":0,"data":{}}`)}
	base := multipartTestTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, ContentLength: -1, Header: http.Header{}, Body: body}, nil
	})
	resp, e := g.wrap(base, false).RoundTrip(nodeProbeIdentityRequest())
	if e != nil || !body.closed {
		t.Fatal("native metadata body leaked")
	}
	data, e := io.ReadAll(resp.Body)
	resp.Body.Close()
	if e != nil || !json.Valid(data) {
		t.Fatal("replacement metadata response unusable")
	}
	g.state.Requests = nodeProbeRequestBudget
	g.state.API = nodeProbeAPIBudget
	if g.journal.save(g.state) != nil {
		t.Fatal("persist exhausted budget")
	}
	start := g.state.Spec.Started
	g.journal.lock.Close()
	reopened, e := openNodeProbeGuard(g.root, g.state.Spec)
	if e != nil {
		t.Fatal("reopen original budget")
	}
	g.journal = reopened.journal
	g.state = reopened.state
	if g.state.Spec.Started != start || g.state.Requests != nodeProbeRequestBudget || g.state.API != nodeProbeAPIBudget {
		t.Fatal("budget or deadline reset")
	}
	calls := 0
	deny := multipartTestTransport(func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("must not dispatch") })
	if _, e = g.wrap(deny, false).RoundTrip(nodeProbeIdentityRequest()); e == nil || calls != 0 {
		t.Fatal("exhausted budget dispatched")
	}
}

type nodeProbeOfflineOperations struct{ receipt cloud.OperationReceipt }

func (s *nodeProbeOfflineOperations) Create(context.Context, cloud.OperationReceipt) error {
	return errors.New("fixture forbids create")
}
func (s *nodeProbeOfflineOperations) Update(context.Context, uint64, cloud.OperationReceipt) error {
	return errors.New("fixture forbids update")
}
func (s *nodeProbeOfflineOperations) Load(_ context.Context, id string) (cloud.OperationReceipt, error) {
	if id != s.receipt.ID {
		return cloud.OperationReceipt{}, cloud.ErrOperationNotFound
	}
	return s.receipt, nil
}

func TestQuarkNodeProbeOfflineUnknownPutOnlyOriginalQuery(t *testing.T) {
	g := nodeProbeOfflineGuard(t)
	g.state.ChildAgent, g.state.ChildTV = "AD", "TD"
	p := largefilePayload{Parent: "AD", Name: g.state.Spec.UploadName, Size: nodeProbeSize, SHA256: g.state.Spec.SourceSHA, PartSize: 4 << 20, TaskID: "same-native-task", Phase: "put_inflight", InFlightPart: 1}
	raw, _ := json.Marshal(p)
	parent := quark.QualifyID(quark.RouteTV, g.state.Spec.Identity.TVAccount, "TD")
	ops := &nodeProbeOfflineOperations{receipt: cloud.OperationReceipt{ID: nodeProbeNativeID(g.state.Spec.operationID(), "upload", parent, p.Name), Provider: cloud.ProviderQuark, Route: string(quark.RouteAgent), AccountID: g.state.Spec.Identity.AgentAccount, Kind: "upload", State: "accepted", Payload: raw}}
	g.native = ops
	d := largefileDestination{Number: 1, URL: "https://objects.example/part1"}
	d.Signature.Value = "object-part-signature"
	g.state.Destinations[1] = d
	g.state.PartCount = 3
	putCalls, queryCalls := 0, 0
	base := multipartTestTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method == "PUT" {
			putCalls++
			n, e := io.Copy(io.Discard, r.Body)
			if e != nil || n != 4<<20 {
				t.Fatal("put did not read fixed source")
			}
			return nil, errors.New("upstream SECRET account URL")
		}
		queryCalls++
		return &http.Response{StatusCode: 200, ContentLength: -1, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"status":0,"data":{"uploaded_parts":[{"part_number":1,"etag":"confirmed-original"}]}}`))}, nil
	})
	put := func() error {
		f, e := os.Open(filepath.Join(g.root, "source", "sample.bin"))
		if e != nil {
			return e
		}
		defer f.Close()
		r, _ := http.NewRequest("PUT", d.URL, io.NopCloser(io.NewSectionReader(f, 0, 4<<20)))
		r.ContentLength = 4 << 20
		r.Header.Set("Authorization", d.Signature.Value)
		r.Header.Set("Accept-Encoding", "identity")
		_, e = g.wrap(base, true).RoundTrip(r)
		return e
	}
	if e := put(); e == nil || strings.Contains(e.Error(), "SECRET") || putCalls != 1 || g.state.UploadActual != 4<<20 || !g.state.Writes["part:1"] {
		t.Fatal("unknown ACK lost reservation/redaction")
	}
	initial := g.state
	g.journal.lock.Close()
	next, e := openNodeProbeGuard(g.root, g.state.Spec)
	if e != nil {
		t.Fatal("reopen unknown ACK")
	}
	g.journal = next.journal
	g.state = next.state
	if g.state.Requests != initial.Requests || g.state.UploadReserved != initial.UploadReserved || g.state.Spec.Started != initial.Spec.Started {
		t.Fatal("unknown ACK reset on reopen")
	}
	if put() == nil || putCalls != 1 {
		t.Fatal("unknown PUT replayed")
	}
	query := func(task string) error {
		data, _ := json.Marshal(map[string]string{"task_id": task})
		r, _ := http.NewRequest("POST", "https://open-api-drive.quark.cn/open/v1/file/list_uploaded_parts?access_token=guard-agent-secret", bytes.NewReader(data))
		resp, e := g.wrap(base, false).RoundTrip(r)
		if resp != nil {
			resp.Body.Close()
		}
		return e
	}
	if query("different-native-task") == nil || queryCalls != 0 {
		t.Fatal("foreign task reconciliation allowed")
	}
	if query(p.TaskID) != nil || queryCalls != 1 || g.state.ETags[1] != "confirmed-original" || g.state.Put != 1 {
		t.Fatal("original task not reconciled")
	}
}

func nodeProbeReadRequest() *http.Request {
	r, _ := http.NewRequest("GET", "https://objects.example/original", nil)
	r.Header.Set("Accept-Encoding", "identity")
	r.Header.Set("Range", fmt.Sprintf("bytes=0-%d", largefileReadChunk-1))
	return r
}
func TestQuarkNodeProbeOfflineReadFailureAndSecretGuards(t *testing.T) {
	for _, mode := range []string{"partial_close", "overlong", "corrupt", "short", "wrong_range", "cookie", "token", "open_range"} {
		t.Run(mode, func(t *testing.T) {
			g := nodeProbeOfflineGuard(t)
			g.state.ReadURLs["https://objects.example/original"] = nodeProbeSize
			f, e := os.Open(filepath.Join(g.root, "source", "sample.bin"))
			if e != nil {
				t.Fatal("source")
			}
			defer f.Close()
			var body io.Reader = io.NewSectionReader(f, 0, largefileReadChunk)
			switch mode {
			case "overlong":
				body = io.MultiReader(body, strings.NewReader("X"))
			case "short":
				body = strings.NewReader("short")
			case "corrupt":
				body = io.MultiReader(strings.NewReader("corrupt"), body)
			}
			tracked := &nodeProbeTrackBody{Reader: body}
			calls := 0
			base := multipartTestTransport(func(*http.Request) (*http.Response, error) {
				calls++
				cr := fmt.Sprintf("bytes 0-%d/%d", largefileReadChunk-1, nodeProbeSize)
				if mode == "wrong_range" {
					cr = "bytes 1-8388608/8388609"
				}
				return &http.Response{StatusCode: 206, ContentLength: largefileReadChunk, Header: http.Header{"Content-Range": {cr}}, Body: tracked}, nil
			})
			r := nodeProbeReadRequest()
			switch mode {
			case "cookie":
				r.Header.Set("Cookie", "secret")
			case "token":
				r.Header.Set("X-Other", "guard-agent-secret")
			case "open_range":
				r.Header.Set("Range", "bytes=0-")
			}
			resp, e := g.wrap(base, true).RoundTrip(r)
			if mode == "cookie" || mode == "token" || mode == "open_range" {
				if e == nil || calls != 0 {
					t.Fatal("unsafe request dispatched")
				}
				return
			}
			if mode == "wrong_range" {
				if e == nil || !tracked.closed || !g.state.ReadInFlight {
					t.Fatal("invalid range accepted/reservation released")
				}
				return
			}
			if e != nil {
				t.Fatal("unexpected header rejection")
			}
			if mode == "partial_close" {
				var b [1]byte
				_, e = resp.Body.Read(b[:])
				if e != nil {
					t.Fatal("initial source read")
				}
			} else {
				_, e = io.Copy(io.Discard, resp.Body)
				if e == nil {
					t.Fatal("invalid source read succeeded")
				}
			}
			if resp.Body.Close() == nil || g.state.Stopped == "" || !g.state.ReadInFlight || g.state.ReadReserved != largefileReadChunk || g.state.ReadPasses != 0 || !tracked.closed {
				t.Fatal("failed read not durable/failclosed")
			}
			if mode == "overlong" && (tracked.n != largefileReadChunk+1 || g.state.ReadActual != largefileReadChunk || g.state.Detection != 1) {
				t.Fatal("overflow exceeded one detection byte or counted as valid bytes")
			}
			g.journal.lock.Close()
			next, e := openNodeProbeGuard(g.root, g.state.Spec)
			if e != nil {
				t.Fatal("reopen stopped guard")
			}
			g.journal = next.journal
			g.state = next.state
			if _, e = g.wrap(base, true).RoundTrip(nodeProbeReadRequest()); e == nil || calls != 1 {
				t.Fatal("stopped read resumed/replayed")
			}
		})
	}
}

func TestQuarkNodeProbeOfflineCloseCancelsBlockedRead(t *testing.T) {
	g := nodeProbeOfflineGuard(t)
	g.state.ReadURLs["https://objects.example/original"] = nodeProbeSize
	b := &largefileBlockingBody{entered: make(chan struct{}), closed: make(chan struct{})}
	base := multipartTestTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 206, ContentLength: largefileReadChunk, Header: http.Header{"Content-Range": {fmt.Sprintf("bytes 0-%d/%d", largefileReadChunk-1, nodeProbeSize)}}, Body: b}, nil
	})
	resp, e := g.wrap(base, true).RoundTrip(nodeProbeReadRequest())
	if e != nil {
		t.Fatal("read setup")
	}
	readDone, closeDone := make(chan error, 1), make(chan error, 1)
	go func() { var p [1]byte; _, e := resp.Body.Read(p[:]); readDone <- e }()
	select {
	case <-b.entered:
	case <-time.After(time.Second):
		t.Fatal("read did not block")
	}
	go func() { closeDone <- resp.Body.Close() }()
	select {
	case e := <-closeDone:
		if e == nil {
			t.Fatal("partial Close succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("Close blocked")
	}
	select {
	case e := <-readDone:
		if e == nil {
			t.Fatal("cancelled Read succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("Read stayed blocked")
	}
}

func TestQuarkNodeProbeOfflineNegotiatedGeometry(t *testing.T) {
	for _, partSize := range []int64{4 << 20, 8 << 20} {
		g := nodeProbeOfflineGuard(t)
		raw, _ := json.Marshal(map[string]any{"status": 0, "data": map[string]any{"task_id": "native-task", "part_size": partSize}})
		r, _ := http.NewRequest("POST", "https://open-api-drive.quark.cn/open/v1/file/upload_pre", nil)
		if e := g.learn(r, raw, nil, "pre", 0); e != nil || g.state.PartCount != int((nodeProbeSize+partSize-1)/partSize) || g.state.Stopped != "" {
			t.Fatal("valid native geometry rejected")
		}
	}
}

func TestQuarkNodeProbeOfflineEarlyPutResponseClosed(t *testing.T) {
	g := nodeProbeOfflineGuard(t)
	g.state.ChildAgent, g.state.ChildTV = "AD", "TD"
	p := largefilePayload{Parent: "AD", Name: g.state.Spec.UploadName, Size: nodeProbeSize, SHA256: g.state.Spec.SourceSHA, PartSize: 4 << 20, TaskID: "original", Phase: "put_inflight", InFlightPart: 1}
	raw, _ := json.Marshal(p)
	parent := quark.QualifyID(quark.RouteTV, g.state.Spec.Identity.TVAccount, "TD")
	g.native = &nodeProbeOfflineOperations{receipt: cloud.OperationReceipt{ID: nodeProbeNativeID(g.state.Spec.operationID(), "upload", parent, p.Name), Provider: cloud.ProviderQuark, Route: string(quark.RouteAgent), AccountID: g.state.Spec.Identity.AgentAccount, Kind: "upload", State: "accepted", Payload: raw}}
	d := largefileDestination{Number: 1, URL: "https://objects.example/part1"}
	d.Signature.Value = "object-part-signature"
	g.state.Destinations[1] = d
	body := &nodeProbeTrackBody{Reader: strings.NewReader("early rejection")}
	base := multipartTestTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 403, ContentLength: -1, Header: http.Header{}, Body: body}, nil
	})
	f, e := os.Open(filepath.Join(g.root, "source", "sample.bin"))
	if e != nil {
		t.Fatal("source")
	}
	defer f.Close()
	r, _ := http.NewRequest("PUT", d.URL, io.NopCloser(io.NewSectionReader(f, 0, 4<<20)))
	r.ContentLength = 4 << 20
	r.Header.Set("Authorization", d.Signature.Value)
	r.Header.Set("Accept-Encoding", "identity")
	if _, e = g.wrap(base, true).RoundTrip(r); e == nil || !body.closed || body.n != 0 || g.state.Stopped != "put_bytes_unknown" || g.state.UploadReserved != 4<<20 {
		t.Fatal("early response leaked or unknown upload reservation cleared")
	}
}
