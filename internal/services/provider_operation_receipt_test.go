package services

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/credential"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/database"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"testing"
)

func TestProviderOperationReceiptEncryptedAccountScopeCASAndRestart(t *testing.T) {
	db, err := database.Open(t.TempDir() + "/receipts.db")
	if err != nil {
		t.Fatal(err)
	}
	sql, _ := db.DB()
	defer sql.Close()
	if err = database.Migrate(db); err != nil {
		t.Fatal(err)
	}
	credentials, err := credential.Open("", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{17}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	store := NewProviderOperationStore(db, credentials, 7, "pan115", "account-private")
	ctx := context.Background()
	r := cloud.OperationReceipt{ID: "omc-plan-private/submit", Provider: "pan115", Route: "cookie", AccountID: "account-private", Kind: "selective_offline_submit", State: "prepared", Revision: 1, Payload: json.RawMessage(`{"seed":"seed-private","wanted":[2]}`)}
	if err = store.Create(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err = store.Create(ctx, r); !errors.Is(err, ErrProviderOperationConflict) {
		t.Fatal("duplicate", err)
	}
	var row models.ProviderOperationReceipt
	if err = db.First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(row.CheckpointCiphertext, []byte("seed-private")) || row.AccountKey == r.AccountID {
		t.Fatal("private receipt leaked")
	}
	r.Revision++
	r.State = "submitting"
	if err = store.Update(ctx, 1, r); err != nil {
		t.Fatal(err)
	}
	r.Revision++
	r.State = "unknown"
	if err = store.Update(ctx, 2, r); err != nil {
		t.Fatal(err)
	}
	restart := NewProviderOperationStore(db, credentials, 7, "pan115", "account-private")
	loaded, err := restart.Load(ctx, r.ID)
	if err != nil || loaded.State != "unknown" || !bytes.Equal(loaded.Payload, r.Payload) {
		t.Fatal("restart lost unknown", err)
	}
	loaded.Revision++
	loaded.State = "submitting"
	if err = restart.Update(ctx, 3, loaded); !errors.Is(err, ErrProviderOperationConflict) {
		t.Fatal("unknown replay accepted", err)
	}
	loaded.State = "confirmed"
	loaded.Payload = json.RawMessage(`{"seed":"seed-private","wanted":[2],"confirmed":true}`)
	if err = restart.Update(ctx, 3, loaded); err != nil {
		t.Fatal(err)
	}
	if err = restart.Update(ctx, 3, loaded); !errors.Is(err, ErrProviderOperationConflict) {
		t.Fatal("stale cas", err)
	}
	changed := NewProviderOperationStore(db, credentials, 7, "pan115", "another-account")
	if _, err = changed.Load(ctx, r.ID); err == nil || errors.Is(err, cloud.ErrOperationNotFound) {
		t.Fatal("account switch became absence", err)
	}
	r.AccountID = "another-account"
	if err = changed.Create(ctx, r); err == nil {
		t.Fatal("account switch overwrote receipt")
	}
	if _, err = NewProviderOperationStore(db, credentials, 8, "pan115", "account-private").Load(ctx, r.ID); !errors.Is(err, cloud.ErrOperationNotFound) {
		t.Fatal("wrong connection", err)
	}
	if _, err = NewQuarkOperationStore(db, credentials, 7).Load(ctx, r.ID); !errors.Is(err, cloud.ErrOperationNotFound) {
		t.Fatal("115 leaked into old quark namespace", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = restart.Load(cancelled, "absent"); err == nil || errors.Is(err, cloud.ErrOperationNotFound) {
		t.Fatal("cancelled DB read became absence", err)
	}
	if err = db.Model(&row).Update("checkpoint_ciphertext", []byte("tampered")).Error; err != nil {
		t.Fatal(err)
	}
	if _, err = restart.Load(ctx, loaded.ID); err == nil || errors.Is(err, cloud.ErrOperationNotFound) {
		t.Fatal("tamper accepted", err)
	}
}
func TestProviderOperationStoreInitialCacheBindsOnlyConfirmedAccount(t *testing.T) {
	db, err := database.Open(t.TempDir() + "/first.db")
	if err != nil {
		t.Fatal(err)
	}
	sql, _ := db.DB()
	defer sql.Close()
	if err = database.Migrate(db); err != nil {
		t.Fatal(err)
	}
	credentials, _ := credential.Open("", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{18}, 32)))
	connection := models.Connection{Name: "115", NameNormalized: "115", Provider: "pan115", Enabled: true, Revision: 1}
	if err = db.Create(&connection).Error; err != nil {
		t.Fatal(err)
	}
	store := NewProviderOperationStore(db, credentials, connection.ID, "pan115", "")
	r := cloud.OperationReceipt{ID: "first-plan", Provider: "pan115", Route: "cookie", AccountID: "confirmed-account", Kind: "selective_offline_plan", State: "prepared", Revision: 1, Payload: json.RawMessage(`{}`)}
	if err = store.Create(context.Background(), r); err == nil {
		t.Fatal("unconfirmed account admitted")
	}
	if err = db.Model(&connection).Update("account_id", "confirmed-account").Error; err != nil {
		t.Fatal(err)
	}
	if err = store.Create(context.Background(), r); err != nil {
		t.Fatal("initial cached driver did not bind healthy probe", err)
	}
	if err = db.Model(&connection).Update("account_id", "other-account").Error; err != nil {
		t.Fatal(err)
	}
	if _, err = store.Load(context.Background(), r.ID); err == nil || errors.Is(err, cloud.ErrOperationNotFound) {
		t.Fatal("changed account became new operation", err)
	}
}

func TestConnectionFactoryInjectsNeutral115OperationStore(t *testing.T) {
	driver := &fakeCloudDriver{account: cloud.Account{ID: "100"}}
	_, _, service, actor := newConnectionTestService(t, driver)
	created, err := service.Create(actor, ConnectionInput{Name: "scope", Provider: cloud.ProviderPan115, Cookie: testPan115Cookie, Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	var captured cloud.Config
	registry := cloud.NewRegistry()
	if err = registry.Register(cloud.ProviderPan115, func(config cloud.Config) (cloud.Driver, error) { captured = config; return driver, nil }); err != nil {
		t.Fatal(err)
	}
	service.registry = registry
	service.mu.Lock()
	delete(service.drivers, created.ID)
	delete(service.driverRevisions, created.ID)
	service.mu.Unlock()
	if _, _, err = service.Driver(actor, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := captured.Operations.(*providerOperationStore); !ok {
		t.Fatal("neutral115 store not injected")
	}
}

func TestProviderOperationExclusiveResourceClaimCrossesConnectionAliases(t *testing.T) {
	db, err := database.Open(t.TempDir() + "/claims.db")
	if err != nil {
		t.Fatal(err)
	}
	sql, _ := db.DB()
	defer sql.Close()
	if err = database.Migrate(db); err != nil {
		t.Fatal(err)
	}
	secrets, _ := credential.Open("", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{19}, 32)))
	left := NewProviderOperationStore(db, secrets, 1, "pan115", "same-account").(cloud.ExclusiveOperationStore)
	right := NewProviderOperationStore(db, secrets, 2, "pan115", "same-account").(cloud.ExclusiveOperationStore)
	r := cloud.OperationReceipt{ID: "owner-a", Provider: "pan115", AccountID: "same-account", Route: "cookie", Kind: "native_owner", State: "prepared", Revision: 1, Payload: json.RawMessage(`{"plan":"one"}`)}
	if err = left.Claim(context.Background(), providerReceiptHash("resource"), r); err != nil {
		t.Fatal(err)
	}
	r.ID = "owner-b"
	if err = right.Claim(context.Background(), providerReceiptHash("resource"), r); !errors.Is(err, ErrProviderOperationConflict) {
		t.Fatal("connection alias double claimed", err)
	}
	if _, err = left.Load(context.Background(), "owner-a"); err != nil {
		t.Fatal("claim envelope failed", err)
	}
}

func TestProviderOperationTemporaryResourceCASReleasePreservesReceiptAndSelectedOwner(t *testing.T) {
	db, err := database.Open(t.TempDir() + "/release.db")
	if err != nil {
		t.Fatal(err)
	}
	sql, _ := db.DB()
	defer sql.Close()
	if err = database.Migrate(db); err != nil {
		t.Fatal(err)
	}
	secrets, _ := credential.Open("", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{29}, 32)))
	left := NewProviderOperationStore(db, secrets, 1, "pan115", "100").(cloud.ResourceOperationStore)
	right := NewProviderOperationStore(db, secrets, 2, "pan115", "100").(cloud.ResourceOperationStore)
	key := providerReceiptHash("known resource")
	r := cloud.OperationReceipt{ID: "ordinary-operation", Provider: "pan115", AccountID: "100", Route: "cookie", Kind: cloud.OfflineResourceGuardKind, State: "prepared", Revision: 1, Payload: json.RawMessage(`{"native_ack":false}`)}
	ctx := context.Background()
	if err = left.Claim(ctx, key, r); err != nil {
		t.Fatal(err)
	}
	if observed, e := right.ResourceClaim(ctx, key); e != nil || observed.ID != r.ID {
		t.Fatal("alias cannot read original encrypted owner", e)
	}
	r.Revision++
	r.State = "submitting"
	if err = left.Update(ctx, 1, r); err != nil {
		t.Fatal(err)
	}
	confirmed := r
	confirmed.Revision++
	confirmed.State = "confirmed"
	confirmed.Payload = json.RawMessage(`{"native_ack":true}`)
	if err = right.ReleaseResourceClaim(ctx, key, 2, confirmed); err == nil {
		t.Fatal("another Connection released owner")
	}
	if err = left.ReleaseResourceClaim(ctx, key, 1, confirmed); err == nil {
		t.Fatal("stale CAS released owner")
	}
	if err = left.ReleaseResourceClaim(ctx, key, 2, confirmed); err != nil {
		t.Fatal(err)
	}
	if _, err = left.ResourceClaim(ctx, key); !errors.Is(err, cloud.ErrOperationNotFound) {
		t.Fatal("resource not released", err)
	}
	if saved, e := left.Load(ctx, r.ID); e != nil || saved.State != "confirmed" || string(saved.Payload) != string(confirmed.Payload) {
		t.Fatal("released receipt lost", e)
	}
	selected := cloud.OperationReceipt{ID: "selected-successor", Provider: "pan115", AccountID: "100", Route: "cookie", Kind: "selective_offline_owner", State: "prepared", Revision: 1, Payload: json.RawMessage(`{"plan":"selected"}`)}
	if err = right.Claim(ctx, key, selected); err != nil {
		t.Fatal(err)
	}
	if err = left.ReleaseResourceClaim(ctx, key, 2, confirmed); err == nil {
		t.Fatal("late release succeeded")
	}
	selected.Revision++
	selected.State = "confirmed"
	if err = right.ReleaseResourceClaim(ctx, key, 1, selected); err == nil {
		t.Fatal("permanent selected claim released as temporary")
	}
	if owner, e := right.ResourceClaim(ctx, key); e != nil || owner.ID != selected.ID {
		t.Fatal("late release lost successor", e)
	}
}

func TestProviderOperationUndispatchedFailureCanRearmButStaleWaiterCannotDispatch(t *testing.T) {
	db, err := database.Open(t.TempDir() + "/rearm.db")
	if err != nil {
		t.Fatal(err)
	}
	sql, _ := db.DB()
	defer sql.Close()
	if err = database.Migrate(db); err != nil {
		t.Fatal(err)
	}
	secrets, _ := credential.Open("", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{31}, 32)))
	store := NewProviderOperationStore(db, secrets, 1, "pan115", "100").(cloud.ResourceOperationStore)
	key := providerReceiptHash("resource")
	r := cloud.OperationReceipt{ID: "original", Provider: "pan115", AccountID: "100", Route: "cookie", Kind: cloud.OfflineResourceGuardKind, State: "prepared", Revision: 1, Payload: json.RawMessage(`{"request":"frozen"}`)}
	ctx := context.Background()
	if err = store.Claim(ctx, key, r); err != nil {
		t.Fatal(err)
	}
	failed := r
	failed.Revision++
	failed.State = "failed"
	if err = store.ReleaseResourceClaim(ctx, key, 1, failed); err != nil {
		t.Fatal(err)
	}
	oldWaiter := r
	oldWaiter.Revision++
	oldWaiter.State = "submitting"
	if err = store.Update(ctx, 1, oldWaiter); err == nil {
		t.Fatal("stale prepared waiter dispatched")
	}
	retry := failed
	retry.Revision++
	retry.State = "prepared"
	changed := retry
	changed.Payload = json.RawMessage(`{"request":"changed"}`)
	if err = store.ReclaimResourceClaim(ctx, key, failed.Revision, changed); err == nil {
		t.Fatal("rearm changed source")
	}
	if err = store.ReclaimResourceClaim(ctx, key, failed.Revision, retry); err != nil {
		t.Fatal(err)
	}
	if claimed, e := store.ResourceClaim(ctx, key); e != nil || claimed.Revision != retry.Revision {
		t.Fatal("original did not reacquire", e)
	}
	submitted := retry
	submitted.Revision++
	submitted.State = "submitting"
	if err = store.Update(ctx, retry.Revision, submitted); err != nil {
		t.Fatal(err)
	}
	failed = submitted
	failed.Revision++
	failed.State = "failed"
	failed.Payload = json.RawMessage(`{"request":"frozen","DefiniteRejection":true}`)
	if err = store.ReleaseResourceClaim(ctx, key, submitted.Revision, failed); err != nil {
		t.Fatal(err)
	}
	selected := cloud.OperationReceipt{ID: "selected", Provider: "pan115", AccountID: "100", Route: "cookie", Kind: "selective_offline_owner", State: "prepared", Revision: 1, Payload: json.RawMessage(`{"owner":"selected"}`)}
	if err = store.Claim(ctx, key, selected); err != nil {
		t.Fatal(err)
	}
	retry = failed
	retry.Revision++
	retry.State = "prepared"
	if err = store.ReclaimResourceClaim(ctx, key, failed.Revision, retry); err == nil {
		t.Fatal("late rearm crossed selected owner")
	}
}
