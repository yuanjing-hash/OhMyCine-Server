package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
)

func TestQuarkAccountCooldownCannotBeBypassedByDuplicateConnection(t *testing.T) {
	f := newObservationFixture(t)
	if err := f.db.AutoMigrate(&models.Connection{}); err != nil {
		t.Fatal(err)
	}
	connections := []models.Connection{
		{ID: 1, Name: "first", NameNormalized: "first", Provider: models.ConnectionProviderQuark, AccountID: quarkAccountReference("same-tv-account"), Enabled: true},
		{ID: 2, Name: "second", NameNormalized: "second", Provider: models.ConnectionProviderQuark, AccountID: quarkAccountReference("same-tv-account"), Enabled: true},
		{ID: 3, Name: "unrelated", NameNormalized: "unrelated", Provider: models.ConnectionProviderQuark, AccountID: quarkAccountReference("another-tv-account"), Enabled: true},
	}
	if err := f.db.Create(&connections).Error; err != nil {
		t.Fatal(err)
	}
	RecordQuarkAccountCooldown(f.db, connections[0].ID, connections[0].AccountID, &quark.Failure{Code: quark.CodeLimited, RetryAfter: time.Minute})
	var limited *quark.Failure
	if err := CheckQuarkAccountCooldown(context.Background(), f.db, connections[1].ID); !errors.As(err, &limited) || limited.RetryAfter <= 0 {
		t.Fatalf("same account bypassed provider cooldown through another connection: %v", err)
	}
	if err := CheckQuarkAccountCooldown(context.Background(), f.db, connections[2].ID); err != nil {
		t.Fatalf("unrelated account inherited cooldown: %v", err)
	}
}

func TestQuarkAccountCooldownResponseRemainsBoundToRequestIdentity(t *testing.T) {
	f := newObservationFixture(t)
	if err := f.db.AutoMigrate(&models.Connection{}); err != nil {
		t.Fatal(err)
	}
	oldAccount, newAccount := quarkAccountReference("old-tv"), quarkAccountReference("new-tv")
	connections := []models.Connection{
		{ID: 1, Name: "changing", NameNormalized: "changing", Provider: "quark", AccountID: oldAccount},
		{ID: 2, Name: "old-account", NameNormalized: "old-account", Provider: "quark", AccountID: oldAccount},
		{ID: 3, Name: "new-account", NameNormalized: "new-account", Provider: "quark", AccountID: newAccount},
	}
	if err := f.db.Create(&connections).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.Model(&connections[0]).Update("account_id", newAccount).Error; err != nil {
		t.Fatal(err)
	}
	RecordQuarkAccountCooldown(f.db, 1, oldAccount, &quark.Failure{Code: quark.CodeLimited, RetryAfter: time.Minute})
	for _, id := range []uint{1, 2} {
		var limited *quark.Failure
		if err := CheckQuarkAccountCooldown(context.Background(), f.db, id); !errors.As(err, &limited) {
			t.Fatalf("connection/old account lost delayed cooldown: %v", err)
		}
	}
	if err := CheckQuarkAccountCooldown(context.Background(), f.db, 3); err != nil {
		t.Fatalf("late response attributed to new account: %v", err)
	}
}

func TestQuarkAccountBudgetUnknownIdentityStaysPrivate(t *testing.T) {
	for _, identity := range []string{"", "agent:opaque", "quark:invalid", "quark:"} {
		if models.QuarkObservationAccountKey(1, identity) == models.QuarkObservationAccountKey(2, identity) {
			t.Fatalf("unverified identity shared an account budget: %q", identity)
		}
	}
	verified := quarkAccountReference("verified-tv")
	if models.QuarkObservationAccountKey(1, verified) != models.QuarkObservationAccountKey(2, verified) {
		t.Fatal("verified TV identity not shared")
	}
}

func TestQuarkAccountBudgetSharedLeaseAndHourlyUsageAcrossConnections(t *testing.T) {
	f := newObservationFixture(t)
	ctx := context.Background()
	key := models.QuarkObservationAccountKey(1, quarkAccountReference("verified-tv"))
	if err := f.db.Model(&f.scope).Update("account_key", key).Error; err != nil {
		t.Fatal(err)
	}
	f.scope.AccountKey = key
	second, err := f.s.EnsureScope(ctx, ObservationScopeInput{LibraryID: 2, ConnectionID: 2, AccountKey: models.QuarkObservationAccountKey(2, quarkAccountReference("verified-tv")), Provider: "quark", Kind: "library", RootID: "second", RootPath: "/second", Fence: "second-fence"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.claimAccount(ctx, f.scope, true, "first"); err != nil {
		t.Fatal(err)
	}
	if err := f.s.claimAccount(ctx, second, true, "second"); !errors.Is(err, ErrObservationDeferred) {
		t.Fatalf("duplicate connection obtained parallel account lease: %v", err)
	}
	f.s.releaseAccount(ctx, key, "first")
	if err := f.db.Model(&models.ProviderObservationAccountBudget{}).Where("account_key = ?", key).Updates(map[string]any{"hot_requests": observationHourlyBudget, "cold_requests": 0}).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.s.claimAccount(ctx, second, true, "second"); !errors.Is(err, ErrObservationDeferred) {
		t.Fatalf("duplicate connection reset hourly use: %v", err)
	}
	f.advance(time.Hour)
	if err := f.s.claimAccount(ctx, second, true, "second"); err != nil {
		t.Fatalf("expired window did not replenish: %v", err)
	}
}

func TestQuarkAccountBudgetProductionScopeAndDriverUseVerifiedIdentity(t *testing.T) {
	f, libraryID, _ := quarkIntakeFixture(t)
	input, err := quarkObservationScopeTx(f.db, libraryID)
	if err != nil {
		t.Fatal(err)
	}
	var connection models.Connection
	if err := f.db.First(&connection, input.ConnectionID).Error; err != nil {
		t.Fatal(err)
	}
	keys, err := quarkAccountCooldownKeys(context.Background(), f.db, connection.ID)
	if err != nil || input.AccountKey != keys[1] || input.AccountKey != models.QuarkObservationAccountKey(connection.ID+1000, connection.AccountID) {
		t.Fatalf("production scope/cooldown did not share TV account: %v", err)
	}
	var captured cloud.Config
	service := f.libraries.connections
	service.registry = cloud.NewRegistry()
	if err := service.registry.Register(cloud.ProviderQuark, func(config cloud.Config) (cloud.Driver, error) {
		captured = config
		return f.driver, nil
	}); err != nil {
		t.Fatal(err)
	}
	service.invalidate(connection.ID)
	if _, _, err := service.driver(connection.ID); err != nil {
		t.Fatal(err)
	}
	if captured.OnCallResult == nil {
		t.Fatal("driver has no shared cooldown callback")
	}
	newAccount := quarkAccountReference("replacement-account")
	if err := f.db.Model(&connection).Update("account_id", newAccount).Error; err != nil {
		t.Fatal(err)
	}
	captured.OnCallResult(&quark.Failure{Code: quark.CodeLimited, RetryAfter: time.Minute})
	var budget models.ProviderObservationAccountBudget
	if err := f.db.First(&budget, "account_key = ?", input.AccountKey).Error; err != nil || !budget.CooldownUntil.After(time.Now()) {
		t.Fatalf("driver's in-flight identity not preserved: %v", err)
	}
	var count int64
	if err := f.db.Model(&models.ProviderObservationAccountBudget{}).Where("account_key = ?", models.QuarkObservationAccountKey(connection.ID, newAccount)).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("old driver attributed response to replacement: %v", err)
	}
}
