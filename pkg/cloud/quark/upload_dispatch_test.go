package quark

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

type uploadAdmissionStore struct {
	*memoryOperations
	after func(cloud.OperationReceipt)
}

func (s *uploadAdmissionStore) Update(ctx context.Context, revision uint64, r cloud.OperationReceipt) error {
	if err := s.memoryOperations.Update(ctx, revision, r); err != nil {
		return err
	}
	if s.after != nil {
		s.after(r)
	}
	return nil
}

func TestCommonSpaceAdmissionBeforeDispatchResumesOriginalIntent(t *testing.T) {
	for _, phase := range []string{"pre_inflight", "finish_inflight"} {
		t.Run(phase, func(t *testing.T) {
			f := newProofRecoveryFixture(t, "")
			d := f.client()
			blocked := false
			d.config.Operations = &uploadAdmissionStore{memoryOperations: f.store, after: func(r cloud.OperationReceipt) {
				var p uploadPayload
				_ = json.Unmarshal(r.Payload, &p)
				if p.Phase == phase && !blocked {
					blocked = true
					d.api.config.BeforeCall = func(context.Context) error { return failure(CodeLimited) }
				}
			}}
			_, err := d.VerifyCommonSpace(context.Background(), "proof", "/媒体库")
			errorCode(t, err, CodeLimited)
			var original proofPayload
			_ = json.Unmarshal(f.store.receipts["proof"].Payload, &original)
			if f.finish != 0 || phase == "pre_inflight" && f.pre != 0 {
				t.Fatal("denied request reached transport")
			}
			result, err := f.client().VerifyCommonSpace(context.Background(), "proof", "/媒体库")
			if err != nil || !f.credentials.HasProof(result.TVRoot, result.AgentRoot) || f.pre != 1 || f.put != 1 || f.finish != 1 || f.name != original.Name {
				t.Fatalf("never-dispatched original intent did not resume: %v pre=%d put=%d finish=%d", err, f.pre, f.put, f.finish)
			}
		})
	}
}

func TestCommonSpaceLocalPUTValidationDoesNotClaimDispatch(t *testing.T) {
	f := newProofRecoveryFixture(t, "")
	d := f.client()
	d.api.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/open/v1/file/upload_pre" {
			f.pre++
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.name = body["file_name"].(string)
			return envelope(r, map[string]any{"task_id": "original-task", "part_size": 64, "upload_urls": []any{map[string]any{"part_number": 1, "upload_url": "https://objects.example/put", "signature_info": map[string]string{"signature": "object-only"}}}, "common_headers": map[string]string{"Cookie": "synthetic-forbidden-header"}}), nil
		}
		return f.api(r)
	})
	_, err := d.VerifyCommonSpace(context.Background(), "proof", "/媒体库")
	errorCode(t, err, CodeInvalid)
	var p uploadPayload
	r := f.store.receipts["proof/upload"]
	_ = json.Unmarshal(r.Payload, &p)
	if r.State != "accepted" || p.Phase != "pre_received" || p.InFlightPart != 0 || f.pre != 1 || f.put != 0 || f.finish != 0 {
		t.Fatal("local header validation created an ambiguous PUT checkpoint")
	}
}

func TestCommonSpaceAdmissionCheckpointCASFailureRemainsUnknown(t *testing.T) {
	f := newProofRecoveryFixture(t, "")
	d := f.client()
	collided := false
	d.config.Operations = &uploadAdmissionStore{memoryOperations: f.store, after: func(r cloud.OperationReceipt) {
		var p uploadPayload
		_ = json.Unmarshal(r.Payload, &p)
		if p.Phase == "pre_inflight" && !collided {
			collided = true
			r.Revision++
			f.store.receipts[r.ID] = r
			d.api.config.BeforeCall = func(context.Context) error { return failure(CodeLimited) }
		}
	}}
	_, err := d.VerifyCommonSpace(context.Background(), "proof", "/媒体库")
	if err == nil {
		t.Fatal("failed checkpoint was accepted")
	}
	_, err = f.client().VerifyCommonSpace(context.Background(), "proof", "/媒体库")
	errorCode(t, err, cloud.CodeMutationUnknown)
	if f.pre != 0 || f.put != 0 || f.finish != 0 {
		t.Fatal("CAS conflict authorized a fresh request")
	}
}

func TestUploadTransportFailureIsNotProvenUnsent(t *testing.T) {
	f := newProofRecoveryFixture(t, "pre")
	d := f.client()
	_, err := d.VerifyCommonSpace(context.Background(), "proof", "/媒体库")
	var provider *Failure
	if !errors.As(err, &provider) || !provider.UnknownOutcome || f.pre != 1 {
		t.Fatal("transport failure lost its ambiguous outcome")
	}
}

func TestUploadAdmissionEvidenceIsLimitedToBeforeDo(t *testing.T) {
	for _, mode := range []string{"before_call", "cooldown", "cancelled", "transport"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			d := fixtureClient(t, func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("ambiguous transport") })
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mode {
			case "before_call":
				d.api.config.BeforeCall = func(context.Context) error { return failure(CodeLimited) }
			case "cooldown":
				d.api.gate.until = d.api.clock().Add(time.Minute)
			case "cancelled":
				cancel()
			}
			r, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://open-api-drive.quark.cn/open/v1/file/upload_pre", nil)
			_, err := d.api.call(ctx, r)
			var unsent *requestNotDispatchedError
			if mode == "transport" {
				if errors.As(err, &unsent) || calls != 1 {
					t.Fatal("Do failure was falsely proven unsent")
				}
			} else if !errors.As(err, &unsent) || calls != 0 {
				t.Fatal("pre-dispatch evidence missing")
			}
		})
	}
}
