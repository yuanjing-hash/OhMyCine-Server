package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/credential"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var ErrQuarkOperationConflict = errors.New("quark operation receipt conflict")

type quarkOperationStore struct {
	db           *gorm.DB
	credentials  *credential.Store
	connectionID uint
}
type observationOperationScopeKey struct{}
type observationOperationScope struct{ id, directoryID, fence string }

// WithObservationOperationScope binds writes to the directory whose missing
// detection must wait. An unscoped connection write conservatively blocks
// missing inference in all its scopes until its final result is confirmed.
func WithObservationOperationScope(ctx context.Context, scopeID, directoryID, fence string) context.Context {
	return context.WithValue(ctx, observationOperationScopeKey{}, observationOperationScope{scopeID, directoryID, fence})
}

func NewQuarkOperationStore(db *gorm.DB, credentials *credential.Store, connectionID uint) cloud.OperationStore {
	return &quarkOperationStore{db: db, credentials: credentials, connectionID: connectionID}
}

func (s *quarkOperationStore) key(id string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("quark-operation-v1\x00%d\x00%s", s.connectionID, id)))
	return hex.EncodeToString(sum[:])
}
func (s *quarkOperationStore) purpose(key string) string {
	return fmt.Sprintf("quark-operation-v1:%d:%s", s.connectionID, key)
}

func validQuarkReceipt(r cloud.OperationReceipt) bool {
	if r.ID == "" || len(r.ID) > 512 || r.Provider != "quark" || r.AccountID == "" || len(r.AccountID) > 512 || r.Kind == "" || len(r.Kind) > 80 || len(r.Payload) > 1<<20 || !json.Valid(r.Payload) || strings.ContainsAny(r.ID+r.AccountID+r.Kind, "\x00\r\n") {
		return false
	}
	if r.Route != "tv" && r.Route != "agent" && r.Route != "web" {
		return false
	}
	switch r.State {
	case "prepared", "submitting", "accepted", "reconciling", "confirmed", "rejected", "uncertain", "unknown", "failed", "catalog_published", "artifacts_ready":
		return true
	}
	return false
}

func (s *quarkOperationStore) encode(r cloud.OperationReceipt) ([]byte, error) {
	if s.db == nil || s.credentials == nil || s.connectionID == 0 || !validQuarkReceipt(r) {
		return nil, ErrObservationInvalid
	}
	data, err := json.Marshal(r)
	if err != nil {
		return nil, ErrObservationInvalid
	}
	sealed, err := s.credentials.Encrypt(s.purpose(s.key(r.ID)), string(data))
	if err != nil {
		return nil, err
	}
	return []byte(sealed), nil
}
func (s *quarkOperationStore) decode(row models.QuarkOperationReceipt) (cloud.OperationReceipt, error) {
	var receipt cloud.OperationReceipt
	if row.ConnectionID != s.connectionID {
		return receipt, ErrQuarkOperationConflict
	}
	plain, err := s.credentials.Decrypt(s.purpose(row.ID), string(row.CheckpointCiphertext))
	if err != nil {
		return receipt, err
	}
	if json.Unmarshal([]byte(plain), &receipt) != nil || !validQuarkReceipt(receipt) || s.key(receipt.ID) != row.ID || receipt.Revision != row.Revision || receipt.Route != row.Adapter || receipt.State != row.State {
		return cloud.OperationReceipt{}, ErrObservationInvalid
	}
	return receipt, nil
}

func (s *quarkOperationStore) Create(ctx context.Context, r cloud.OperationReceipt) error {
	if r.State != "prepared" || r.Revision > 1 {
		return ErrObservationInvalid
	}
	sealed, err := s.encode(r)
	if err != nil {
		return err
	}
	key := s.key(r.ID)
	now := time.Now().UTC()
	row := models.QuarkOperationReceipt{ID: key, ConnectionID: s.connectionID, OperationKey: key, Adapter: r.Route, Kind: r.Kind, State: r.State, Revision: r.Revision, CheckpointCiphertext: sealed, CreatedAt: now, UpdatedAt: now}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if scope, ok := ctx.Value(observationOperationScopeKey{}).(observationOperationScope); ok {
			var current models.ProviderObservationScope
			if err := tx.First(&current, "id = ? AND enabled = ?", scope.id, true).Error; err != nil {
				return err
			}
			if current.ConnectionID != s.connectionID || current.Fence != scope.fence {
				return ErrObservationFence
			}
			row.ScopeID = scope.id
			row.Fence = scope.fence
			row.DirectoryID = scope.directoryID
		}
		result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrQuarkOperationConflict
		}
		return nil
	})
}

func (s *quarkOperationStore) Load(ctx context.Context, id string) (cloud.OperationReceipt, error) {
	var row models.QuarkOperationReceipt
	if s.db == nil || s.credentials == nil || id == "" {
		return cloud.OperationReceipt{}, ErrObservationInvalid
	}
	if err := s.db.WithContext(ctx).First(&row, "id = ? AND connection_id = ?", s.key(id), s.connectionID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return cloud.OperationReceipt{}, cloud.ErrOperationNotFound
		}
		return cloud.OperationReceipt{}, err
	}
	return s.decode(row)
}

func quarkReceiptTransition(from, to string) bool {
	if from == to {
		return true
	}
	switch from {
	case "prepared":
		return to == "submitting" || to == "accepted" || to == "uncertain" || to == "unknown" || to == "rejected"
	case "submitting", "uncertain", "unknown":
		return to == "accepted" || to == "reconciling" || to == "confirmed" || to == "uncertain" || to == "unknown" || to == "rejected" || to == "failed"
	case "accepted", "reconciling":
		return to == "reconciling" || to == "confirmed" || to == "uncertain" || to == "unknown" || to == "rejected" || to == "failed"
	case "confirmed":
		return to == "catalog_published"
	case "catalog_published":
		return to == "artifacts_ready"
	}
	return false
}

func (s *quarkOperationStore) Update(ctx context.Context, expected uint64, r cloud.OperationReceipt) error {
	if r.Revision != expected+1 {
		return ErrQuarkOperationConflict
	}
	sealed, err := s.encode(r)
	if err != nil {
		return err
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row models.QuarkOperationReceipt
		if err := tx.First(&row, "id = ? AND connection_id = ?", s.key(r.ID), s.connectionID).Error; err != nil {
			return err
		}
		old, err := s.decode(row)
		if err != nil {
			return err
		}
		if row.Revision != expected || old.ID != r.ID || old.Route != r.Route || old.AccountID != r.AccountID || old.Kind != r.Kind || !quarkReceiptTransition(old.State, r.State) {
			return ErrQuarkOperationConflict
		}
		result := tx.Model(&models.QuarkOperationReceipt{}).Where("id = ? AND revision = ?", row.ID, expected).Updates(map[string]any{"revision": r.Revision, "state": r.State, "checkpoint_ciphertext": sealed, "updated_at": time.Now().UTC()})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrQuarkOperationConflict
		}
		return nil
	})
}
