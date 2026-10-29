package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/credential"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var ErrProviderOperationConflict = errors.New("provider operation receipt conflict")

type providerOperationStore struct {
	resourceKey       string
	db                *gorm.DB
	credentials       *credential.Store
	connectionID      uint
	provider, account string
}

// Account identity is frozen into the encrypted scope, while the lookup key
// deliberately remains stable across credential changes. A changed account
// finds a conflict, never false absence followed by another provider mutation.
func NewProviderOperationStore(db *gorm.DB, credentials *credential.Store, connectionID uint, provider, account string) cloud.OperationStore {
	return &providerOperationStore{db: db, credentials: credentials, connectionID: connectionID, provider: provider, account: account}
}
func providerReceiptHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// A first healthy probe fills Connection.AccountID without necessarily
// replacing its cached driver. Resolve only that initially unbound scope from
// the confirmed row; never derive an account identity from a UI label.
func (s *providerOperationStore) scope(ctx context.Context) (*providerOperationStore, error) {
	if s.account != "" {
		return s, nil
	}
	if s.db == nil || s.credentials == nil || s.connectionID == 0 {
		return nil, ErrProviderOperationConflict
	}
	var connection models.Connection
	if err := s.db.WithContext(ctx).First(&connection, "id=? AND provider=?", s.connectionID, s.provider).Error; err != nil {
		return nil, err
	}
	if !receiptScalar(connection.AccountID, 512) {
		return nil, ErrProviderOperationConflict
	}
	scoped := *s
	scoped.account = connection.AccountID
	return &scoped, nil
}

func (s *providerOperationStore) key(id string) string {
	return providerReceiptHash(fmt.Sprintf("provider-operation-v1\x00%s\x00%d\x00%s", s.provider, s.connectionID, id))
}
func (s *providerOperationStore) purpose(key string) string {
	return fmt.Sprintf("provider-operation-v1:%s:%d:%s:%s", s.provider, s.connectionID, providerReceiptHash(s.account), key)
}
func receiptScalar(value string, maximum int) bool {
	return value != "" && len(value) <= maximum && !strings.ContainsAny(value, "\x00\r\n")
}
func (s *providerOperationStore) valid(r cloud.OperationReceipt) bool {
	if s.db == nil || s.credentials == nil || s.connectionID == 0 || !receiptScalar(s.provider, 64) || !receiptScalar(s.account, 512) || r.Provider != s.provider || r.AccountID != s.account || !receiptScalar(r.ID, 512) || !receiptScalar(r.Route, 80) || !receiptScalar(r.Kind, 80) || r.Revision == 0 || r.Revision > math.MaxInt64 || len(r.Payload) > 1<<20 || !json.Valid(r.Payload) {
		return false
	}
	switch r.State {
	case "prepared", "submitting", "unknown", "confirmed", "failed":
		return true
	}
	return false
}
func (s *providerOperationStore) encode(r cloud.OperationReceipt) ([]byte, error) {
	if !s.valid(r) {
		return nil, ErrProviderOperationConflict
	}
	raw, err := json.Marshal(struct {
		ResourceKey string                 `json:"resource_key"`
		Receipt     cloud.OperationReceipt `json:"receipt"`
	}{s.resourceKey, r})
	if err != nil {
		return nil, err
	}
	sealed, err := s.credentials.Encrypt(s.purpose(s.key(r.ID)), string(raw))
	return []byte(sealed), err
}
func (s *providerOperationStore) decode(row models.ProviderOperationReceipt) (cloud.OperationReceipt, error) {
	var r cloud.OperationReceipt
	if row.ConnectionID != s.connectionID || row.Provider != s.provider || row.AccountKey != providerReceiptHash(s.account) {
		return r, ErrProviderOperationConflict
	}
	raw, err := s.credentials.Decrypt(s.purpose(row.ID), string(row.CheckpointCiphertext))
	if err != nil {
		return r, err
	}
	var envelope struct {
		ResourceKey string                 `json:"resource_key"`
		Receipt     cloud.OperationReceipt `json:"receipt"`
	}
	if json.Unmarshal([]byte(raw), &envelope) != nil || envelope.ResourceKey != row.ResourceKey {
		return r, ErrProviderOperationConflict
	}
	r = envelope.Receipt
	if !s.valid(r) || s.key(r.ID) != row.ID || r.Revision != row.Revision || r.Route != row.Route || r.Kind != row.Kind || r.State != row.State {
		return cloud.OperationReceipt{}, ErrProviderOperationConflict
	}
	return r, nil
}
func (s *providerOperationStore) Create(ctx context.Context, r cloud.OperationReceipt) error {
	scoped, err := s.scope(ctx)
	if err != nil {
		return err
	}
	if scoped != s {
		return scoped.Create(ctx, r)
	}

	if r.State != "prepared" || r.Revision != 1 {
		return ErrProviderOperationConflict
	}
	sealed, err := s.encode(r)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	row := models.ProviderOperationReceipt{ResourceKey: s.resourceKey, ID: s.key(r.ID), ConnectionID: s.connectionID, Provider: s.provider, AccountKey: providerReceiptHash(s.account), Route: r.Route, Kind: r.Kind, State: r.State, Revision: r.Revision, CheckpointCiphertext: sealed, CreatedAt: now, UpdatedAt: now}
	result := s.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&row)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrProviderOperationConflict
	}
	return nil
}
func (s *providerOperationStore) Load(ctx context.Context, id string) (cloud.OperationReceipt, error) {
	scoped, err := s.scope(ctx)
	if err != nil {
		return cloud.OperationReceipt{}, err
	}
	if scoped != s {
		return scoped.Load(ctx, id)
	}

	if s.db == nil || s.credentials == nil || !receiptScalar(id, 512) {
		return cloud.OperationReceipt{}, ErrProviderOperationConflict
	}
	var row models.ProviderOperationReceipt
	err = s.db.WithContext(ctx).First(&row, "id = ? AND connection_id = ? AND provider = ?", s.key(id), s.connectionID, s.provider).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return cloud.OperationReceipt{}, cloud.ErrOperationNotFound
	}
	if err != nil {
		return cloud.OperationReceipt{}, err
	}
	return s.decode(row)
}
func (s *providerOperationStore) Update(ctx context.Context, revision uint64, r cloud.OperationReceipt) error {
	scoped, err := s.scope(ctx)
	if err != nil {
		return err
	}
	if scoped != s {
		return scoped.Update(ctx, revision, r)
	}

	if revision == 0 || revision >= math.MaxInt64 || r.Revision != revision+1 {
		return ErrProviderOperationConflict
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		scoped := *s
		scoped.db = tx
		old, err := scoped.Load(ctx, r.ID)
		if err != nil {
			return err
		}
		if old.Revision != revision || old.Route != r.Route || old.Kind != r.Kind || old.AccountID != r.AccountID || old.Provider != r.Provider {
			return ErrProviderOperationConflict
		}
		allowed := old.State == r.State || old.State == "prepared" && (r.State == "submitting" || r.State == "confirmed" || r.State == "failed") || old.State == "submitting" && (r.State == "unknown" || r.State == "confirmed" || r.State == "failed") || old.State == "unknown" && (r.State == "confirmed" || r.State == "failed")
		if !allowed || old.State == "confirmed" && string(old.Payload) != string(r.Payload) || old.State == "failed" && string(old.Payload) != string(r.Payload) {
			return ErrProviderOperationConflict
		}
		var currentRow models.ProviderOperationReceipt
		if err := tx.First(&currentRow, "id=?", s.key(r.ID)).Error; err != nil {
			return err
		}
		encoder := *s
		encoder.resourceKey = currentRow.ResourceKey
		sealed, err := encoder.encode(r)
		if err != nil {
			return err
		}
		result := tx.Model(&models.ProviderOperationReceipt{}).Where("id=? AND connection_id=? AND provider=? AND account_key=? AND revision=?", s.key(r.ID), s.connectionID, s.provider, providerReceiptHash(s.account), revision).Updates(map[string]any{"state": r.State, "revision": r.Revision, "checkpoint_ciphertext": sealed, "updated_at": time.Now().UTC()})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrProviderOperationConflict
		}
		return nil
	})
}

func (s *providerOperationStore) Claim(ctx context.Context, resourceKey string, r cloud.OperationReceipt) error {
	if len(resourceKey) != 64 {
		return ErrProviderOperationConflict
	}
	if _, err := hex.DecodeString(resourceKey); err != nil {
		return ErrProviderOperationConflict
	}
	scoped, err := s.scope(ctx)
	if err != nil {
		return err
	}
	claim := *scoped
	claim.resourceKey = resourceKey
	return claim.Create(ctx, r)
}

func (s *providerOperationStore) ResourceClaim(ctx context.Context, resourceKey string) (cloud.OperationReceipt, error) {
	if len(resourceKey) != 64 {
		return cloud.OperationReceipt{}, ErrProviderOperationConflict
	}
	if _, err := hex.DecodeString(resourceKey); err != nil {
		return cloud.OperationReceipt{}, ErrProviderOperationConflict
	}
	scoped, err := s.scope(ctx)
	if err != nil {
		return cloud.OperationReceipt{}, err
	}
	var row models.ProviderOperationReceipt
	err = scoped.db.WithContext(ctx).First(&row, "provider=? AND account_key=? AND resource_key=?", scoped.provider, providerReceiptHash(scoped.account), resourceKey).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return cloud.OperationReceipt{}, cloud.ErrOperationNotFound
	}
	if err != nil {
		return cloud.OperationReceipt{}, err
	}
	// The owner may be another Connection alias for the same confirmed account.
	// Decode with its original purpose, without changing its operation identity.
	owner := *scoped
	owner.connectionID = row.ConnectionID
	return owner.decode(row)
}

func (s *providerOperationStore) ReleaseResourceClaim(ctx context.Context, resourceKey string, revision uint64, r cloud.OperationReceipt) error {
	scoped, err := s.scope(ctx)
	if err != nil {
		return err
	}
	if scoped != s {
		return scoped.ReleaseResourceClaim(ctx, resourceKey, revision, r)
	}
	if revision == 0 || revision >= math.MaxInt64 || r.Revision != revision+1 || r.Kind != cloud.OfflineResourceGuardKind || (r.State != "confirmed" && r.State != "failed") || len(resourceKey) != 64 {
		return ErrProviderOperationConflict
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		local := *s
		local.db = tx
		old, err := local.Load(ctx, r.ID)
		if err != nil {
			return err
		}
		if old.Revision != revision || old.Kind != r.Kind || old.Route != r.Route || old.AccountID != r.AccountID || old.Provider != r.Provider || old.State == "confirmed" || old.State == "failed" {
			return ErrProviderOperationConflict
		}
		if r.State == "failed" && old.State != "prepared" {
			var rejection struct{ DefiniteRejection bool }
			if old.State != "submitting" || json.Unmarshal(r.Payload, &rejection) != nil || !rejection.DefiniteRejection {
				return ErrProviderOperationConflict
			}
		}
		encoder := *s
		encoder.resourceKey = ""
		sealed, err := encoder.encode(r)
		if err != nil {
			return err
		}
		result := tx.Model(&models.ProviderOperationReceipt{}).Where("id=? AND connection_id=? AND provider=? AND account_key=? AND resource_key=? AND revision=?", s.key(r.ID), s.connectionID, s.provider, providerReceiptHash(s.account), resourceKey, revision).Updates(map[string]any{"resource_key": "", "state": r.State, "revision": r.Revision, "checkpoint_ciphertext": sealed, "updated_at": time.Now().UTC()})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrProviderOperationConflict
		}
		return nil
	})
}

func (s *providerOperationStore) ReclaimResourceClaim(ctx context.Context, resourceKey string, revision uint64, r cloud.OperationReceipt) error {
	scoped, err := s.scope(ctx)
	if err != nil {
		return err
	}
	if scoped != s {
		return scoped.ReclaimResourceClaim(ctx, resourceKey, revision, r)
	}
	if revision == 0 || revision >= math.MaxInt64 || r.Revision != revision+1 || r.Kind != cloud.OfflineResourceGuardKind || r.State != "prepared" || len(resourceKey) != 64 {
		return ErrProviderOperationConflict
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		local := *s
		local.db = tx
		old, err := local.Load(ctx, r.ID)
		if err != nil {
			return err
		}
		if old.Revision != revision || old.State != "failed" || old.Kind != r.Kind || old.Route != r.Route || old.AccountID != r.AccountID || old.Provider != r.Provider || string(old.Payload) != string(r.Payload) {
			return ErrProviderOperationConflict
		}
		encoder := *s
		encoder.resourceKey = resourceKey
		sealed, err := encoder.encode(r)
		if err != nil {
			return err
		}
		result := tx.Model(&models.ProviderOperationReceipt{}).Where("id=? AND connection_id=? AND provider=? AND account_key=? AND resource_key='' AND revision=? AND state='failed'", s.key(r.ID), s.connectionID, s.provider, providerReceiptHash(s.account), revision).Updates(map[string]any{"resource_key": resourceKey, "state": r.State, "revision": r.Revision, "checkpoint_ciphertext": sealed, "updated_at": time.Now().UTC()})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrProviderOperationConflict
		}
		return nil
	})
}

var _ cloud.ExclusiveOperationStore = (*providerOperationStore)(nil)
var _ cloud.ResourceOperationStore = (*providerOperationStore)(nil)
