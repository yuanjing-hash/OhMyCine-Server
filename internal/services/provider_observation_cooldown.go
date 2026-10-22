package services

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
	"gorm.io/gorm"
)

func quarkAccountCooldownKeys(ctx context.Context, db *gorm.DB, connectionID uint) ([]string, error) {
	var connection models.Connection
	if err := db.WithContext(ctx).Select("id,account_id").First(&connection, connectionID).Error; err != nil {
		return nil, err
	}
	return quarkCooldownKeys(connection.ID, connection.AccountID), nil
}

func quarkCooldownKeys(connectionID uint, accountReference string) []string {
	// Keep a connection-level gate across initial authorization/account changes;
	// the account key is identical to the observation scheduler's namespace.
	return []string{fmt.Sprintf("quark-connection:%d", connectionID), models.QuarkObservationAccountKey(connectionID, accountReference)}
}

// CheckQuarkAccountCooldown is the BeforeCall guard for every Quark route,
// including playback/interactive/auth requests. It does not spend observation
// quota, but cannot bypass a server-supplied Retry-After.
func CheckQuarkAccountCooldown(ctx context.Context, db *gorm.DB, connectionID uint) error {
	keys, err := quarkAccountCooldownKeys(ctx, db, connectionID)
	if err != nil {
		return err
	}
	var rows []models.ProviderObservationAccountBudget
	if err = db.WithContext(ctx).Where("account_key IN ?", keys).Find(&rows).Error; err != nil {
		return err
	}
	until := time.Time{}
	for _, row := range rows {
		if row.CooldownUntil.After(until) {
			until = row.CooldownUntil
		}
	}
	if wait := time.Until(until); wait > 0 {
		return &quark.Failure{Code: quark.CodeLimited, RetryAfter: wait}
	}
	return nil
}

// The provider error contains only allowlisted status and delay. This hook
// persists the cooldown, never provider response text, credentials or URLs.
func RecordQuarkAccountCooldown(db *gorm.DB, connectionID uint, accountReference string, cause error) {
	var limited *quark.Failure
	if !errors.As(cause, &limited) || limited.RetryAfter <= 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// Use the identity captured by the requesting client, not whichever account
	// may have replaced it while its response was in flight.
	keys := quarkCooldownKeys(connectionID, accountReference)
	scheduler := NewProviderObservationService(db)
	for _, key := range keys {
		_ = scheduler.Cooldown(ctx, key, limited.RetryAfter)
	}
}
