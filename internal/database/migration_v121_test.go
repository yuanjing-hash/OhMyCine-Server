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

func TestMigrationQuarkAccountBudgetPreservesUsageCooldownAndObservationIdentity(t *testing.T) {
	db, err := Open(t.TempDir() + "/budget-upgrade.db")
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	applyMigrationsThrough(t, db, 120)
	now := time.Now().UTC().Truncate(time.Second)
	account := "quark:" + strings.Repeat("a", 32)
	connections := []models.Connection{
		{ID: 1, Name: "one", NameNormalized: "one", Provider: "quark", AccountID: account, Enabled: true},
		{ID: 2, Name: "two", NameNormalized: "two", Provider: "quark", AccountID: account, Enabled: true},
		{ID: 3, Name: "expired", NameNormalized: "expired", Provider: "quark", AccountID: account},
		{ID: 4, Name: "unknown", NameNormalized: "unknown", Provider: "quark", AccountID: ""},
	}
	if err := db.Create(&connections).Error; err != nil {
		t.Fatal(err)
	}
	legacyKey := func(id uint, value string) string {
		sum := sha256.Sum256([]byte(fmt.Sprintf("quark:%d:%s", id, value)))
		return hex.EncodeToString(sum[:])
	}
	canonical := models.QuarkObservationAccountKey(1, account)
	unknownKey, historicalKey := legacyKey(4, ""), legacyKey(1, "quark:"+strings.Repeat("b", 32))
	budgets := []models.ProviderObservationAccountBudget{
		{AccountKey: canonical, WindowStart: now.Add(-5 * time.Minute), HotRequests: 50, ColdRequests: 10, UpdatedAt: now},
		{AccountKey: legacyKey(1, account), WindowStart: now.Add(-40 * time.Minute), HotRequests: 100, ColdRequests: 20, CooldownUntil: now.Add(2 * time.Minute)},
		{AccountKey: legacyKey(2, account), WindowStart: now.Add(-20 * time.Minute), HotRequests: 200, ColdRequests: 30, NextRequestAt: now.Add(5 * time.Second), LeaseToken: "inflight-one", LeaseUntil: now.Add(15 * time.Second)},
		{AccountKey: legacyKey(3, account), WindowStart: now.Add(-2 * time.Hour), HotRequests: 900, ColdRequests: 900, CooldownUntil: now.Add(3 * time.Minute), LeaseToken: "inflight-two", LeaseUntil: now.Add(25 * time.Second)},
		{AccountKey: unknownKey, WindowStart: now, HotRequests: 40, CooldownUntil: now.Add(4 * time.Minute)},
		{AccountKey: historicalKey, WindowStart: now, HotRequests: 60, CooldownUntil: now.Add(6 * time.Minute)},
	}
	if err := db.Create(&budgets).Error; err != nil {
		t.Fatal(err)
	}
	scopes := []models.ProviderObservationScope{
		{ID: "keep-scope", LibraryID: 11, ConnectionID: 1, AccountKey: legacyKey(1, account), Provider: "quark", RootID: "tv:root", Fence: "same-fence", Enabled: true, CoverageRound: 7},
		{ID: "unknown-scope", ConnectionID: 4, AccountKey: unknownKey, Provider: "quark"},
		{ID: "history-scope", ConnectionID: 1, AccountKey: historicalKey, Provider: "quark"},
	}
	if err := db.Create(&scopes).Error; err != nil {
		t.Fatal(err)
	}
	batch := models.ProviderObservationBatch{ID: "keep-batch", ScopeID: scopes[0].ID, DirectoryID: "tv:root", Fence: "same-fence", State: "publishing", Revision: 4}
	item := models.ProviderObservationItem{ScopeID: scopes[0].ID, ResourceID: "tv:file", Name: "keep.mkv", RelativePath: "/keep.mkv", Revision: 8, CatalogSourceEpoch: 3}
	if err := db.Create(&batch).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&item).Error; err != nil {
		t.Fatal(err)
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	var merged models.ProviderObservationAccountBudget
	if err := db.First(&merged, "account_key = ?", canonical).Error; err != nil {
		t.Fatal(err)
	}
	if merged.HotRequests != 350 || merged.ColdRequests != 60 || !merged.WindowStart.Equal(budgets[0].WindowStart) || !merged.CooldownUntil.Equal(budgets[3].CooldownUntil) || !merged.NextRequestAt.Equal(budgets[2].NextRequestAt) || !merged.LeaseUntil.Equal(budgets[3].LeaseUntil) || merged.LeaseToken == "" || merged.LeaseToken == "inflight-two" {
		t.Fatalf("migration reset or weakened account admission: %+v", merged)
	}
	var actual models.ProviderObservationScope
	if err := db.First(&actual, "id = ?", scopes[0].ID).Error; err != nil {
		t.Fatal(err)
	}
	if actual.AccountKey != canonical || actual.Fence != scopes[0].Fence || actual.CoverageRound != 7 || actual.RootID != scopes[0].RootID || !actual.Enabled {
		t.Fatal("scope identity/evidence was rebuilt")
	}
	var gotBatch models.ProviderObservationBatch
	var gotItem models.ProviderObservationItem
	if err := db.First(&gotBatch, "id = ?", batch.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&gotItem, "scope_id = ? AND resource_id = ?", item.ScopeID, item.ResourceID).Error; err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(batch, gotBatch) || !reflect.DeepEqual(item, gotItem) {
		t.Fatal("batch or file identity changed")
	}
	for _, index := range []int{4, 5} {
		var got models.ProviderObservationAccountBudget
		if err := db.First(&got, "account_key = ?", budgets[index].AccountKey).Error; err != nil || !reflect.DeepEqual(got, budgets[index]) {
			t.Fatalf("unknown/historical account was reinterpreted: %v", err)
		}
	}
	for _, scope := range scopes[1:] {
		var got models.ProviderObservationScope
		if err := db.First(&got, "id = ?", scope.ID).Error; err != nil || !reflect.DeepEqual(got, scope) {
			t.Fatalf("unproven scope was rebound: %v", err)
		}
	}
	// Repeat the exact migration body as well as the versioned runner: merged
	// usage must not be counted twice, reset, or have its window extended again.
	if err := db.Transaction(migrateQuarkAccountBudgets); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	var repeated models.ProviderObservationAccountBudget
	if err := db.First(&repeated, "account_key = ?", canonical).Error; err != nil || !reflect.DeepEqual(repeated, merged) {
		t.Fatalf("migration was not idempotent: %v", err)
	}
}

func TestMigrationQuarkAccountBudgetExpiredWindowRetainsFutureCooldown(t *testing.T) {
	db, err := Open(t.TempDir() + "/expired-budget.db")
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	applyMigrationsThrough(t, db, 120)
	now := time.Now().UTC().Truncate(time.Second)
	account := "quark:" + strings.Repeat("c", 32)
	connection := models.Connection{ID: 1, Name: "expired", NameNormalized: "expired", Provider: "quark", AccountID: account}
	if err := db.Create(&connection).Error; err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("quark:%d:%s", connection.ID, account)))
	old := models.ProviderObservationAccountBudget{AccountKey: hex.EncodeToString(sum[:]), WindowStart: now.Add(-2 * time.Hour), HotRequests: 600, CooldownUntil: now.Add(time.Hour), LeaseUntil: now.Add(-time.Minute), LeaseToken: "expired"}
	if err := db.Create(&old).Error; err != nil {
		t.Fatal(err)
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	var got models.ProviderObservationAccountBudget
	if err := db.First(&got, "account_key = ?", models.QuarkObservationAccountKey(connection.ID, account)).Error; err != nil {
		t.Fatal(err)
	}
	if got.HotRequests != 0 || got.ColdRequests != 0 || got.WindowStart.Before(now) || !got.CooldownUntil.Equal(old.CooldownUntil) || got.LeaseToken != "" {
		t.Fatalf("expired window/cooldown lost: %+v", got)
	}
}
