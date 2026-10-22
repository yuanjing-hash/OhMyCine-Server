package services

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/credential"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

func TestQuarkOperationReceiptEncryptedFrozenRouteCASAndRestart(t *testing.T) {
	f := newObservationFixture(t)
	secrets, err := credential.Open("", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{42}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	store := NewQuarkOperationStore(f.db, secrets, 1)
	ctx := WithObservationOperationScope(context.Background(), f.scope.ID, "root", f.scope.Fence)
	r := cloud.OperationReceipt{ID: "operation-private", Provider: "quark", Route: "agent", AccountID: "account-private", Kind: "upload", State: "prepared", Revision: 1, Payload: json.RawMessage(`{"signed_url":"https://private.invalid/?secret=hidden-token"}`)}
	if err = store.Create(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err = store.Create(ctx, r); !errors.Is(err, ErrQuarkOperationConflict) {
		t.Fatalf("duplicate=%v", err)
	}
	var row models.QuarkOperationReceipt
	if err = f.db.First(&row).Error; err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(row)
	if bytes.Contains(row.CheckpointCiphertext, []byte("hidden-token")) || bytes.Contains(encoded, []byte("account-private")) || row.ScopeID != f.scope.ID {
		t.Fatal("receipt leaked secrets or lost scope")
	}
	r.State, r.Revision = "submitting", 2
	if err = store.Update(ctx, 1, r); err != nil {
		t.Fatal(err)
	}
	if err = store.Update(ctx, 1, r); !errors.Is(err, ErrQuarkOperationConflict) {
		t.Fatalf("stale CAS=%v", err)
	}
	r.State, r.Revision = "unknown", 3
	if err = store.Update(ctx, 2, r); err != nil {
		t.Fatal(err)
	}
	restarted := NewQuarkOperationStore(f.db, secrets, 1)
	got, err := restarted.Load(ctx, r.ID)
	if err != nil || !bytes.Equal(got.Payload, r.Payload) || got.State != "unknown" {
		t.Fatalf("restart=%+v %v", got, err)
	}
	got.State, got.Revision = "submitting", 4
	if err = store.Update(ctx, 3, got); !errors.Is(err, ErrQuarkOperationConflict) {
		t.Fatalf("unknown resubmission=%v", err)
	}
	got.State, got.Route = "confirmed", "web"
	if err = store.Update(ctx, 3, got); !errors.Is(err, ErrQuarkOperationConflict) {
		t.Fatalf("route switch=%v", err)
	}
	got.Route = "agent"
	if err = store.Update(ctx, 3, got); err != nil {
		t.Fatal(err)
	}
	if _, err = NewQuarkOperationStore(f.db, secrets, 2).Load(ctx, r.ID); !errors.Is(err, cloud.ErrOperationNotFound) {
		t.Fatalf("cross connection=%v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = restarted.Load(cancelled, "absent"); err == nil || errors.Is(err, cloud.ErrOperationNotFound) {
		t.Fatal("failed database read was interpreted as affirmative absence")
	}
	// Binding encrypted checkpoint to its connection/receipt prevents a row swap.
	if err = f.db.Model(&row).Update("checkpoint_ciphertext", bytes.Repeat([]byte{'x'}, 50)).Error; err != nil {
		t.Fatal(err)
	}
	if _, err = restarted.Load(ctx, r.ID); err == nil || errors.Is(err, cloud.ErrOperationNotFound) {
		t.Fatal("tampered encrypted checkpoint accepted or interpreted as absent")
	}
}
