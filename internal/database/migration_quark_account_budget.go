package database

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

// Run before supervisors start, under the ordinary exclusive runtime/migration
// transaction. Only a current, verified TV reference can relate legacy keys.
// Scope IDs, batches, facts, identities and source fences are not recreated.
func migrateQuarkAccountBudgets(db *gorm.DB) error {
	var connections []models.Connection
	if err := db.Select("id", "account_id").Where("provider = ?", models.ConnectionProviderQuark).Find(&connections).Error; err != nil {
		return err
	}
	type alias struct {
		connection uint
		key        string
	}
	groups := map[string][]alias{}
	for _, connection := range connections {
		sum := sha256.Sum256([]byte(fmt.Sprintf("quark:%d:%s", connection.ID, connection.AccountID)))
		old := hex.EncodeToString(sum[:])
		current := models.QuarkObservationAccountKey(connection.ID, connection.AccountID)
		if old != current {
			groups[current] = append(groups[current], alias{connection.ID, old})
		}
	}
	now := time.Now().UTC()
	for current, aliases := range groups {
		keys := make([]string, 0, len(aliases))
		for _, item := range aliases {
			keys = append(keys, item.key)
		}
		var legacy []models.ProviderObservationAccountBudget
		if err := db.Where("account_key IN ?", keys).Find(&legacy).Error; err != nil {
			return err
		}
		if len(legacy) > 0 {
			var rows []models.ProviderObservationAccountBudget
			if err := db.Where("account_key IN ?", append(keys, current)).Find(&rows).Error; err != nil {
				return err
			}
			merged := models.ProviderObservationAccountBudget{AccountKey: current, UpdatedAt: now}
			lastUpdated := time.Time{}
			for _, row := range rows {
				// Distinct legacy windows are conservatively kept until the latest
				// active window expires. Never replenish quota during migration.
				if now.Sub(row.WindowStart) < time.Hour {
					merged.HotRequests += row.HotRequests
					merged.ColdRequests += row.ColdRequests
					if row.WindowStart.After(merged.WindowStart) {
						merged.WindowStart = row.WindowStart
					}
				}
				if row.CooldownUntil.After(merged.CooldownUntil) {
					merged.CooldownUntil = row.CooldownUntil
				}
				if row.NextRequestAt.After(merged.NextRequestAt) {
					merged.NextRequestAt = row.NextRequestAt
				}
				if row.LeaseUntil.After(merged.LeaseUntil) {
					merged.LeaseUntil = row.LeaseUntil
				}
				if row.UpdatedAt.After(lastUpdated) {
					lastUpdated, merged.LastScopeID = row.UpdatedAt, row.LastScopeID
				}
			}
			if merged.WindowStart.IsZero() {
				merged.WindowStart = now
			}
			if merged.LeaseUntil.After(now) {
				// Old runtime tokens must not release the merged admission row.
				merged.LeaseToken = "quark-budget-migration-121"
			}
			if err := db.Save(&merged).Error; err != nil {
				return err
			}
			if err := db.Where("account_key IN ?", keys).Delete(&models.ProviderObservationAccountBudget{}).Error; err != nil {
				return err
			}
		}
		for _, item := range aliases {
			if err := db.Model(&models.ProviderObservationScope{}).Where("provider = ? AND connection_id = ? AND account_key = ?", models.ConnectionProviderQuark, item.connection, item.key).Update("account_key", current).Error; err != nil {
				return err
			}
		}
	}
	return nil
}
