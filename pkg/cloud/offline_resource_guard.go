package cloud

import "context"

// ResourceOperationStore extends the same private receipt ledger. A released
// temporary guard retains its receipt; selected exclusive claims are never
// releasable through this interface.
type ResourceOperationStore interface {
	ExclusiveOperationStore
	ResourceClaim(context.Context, string) (OperationReceipt, error)
	ReleaseResourceClaim(context.Context, string, uint64, OperationReceipt) error
	ReclaimResourceClaim(context.Context, string, uint64, OperationReceipt) error
}

const OfflineResourceGuardKind = "offline_resource_guard_v1"

type offlineOperationScopeKey struct{}

// WithOfflineOperationScope carries backend task authority, never wire data.
func WithOfflineOperationScope(ctx context.Context, scope string) context.Context {
	return context.WithValue(ctx, offlineOperationScopeKey{}, scope)
}

func OfflineOperationScope(ctx context.Context) string {
	scope, _ := ctx.Value(offlineOperationScopeKey{}).(string)
	return scope
}

// OfflineAccess is the private ordinary-execution authority. Remote reserves
// last until the original Node source operation is durably completed.
type OfflineAccess struct {
	OperationID string
	URI         string
	DirectoryID string
	Remote      bool
}

type OfflineAccessGuard interface {
	BeginOfflineAccess(context.Context, OfflineAccess) error
	CompleteOfflineAccess(context.Context, OfflineAccess) error
}

// OfflineFactReader is read-only provider evidence for the ledger coordinator.
// It does not adopt a provider task into an ordinary downloader plan.
type OfflineFactReader interface {
	OfflineFacts(context.Context, string) (OfflineTask, error)
}
