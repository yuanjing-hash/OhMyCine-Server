package cloud

import (
	"context"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/resourcecontent"
)

// SelectiveOfflineAuthority is private frozen intent, never a browser payload.
// Provider directory ids are opaque account-space identities, not FS paths.
type SelectiveOfflineAuthority struct {
	Tag, StorageRootID string
	Selection          resourcecontent.Selection
}
type SelectiveOfflineRequest struct {
	Authority SelectiveOfflineAuthority
	ParentID  string
	Torrent   []byte
}
type SelectiveOfflineFile struct {
	Index int
	Path  string
	Item  Item
}
type SelectiveOfflineSnapshot struct {
	Task     OfflineTask
	Name     string
	Files    []SelectiveOfflineFile
	Complete bool
}

// SelectiveOfflineDriver is optional; it creates a native exact wanted set,
// not a whole download followed by output filtering. Missing durable receipts
// disables this capability. Uncertain mutations reconcile the same operation.
type SelectiveOfflineDriver interface {
	NativeOfflineDriver
	SupportsSelectiveOffline() bool
	SubmitSelectedOffline(context.Context, SelectiveOfflineRequest) (SelectiveOfflineSnapshot, error)
	ObserveSelectedOffline(context.Context, string, SelectiveOfflineAuthority) (SelectiveOfflineSnapshot, error)
	CancelSelectedOffline(context.Context, string, SelectiveOfflineAuthority, bool) error
}

// ExclusiveOperationStore adds an account-wide resource claim. Native
// single-plan executors use it when the provider cannot atomically extend or
// share wanted sets. A conflict is not evidence of absence or permission to
// adopt another plan. Normal operation stores remain source-compatible.
type ExclusiveOperationStore interface {
	OperationStore
	Claim(context.Context, string, OperationReceipt) error
}
