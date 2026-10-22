package quark

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

// SDK 1.0.20 restore reads finish_status/fid even when uploaded_parts is empty.
// This is a historical receipt fixture, not evidence of a real account's task.
func TestCommonSpaceRestoresLegacyCompletedTaskWithoutWrites(t *testing.T) {
	for _, scenario := range []struct {
		phase string
		query bool
	}{{"put_inflight", false}, {"finish_inflight", false}, {"put_inflight", true}, {"finish_inflight", true}} {
		name := scenario.phase
		if scenario.query {
			name += "/query"
		}
		t.Run(name, func(t *testing.T) {
			f := newProofRecoveryFixture(t, "")
			f.name, f.content, f.exists = ".omc-verify-original-receipt", bytes.Repeat([]byte("q"), 32), true
			proof := proofPayload{Path: "/媒体库", TVRoot: QualifyID(RouteTV, "tv-account", "T"), AgentRoot: QualifyID(RouteAgent, "agent-account", "A"), TVAccount: "tv-account", Name: f.name, Bytes: f.content}
			proofRaw, _ := json.Marshal(proof)
			f.store.receipts["proof"] = cloud.OperationReceipt{ID: "proof", Provider: cloud.ProviderQuark, Route: string(RouteAgent), AccountID: "agent-account", Kind: "space_proof", State: "prepared", Revision: 1, Payload: proofRaw}
			md, sh, sha, _, _ := uploadHashes(context.Background(), bytes.NewReader(f.content), 32)
			p := uploadPayload{Parent: "A", Name: f.name, Size: 32, MD5: md, SHA1: sh, SHA256: sha, TaskID: "original-private-task", PartSize: 64, Phase: scenario.phase, InFlightPart: 1}
			raw, _ := json.Marshal(p)
			f.store.receipts["proof/upload"] = cloud.OperationReceipt{ID: "proof/upload", Provider: cloud.ProviderQuark, Route: string(RouteAgent), AccountID: "agent-account", Kind: "upload", State: "unknown", Revision: 3, Payload: raw}
			d := f.client()
			queries := 0
			d.api.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/open/v1/file/list_uploaded_parts" {
					queries++
					var input map[string]any
					_ = json.NewDecoder(r.Body).Decode(&input)
					if input["task_id"] != p.TaskID {
						t.Fatal("queried another task")
					}
					return envelope(r, map[string]any{"finish_status": 2, "fid": "created", "uploaded_parts": []any{}}), nil
				}
				return f.api(r)
			})
			if scenario.query {
				operation, err := d.QueryOperation(context.Background(), "proof/upload")
				if err != nil || !operation.Complete || len(operation.Items) != 1 || queries != 1 || f.pre != 0 || f.put != 0 || f.finish != 0 || f.deletes != 0 {
					t.Fatalf("read-only query replayed or failed original task: %v", err)
				}
			}
			result, err := d.VerifyCommonSpace(context.Background(), "proof", "/媒体库")
			if err != nil || !f.credentials.HasProof(result.TVRoot, result.AgentRoot) {
				t.Fatalf("completed original task stuck: %v", err)
			}
			if queries != 1 || f.pre != 0 || f.put != 0 || f.finish != 0 || f.deletes != 1 || len(f.store.receipts) != 2 {
				t.Fatalf("recovery replayed or skipped task proof: query=%d pre=%d put=%d finish=%d delete=%d", queries, f.pre, f.put, f.finish, f.deletes)
			}
			_, err = f.client().VerifyCommonSpace(context.Background(), "proof", "/媒体库")
			if err != nil || f.pre != 0 || f.put != 0 || f.finish != 0 || f.deletes != 1 {
				t.Fatal("confirmed recovery was replayed")
			}
		})
	}
}

func TestUploadPartRecoveryReadsOriginalTaskPagination(t *testing.T) {
	queries := 0
	d := fixtureClient(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/open/v1/file/list_uploaded_parts" {
			t.Fatal("part recovery sent another operation")
		}
		var input map[string]any
		_ = json.NewDecoder(r.Body).Decode(&input)
		if input["task_id"] != "original-task" {
			t.Fatal("wrong task")
		}
		queries++
		if queries == 1 {
			return envelope(r, map[string]any{"finish_status": 0, "uploaded_parts": []uploadPart{{Number: 1, ETag: "one"}}, "next_part_number_marker": 1}), nil
		}
		if input["next_part_number_marker"] != float64(1) {
			t.Fatal("original provider cursor not preserved")
		}
		return envelope(r, map[string]any{"finish_status": 1, "uploaded_parts": []uploadPart{{Number: 2, ETag: "two"}}, "next_part_number_marker": 0}), nil
	})
	p := uploadPayload{TaskID: "original-task", Size: 128, PartSize: 64, Phase: "put_inflight", InFlightPart: 2, Parts: []uploadPart{{Number: 1, ETag: "one"}}}
	raw, _ := json.Marshal(p)
	r := cloud.OperationReceipt{ID: "original", Provider: cloud.ProviderQuark, Route: string(RouteAgent), AccountID: "agent-account", Kind: "upload", State: "unknown", Revision: 4, Payload: raw}
	store := &memoryOperations{receipts: map[string]cloud.OperationReceipt{r.ID: r}}
	d.config.Operations = store
	if err := d.recoverUploadedPart(context.Background(), &r, &p); err != nil || p.InFlightPart != 0 || len(p.Parts) != 2 || queries != 2 {
		t.Fatalf("original later-page part not recovered: %v queries=%d", err, queries)
	}
}

func TestUploadPartRecoveryReportsItsActualPhase(t *testing.T) {
	f := newProofRecoveryFixture(t, "")
	d := f.client()
	d.mediaTransport = transportFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("fixture lost PUT") })
	_, err := d.VerifyCommonSpace(context.Background(), "proof", "/媒体库")
	errorCode(t, err, cloud.CodeMutationUnknown)
	d = f.client()
	d.api.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/open/v1/file/list_uploaded_parts" {
			return envelope(r, map[string]any{"finish_status": 0, "uploaded_parts": []any{}}), nil
		}
		return f.api(r)
	})
	_, err = d.VerifyCommonSpace(context.Background(), "proof", "/媒体库")
	var operation *OperationError
	if !errors.As(err, &operation) || operation.Stage != "upload_part_recovery" {
		t.Fatalf("original task recovery hidden by outer challenge phase: %v", err)
	}
	diagnostic, ok := UploadDiagnosticFromError(err)
	if !ok || diagnostic.Phase != "put_inflight" || diagnostic.State != "unknown" || !diagnostic.HasTask || diagnostic.HasOutput || diagnostic.InFlightPart != 1 || diagnostic.TaskPages != 1 || !diagnostic.TaskListingComplete || diagnostic.TaskFinishStatus == nil || *diagnostic.TaskFinishStatus != 0 {
		t.Fatalf("missing safe receipt/query state: %+v", diagnostic)
	}
}

func TestUploadTaskQueryRejectsInconsistentEvidence(t *testing.T) {
	for _, mode := range []string{"missing_fid", "unknown_status", "repeated_page", "changed_etag", "invalid_part"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			d := fixtureClient(t, func(r *http.Request) (*http.Response, error) {
				if r.URL.Path != "/open/v1/file/list_uploaded_parts" {
					t.Fatal("unexpected write")
				}
				calls++
				body := map[string]any{"finish_status": 0, "uploaded_parts": []any{}}
				switch mode {
				case "missing_fid":
					body["finish_status"] = 2
				case "unknown_status":
					body["finish_status"] = 99
				case "repeated_page":
					body["next_part_number_marker"] = 1
				case "changed_etag":
					body["uploaded_parts"] = []uploadPart{{Number: 1, ETag: "changed"}}
				case "invalid_part":
					body["uploaded_parts"] = []uploadPart{{Number: 500, ETag: "wrong"}}
				}
				return envelope(r, body), nil
			})
			p := uploadPayload{TaskID: "original", Size: 128, PartSize: 64, Parts: []uploadPart{{Number: 1, ETag: "one"}}}
			_, err := d.readUploadTask(context.Background(), &p)
			if err == nil || p.Observation.Complete || calls > 2 {
				t.Fatal("invalid task evidence accepted or loop unbounded")
			}
		})
	}
}
