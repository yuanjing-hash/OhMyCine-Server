package quark

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

// This fixture exercises the production client and durable operation state
// machine, with every HTTP request intercepted in memory. No credentials or
// files outside the synthetic test fixtures are read.
type renameContractFixture struct {
	client        *Client
	store         *memoryOperations
	name          string
	writes, reads int
	onWrite       func(*http.Request) (*http.Response, error)
}

func newRenameContractFixture(t *testing.T) *renameContractFixture {
	t.Helper()
	f := &renameContractFixture{name: "old.mkv"}
	f.client, f.store = managedFixture(t, func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/open/v1/file/info":
			f.reads++
			switch r.URL.Query().Get("fid") {
			case "A":
				return envelope(r, agentFile("A", "0", "library", true, 0)), nil
			case "f":
				return envelope(r, agentFile("f", "A", f.name, false, 9)), nil
			}
		case "/open/v1/file/list":
			f.reads++
			return envelope(r, map[string]any{"last_page": true, "file_list": []any{agentFile("f", "A", f.name, false, 9)}}), nil
		case "/open/v1/file/rename/batch":
			f.writes++
			if f.onWrite != nil {
				return f.onWrite(r)
			}
		}
		t.Fatalf("unexpected synthetic request: %s", r.URL.Path)
		return nil, errors.New("unreachable")
	})
	return f
}

func (f *renameContractFixture) restart(t *testing.T) {
	t.Helper()
	raw, err := json.Marshal(f.client.Credentials())
	if err != nil {
		t.Fatal(err)
	}
	d, err := NewClient(cloud.Config{Credentials: raw, Operations: f.store})
	if err != nil {
		t.Fatal(err)
	}
	d.api.http.Transport = f.client.api.http.Transport
	f.client = d
}

func renameContractRequest(id string) cloud.OperationRequest {
	return cloud.OperationRequest{ID: id, Kind: "rename", ItemID: QualifyID(RouteAgent, "agent-account", "f"), Name: "new.mkv"}
}

func TestRenameContractIndependentBatchPersistedBeforeDispatch(t *testing.T) {
	for _, id := range []string{"followon-long-business-id-with-separate-operation-identity", "0123456789abcdef"} {
		t.Run(id, func(t *testing.T) {
			f := newRenameContractFixture(t)
			req := renameContractRequest(id)
			batch := ""
			f.onWrite = func(r *http.Request) (*http.Response, error) {
				var body struct {
					BatchID   string `json:"batch_id"`
					Operation string `json:"operation"`
					Items     []struct {
						ID     string `json:"fid"`
						Name   string `json:"file_name"`
						Before string `json:"origin_file_name"`
					} `json:"rename_list"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(body.BatchID) || body.BatchID == req.ID || body.Operation != "RENAME" || len(body.Items) != 1 || body.Items[0].ID != "f" || body.Items[0].Name != req.Name || body.Items[0].Before != "old.mkv" {
					t.Fatal("rename wire contract is not an independent 16-hex batch with exact frozen input")
				}
				receipt, err := f.store.Load(context.Background(), req.ID)
				var p operationPayload
				if err != nil || json.Unmarshal(receipt.Payload, &p) != nil || receipt.State != "submitting" || p.BatchID != body.BatchID || p.Request.ID != req.ID {
					t.Fatal("provider batch was not durably saved before dispatch")
				}
				batch = body.BatchID
				f.name = req.Name
				return envelope(r, map[string]any{"finish": true}), nil
			}
			out, err := f.client.BeginOperation(context.Background(), req)
			if err != nil || !out.Complete || batch == "" {
				t.Fatalf("new rename did not confirm: %v", err)
			}
			reads := f.reads
			f.restart(t)
			out, err = f.client.BeginOperation(context.Background(), req)
			var p operationPayload
			_ = json.Unmarshal(out.Receipt.Payload, &p)
			if err != nil || !out.Complete || f.writes != 1 || f.reads != reads || p.BatchID != batch || out.Receipt.ID != req.ID {
				t.Fatal("confirmed cold retry changed batch/business identity or issued I/O")
			}
		})
	}
}

func TestRenameContractIndependentHTTPFailureIsSafeAndNeverResubmits(t *testing.T) {
	f := newRenameContractFixture(t)
	req := renameContractRequest("unknown-operation-with-http-error")
	f.onWrite = func(r *http.Request) (*http.Response, error) {
		return response(r, http.StatusBadRequest, map[string]any{"status": -1, "errno": 987654, "error_info": "private-response-cookie-token-url", "data": map[string]any{"access_token": "must-not-persist"}}), nil
	}
	_, err := f.client.BeginOperation(context.Background(), req)
	var diagnostic *Failure
	if !errors.As(err, &diagnostic) || diagnostic.Code != cloud.CodeMutationUnknown || !diagnostic.UnknownOutcome || diagnostic.HTTPStatus != 400 || diagnostic.Errno != 987654 || err.Error() != cloud.CodeMutationUnknown {
		t.Fatalf("safe numeric mutation diagnosis lost: %v", err)
	}
	original, _ := f.store.Load(context.Background(), req.ID)
	var p operationPayload
	if json.Unmarshal(original.Payload, &p) != nil || p.SubmitFailure == nil || p.SubmitFailure.HTTPStatus != 400 || p.SubmitFailure.Errno != 987654 || p.BatchID == "" || original.State != "unknown" {
		t.Fatal("safe submission diagnostic was not persisted with original batch")
	}
	if strings.Contains(string(original.Payload), "private-response") || strings.Contains(string(original.Payload), "must-not-persist") {
		t.Fatal("upstream text or token leaked into durable receipt")
	}
	f.restart(t)
	_, err = f.client.BeginOperation(context.Background(), req)
	errorCode(t, err, cloud.CodeMutationUnknown)
	after, _ := f.store.Load(context.Background(), req.ID)
	if f.writes != 1 || string(after.Payload) != string(original.Payload) || after.Revision != original.Revision {
		t.Fatal("unknown cold retry regenerated batch, rewrote receipt or resubmitted")
	}
	reads := f.reads
	req.Name = "different.mkv"
	_, err = f.client.BeginOperation(context.Background(), req)
	errorCode(t, err, CodeIdentity)
	if f.writes != 1 || f.reads != reads {
		t.Fatal("changed request reached provider")
	}
}

func TestRenameContractIndependentLegacyReceiptsRemainQueryOnly(t *testing.T) {
	for _, state := range []string{"prepared", "submitting", "unknown", "accepted", "failed", "confirmed"} {
		t.Run(state, func(t *testing.T) {
			f := newRenameContractFixture(t)
			req := renameContractRequest("legacy-long-business-operation")
			p := operationPayload{Request: req, Before: agentItem{ID: "f", Parent: "A", Name: "old.mkv", Size: 9}, Finished: state == "accepted", Outputs: []cloud.Item{{ID: req.ItemID, Name: req.Name, Size: 9}}}
			raw, _ := json.Marshal(p)
			original := cloud.OperationReceipt{ID: req.ID, Provider: cloud.ProviderQuark, AccountID: "agent-account", Route: string(RouteAgent), Kind: "rename", State: state, Revision: 3, Payload: raw}
			// Seed a pre-upgrade durable receipt; Create intentionally accepts
			// only fresh prepared/revision-1 intents, which this is not.
			f.store.receipts[req.ID] = original
			f.restart(t)
			_, _ = f.client.BeginOperation(context.Background(), req)
			after, _ := f.store.Load(context.Background(), req.ID)
			if f.writes != 0 || string(after.Payload) != string(raw) || after.Revision != original.Revision {
				t.Fatal("legacy receipt acquired replacement intent or was resubmitted")
			}
		})
	}
	// A legacy unknown request whose original effect becomes visible may be
	// confirmed by reads, but still must not gain a new provider batch.
	f := newRenameContractFixture(t)
	req := renameContractRequest("legacy-visible-rename")
	p := operationPayload{Request: req, Before: agentItem{ID: "f", Parent: "A", Name: "old.mkv", Size: 9}}
	raw, _ := json.Marshal(p)
	f.store.receipts[req.ID] = cloud.OperationReceipt{ID: req.ID, Provider: cloud.ProviderQuark, AccountID: "agent-account", Route: string(RouteAgent), Kind: "rename", State: "unknown", Revision: 3, Payload: raw}
	f.name = req.Name
	f.restart(t)
	out, err := f.client.BeginOperation(context.Background(), req)
	_ = json.Unmarshal(out.Receipt.Payload, &p)
	if err != nil || !out.Complete || f.writes != 0 || p.BatchID != "" || out.Receipt.ID != req.ID {
		t.Fatal("legacy read reconciliation replaced or resubmitted its intent")
	}
}

type renameContractFailStore struct {
	*memoryOperations
	create bool
}

func (s renameContractFailStore) Create(ctx context.Context, r cloud.OperationReceipt) error {
	if s.create {
		return errors.New("synthetic create failure")
	}
	return s.memoryOperations.Create(ctx, r)
}

func (s renameContractFailStore) Update(context.Context, uint64, cloud.OperationReceipt) error {
	return errors.New("synthetic submitting checkpoint failure")
}

func TestRenameContractIndependentCheckpointFailureNeverDispatches(t *testing.T) {
	for _, create := range []bool{true, false} {
		f := newRenameContractFixture(t)
		f.client.config.Operations = renameContractFailStore{memoryOperations: f.store, create: create}
		req := renameContractRequest("checkpoint-failure")
		if _, err := f.client.BeginOperation(context.Background(), req); err == nil || f.writes != 0 {
			t.Fatal("failed durable intent reached mutation endpoint")
		}
		if !create {
			before, _ := f.store.Load(context.Background(), req.ID)
			var p operationPayload
			if json.Unmarshal(before.Payload, &p) != nil || p.BatchID == "" || before.State != "prepared" {
				t.Fatal("prepared intent did not retain provider batch")
			}
			f.restart(t)
			_, err := f.client.BeginOperation(context.Background(), req)
			errorCode(t, err, cloud.CodeMutationUnknown)
			after, _ := f.store.Load(context.Background(), req.ID)
			if f.writes != 0 || string(after.Payload) != string(before.Payload) {
				t.Fatal("prepared restart auto-dispatched or changed intent")
			}
		}
	}
}
