package quark

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

type proofCheckpointFailureStore struct {
	*memoryOperations
	phase  string
	failed bool
}

func (s *proofCheckpointFailureStore) Update(ctx context.Context, revision uint64, r cloud.OperationReceipt) error {
	var p proofPayload
	var u uploadPayload
	_ = json.Unmarshal(r.Payload, &p)
	_ = json.Unmarshal(r.Payload, &u)
	phase := u.Phase
	if p.Cleanup != nil {
		phase = p.Cleanup.State
	}
	if !s.failed && phase == s.phase {
		s.failed = true
		return errors.New("private local checkpoint failure")
	}
	return s.memoryOperations.Update(ctx, revision, r)
}

func TestCommonSpacePreparedUploadResumesOriginalNeverDispatchedIntent(t *testing.T) {
	f := newProofRecoveryFixture(t, "")
	d := f.client()
	d.config.Operations = &proofCheckpointFailureStore{memoryOperations: f.store, phase: "pre_inflight"}
	_, err := d.VerifyCommonSpace(context.Background(), "proof", "/媒体库")
	if err == nil || f.pre != 0 {
		t.Fatal("failed pre-dispatch checkpoint sent a request")
	}
	var original proofPayload
	_ = json.Unmarshal(f.store.receipts["proof"].Payload, &original)
	_, err = f.client().VerifyCommonSpace(context.Background(), "proof", "/媒体库")
	if err != nil || f.pre != 1 || f.name != original.Name || !bytes.Equal(f.content, original.Bytes) {
		t.Fatalf("prepared intent did not resume in place: %v", err)
	}
}

func TestCommonSpaceCleanupCheckpointRecovery(t *testing.T) {
	for _, phase := range []string{"submitting", "accepted"} {
		t.Run(phase, func(t *testing.T) {
			f := newProofRecoveryFixture(t, "")
			d := f.client()
			d.config.Operations = &proofCheckpointFailureStore{memoryOperations: f.store, phase: phase}
			proof, err := d.VerifyCommonSpace(context.Background(), "proof", "/媒体库")
			if err != nil || !f.credentials.HasProof(proof.TVRoot, proof.AgentRoot) {
				t.Fatal("cleanup checkpoint revoked proof")
			}
			if phase == "submitting" && f.deletes != 0 || phase == "accepted" && f.deletes != 1 {
				t.Fatal("incorrect dispatch boundary")
			}
			state, err := f.client().CleanupCommonSpace(context.Background(), "proof")
			if err != nil || state != "complete" || f.deletes != 1 || f.pre != 1 {
				t.Fatalf("original cleanup failed to recover: %v", err)
			}
		})
	}
}

func TestCommonSpaceCleanupRechecksAccountImmediatelyBeforeDelete(t *testing.T) {
	f := newProofRecoveryFixture(t, "")
	f.onRead = func() {
		if f.reads == 2 {
			f.credentials.Agent.AccountID = "replacement-account"
			f.credentials.Revision++
		}
	}
	_, err := f.client().VerifyCommonSpace(context.Background(), "proof", "/媒体库")
	if err != nil || f.deletes != 0 || !f.exists {
		t.Fatal("account replacement crossed the delete fence")
	}
}

func TestCommonSpaceDoesNotDeleteBeforeCredentialsAreSaved(t *testing.T) {
	f := newProofRecoveryFixture(t, "")
	d := f.client()
	d.config.PersistCredentials = func(context.Context, uint64, json.RawMessage) error {
		return errors.New("private credential write failure")
	}
	_, err := d.VerifyCommonSpace(context.Background(), "proof", "/媒体库")
	if err == nil || f.deletes != 0 || !f.exists || len(f.credentials.Proofs) != 0 {
		t.Fatal("cleanup preceded durable proof")
	}
	_, err = f.client().VerifyCommonSpace(context.Background(), "proof", "/媒体库")
	if err != nil || f.deletes != 1 || f.pre != 1 || f.put != 1 {
		t.Fatalf("retry did not persist proof then clean original file: %v", err)
	}
}

func TestCommonSpaceCleanupRejectsModifiedContentBeforeDelete(t *testing.T) {
	f := newProofRecoveryFixture(t, "")
	f.onRead = func() {
		if f.reads == 2 {
			f.wrongContent = true
		}
	}
	proof, err := f.client().VerifyCommonSpace(context.Background(), "proof", "/媒体库")
	if err != nil || f.deletes != 0 || !f.exists || !f.credentials.HasProof(proof.TVRoot, proof.AgentRoot) {
		t.Fatal("cleanup deleted changed challenge or revoked proof")
	}
	state, err := f.client().CleanupCommonSpace(context.Background(), "proof")
	if err == nil || state != "pending" || f.deletes != 0 {
		t.Fatal("modified challenge was not fenced")
	}
}
