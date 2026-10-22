package database

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

// A late failure must roll back the merged row AND removal of legacy aliases.
// Otherwise retrying the upgrade could replenish or double-count the quota.
func TestQuarkAccountBudgetIndependentMigrationRollbackPreservesAliases(t *testing.T) {
	db, err := Open(t.TempDir() + "/quark-budget-rollback.db")
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	applyMigrationsThrough(t, db, 120)
	now := time.Now().UTC().Truncate(time.Second)
	account := "quark:" + strings.Repeat("d", 32)
	canonical := models.QuarkObservationAccountKey(1, account)
	legacyKey := func(id uint) string {
		sum := sha256.Sum256([]byte(fmt.Sprintf("quark:%d:%s", id, account)))
		return hex.EncodeToString(sum[:])
	}
	connections := []models.Connection{
		{ID: 1, Name: "first", NameNormalized: "first", Provider: "quark", AccountID: account, Enabled: true},
		{ID: 2, Name: "second", NameNormalized: "second", Provider: "quark", AccountID: account, Enabled: true},
	}
	if err := db.Create(&connections).Error; err != nil {
		t.Fatal(err)
	}
	budgets := []models.ProviderObservationAccountBudget{
		{AccountKey: canonical, WindowStart: now.Add(-5 * time.Minute), HotRequests: 80, ColdRequests: 20, CooldownUntil: now.Add(time.Minute), UpdatedAt: now},
		{AccountKey: legacyKey(1), WindowStart: now.Add(-30 * time.Minute), HotRequests: 100, ColdRequests: 30, CooldownUntil: now.Add(3 * time.Minute), NextRequestAt: now.Add(10 * time.Second), UpdatedAt: now.Add(-time.Minute)},
		{AccountKey: legacyKey(2), WindowStart: now.Add(-10 * time.Minute), HotRequests: 200, ColdRequests: 40, LeaseToken: "old-inflight-token", LeaseUntil: now.Add(20 * time.Second), UpdatedAt: now.Add(-2 * time.Minute)},
	}
	if err := db.Create(&budgets).Error; err != nil {
		t.Fatal(err)
	}
	scope := models.ProviderObservationScope{ID: "rollback-scope", LibraryID: 1, ConnectionID: 1, AccountKey: legacyKey(1), Provider: "quark", Kind: "library", RootID: "unchanged-root", Fence: "unchanged-fence", CoverageRound: 9, Enabled: true}
	if err := db.Create(&scope).Error; err != nil {
		t.Fatal(err)
	}
	readBudgets := func() []models.ProviderObservationAccountBudget {
		t.Helper()
		var rows []models.ProviderObservationAccountBudget
		if err := db.Order("account_key").Find(&rows).Error; err != nil {
			t.Fatal(err)
		}
		return rows
	}
	readScope := func() models.ProviderObservationScope {
		t.Helper()
		var row models.ProviderObservationScope
		if err := db.First(&row, "id = ?", scope.ID).Error; err != nil {
			t.Fatal(err)
		}
		return row
	}
	beforeBudgets, beforeScope := readBudgets(), readScope()
	if err := db.Exec("CREATE TRIGGER fail_budget_scope_rebind BEFORE UPDATE OF account_key ON provider_observation_scopes BEGIN SELECT RAISE(ABORT, 'synthetic scope update failure'); END").Error; err != nil {
		t.Fatal(err)
	}
	if err := Migrate(db); err == nil {
		t.Fatal("expected scope rebind failure")
	}
	if !reflect.DeepEqual(beforeBudgets, readBudgets()) || !reflect.DeepEqual(beforeScope, readScope()) {
		t.Fatal("failed migration partially consumed or rewrote budget/scope evidence")
	}
	var applied int64
	if err := db.Table("schema_migrations").Where("version = 121").Count(&applied).Error; err != nil || applied != 0 {
		t.Fatalf("failed migration recorded completion: count=%d err=%v", applied, err)
	}
	if err := db.Exec("DROP TRIGGER fail_budget_scope_rebind").Error; err != nil {
		t.Fatal(err)
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	rows := readBudgets()
	if len(rows) != 1 {
		t.Fatal("successful migration did not replace exact legacy aliases")
	}
	got := rows[0]
	if got.AccountKey != canonical || got.HotRequests != 380 || got.ColdRequests != 90 || !got.WindowStart.Equal(budgets[0].WindowStart) || !got.CooldownUntil.Equal(budgets[1].CooldownUntil) || !got.NextRequestAt.Equal(budgets[1].NextRequestAt) || !got.LeaseUntil.Equal(budgets[2].LeaseUntil) {
		t.Fatal("retry replenished consumption or shortened cooldown/admission")
	}
	if got.LeaseToken == "" || got.LeaseToken == budgets[2].LeaseToken {
		t.Fatal("old runtime token can release merged admission")
	}
	afterScope := readScope()
	beforeScope.AccountKey = canonical
	// A successful GORM update legitimately advances UpdatedAt; the scope's
	// identity, source fence, coverage and every other field must stay intact.
	if afterScope.UpdatedAt.Before(beforeScope.UpdatedAt) {
		t.Fatal("successful account rebind moved scope update time backwards")
	}
	beforeScope.UpdatedAt = afterScope.UpdatedAt
	if !reflect.DeepEqual(beforeScope, afterScope) {
		t.Fatal("successful account rebind changed original scope evidence")
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rows, readBudgets()) {
		t.Fatal("second startup changed the already-migrated budget")
	}
}
