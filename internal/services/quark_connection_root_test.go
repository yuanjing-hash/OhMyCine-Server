package services

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
)

// Exercise the production driver, encrypted SQLite receipts and credential
// persister together. No provider request can leave this local TLS fixture.
func TestQuarkRootVerificationNativePersistentRecovery(t *testing.T) {
	s, actor, created := createQuarkConnection(t)
	if err := s.registry.Register(cloud.ProviderQuark, quark.New); err != nil {
		t.Fatal(err)
	}
	saveQuarkTestBundle(t, s, created.ID, func(c *quark.Credentials) {
		c.TV, c.Agent = testQuarkAuthorization(), testQuarkAuthorization()
		c.TV.AccountID, c.Agent.AccountID = "tv-private-account", "agent-private-account"
	})
	var mu sync.Mutex
	var content []byte
	name := ""
	uploaded := false
	rejectManagement := false
	pre, puts, finish, reads := 0, 0, 0, 0
	fixture := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		file := func(id, parent, name string, dir bool) map[string]any {
			kind, size := "1", 32
			if dir {
				kind, size = "0", 0
			}
			return map[string]any{"fid": id, "parent_fid": parent, "filename": name, "file_type": kind, "size": size}
		}
		var data any
		switch r.URL.Path {
		case "/file":
			if r.URL.Query().Get("method") == "download" {
				if r.URL.Query().Get("fid") != "tv-created" {
					t.Error("non-TV file handle used for original read")
				}
				data = map[string]any{"fid": "tv-created", "file_name": name, "size": 32, "download_url": "https://objects.example/read"}
				break
			}
			files := []any{}
			switch r.URL.Query().Get("parent_fid") {
			case "0":
				files = append(files, map[string]any{"fid": "tv-root", "filename": "媒体库", "size": 0, "isdir": 1})
			case "tv-root":
				if uploaded {
					files = append(files, map[string]any{"fid": "tv-created", "filename": name, "size": 32, "isdir": 0})
				}
			default:
				t.Error("unexpected TV directory")
			}
			data = map[string]any{"total_count": len(files), "files": files}
		case "/open/v1/file/list":
			if rejectManagement {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{"status": -1, "errno": 23018, "message": "private provider URL or token"})
				return
			}
			var input struct {
				Parent string `json:"parent_fid"`
			}
			_ = json.NewDecoder(r.Body).Decode(&input)
			files := []any{}
			if input.Parent == "0" {
				files = append(files, file("listed|root", "0", "媒体库", true))
			} else if input.Parent == "listed|root" && uploaded {
				files = append(files, file("listed|created", input.Parent, name, false))
			}
			data = map[string]any{"last_page": true, "file_list": files}
		case "/open/v1/file/upload_pre":
			pre++
			var input struct {
				Name string `json:"file_name"`
				Size int    `json:"size"`
			}
			_ = json.NewDecoder(r.Body).Decode(&input)
			name = input.Name
			if input.Size != 32 || !quark.IsVerificationName(name) {
				t.Error("invalid challenge upload")
			}
			data = map[string]any{"task_id": "proof-upload", "part_size": 64, "common_headers": map[string]string{}, "upload_urls": []any{map[string]any{"part_number": 1, "upload_url": "https://objects.example/put", "signature_info": map[string]string{"signature": "object-fixture"}}}}
		case "/put":
			puts++
			content, _ = io.ReadAll(io.LimitReader(r.Body, 33))
			w.Header().Set("ETag", "fixture-part")
			w.WriteHeader(http.StatusOK)
			return
		case "/open/v1/file/upload_finish":
			finish++
			uploaded = true
			data = map[string]any{"fid": "created-ack", "finish": true}
		case "/open/v1/file/info":
			data = file("created-ack", "listed|root", name, false)
		case "/read":
			reads++
			if r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" || r.Header.Get("Range") != "bytes=0-31" {
				t.Error("challenge read not anonymous and bounded")
			}
			w.Header().Set("Content-Range", "bytes 0-31/32")
			w.Header().Set("Content-Length", "32")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(content)
			return
		default:
			t.Error("unexpected endpoint")
			http.Error(w, "unexpected", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": 0, "data": data})
	}))
	t.Cleanup(fixture.Close)
	oldTransport := http.DefaultTransport
	transport := oldTransport.(*http.Transport).Clone()
	roots := x509.NewCertPool()
	roots.AddCert(fixture.Certificate())
	tlsConfig := &tls.Config{RootCAs: roots, ServerName: fixture.Certificate().DNSNames[0], MinVersion: tls.VersionTLS12}
	transport.Proxy = nil
	transport.DialTLSContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&tls.Dialer{NetDialer: &net.Dialer{}, Config: tlsConfig}).DialContext(ctx, network, fixture.Listener.Addr().String())
	}
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = oldTransport; transport.CloseIdleConnections() })
	root := quark.QualifyID(quark.RouteTV, "tv-private-account", "tv-root")
	for attempt := 0; attempt < 2; attempt++ {
		if attempt == 1 {
			s.invalidate(created.ID)
		}
		result, err := s.VerifyQuarkRoot(context.Background(), actor, created.ID, root, "/媒体库", RequestContext{})
		if err != nil || !result.Verified {
			t.Fatalf("attempt %d: verification failed %v (pre=%d puts=%d finish=%d reads=%d)", attempt, err, pre, puts, finish, reads)
		}
	}
	if pre != 1 || puts != 1 || finish != 1 || reads != 2 {
		t.Fatalf("challenge replayed: pre=%d puts=%d finish=%d reads=%d", pre, puts, finish, reads)
	}
	var receipts []models.QuarkOperationReceipt
	if err := s.db.Find(&receipts).Error; err != nil {
		t.Fatal(err)
	}
	if len(receipts) != 2 {
		t.Fatalf("expected proof and upload receipts, got %d", len(receipts))
	}
	for _, receipt := range receipts {
		if receipt.State != "confirmed" || bytes.Contains(receipt.CheckpointCiphertext, []byte(name)) || bytes.Contains(receipt.CheckpointCiphertext, []byte("private-account")) {
			t.Fatal("receipt not confirmed and encrypted")
		}
	}
	status, err := s.QuarkStatus(context.Background(), actor, created.ID)
	if err != nil || status.VerifiedRoots != 1 || !status.Capabilities.FileUpload {
		t.Fatalf("verified capabilities missing: %v", err)
	}
	raw, _ := json.Marshal(status)
	if strings.Contains(string(raw), "private-account") || strings.Contains(string(raw), name) {
		t.Fatal("proof identities leaked")
	}
	// Exercise the public service path, not only the diagnostic formatter. A
	// provider rejection after successful authorizations must retain its phase.
	mu.Lock()
	rejectManagement = true
	mu.Unlock()
	s.invalidate(created.ID)
	_, err = s.VerifyQuarkRoot(context.Background(), actor, created.ID, root, "/媒体库", RequestContext{})
	if ErrorCode(err) != CodeConnectionUnavailable || !strings.Contains(ErrorMessage(err), "管理授权下的目录") || !strings.Contains(ErrorMessage(err), "HTTP 400") || !strings.Contains(ErrorMessage(err), "23018") || strings.Contains(ErrorMessage(err), "private") {
		t.Fatalf("native provider failure was flattened: %v", err)
	}
	if pre != 1 || puts != 1 || finish != 1 || reads != 2 {
		t.Fatal("rejected directory listing replayed the confirmed upload")
	}
}
