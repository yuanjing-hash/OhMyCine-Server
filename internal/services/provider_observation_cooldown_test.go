package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
)

func TestProviderObservationRetryDeadlineActuallyDefersRequests(t *testing.T) {
	f := newObservationFixture(t)
	f.reader.hook = func(string, int64) (ObservationDirectoryPage, error, bool) {
		return ObservationDirectoryPage{}, errors.New("fixture transient"), true
	}
	if err := f.s.RunDue(context.Background(), 1); err == nil {
		t.Fatal("missing read failure")
	}
	calls := len(f.reader.calls)
	f.advance(2 * time.Second)
	f.reader.hook = nil
	if err := f.s.RunDue(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if len(f.reader.calls) != calls {
		t.Fatal("retried before published retry deadline")
	}
	status, err := f.s.Status(context.Background(), f.scope.ID)
	if err != nil || status.ErrorCode == "" || status.NextRetryAt == nil {
		t.Fatalf("failure invisible %+v %v", status, err)
	}
	f.advance(time.Minute)
	if err = f.s.RunDue(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if len(f.reader.calls) != calls+1 {
		t.Fatal("did not resume after retry deadline")
	}
	status, err = f.s.Status(context.Background(), f.scope.ID)
	if err != nil || status.ErrorCode != "" {
		t.Fatalf("recovered failure remains %+v %v", status, err)
	}
}

func TestProviderObservationCooldownSharedWithInteractiveAndAccountRebind(t *testing.T) {
	f := newObservationFixture(t)
	if err := f.db.AutoMigrate(&models.Connection{}); err != nil {
		t.Fatal(err)
	}
	connection := models.Connection{ID: 1, Name: "fixture", NameNormalized: "fixture", Provider: models.ConnectionProviderQuark, AccountID: "one", Enabled: true}
	if err := f.db.Create(&connection).Error; err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := CheckQuarkAccountCooldown(ctx, f.db, connection.ID); err != nil {
		t.Fatal(err)
	}
	RecordQuarkAccountCooldown(f.db, connection.ID, connection.AccountID, &quark.Failure{Code: quark.CodeLimited, RetryAfter: time.Minute})
	var failure *quark.Failure
	if err := CheckQuarkAccountCooldown(ctx, f.db, connection.ID); !errors.As(err, &failure) || failure.RetryAfter <= 0 {
		t.Fatalf("interactive ignored cooldown %v", err)
	}
	keys, err := quarkAccountCooldownKeys(ctx, f.db, connection.ID)
	if err != nil {
		t.Fatal(err)
	}
	var budget models.ProviderObservationAccountBudget
	if err = f.db.First(&budget, "account_key = ?", keys[1]).Error; err != nil || !budget.CooldownUntil.After(time.Now()) {
		t.Fatalf("observer account cooldown missing %+v %v", budget, err)
	}
	if err = f.db.Model(&connection).Update("account_id", "two").Error; err != nil {
		t.Fatal(err)
	}
	if err = CheckQuarkAccountCooldown(ctx, f.db, connection.ID); !errors.As(err, &failure) {
		t.Fatal("authorization rebind bypassed existing connection cooldown")
	}
}
