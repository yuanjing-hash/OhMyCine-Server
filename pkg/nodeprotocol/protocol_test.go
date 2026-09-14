package nodeprotocol

import (
	"encoding/json"
	"testing"
	"time"
)

func TestOperationPlanDigestCanonicalizesPayload(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	left := OperationPlan{ProtocolVersion: 1, OperationKey: "op-1", TaskID: "task-1", NodeID: "node-1", Kind: "qbittorrent_submit", PlanRevision: 1, LeaseEpoch: 1, LeaseExpiresAt: now, Payload: json.RawMessage(`{"b":2,"a":1}`)}
	right := left
	right.Payload = json.RawMessage(`{ "a": 1, "b": 2 }`)
	leftDigest, err := left.Digest()
	if err != nil {
		t.Fatal(err)
	}
	rightDigest, err := right.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if leftDigest != rightDigest {
		t.Fatalf("digest differs: %s != %s", leftDigest, rightDigest)
	}
}

func TestOperationPlanRejectsLongLease(t *testing.T) {
	now := time.Now().UTC()
	plan := OperationPlan{ProtocolVersion: 1, OperationKey: "op-1", TaskID: "task-1", NodeID: "node-1", Kind: "test", PlanRevision: 1, LeaseEpoch: 1, LeaseExpiresAt: now.Add(16 * time.Minute), Payload: json.RawMessage(`{}`)}
	if err := plan.Validate(now); err == nil {
		t.Fatal("expected long lease rejection")
	}
}

func TestCapabilitiesCanonical(t *testing.T) {
	got := (Capabilities{Codes: []string{"z", "a", "z"}}).Canonical()
	if len(got.Codes) != 2 || got.Codes[0] != "a" || got.Codes[1] != "z" {
		t.Fatalf("unexpected codes: %#v", got.Codes)
	}
}
