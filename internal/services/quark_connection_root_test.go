package services

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
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
	testQuarkRootVerificationNativePersistentRecovery(t, "finish")
}

func TestQuarkRootVerificationNativePersistentUnknownPartRecovery(t *testing.T) {
	for _, recovery := range []string{"completed_task", "exact_original_file"} {
		t.Run(recovery, func(t *testing.T) {
			testQuarkRootVerificationNativePersistentRecovery(t, recovery)
		})
	}
}

func TestQuarkRootExplicitIndependentNativeProofAndCleanup(t *testing.T) {
	testQuarkRootVerificationNativePersistentRecovery(t, "independent_attempt")
}

func testQuarkRootVerificationNativePersistentRecovery(t *testing.T, recovery string) {
	t.Helper()
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
	pre, puts, finish, reads, deletes := 0, 0, 0, 0, 0
	var preMD5, preSHA1 string
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
				Name     string `json:"file_name"`
				Size     int    `json:"size"`
				MD5      string `json:"md5"`
				SHA1     string `json:"sha1"`
				Parallel bool   `json:"parallel_upload"`
				Parts    []struct {
					Number  int `json:"part_number"`
					Size    int `json:"part_size"`
					Context struct {
						Offset int      `json:"part_offset"`
						H      []uint32 `json:"h"`
					} `json:"parallel_sha1_ctx"`
				} `json:"part_info_list"`
			}
			_ = json.NewDecoder(r.Body).Decode(&input)
			name = input.Name
			preMD5, preSHA1 = input.MD5, input.SHA1
			if input.Size != 32 || !quark.IsVerificationName(name) {
				t.Error("invalid challenge upload")
			}
			if recovery == "independent_attempt" && (!input.Parallel || len(input.Parts) != 1 || input.Parts[0].Number != 1 || input.Parts[0].Size != 32 || input.Parts[0].Context.Offset != 0 || len(input.Parts[0].Context.H) != 5 || input.Parts[0].Context.H[0] != 0x67452301 || input.Parts[0].Context.H[4] != 0xc3d2e1f0) {
				t.Error("tiny FormUpload wire differs from pinned SDK shape")
			}
			data = map[string]any{"task_id": "proof-upload", "part_size": 64, "common_headers": map[string]string{}, "upload_urls": []any{map[string]any{"part_number": 1, "upload_url": "https://objects.example/put", "signature_info": map[string]string{"signature": "object-fixture"}}}}
		case "/put":
			puts++
			content, _ = io.ReadAll(io.LimitReader(r.Body, 33))
			if recovery == "independent_attempt" {
				md, sh := md5.Sum(content), sha1.Sum(content)
				if len(content) != 32 || r.ContentLength != 32 || r.Header.Get("Authorization") != "object-fixture" || r.Header.Get("Cookie") != "" || r.Header.Get("X-Agent-ID") != "" || hex.EncodeToString(md[:]) != preMD5 || hex.EncodeToString(sh[:]) != preSHA1 {
					t.Error("new tiny upload changed bytes/hash or leaked account credentials")
				}
			}
			if recovery == "independent_attempt" && puts == 1 {
				// The original task has no acknowledged part and no visible
				// output. Ordinary retry cannot prove it safe to resend.
				http.Error(w, "fixture lost original PUT", http.StatusServiceUnavailable)
				return
			}
			if recovery != "finish" && recovery != "independent_attempt" {
				uploaded = true
				// The original PUT result is unknown; the retry must observe
				// that exact operation/file without issuing another PUT.
				http.Error(w, "fixture lost PUT acknowledgement", http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("ETag", "fixture-part")
			w.WriteHeader(http.StatusOK)
			return
		case "/open/v1/file/upload_finish":
			finish++
			uploaded = true
			if recovery == "independent_attempt" {
				data = map[string]any{"fid": "ack|created", "finish": true}
				break
			}
			// The cloud committed, but no acknowledgement/handle was received.
			http.Error(w, "fixture lost finish acknowledgement", http.StatusServiceUnavailable)
			return
		case "/open/v1/file/list_uploaded_parts":
			data = map[string]any{"finish_status": 0, "uploaded_parts": []any{}}
			if recovery == "completed_task" {
				data = map[string]any{"finish_status": 2, "fid": "ack|created", "uploaded_parts": []any{}, "next_part_number_marker": ""}
			}
		case "/open/v1/file/info":
			data = file(r.URL.Query().Get("fid"), "listed|root", name, false)
		case "/open/v1/file/delete":
			deletes++
			var body struct {
				Action int      `json:"action_type"`
				IDs    []string `json:"fid_list"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Action != 1 || len(body.IDs) != 1 || body.IDs[0] != "listed|created" {
				t.Error("cleanup did not target the exact original challenge")
			}
			uploaded = false
			http.Error(w, "fixture lost delete acknowledgement", http.StatusServiceUnavailable)
			return
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
	cleanup, err := s.RetryQuarkProofCleanup(context.Background(), actor, created.ID, RequestContext{})
	if err != nil || cleanup.Pending != 0 || cleanup.Message != "没有需要核对的验证文件" {
		t.Fatal("empty proof history claimed a verified removal")
	}
	root := quark.QualifyID(quark.RouteTV, "tv-private-account", "tv-root")
	lostStage := "确认验证文件上传"
	wantFinish := 1
	wantReads := 3 // Full candidate mapping, independent proof and owned cleanup.
	if recovery != "finish" {
		lostStage, wantFinish = "写入验证文件内容", 0
	}
	if recovery == "completed_task" {
		// The original task acknowledgement identifies the output directly;
		// independent proof and cleanup still each read all 32 bytes.
		wantReads = 2
	}
	if _, err := s.VerifyQuarkRoot(context.Background(), actor, created.ID, root, "/媒体库", RequestContext{}); ErrorCode(err) != CodeConnectionUnavailable || !strings.Contains(ErrorMessage(err), lostStage) {
		t.Fatalf("lost original %s was not retained: %v", recovery, err)
	}
	if recovery == "independent_attempt" {
		ctx := context.Background()
		_, bundle, _ := s.quarkCredentials(ctx, created.ID)
		base := quarkRootProofBase(created.ID, bundle, root, "/媒体库")
		store := NewQuarkOperationStore(s.db, s.credentials, created.ID)
		oldProof, _ := store.Load(ctx, base)
		oldUpload, _ := store.Load(ctx, base+"/upload")
		for i := 0; i < 2; i++ {
			if _, err := s.VerifyQuarkRoot(ctx, actor, created.ID, root, "/媒体库", RequestContext{}); err == nil {
				t.Fatal("unfinished original task pretended to prove root")
			}
		}
		if pre != 1 || puts != 1 || finish != 0 {
			t.Fatal("ordinary retry replayed original upload")
		}
		info, err := s.QuarkRootRecoveryInfo(ctx, actor, created.ID, root, "/媒体库")
		if err != nil || !info.Available {
			t.Fatalf("native recovery unavailable: %v", err)
		}
		other, err := s.QuarkRootRecoveryInfo(ctx, actor, created.ID, root, "/媒体库")
		if err != nil || !other.Available {
			t.Fatal(err)
		}
		results := make(chan error, 2)
		for _, token := range []string{info.RecoveryToken, other.RecoveryToken} {
			go func(token string) {
				result, err := s.StartQuarkRootAttempt(ctx, actor, created.ID, root, "/媒体库", token, RequestContext{})
				if err == nil && !result.Verified {
					err = io.ErrUnexpectedEOF
				}
				results <- err
			}(token)
		}
		for i := 0; i < 2; i++ {
			if err := <-results; err != nil {
				t.Fatalf("independent native proof failed: %v", err)
			}
		}
		s.invalidate(created.ID)
		if result, err := s.StartQuarkRootAttempt(ctx, actor, created.ID, root, "/媒体库", info.RecoveryToken, RequestContext{}); err != nil || !result.Verified {
			t.Fatalf("lost response/restart replay failed: %v", err)
		}
		// The successful finish supplies its original handle. Independent
		// proof and exact owned cleanup each still read all32 random bytes.
		if pre != 2 || puts != 2 || finish != 1 || reads != 2 || deletes != 1 {
			t.Fatalf("new proof did not preserve exactly one complete chain: pre=%d puts=%d finish=%d reads=%d deletes=%d", pre, puts, finish, reads, deletes)
		}
		if current, _ := store.Load(ctx, base); current.Revision != oldProof.Revision || current.State != oldProof.State || !bytes.Equal(current.Payload, oldProof.Payload) {
			t.Fatal("old proof rewritten")
		}
		if current, _ := store.Load(ctx, base+"/upload"); current.Revision != oldUpload.Revision || current.State != oldUpload.State || !bytes.Equal(current.Payload, oldUpload.Payload) {
			t.Fatal("old upload replayed or reset")
		}
		info, err = s.QuarkRootRecoveryInfo(ctx, actor, created.ID, root, "/媒体库")
		if err != nil || info.Available || info.RecoveryToken != "" {
			t.Fatal("valid saved proof still offered new upload")
		}
		status, err := s.QuarkStatus(ctx, actor, created.ID)
		if err != nil || status.VerifiedRoots != 1 || !status.Capabilities.FileUpload || status.RootCleanupPending != 0 {
			t.Fatal("new proof capabilities/cleanup missing")
		}
		return
	}
	s.invalidate(created.ID)
	for attempt := 0; attempt < 2; attempt++ {
		if attempt == 1 {
			s.invalidate(created.ID)
		}
		result, err := s.VerifyQuarkRoot(context.Background(), actor, created.ID, root, "/媒体库", RequestContext{})
		if err != nil || !result.Verified {
			t.Fatalf("attempt %d: verification failed %v (pre=%d puts=%d finish=%d reads=%d)", attempt, err, pre, puts, finish, reads)
		}
		if attempt == 0 {
			if result.CleanupState != "pending" || result.CleanupMessage == "" {
				t.Fatal("valid proof hid pending cleanup")
			}
			status, err := s.QuarkStatus(context.Background(), actor, created.ID)
			if err != nil || status.RootCleanupPending != 1 || !status.Capabilities.FileUpload {
				t.Fatal("cleanup failure blocked proof capability or disappeared")
			}
			if err := s.db.Model(&models.Connection{}).Where("id = ?", created.ID).Update("enabled", false).Error; err != nil {
				t.Fatal(err)
			}
			status, err = s.QuarkStatus(context.Background(), actor, created.ID)
			if err != nil || status.RootCleanupPending != 1 || status.Capabilities.FileUpload {
				t.Fatal("unavailable driver incorrectly reported cleanup complete")
			}
			if err := s.db.Model(&models.Connection{}).Where("id = ?", created.ID).Update("enabled", true).Error; err != nil {
				t.Fatal(err)
			}
			// A new service driver restores only encrypted receipts. No picker,
			// reauthorization, verify-root call or upload is needed for cleanup.
			s.invalidate(created.ID)
			cleanup, err := s.RetryQuarkProofCleanup(context.Background(), actor, created.ID, RequestContext{})
			if err != nil || cleanup.Pending != 0 {
				t.Fatalf("cleanup receipt did not reconcile: %v", err)
			}
		}
	}
	if pre != 1 || puts != 1 || finish != wantFinish || reads != wantReads || deletes != 1 {
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
	if err != nil || status.VerifiedRoots != 1 || !status.Capabilities.FileUpload || status.RootCleanupPending != 0 {
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
	if pre != 1 || puts != 1 || finish != wantFinish || reads != wantReads || deletes != 1 {
		t.Fatal("rejected directory listing replayed the confirmed upload")
	}
}
