package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// ErrOperationNotFound is positive absence, distinct from an unreadable store.
// Consumers must never infer absence from an arbitrary storage error.
var ErrOperationNotFound = errors.New("cloud_operation_not_found")

// OperationReceipt is PRIVATE persisted state, not a job DTO. Payload can
// contain provider handles and temporary upload/share credentials and must be
// encrypted by its owner. A receipt ID is chosen and persisted before I/O.
type OperationReceipt struct {
	ID        string          `json:"id"`
	Provider  string          `json:"provider"`
	Route     string          `json:"route"`
	AccountID string          `json:"account_id"`
	Kind      string          `json:"kind"`
	Revision  uint64          `json:"revision"`
	State     string          `json:"state"`
	Payload   json.RawMessage `json:"payload"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// OperationStore.Create must reject duplicate IDs atomically. Update is CAS;
// callbacks must not start cloud calls. Receipt storage is required for writes.
type OperationStore interface {
	Create(context.Context, OperationReceipt) error
	Load(context.Context, string) (OperationReceipt, error)
	Update(context.Context, uint64, OperationReceipt) error
}

type OperationRequest struct {
	ID       string
	Kind     string
	ItemID   string
	ParentID string
	Name     string
	Share    *ShareSnapshot
}

type OperationResult struct {
	Receipt  OperationReceipt
	Items    []Item
	Complete bool
}

// AsyncMutationDriver adds durable asynchronous writes without changing the
// legacy synchronous115 interface. Repeating Begin with an existing ID only
// queries that saved operation; it never issues another mutation.
type AsyncMutationDriver interface {
	Driver
	BeginOperation(context.Context, OperationRequest) (OperationResult, error)
	QueryOperation(context.Context, string) (OperationResult, error)
}
