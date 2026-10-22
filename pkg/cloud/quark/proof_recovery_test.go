package quark

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

// A complete remote write can outlive its HTTP acknowledgement. Every fresh
// Client below shares only durable receipts and credentials, not observed IDs.
type proofRecoveryFixture struct {
	t                                *testing.T
	store                            *memoryOperations
	credentials                      Credentials
	name                             string
	content                          []byte
	exists                           bool
	lost                             string
	pre, put, finish, deletes, reads int
	deleteUnknown                    bool
	deleteKeep                       bool
	wrongContent                     bool
	duplicate                        bool
	replaced                         bool
	onRead                           func()
}

func newProofRecoveryFixture(t *testing.T, lost string) *proofRecoveryFixture {
	return &proofRecoveryFixture{t: t, lost: lost, store: &memoryOperations{receipts: map[string]cloud.OperationReceipt{}}, credentials: fixtureCredentials()}
}

func (f *proofRecoveryFixture) client() *Client {
	d := fixtureClient(f.t, f.api)
	d.credentials = cloneCredentials(f.credentials)
	d.config.Operations = f.store
	d.config.LoadCredentials = func(context.Context) (json.RawMessage, error) { return json.Marshal(f.credentials) }
	d.config.PersistCredentials = func(_ context.Context, revision uint64, raw json.RawMessage) error {
		if revision != f.credentials.Revision {
			return errors.New("CAS")
		}
		return json.Unmarshal(raw, &f.credentials)
	}
	d.mediaTransport = transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPut {
			f.put++
			f.content, _ = io.ReadAll(r.Body)
			return &http.Response{StatusCode: 200, Header: http.Header{"Etag": {"challenge-part"}}, Body: io.NopCloser(bytes.NewReader(nil))}, nil
		}
		f.reads++
		if f.onRead != nil {
			f.onRead()
		}
		data := f.content
		if f.wrongContent {
			data = bytes.Repeat([]byte("x"), 32)
		}
		return &http.Response{StatusCode: 200, ContentLength: int64(len(data)), Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(data))}, nil
	})
	return d
}

func (f *proofRecoveryFixture) api(r *http.Request) (*http.Response, error) {
	id := "created"
	if f.replaced {
		id = "replacement"
	}
	switch r.URL.Path {
	case "/file":
		if r.URL.Query().Get("method") == "download" {
			return envelope(r, map[string]any{"fid": "tv-" + id, "file_name": f.name, "size": 32, "download_url": "https://objects.example/read"}), nil
		}
		files := []any{}
		if r.URL.Query().Get("parent_fid") == "0" {
			files = append(files, map[string]any{"fid": "T", "filename": "媒体库", "size": 0, "isdir": 1})
		} else if f.exists {
			files = append(files, map[string]any{"fid": "tv-" + id, "filename": f.name, "size": 32, "isdir": 0})
		}
		return envelope(r, map[string]any{"total_count": len(files), "files": files}), nil
	case "/open/v1/file/list":
		var input struct {
			Parent string `json:"parent_fid"`
		}
		_ = json.NewDecoder(r.Body).Decode(&input)
		files := []any{}
		if input.Parent == "0" {
			files = append(files, agentFile("A", "0", "媒体库", true, 0))
		} else if input.Parent == "A" && f.exists {
			files = append(files, agentFile(id, "A", f.name, false, 32))
			if f.duplicate {
				files = append(files, agentFile("duplicate", "A", f.name, false, 32))
			}
		}
		return envelope(r, map[string]any{"last_page": true, "file_list": files}), nil
	case "/open/v1/file/info":
		return envelope(r, agentFile(id, "A", f.name, false, 32)), nil
	case "/open/v1/file/upload_pre":
		f.pre++
		var input struct {
			Name string `json:"file_name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&input)
		f.name = input.Name
		if f.lost == "pre" {
			// Model a completed instant-upload response lost after provider acceptance.
			var p proofPayload
			_ = json.Unmarshal(f.store.receipts["proof"].Payload, &p)
			f.content, f.exists = append([]byte(nil), p.Bytes...), true
			return nil, errors.New("lost acknowledgement")
		}
		return envelope(r, map[string]any{"task_id": "original-task", "part_size": 64, "upload_urls": []any{map[string]any{"part_number": 1, "upload_url": "https://objects.example/put", "signature_info": map[string]string{"signature": "object-only"}}}}), nil
	case "/open/v1/file/upload_finish":
		f.finish++
		f.exists = true
		if f.lost == "finish" {
			return nil, errors.New("lost acknowledgement")
		}
		return envelope(r, map[string]any{"fid": id, "finish": true}), nil
	case "/open/v1/file/list_uploaded_parts":
		return envelope(r, map[string]any{"finish_status": 0, "uploaded_parts": []any{}}), nil
	case "/open/v1/file/delete":
		f.deletes++
		var body struct {
			Action int      `json:"action_type"`
			IDs    []string `json:"fid_list"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Action != 1 || len(body.IDs) != 1 || body.IDs[0] != "created" || len(f.credentials.Proofs) != 1 {
			f.t.Fatal("cleanup exceeded the persisted proof's exact owned challenge")
		}
		if !f.deleteKeep {
			f.exists = false
		}
		if f.deleteUnknown {
			return nil, errors.New("lost delete acknowledgement")
		}
		return envelope(r, map[string]any{}), nil
	default:
		f.t.Fatalf("unexpected endpoint %s", r.URL.Path)
		return nil, nil
	}
}

func TestCommonSpaceRecoversOriginalChallengeAfterLostAcknowledgement(t *testing.T) {
	for _, stage := range []string{"pre", "finish"} {
		t.Run(stage, func(t *testing.T) {
			f := newProofRecoveryFixture(t, stage)
			_, err := f.client().VerifyCommonSpace(context.Background(), "proof", "/媒体库")
			errorCode(t, err, cloud.CodeMutationUnknown)
			original := f.name
			proof, err := f.client().VerifyCommonSpace(context.Background(), "proof", "/媒体库")
			if err != nil || !f.credentials.HasProof(proof.TVRoot, proof.AgentRoot) {
				t.Fatalf("original completed challenge could not recover: %v", err)
			}
			if f.pre != 1 || f.finish > 1 || f.put > 1 || f.name != original || len(f.store.receipts) != 2 {
				t.Fatal("recovery replayed or replaced the original upload")
			}
			state, err := f.client().ProofCleanupState(context.Background(), "proof")
			if err != nil || state != "complete" || f.exists || f.deletes != 1 {
				t.Fatalf("owned challenge not cleaned: %s %v", state, err)
			}
		})
	}
}

func TestCommonSpaceRecoveryRejectsUnprovenChallenge(t *testing.T) {
	for _, bad := range []string{"absent", "bytes", "duplicate", "account"} {
		t.Run(bad, func(t *testing.T) {
			f := newProofRecoveryFixture(t, "finish")
			_, _ = f.client().VerifyCommonSpace(context.Background(), "proof", "/媒体库")
			switch bad {
			case "absent":
				f.exists = false
			case "bytes":
				f.wrongContent = true
			case "duplicate":
				f.duplicate = true
			case "account":
				f.credentials.Agent.AccountID = "other-account"
			}
			_, err := f.client().VerifyCommonSpace(context.Background(), "proof", "/媒体库")
			if err == nil || len(f.credentials.Proofs) != 0 || f.pre != 1 || f.put != 1 || f.finish != 1 || f.deletes != 0 {
				t.Fatal("unproven challenge adopted or replayed")
			}
		})
	}
}

func TestCommonSpaceCleanupUnknownIsReadOnlyAcrossRestart(t *testing.T) {
	for _, kept := range []bool{false, true} {
		t.Run(map[bool]string{false: "deleted", true: "still_present"}[kept], func(t *testing.T) {
			f := newProofRecoveryFixture(t, "")
			f.deleteUnknown, f.deleteKeep = true, kept
			proof, err := f.client().VerifyCommonSpace(context.Background(), "proof", "/媒体库")
			if err != nil || !f.credentials.HasProof(proof.TVRoot, proof.AgentRoot) || f.deletes != 1 {
				t.Fatal("cleanup failure revoked valid proof")
			}
			state, err := f.client().CleanupCommonSpace(context.Background(), "proof")
			if kept {
				if state != "pending" || err == nil {
					t.Fatal("unconfirmed delete claimed complete")
				}
			} else if state != "complete" || err != nil {
				t.Fatalf("lost delete ACK did not reconcile: %v", err)
			}
			_, err = f.client().VerifyCommonSpace(context.Background(), "proof", "/媒体库")
			if err != nil || f.deletes != 1 || f.pre != 1 || f.put != 1 || f.finish != 1 {
				t.Fatal("cleanup retry replayed deletion or upload")
			}
		})
	}
}

func TestCommonSpaceCleanupDoesNotDeleteSameNameReplacement(t *testing.T) {
	f := newProofRecoveryFixture(t, "")
	f.deleteKeep, f.deleteUnknown = true, true
	proof, err := f.client().VerifyCommonSpace(context.Background(), "proof", "/媒体库")
	if err != nil {
		t.Fatal(err)
	}
	f.replaced = true
	state, err := f.client().CleanupCommonSpace(context.Background(), "proof")
	if err != nil || state != "complete" || f.deletes != 1 || !f.exists || !f.credentials.HasProof(proof.TVRoot, proof.AgentRoot) {
		t.Fatal("cleanup retargeted or rejected unrelated replacement")
	}
}

func TestCommonSpaceCleanupRequiresOriginalProofGeneration(t *testing.T) {
	f := newProofRecoveryFixture(t, "")
	f.deleteKeep = true
	_, err := f.client().VerifyCommonSpace(context.Background(), "proof", "/媒体库")
	if err != nil {
		t.Fatal(err)
	}
	f.credentials.Proofs[0].Generation++
	_, err = f.client().CleanupCommonSpace(context.Background(), "proof")
	errorCode(t, err, CodeIdentity)
	if f.deletes != 1 || f.pre != 1 {
		t.Fatal("stale proof dispatched")
	}
}

func TestCommonSpaceRetryPreservesOriginalUploadDiagnostic(t *testing.T) {
	f := newProofRecoveryFixture(t, "pre")
	d := f.client()
	d.api.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/open/v1/file/upload_pre" {
			f.pre++
			return response(r, 400, map[string]any{"status": -1, "errno": 23018, "message": "private-upstream-body"}), nil
		}
		return f.api(r)
	})
	for attempt := 0; attempt < 2; attempt++ {
		_, err := d.VerifyCommonSpace(context.Background(), "proof", "/媒体库")
		var phase *OperationError
		var diagnostic *Failure
		if !errors.As(err, &phase) || phase.Stage != "upload_prepare" || !errors.As(err, &diagnostic) || diagnostic.HTTPStatus != 400 || diagnostic.Errno != 23018 || !diagnostic.UnknownOutcome {
			t.Fatalf("attempt %d lost original safe diagnostic: %v", attempt, err)
		}
	}
	if f.pre != 1 {
		t.Fatal("diagnostic retry re-submitted")
	}
}
