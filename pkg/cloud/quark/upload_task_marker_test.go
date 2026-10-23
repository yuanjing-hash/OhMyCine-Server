package quark

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

// The retained official SDK passes next_part_number_marker through unchanged
// and ends on an empty marker. Exercise that wire behavior from the persisted
// original unknown PUT, not a new successful upload or a replacement proof.
func TestCommonSpaceRestoresOriginalTaskWithStringMarkers(t *testing.T) {
	for _, query := range []bool{false, true} {
		name := "verify"
		if query {
			name = "query"
		}
		t.Run(name, func(t *testing.T) {
			f, p := originalUnknownChallengeTaskFixture(t, true)
			d := f.client()
			queries := 0
			d.api.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path != "/open/v1/file/list_uploaded_parts" {
					return f.api(r)
				}
				queries++
				var input map[string]any
				if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input["task_id"] != p.TaskID {
					t.Fatal("query lost the original task identity")
				}
				if queries == 1 {
					if _, exists := input["next_part_number_marker"]; exists {
						t.Fatal("first query invented a continuation")
					}
					return envelope(r, map[string]any{"finish_status": 1, "uploaded_parts": []any{}, "next_part_number_marker": "1"}), nil
				}
				if queries != 2 || input["next_part_number_marker"] != "1" {
					t.Fatal("original provider string marker was changed or looped")
				}
				return envelope(r, map[string]any{"finish_status": 2, "fid": "created", "uploaded_parts": []any{}, "next_part_number_marker": ""}), nil
			})
			if query {
				result, err := d.QueryOperation(context.Background(), "proof/upload")
				if err != nil || !result.Complete || len(result.Items) != 1 {
					t.Fatalf("original task query failed on SDK-compatible markers: %v", err)
				}
			}
			result, err := d.VerifyCommonSpace(context.Background(), "proof", "/媒体库")
			if err != nil || !f.credentials.HasProof(result.TVRoot, result.AgentRoot) {
				t.Fatalf("original proof remained stuck on SDK-compatible markers: %v", err)
			}
			if queries != 2 || f.pre != 0 || f.put != 0 || f.finish != 0 || f.deletes != 1 || len(f.store.receipts) != 2 {
				t.Fatalf("task recovery replayed a write: queries=%d pre=%d put=%d finish=%d delete=%d", queries, f.pre, f.put, f.finish, f.deletes)
			}
			_, err = f.client().VerifyCommonSpace(context.Background(), "proof", "/媒体库")
			if err != nil || f.pre != 0 || f.put != 0 || f.finish != 0 || f.deletes != 1 {
				t.Fatal("cold retry replayed the confirmed operation")
			}
		})
	}
}

func originalUnknownChallengeTaskFixture(t *testing.T, exists bool) (*proofRecoveryFixture, uploadPayload) {
	t.Helper()
	f := newProofRecoveryFixture(t, "")
	f.name, f.content, f.exists = ".omc-verify-original-marker", bytes.Repeat([]byte("m"), 32), exists
	proof := proofPayload{Path: "/媒体库", TVRoot: QualifyID(RouteTV, "tv-account", "T"), AgentRoot: QualifyID(RouteAgent, "agent-account", "A"), TVAccount: "tv-account", Name: f.name, Bytes: f.content}
	proofRaw, _ := json.Marshal(proof)
	f.store.receipts["proof"] = cloud.OperationReceipt{ID: "proof", Provider: cloud.ProviderQuark, Route: string(RouteAgent), AccountID: "agent-account", Kind: "space_proof", State: "prepared", Revision: 1, Payload: proofRaw}
	md, sh, sha, _, _ := uploadHashes(context.Background(), bytes.NewReader(f.content), 32)
	target, err := f.client().freezeChallengeTarget(context.Background(), "proof/upload", "A", f.name, sha)
	if err != nil {
		t.Fatal(err)
	}
	p := uploadPayload{Parent: "A", Name: f.name, Size: 32, MD5: md, SHA1: sh, SHA256: sha, TaskID: "original-private-task", PartSize: 64, Phase: "put_inflight", InFlightPart: 1, Target: target}
	uploadRaw, _ := json.Marshal(p)
	f.store.receipts["proof/upload"] = cloud.OperationReceipt{ID: "proof/upload", Provider: cloud.ProviderQuark, Route: string(RouteAgent), AccountID: "agent-account", Kind: "upload", State: "unknown", Revision: 3, Payload: uploadRaw}
	return f, p
}

func TestCommonSpaceRecoversOriginalUnknownPartFromExactChallenge(t *testing.T) {
	for _, query := range []bool{false, true} {
		for _, malformed := range []bool{false, true} {
			name := "verify"
			if query {
				name = "query"
			}
			if malformed {
				name += "/invalid_task_response"
			}
			t.Run(name, func(t *testing.T) {
				f, p := originalUnknownChallengeTaskFixture(t, true)
				d := f.client()
				queries := 0
				d.api.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
					if r.URL.Path != "/open/v1/file/list_uploaded_parts" {
						return f.api(r)
					}
					queries++
					var input map[string]any
					if json.NewDecoder(r.Body).Decode(&input) != nil || input["task_id"] != p.TaskID || queries > 1 {
						t.Fatal("original task changed or recovery queried it twice")
					}
					body := map[string]any{"finish_status": 0, "uploaded_parts": []any{}}
					if malformed {
						body["next_part_number_marker"] = map[string]any{"private-token": true}
					}
					return envelope(r, body), nil
				})
				if query {
					result, err := d.QueryOperation(context.Background(), "proof/upload")
					if err != nil || !result.Complete || len(result.Items) != 1 || f.deletes != 0 {
						t.Fatalf("exact original challenge did not recover read-only: %v", err)
					}
				}
				result, err := d.VerifyCommonSpace(context.Background(), "proof", "/媒体库")
				if err != nil || !f.credentials.HasProof(result.TVRoot, result.AgentRoot) || queries != 1 || f.reads < 2 {
					t.Fatalf("exact original bytes did not recover proof: %v queries=%d reads=%d", err, queries, f.reads)
				}
				if f.pre != 0 || f.put != 0 || f.finish != 0 || f.deletes != 1 || len(f.store.receipts) != 2 {
					t.Fatal("exact artifact recovery replayed an upload or replaced its receipts")
				}
			})
		}
	}
}

func TestOriginalUnknownPartCannotAdoptUnprovenChallenge(t *testing.T) {
	for _, query := range []bool{false, true} {
		for _, bad := range []string{"absent", "invalid_task_absent", "bytes", "duplicate", "account", "proof_bytes", "part_conflict", "status_conflict", "output_conflict", "ordinary_upload"} {
			if bad == "ordinary_upload" && !query {
				continue
			}
			name := "verify/" + bad
			if query {
				name = "query/" + bad
			}
			t.Run(name, func(t *testing.T) {
				f, p := originalUnknownChallengeTaskFixture(t, bad != "absent" && bad != "invalid_task_absent")
				switch bad {
				case "bytes":
					f.wrongContent = true
				case "duplicate":
					f.duplicate = true
				case "account":
					f.credentials.Agent.AccountID = "different-account"
				case "proof_bytes":
					proofReceipt := f.store.receipts["proof"]
					var proof proofPayload
					_ = json.Unmarshal(proofReceipt.Payload, &proof)
					proof.Bytes[0] ^= 1
					proofReceipt.Payload, _ = json.Marshal(proof)
					f.store.receipts["proof"] = proofReceipt
				case "part_conflict":
					p.Parts = []uploadPart{{Number: 1, ETag: "original-etag"}}
				case "ordinary_upload":
					p.Target.Challenge = false
				}
				uploadReceipt := f.store.receipts["proof/upload"]
				uploadReceipt.Payload, _ = json.Marshal(p)
				f.store.receipts["proof/upload"] = uploadReceipt
				d := f.client()
				queries := 0
				d.api.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
					if r.URL.Path != "/open/v1/file/list_uploaded_parts" {
						return f.api(r)
					}
					queries++
					parts := []uploadPart{}
					if bad == "part_conflict" {
						parts = append(parts, uploadPart{Number: 1, ETag: "changed-etag"})
					}
					body := map[string]any{"finish_status": 0, "uploaded_parts": parts}
					switch bad {
					case "invalid_task_absent":
						body["next_part_number_marker"] = map[string]any{"private-token": true}
					case "status_conflict", "output_conflict":
						body["finish_status"], body["fid"] = 2, "created"
						if queries == 1 {
							body["next_part_number_marker"] = "original-next-page"
						} else if bad == "status_conflict" {
							body["finish_status"] = 1
						} else {
							body["fid"] = "different-output"
						}
					}
					return envelope(r, body), nil
				})
				var err error
				if query {
					_, err = d.QueryOperation(context.Background(), "proof/upload")
				} else {
					_, err = d.VerifyCommonSpace(context.Background(), "proof", "/媒体库")
				}
				wantQueries := 1
				if bad == "status_conflict" || bad == "output_conflict" {
					wantQueries = 2
				}
				if err == nil || f.pre != 0 || f.put != 0 || f.finish != 0 || f.deletes != 0 || queries > wantQueries || len(f.credentials.Proofs) != 0 || len(f.store.receipts) != 2 {
					t.Fatal("unproven original challenge was adopted or a write was replayed")
				}
				if saved := f.store.receipts["proof/upload"]; saved.Revision != uploadReceipt.Revision || saved.State != "unknown" || !bytes.Equal(saved.Payload, uploadReceipt.Payload) {
					t.Fatal("unresolved original upload was reset or rewritten")
				}
				if bad == "absent" {
					diagnostic, ok := UploadDiagnosticFromError(err)
					if !ok || diagnostic.TaskQueryReason != "original_part_unconfirmed" || !diagnostic.TaskListingComplete || diagnostic.TaskPages != 1 {
						t.Fatalf("absence lost its precise unresolved diagnosis: %+v", diagnostic)
					}
				}
				if bad == "invalid_task_absent" || bad == "status_conflict" || bad == "output_conflict" {
					wantReason := "marker_invalid"
					if bad == "status_conflict" {
						wantReason = "finish_status_regressed"
					} else if bad == "output_conflict" {
						wantReason = "completed_fid_changed"
					}
					var operation *OperationError
					diagnostic, ok := UploadDiagnosticFromError(err)
					if !errors.As(err, &operation) || operation.Stage != "upload_task_query" || !ok || diagnostic.TaskQueryReason != wantReason || diagnostic.TaskPages != wantQueries || diagnostic.TaskListingComplete || f.reads != 0 {
						t.Fatalf("original query failure was hidden or conflicting output was adopted: %+v", diagnostic)
					}
				}
			})
		}
	}
}

// Keep a complete first page from becoming completion evidence after a later
// failure. These queries may only read the same saved operation; no receipt
// checkpoint or upload endpoint can be used to work around a bad continuation.
func TestUploadTaskStringPaginationKeepsOriginalReceiptOnFailure(t *testing.T) {
	for _, mode := range []string{"cycle", "page_limit", "late_malformed", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			d := fixtureClient(t, func(r *http.Request) (*http.Response, error) {
				if r.URL.Path != "/open/v1/file/list_uploaded_parts" {
					t.Fatal("failed original-task query dispatched a different endpoint")
				}
				calls++
				var input map[string]json.RawMessage
				if json.NewDecoder(r.Body).Decode(&input) != nil || string(input["task_id"]) != `"original-private-task"` {
					t.Fatal("pagination changed the saved task identity")
				}
				if calls == 1 {
					if _, present := input["next_part_number_marker"]; present {
						t.Fatal("page one contained an invented marker")
					}
				} else {
					want := `"original-next-page"`
					if mode == "page_limit" {
						want = `"page-` + strconv.Itoa(calls-1) + `"`
					}
					if string(input["next_part_number_marker"]) != want {
						t.Fatal("pagination changed the prior string marker")
					}
				}
				body := map[string]any{"finish_status": 2, "fid": "private-completed-output", "uploaded_parts": []any{}, "next_part_number_marker": "original-next-page"}
				switch mode {
				case "page_limit":
					body["next_part_number_marker"] = "page-" + strconv.Itoa(calls)
				case "late_malformed":
					if calls == 2 {
						body["finish_status"] = "private-invalid-status"
					}
				case "cancel":
					cancel()
				}
				return envelope(r, body), nil
			})
			p := uploadPayload{TaskID: "original-private-task", Size: 32, PartSize: 64, Phase: "put_inflight", InFlightPart: 1}
			raw, _ := json.Marshal(p)
			r := cloud.OperationReceipt{ID: "original-private-receipt", Provider: cloud.ProviderQuark, Route: "agent", AccountID: "agent-account", Kind: "upload", State: "unknown", Revision: 3, Payload: raw}
			store := &memoryOperations{receipts: map[string]cloud.OperationReceipt{r.ID: r}}
			d.config.Operations = store
			result, err := d.QueryOperation(ctx, r.ID)
			wantCalls, wantReason := 2, "cursor_repeated"
			switch mode {
			case "page_limit":
				wantCalls, wantReason = 128, "page_limit"
			case "late_malformed":
				wantReason = "response_type_invalid"
			case "cancel":
				wantCalls, wantReason = 1, ""
				if !errors.Is(err, context.Canceled) {
					t.Fatal("query cancellation lost its cause")
				}
			}
			diagnostic, ok := UploadDiagnosticFromError(err)
			if err == nil || result.Complete || len(result.Items) != 0 || calls != wantCalls || !ok || diagnostic.TaskListingComplete || diagnostic.TaskQueryReason != wantReason {
				t.Fatalf("later-page failure was accepted or escaped bounds: calls=%d diagnostic=%+v err=%v", calls, diagnostic, err)
			}
			if saved := store.receipts[r.ID]; len(store.receipts) != 1 || saved.Revision != r.Revision || saved.State != r.State || !bytes.Equal(saved.Payload, raw) {
				t.Fatal("failed pagination rewrote the original receipt")
			}
		})
	}
}

func TestUploadTaskReadAdmissionDoesNotProveOriginalUploadUnsent(t *testing.T) {
	d := fixtureClient(t, func(*http.Request) (*http.Response, error) {
		t.Fatal("admission-denied original-task query reached the provider")
		return nil, nil
	})
	d.config.BeforeCall = func(context.Context) error { return failure(CodeLimited) }
	d.api.config.BeforeCall = d.config.BeforeCall
	p := uploadPayload{TaskID: "original-private-task", Size: 32, PartSize: 64, Phase: "put_inflight", InFlightPart: 1}
	raw, _ := json.Marshal(p)
	r := cloud.OperationReceipt{ID: "original-private-receipt", Provider: cloud.ProviderQuark, Route: "agent", AccountID: "agent-account", Kind: "upload", State: "unknown", Revision: 3, Payload: raw}
	store := &memoryOperations{receipts: map[string]cloud.OperationReceipt{r.ID: r}}
	d.config.Operations = store
	result, err := d.QueryOperation(context.Background(), r.ID)
	var unsent *requestNotDispatchedError
	diagnostic, ok := UploadDiagnosticFromError(err)
	if err == nil || !errors.As(err, &unsent) || result.Complete || !ok || diagnostic.Phase != "put_inflight" || diagnostic.State != "unknown" || diagnostic.NotDispatched {
		t.Fatalf("undispatched read was mistaken for an undispatched original upload: %+v", diagnostic)
	}
	if saved := store.receipts[r.ID]; len(store.receipts) != 1 || saved.Revision != r.Revision || saved.State != r.State || !bytes.Equal(saved.Payload, raw) {
		t.Fatal("read admission changed the original unknown upload")
	}
}

func TestUploadTaskWriteAdmissionRetainsNoDispatchDiagnostic(t *testing.T) {
	for _, phase := range []string{"pre_inflight", "put_inflight", "finish_inflight"} {
		t.Run(phase, func(t *testing.T) {
			f := newProofRecoveryFixture(t, "")
			d := f.client()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			blocked := false
			d.config.Operations = &uploadAdmissionStore{memoryOperations: f.store, after: func(r cloud.OperationReceipt) {
				var p uploadPayload
				_ = json.Unmarshal(r.Payload, &p)
				if p.Phase != phase || blocked {
					return
				}
				blocked = true
				if phase == "put_inflight" {
					cancel()
				} else {
					d.api.config.BeforeCall = func(context.Context) error { return failure(CodeLimited) }
				}
			}}
			_, err := d.VerifyCommonSpace(ctx, "proof", "/媒体库")
			diagnostic, ok := UploadDiagnosticFromError(err)
			wantPre, wantPut := 0, 0
			if phase != "pre_inflight" {
				wantPre = 1
			}
			if phase == "finish_inflight" {
				wantPut = 1
			}
			if err == nil || !blocked || !ok || !diagnostic.NotDispatched || f.pre != wantPre || f.put != wantPut || f.finish != 0 {
				t.Fatalf("affirmative original write admission evidence was lost: %+v", diagnostic)
			}
		})
	}
}

func TestUploadTaskMarkerPreservesWireAndBounds(t *testing.T) {
	for _, tt := range []struct {
		name, marker string
		continuation bool
		invalid      bool
	}{
		{"missing", "", false, false},
		{"null", "null", false, false},
		{"numeric_terminal", "0", false, false},
		{"string_terminal", `""`, false, false},
		{"number", "1", true, false},
		{"numeric_string", `"1"`, true, false},
		{"opaque_string", `"original-page-token"`, true, false},
		{"zero_string_is_not_numeric_zero", `"0"`, true, false},
		{"negative", "-1", false, true},
		{"oversized_number", "10001", false, true},
		{"fraction", "1.25", false, true},
		{"boolean", "false", false, true},
		{"object", `{}`, false, true},
		{"array", `[]`, false, true},
		{"control", `"private\u0000marker"`, false, true},
		{"oversized_string", `"` + strings.Repeat("p", 129) + `"`, false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			d := fixtureClient(t, func(r *http.Request) (*http.Response, error) {
				if r.URL.Path != "/open/v1/file/list_uploaded_parts" {
					t.Fatal("marker read dispatched a different operation")
				}
				calls++
				body := map[string]any{"finish_status": 2, "fid": "original-output", "uploaded_parts": []any{}}
				if calls == 1 && tt.marker != "" {
					body["next_part_number_marker"] = json.RawMessage(tt.marker)
				}
				if calls == 2 {
					var input map[string]json.RawMessage
					if json.NewDecoder(r.Body).Decode(&input) != nil || string(input["next_part_number_marker"]) != tt.marker {
						t.Fatal("provider continuation changed its value or JSON type")
					}
				}
				if calls > 2 {
					t.Fatal("marker query did not terminate")
				}
				return envelope(r, body), nil
			})
			p := uploadPayload{TaskID: "original-task", Size: 32, PartSize: 64}
			result, err := d.readUploadTask(context.Background(), &p)
			if tt.invalid {
				if err == nil || calls != 1 || p.Observation.Complete || p.Observation.Reason != "marker_invalid" || p.Observation.Field != "next_part_number_marker" {
					t.Fatal("invalid marker escaped its read-only bound")
				}
				return
			}
			wantCalls := 1
			if tt.continuation {
				wantCalls = 2
			}
			if err != nil || calls != wantCalls || !p.Observation.Complete || result.Handle != "original-output" {
				t.Fatalf("valid original marker not recovered: %v calls=%d", err, calls)
			}
		})
	}
}

func TestUploadTaskQueryDiagnosticsIdentifySafeResponseField(t *testing.T) {
	for _, tt := range []struct {
		name, raw, reason, field, valueType string
	}{
		{"status_string", `{"finish_status":"2","fid":"private-output"}`, "response_type_invalid", "finish_status", "string"},
		{"fid_array", `{"finish_status":2,"fid":["private-output"]}`, "response_type_invalid", "fid", "array"},
		{"parts_object", `{"finish_status":0,"uploaded_parts":{"private-key":"private-value"}}`, "response_type_invalid", "uploaded_parts", "object"},
		{"part_number_string", `{"finish_status":0,"uploaded_parts":[{"part_number":"1","etag":"private-etag"}]}`, "response_type_invalid", "uploaded_parts.part_number", "string"},
		{"part_etag_object", `{"finish_status":0,"uploaded_parts":[{"part_number":1,"etag":{"private-key":true}}]}`, "response_type_invalid", "uploaded_parts.etag", "object"},
		{"status_range", `{"finish_status":99}`, "finish_status_invalid", "finish_status", ""},
		{"marker_object", `{"finish_status":0,"next_part_number_marker":{"private-key":true}}`, "marker_invalid", "next_part_number_marker", "object"},
		{"missing_fid", `{"finish_status":2}`, "completed_fid_invalid", "fid", ""},
		{"response_array", `[{"private-key":true}]`, "response_type_invalid", "response", "array"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := fixtureClient(t, func(r *http.Request) (*http.Response, error) {
				if r.URL.Path != "/open/v1/file/list_uploaded_parts" {
					t.Fatal("response rejection dispatched a different operation")
				}
				return envelope(r, json.RawMessage(tt.raw)), nil
			})
			p := uploadPayload{TaskID: "private-task", Size: 32, PartSize: 64, Phase: "put_inflight", InFlightPart: 1}
			raw, _ := json.Marshal(p)
			r := cloud.OperationReceipt{ID: "private-receipt", Provider: cloud.ProviderQuark, Route: "agent", AccountID: "agent-account", Kind: "upload", State: "unknown", Revision: 3, Payload: raw}
			d.config.Operations = &memoryOperations{receipts: map[string]cloud.OperationReceipt{r.ID: r}}
			_, err := d.QueryOperation(context.Background(), r.ID)
			var operation *OperationError
			if !errors.As(err, &operation) || operation.Stage != "upload_task_query" {
				t.Fatal("invalid task response lost the precise query stage")
			}
			diagnostic, ok := UploadDiagnosticFromError(err)
			if !ok || diagnostic.TaskQueryReason != tt.reason || diagnostic.TaskResponseField != tt.field || diagnostic.TaskResponseType != tt.valueType || diagnostic.TaskPages != 1 || diagnostic.TaskListingComplete || !diagnostic.HasTask || diagnostic.HasOutput {
				t.Fatalf("missing safe response diagnosis: %+v", diagnostic)
			}
			diagnosticRaw, _ := json.Marshal(diagnostic)
			if strings.Contains(err.Error(), "private") || strings.Contains(string(diagnosticRaw), "private") {
				t.Fatal("raw task data escaped the fixed diagnostic vocabulary")
			}
		})
	}
}
