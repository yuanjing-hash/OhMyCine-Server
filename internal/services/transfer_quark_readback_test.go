package services

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
)

type quarkReadbackOperations struct {
	mu   sync.Mutex
	rows map[string]cloud.OperationReceipt
}

func (s *quarkReadbackOperations) Load(_ context.Context, id string) (cloud.OperationReceipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[id]
	if !ok {
		return r, cloud.ErrOperationNotFound
	}
	return r, nil
}
func (s *quarkReadbackOperations) Create(_ context.Context, r cloud.OperationReceipt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.rows[r.ID]; ok {
		return errors.New("duplicate receipt")
	}
	s.rows[r.ID] = r
	return nil
}
func (s *quarkReadbackOperations) Update(_ context.Context, revision uint64, r cloud.OperationReceipt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rows[r.ID].Revision != revision || r.Revision != revision+1 {
		return errors.New("receipt revision conflict")
	}
	s.rows[r.ID] = r
	return nil
}

// Both API namespaces and original-file reads use a verified local TLS server.
// DialTLSContext pins every connection, including the native media transport,
// to this listener. No DNS, provider account or external endpoint is involved.
type quarkUploadReadbackFixture struct {
	created, corrupt, duplicate, replacedRoot, changedRevision atomic.Bool
	submits, reads, readBytes                                  atomic.Int64
	mode                                                       string
	sameDomain                                                 bool
	cancel                                                     context.CancelFunc
	store                                                      *quarkReadbackOperations
	credentials                                                json.RawMessage
	root                                                       string
}

func newQuarkUploadReadbackFixture(t *testing.T, mode string) *quarkUploadReadbackFixture {
	t.Helper()
	f := &quarkUploadReadbackFixture{mode: mode, sameDomain: mode == "same-domain", store: &quarkReadbackOperations{rows: map[string]cloud.OperationReceipt{}}}
	fixture := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var data any
		file := func(id, parent, name string, dir bool) map[string]any {
			kind, size := "1", 3
			if dir {
				kind, size = "0", 0
			}
			return map[string]any{"fid": id, "parent_fid": parent, "filename": name, "file_type": kind, "size": size}
		}
		switch r.URL.Path {
		case "/open/v1/file/info":
			id := r.URL.Query().Get("fid")
			switch id {
			case "list|root", "ack-root":
				data = file(id, "0", "library", true)
			case "creation-ack":
				parent := "ack-root"
				if f.sameDomain {
					parent = "list|root"
				}
				data = file(id, parent, "created.bin", false)
			default:
				t.Error("unplanned management identity")
				http.Error(w, "unexpected", 500)
				return
			}
		case "/open/v1/file/list":
			var input struct {
				Parent string `json:"parent_fid"`
			}
			_ = json.NewDecoder(r.Body).Decode(&input)
			items := []any{}
			if input.Parent == "0" {
				items = append(items, file("list|root", "0", "library", true))
			} else if input.Parent == "list|root" {
				if f.created.Load() {
					id := "list|created"
					if f.sameDomain {
						id = "creation-ack"
					}
					items = append(items, file(id, input.Parent, "created.bin", false))
				}
			} else {
				t.Error("unplanned management directory")
				http.Error(w, "unexpected", 500)
				return
			}
			data = map[string]any{"last_page": true, "file_list": items}
		case "/open/v1/file/upload_pre":
			f.submits.Add(1)
			f.created.Store(true)
			data = map[string]any{"fid": "creation-ack", "finish": true}
		case "/file":
			if r.URL.Query().Get("method") == "download" {
				if r.URL.Query().Get("fid") != "tv-created" {
					t.Error("non-TV identity used for original read")
				}
				data = map[string]any{"fid": "tv-created", "file_name": "created.bin", "size": 3, "download_url": "https://objects.example/readback"}
				break
			}
			items := []any{}
			switch r.URL.Query().Get("parent_fid") {
			case "0":
				id := "T"
				if f.replacedRoot.Load() {
					id = "replacement"
				}
				items = append(items, map[string]any{"fid": id, "filename": "library", "isdir": 1, "size": 0})
			case "T":
				if f.created.Load() {
					version := "v1"
					if f.changedRevision.Load() {
						version = "v2"
					}
					items = append(items, map[string]any{"fid": "tv-created", "filename": "created.bin", "isdir": 0, "size": 3, "version": version})
					if f.duplicate.Load() {
						items = append(items, map[string]any{"fid": "other", "filename": "created.bin", "isdir": 0, "size": 3, "version": version})
					}
				}
			default:
				t.Error("unplanned TV directory")
				http.Error(w, "unexpected", 500)
				return
			}
			data = map[string]any{"total_count": len(items), "files": items}
		case "/readback":
			if r.Method != http.MethodGet || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("Range") != "bytes=0-2" {
				t.Error("original read leaked acquisition headers or lost bounded range")
			}
			n := f.reads.Add(1)
			if n == 1 && !f.sameDomain {
				switch f.mode {
				case "root-drift":
					f.replacedRoot.Store(true)
				case "revision-drift":
					f.changedRevision.Store(true)
				case "duplicate":
					f.duplicate.Store(true)
				}
			}
			if n == 2 && f.cancel != nil {
				f.cancel()
				<-r.Context().Done()
				return
			}
			body := []byte("abc")
			if n > 1 && f.corrupt.Load() {
				body = []byte("xyz")
			}
			w.Header().Set("Content-Range", "bytes 0-2/3")
			w.Header().Set("Content-Length", "3")
			w.WriteHeader(http.StatusPartialContent)
			written, _ := w.Write(body)
			f.readBytes.Add(int64(written))
			return
		default:
			t.Error("unplanned provider or renewal endpoint")
			http.Error(w, "unexpected", 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": 0, "data": data})
	}))
	t.Cleanup(fixture.Close)
	old := http.DefaultTransport
	transport := old.(*http.Transport).Clone()
	roots := x509.NewCertPool()
	roots.AddCert(fixture.Certificate())
	config := &tls.Config{RootCAs: roots, ServerName: fixture.Certificate().DNSNames[0], MinVersion: tls.VersionTLS12}
	transport.Proxy = nil
	transport.DialTLSContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&tls.Dialer{NetDialer: &net.Dialer{}, Config: config}).DialContext(ctx, network, fixture.Listener.Addr().String())
	}
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = old; transport.CloseIdleConnections() })
	tv := &quark.Authorization{AccountID: "tv-account", AccessToken: "fixture-tv", DeviceID: "fixture-tv-device", Generation: 1, IdentityVerified: true}
	agent := &quark.Authorization{AccountID: "agent-account", AccessToken: "fixture-agent", DeviceID: "fixture-agent-device", ClientDeviceID: "fixture-agent-client", Generation: 1, IdentityVerified: true}
	f.root = quark.QualifyID(quark.RouteTV, tv.AccountID, "T")
	proof := quark.SpaceProof{Version: 1, Generation: 1, TVAccount: tv.AccountID, AgentAccount: agent.AccountID, TVRoot: f.root, AgentRoot: quark.QualifyID(quark.RouteAgent, agent.AccountID, "list|root"), Path: "/library", ReceiptID: "fixture-proof", ContentSHA256: strings.Repeat("a", 64), VerifiedAt: time.Now()}
	f.credentials, _ = json.Marshal(quark.Credentials{Version: 1, Revision: 1, TV: tv, Agent: agent, Proofs: []quark.SpaceProof{proof}})
	return f
}

func (f *quarkUploadReadbackFixture) driver(t *testing.T) *quarkTransferDriver {
	t.Helper()
	client, err := quark.NewClient(cloud.Config{Credentials: f.credentials, Operations: f.store})
	if err != nil {
		t.Fatal(err)
	}
	d, err := transferCloudDriver(client, "readback-task")
	if err != nil {
		t.Fatal(err)
	}
	return d.(*quarkTransferDriver)
}

func TestQuarkNativeTransferUploadUsesTwoCompleteReadbacks(t *testing.T) {
	for _, mode := range []string{"cross-domain", "same-domain", "corruption", "root-drift", "revision-drift", "duplicate", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			f := newQuarkUploadReadbackFixture(t, mode)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "corruption" {
				f.corrupt.Store(true)
			}
			if mode == "cancel" {
				f.cancel = cancel
			}
			upload := func(d *quarkTransferDriver, ctx context.Context) (cloud.Item, error) {
				return d.Upload(ctx, cloud.UploadRequest{ParentID: f.root, Name: "created.bin", Size: 3, Reader: bytes.NewReader([]byte("abc"))})
			}
			d := f.driver(t)
			item, err := upload(d, ctx)
			success := mode == "cross-domain" || mode == "same-domain"
			if (err == nil) != success || f.submits.Load() != 1 {
				t.Fatalf("mode=%s submits=%d error=%v", mode, f.submits.Load(), err)
			}
			if success {
				want := int64(2)
				if f.sameDomain {
					want = 1
				}
				if f.reads.Load() != want || f.readBytes.Load() != want*3 || item.ID != quark.QualifyID(quark.RouteTV, "tv-account", "tv-created") || !d.verifiedUploads[item.ID].ValidReadbackSHA256("ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad") {
					t.Fatalf("unverified result or duplicated native reads: reads=%d bytes=%d", f.reads.Load(), f.readBytes.Load())
				}
			} else if len(d.verifiedUploads) != 0 {
				t.Fatal("failed candidate/readback supplied delivery proof")
			}
			// Recreate both wrapper and native adapter from the original private
			// receipt. Even a successful previous hash is not fresh content proof.
			f.corrupt.Store(false)
			f.replacedRoot.Store(false)
			f.changedRevision.Store(false)
			f.duplicate.Store(false)
			before := f.reads.Load()
			d = f.driver(t)
			item, err = upload(d, context.Background())
			if err != nil || f.submits.Load() != 1 || f.reads.Load() != before+1 || len(d.verifiedUploads) != 1 {
				t.Fatalf("cold recovery bypassed final read or repeated upload: reads=%d before=%d submits=%d error=%v", f.reads.Load(), before, f.submits.Load(), err)
			}
		})
	}
}
