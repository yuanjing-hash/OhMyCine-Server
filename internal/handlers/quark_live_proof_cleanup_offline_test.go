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
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
)

func proofCleanupOfflineStore(t *testing.T) *proofCleanupStore {
	t.Helper()
	dir := t.TempDir()
	cipher, err := credential.Open(filepath.Join(dir, "proof-cleanup.key"), "")
	if err != nil {
		t.Fatal(err)
	}
	nonce := strings.Repeat("a", 32)
	now := time.Now().UTC()
	identity := proofCleanupIdentity{JournalSHA: strings.Repeat("1", 64), KeySHA: strings.Repeat("2", 64), RootName: "OMC-Quark-Validation-20261021-" + strings.Repeat("b", 32), RootACK: "A", Proof: quark.SpaceProof{ReceiptID: "donor-proof", Path: "/OMC-Quark-Validation-20261021-" + strings.Repeat("b", 32), AgentAccount: "agent-account", TVAccount: "tv-account", AgentRoot: quark.QualifyID(quark.RouteAgent, "agent-account", "A"), TVRoot: quark.QualifyID(quark.RouteTV, "tv-account", "T")}}
	access := quark.NewCredentials()
	access.Revision = 1
	access.TV = &quark.Authorization{AccessToken: "private-tv-token", DeviceID: "device-tv", AccountID: "tv-account", Generation: 1, IdentityVerified: true}
	access.Agent = &quark.Authorization{AccessToken: "private-agent-token", DeviceID: "device-agent", ClientDeviceID: "client-agent", AccountID: "agent-account", Generation: 1, IdentityVerified: true}
	s := &proofCleanupStore{dir: dir, cipher: cipher, state: proofCleanupState{Version: 1, Identity: identity, Nonce: nonce, ProofID: "proof-cleanup-" + nonce, Bundle: access, Operations: map[string]cloud.OperationReceipt{}, Writes: map[string]bool{}, Started: now, Deadline: now.Add(proofCleanupDuration), RootVerified: true}}
	if s.save() != nil {
		t.Fatal("offline encrypted journal")
	}
	return s
}

func proofCleanupCold(t *testing.T, s *proofCleanupStore) *proofCleanupStore {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(s.dir, "proof-cleanup.enc"))
	if err != nil || bytes.Contains(raw, []byte("private-agent-token")) || bytes.Contains(raw, []byte("agent-account")) {
		t.Fatal("private encrypted journal boundary")
	}
	plain, err := s.cipher.Decrypt(proofCleanupPurpose, string(raw))
	cold := &proofCleanupStore{dir: s.dir, cipher: s.cipher}
	if err != nil || json.Unmarshal([]byte(plain), &cold.state) != nil || !proofCleanupManifestValid(cold.state) {
		t.Fatal("durable cold journal")
	}
	return cold
}

func TestQuarkProofCleanupNativeOffline(t *testing.T) {
	for _, mode := range []string{"success", "lost_delete_absent", "lost_delete_present"} {
		t.Run(mode, func(t *testing.T) {
			s := proofCleanupOfflineStore(t)
			exists, name, content := false, "", []byte(nil)
			pre, put, finish, deletes, reads := 0, 0, 0, 0, 0
			file := func(id, parent, name string, dir bool, size int64) map[string]any {
				kind := "1"
				if dir {
					kind = "0"
				}
				return map[string]any{"fid": id, "parent_fid": parent, "filename": name, "file_type": kind, "size": size}
			}
			fixture := observationOfflineTransport(func(r *http.Request) (*http.Response, error) {
				var data any
				switch r.URL.Path {
				case "/open/v1/file/list":
					var in struct {
						Parent string `json:"parent_fid"`
					}
					_ = json.NewDecoder(r.Body).Decode(&in)
					files := []any{}
					if in.Parent == "0" {
						files = append(files, file("A", "0", s.state.Identity.RootName, true, 0))
					} else if in.Parent == "A" && exists {
						files = append(files, file("created", "A", name, false, 32))
					}
					data = map[string]any{"file_list": files, "last_page": true}
				case "/open/v1/file/info":
					if r.URL.Query().Get("fid") == "A" {
						data = file("A", "0", s.state.Identity.RootName, true, 0)
					} else {
						data = file("created", "A", name, false, 32)
					}
				case "/file":
					if r.URL.Query().Get("method") == "download" {
						data = map[string]any{"fid": "tv-created", "file_name": name, "size": 32, "download_url": "http://objects.example/proof-read"}
					} else {
						files := []any{}
						if r.URL.Query().Get("parent_fid") == "0" {
							files = append(files, map[string]any{"fid": "T", "filename": s.state.Identity.RootName, "isdir": 1, "size": 0})
						} else if exists {
							files = append(files, map[string]any{"fid": "tv-created", "filename": name, "isdir": 0, "size": 32})
						}
						data = map[string]any{"files": files, "total_count": len(files)}
					}
				case "/open/v1/file/upload_pre":
					pre++
					var in struct {
						Name string `json:"file_name"`
					}
					_ = json.NewDecoder(r.Body).Decode(&in)
					name = in.Name
					data = map[string]any{"task_id": "private-original-task", "part_size": 64, "upload_urls": []any{map[string]any{"part_number": 1, "upload_url": "http://objects.example/proof-put", "signature_info": map[string]string{"signature": "object-only"}}}}
				case "/proof-put":
					put++
					content, _ = io.ReadAll(r.Body)
					response := observationOfflineResponse("")
					response.Header.Set("ETag", "part-ack")
					return response, nil
				case "/open/v1/file/upload_finish":
					finish++
					exists = true
					data = map[string]any{"fid": "created", "finish": true}
				case "/proof-read":
					reads++
					return observationOfflineResponse(string(content)), nil
				case "/open/v1/file/delete":
					deletes++
					cold := proofCleanupCold(t, s)
					if len(cold.state.Bundle.Proofs) != 1 || !cold.state.Writes["delete"] {
						t.Fatal("delete before durable proof/dispatch reservation")
					}
					if mode != "lost_delete_present" {
						exists = false
					}
					if mode != "success" {
						return nil, errors.New("private provider response")
					}
					data = map[string]any{}
				default:
					t.Fatalf("unexpected fixture endpoint: %s", r.URL.Path)
				}
				raw, _ := json.Marshal(map[string]any{"status": 0, "data": data})
				return observationOfflineResponse(string(raw)), nil
			})
			client := func(store *proofCleanupStore) (*quark.Client, *proofCleanupGuard) {
				g := newProofCleanupGuard(store)
				g.delay = 0
				raw, _ := store.credentials(context.Background())
				d, err := quark.NewObservationValidationClient(cloud.Config{Credentials: raw, Operations: store, PersistCredentials: store.persist, LoadCredentials: store.credentials}, func(_ http.RoundTripper, media bool) http.RoundTripper { return g.wrap(fixture, media) })
				if err != nil {
					t.Fatal(err)
				}
				return d, g
			}
			d, g := client(s)
			proof, err := d.VerifyCommonSpace(context.Background(), s.state.ProofID, "/"+s.state.Identity.RootName)
			if err != nil || !s.state.Bundle.HasProof(proof.TVRoot, proof.AgentRoot) {
				t.Fatalf("proof failed: %v endpoint=%s outcome=%s", err, g.lastEndpoint, g.lastOutcome)
			}
			s = proofCleanupCold(t, s)
			d, g = client(s)
			state, err := d.CleanupCommonSpace(context.Background(), s.state.ProofID)
			if mode == "lost_delete_present" {
				if err == nil || state != "pending" {
					t.Fatal("unknown present target falsely completed")
				}
			} else if err != nil || state != "complete" {
				t.Fatalf("cleanup not recovered: %v endpoint=%s", err, g.lastEndpoint)
			}
			if pre != 1 || put != 1 || finish != 1 || deletes != 1 || reads < 2 || s.state.ReadBytes > 128 || len(s.state.Operations) != 2 {
				t.Fatalf("scope replay or missing readback: pre=%d put=%d finish=%d delete=%d read=%d", pre, put, finish, deletes, reads)
			}
		})
	}
}

func TestQuarkProofCleanupGuardRejectsScopeAndBudgets(t *testing.T) {
	for _, mode := range []string{"refresh", "mkdir", "media_root", "wrong_task", "unproved_delete", "deadline", "api_budget", "metadata_budget", "completed"} {
		t.Run(mode, func(t *testing.T) {
			s := proofCleanupOfflineStore(t)
			g := newProofCleanupGuard(s)
			g.delay = 0
			path, body := "/open/v1/user/info", ""
			method := http.MethodGet
			switch mode {
			case "refresh":
				method, path, body = http.MethodPost, "/agent/v1/oauth/access_token/rotate", `{}`
			case "mkdir":
				method, path, body = http.MethodPost, "/open/v1/dir", `{}`
			case "media_root":
				method, path, body = http.MethodPost, "/open/v1/file/list", `{"parent_fid":"user-media-root","size":100}`
			case "wrong_task":
				method, path, body = http.MethodPost, "/open/v1/file/list_uploaded_parts", `{"task_id":"other"}`
			case "unproved_delete":
				method, path, body = http.MethodPost, "/open/v1/file/delete", `{"fid_list":["other"],"action_type":1}`
			case "deadline":
				s.state.Started = time.Now().Add(-11 * time.Minute)
				s.state.Deadline = s.state.Started.Add(proofCleanupDuration)
			case "api_budget":
				s.state.API, s.state.Requests = 96, 96
			case "metadata_budget":
				s.state.MetadataBytes = proofCleanupMetadataBudget
			case "completed":
				s.state.Completed = true
			}
			base := observationOfflineTransport(func(*http.Request) (*http.Response, error) { t.Fatal("forbidden dispatch"); return nil, nil })
			r, _ := http.NewRequest(method, "https://open-api-drive.quark.cn"+path, strings.NewReader(body))
			if _, err := g.wrap(base, false).RoundTrip(r); err == nil {
				t.Fatal("scope/budget accepted")
			}
		})
	}
}

func TestQuarkProofCleanupJournalCASAndImmutableNonce(t *testing.T) {
	s := proofCleanupOfflineStore(t)
	ctx := context.Background()
	p := observationProof{Path: "/" + s.state.Identity.RootName, TVRoot: s.state.Identity.Proof.TVRoot, AgentRoot: s.state.Identity.Proof.AgentRoot, Name: ".omc-verify-original", Bytes: bytes.Repeat([]byte("q"), 32)}
	raw, _ := json.Marshal(p)
	r := cloud.OperationReceipt{ID: s.state.ProofID, Kind: "space_proof", Provider: cloud.ProviderQuark, Route: "agent", AccountID: "agent-account", Revision: 1, State: "prepared", Payload: raw}
	if s.Create(ctx, r) != nil || s.Create(ctx, r) == nil {
		t.Fatal("single receipt creation")
	}
	r.Revision = 2
	p.Bytes[0] = 'x'
	r.Payload, _ = json.Marshal(p)
	if s.Update(ctx, 1, r) == nil {
		t.Fatal("replacement challenge accepted")
	}
	r.Payload = raw
	if s.Update(ctx, 1, r) != nil || s.Update(ctx, 1, r) == nil {
		t.Fatal("receipt revision fence")
	}
	cold := proofCleanupCold(t, s)
	if cold.state.ProofID != s.state.ProofID || !cold.state.Deadline.Equal(s.state.Deadline) || cold.state.Operations[r.ID].Revision != 2 {
		t.Fatal("restart replaced original intent")
	}
}

func TestQuarkProofCleanupDonorNeverInitializes(t *testing.T) {
	dir := observationOfflinePrivateDir(t)
	_, err := openObservationStoreReadOnly(dir, &observationAgentState{}, &liveTVStore{dir: t.TempDir()})
	if err == nil {
		t.Fatal("missing completed donor accepted")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != "observation.runtime.lock" {
			t.Fatalf("read-only donor initialized state: %s", entry.Name())
		}
	}
}
