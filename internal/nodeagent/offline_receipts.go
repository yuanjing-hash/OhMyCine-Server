package nodeagent

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

// Node provider handles are encrypted with a purpose-specific key derived from
// its existing private sealing identity. No clear upload URL/token is stored.
type nodeOfflineReceiptStore struct {
	store                  *Store
	serverID, operationKey string
	aead                   cipher.AEAD
}

func (a *Agent) offlineReceipts(serverID, operationKey string) (*nodeOfflineReceiptStore, error) {
	if a.sealingPrivateKey == nil || serverID == "" || operationKey == "" {
		return nil, errors.New("node_private_receipt_identity_missing")
	}
	key := sha256.Sum256(append([]byte("ohmycine-node-cloud-offline-lifecycle-v1\x00"), a.sealingPrivateKey.Bytes()...))
	block, e := aes.NewCipher(key[:])
	if e != nil {
		return nil, e
	}
	g, e := cipher.NewGCM(block)
	if e != nil {
		return nil, e
	}
	return &nodeOfflineReceiptStore{a.store, serverID, operationKey, g}, nil
}
func offlineReceiptBinding(r cloud.OperationReceipt) string {
	b, _ := json.Marshal([]string{r.Provider, r.Route, r.AccountID, r.Kind})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func (s *nodeOfflineReceiptStore) aad(id, binding string, revision uint64) []byte {
	return []byte(fmt.Sprintf("node-cloud-offline-v1\x00%s\x00%s\x00%s\x00%s\x00%d", s.serverID, s.operationKey, id, binding, revision))
}
func validOfflineReceipt(r cloud.OperationReceipt) bool {
	return r.ID != "" && len(r.ID) <= 128 && !strings.ContainsAny(r.ID, "\x00\r\n") && r.Provider == cloud.ProviderPan115 && r.Route == "cookie" && r.AccountID != "" && r.Kind != "" && r.Revision > 0 && len(r.Payload) <= 1<<20 && json.Valid(r.Payload)
}
func (s *nodeOfflineReceiptStore) seal(r cloud.OperationReceipt) ([]byte, error) {
	if !validOfflineReceipt(r) {
		return nil, ErrPlanConflict
	}
	raw, e := json.Marshal(r)
	if e != nil {
		return nil, e
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, e = rand.Read(nonce); e != nil {
		return nil, e
	}
	return s.aead.Seal(nonce, nonce, raw, s.aad(r.ID, offlineReceiptBinding(r), r.Revision)), nil
}
func (s *nodeOfflineReceiptStore) Create(ctx context.Context, r cloud.OperationReceipt) error {
	if r.Revision != 1 || r.State != "prepared" {
		return ErrPlanConflict
	}
	sealed, e := s.seal(r)
	if e != nil {
		return e
	}
	_, e = s.store.db.ExecContext(ctx, `INSERT INTO cloud_operation_receipts(paired_server_id,operation_key,receipt_id,revision,binding,ciphertext) VALUES(?,?,?,?,?,?)`, s.serverID, s.operationKey, r.ID, r.Revision, offlineReceiptBinding(r), sealed)
	return e
}
func (s *nodeOfflineReceiptStore) Load(ctx context.Context, id string) (cloud.OperationReceipt, error) {
	var ciphertext []byte
	var revision uint64
	var binding string
	e := s.store.db.QueryRowContext(ctx, `SELECT revision,binding,ciphertext FROM cloud_operation_receipts WHERE paired_server_id=? AND operation_key=? AND receipt_id=?`, s.serverID, s.operationKey, id).Scan(&revision, &binding, &ciphertext)
	if errors.Is(e, sql.ErrNoRows) {
		return cloud.OperationReceipt{}, cloud.ErrOperationNotFound
	}
	if e != nil {
		return cloud.OperationReceipt{}, e
	}
	if len(ciphertext) < s.aead.NonceSize() {
		return cloud.OperationReceipt{}, errors.New("node_private_receipt_invalid")
	}
	n := s.aead.NonceSize()
	raw, e := s.aead.Open(nil, ciphertext[:n], ciphertext[n:], s.aad(id, binding, revision))
	if e != nil {
		return cloud.OperationReceipt{}, errors.New("node_private_receipt_invalid")
	}
	var r cloud.OperationReceipt
	if json.Unmarshal(raw, &r) != nil || !validOfflineReceipt(r) || r.ID != id || r.Revision != revision || offlineReceiptBinding(r) != binding {
		return cloud.OperationReceipt{}, ErrPlanConflict
	}
	return r, nil
}
func (s *nodeOfflineReceiptStore) Update(ctx context.Context, expected uint64, r cloud.OperationReceipt) error {
	if r.Revision != expected+1 {
		return ErrPlanConflict
	}
	sealed, e := s.seal(r)
	if e != nil {
		return e
	}
	result, e := s.store.db.ExecContext(ctx, `UPDATE cloud_operation_receipts SET revision=?,ciphertext=? WHERE paired_server_id=? AND operation_key=? AND receipt_id=? AND revision=? AND binding=?`, r.Revision, sealed, s.serverID, s.operationKey, r.ID, expected, offlineReceiptBinding(r))
	if e != nil {
		return e
	}
	n, e := result.RowsAffected()
	if e != nil {
		return e
	}
	if n != 1 {
		return ErrPlanConflict
	}
	return nil
}
