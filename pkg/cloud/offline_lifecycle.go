package cloud

import (
	"context"
	"time"
)

// A recorder runs after provider/Node admission and before the original native
// submit. Failure prevents the write. Recovery never calls it to renew a clock.
type offlineDispatchRecorderKey struct{}

func WithOfflineDispatchRecorder(ctx context.Context, recorder func(time.Time) error) context.Context {
	return context.WithValue(ctx, offlineDispatchRecorderKey{}, recorder)
}
func RecordOfflineDispatch(ctx context.Context, at time.Time) error {
	if recorder, ok := ctx.Value(offlineDispatchRecorderKey{}).(func(time.Time) error); ok && recorder != nil {
		return recorder(at.UTC())
	}
	return nil
}

type OfflineFinalization string

const (
	OfflineFinalizationUnknown  OfflineFinalization = "unknown"
	OfflineFinalizationPending  OfflineFinalization = "pending"
	OfflineFinalizationComplete OfflineFinalization = "complete"
)

type FinalizedFile struct {
	Item  Item
	State OfflineFinalization
}

// FinalizedFileReader supplies an explicit upstream finalized-object assertion
// for exact previously rooted leaves. Metadata existence/size/hash is not proof.
type FinalizedFileReader interface {
	FinalizedFiles(context.Context, []Item) ([]FinalizedFile, error)
}

// This marker is evidence, never an inference from an auth/quota/error code.
// Providers add it only after persisting an explicit original no-task rejection.
type definiteSubmissionNonAcceptance struct{ error }

func (e *definiteSubmissionNonAcceptance) SubmissionNotAccepted() bool { return true }
func (e *definiteSubmissionNonAcceptance) Unwrap() error               { return e.error }
func WithDefiniteSubmissionNonAcceptance(err error) error {
	if err == nil {
		return nil
	}
	return &definiteSubmissionNonAcceptance{err}
}

type offlineNonAcceptanceRecorderKey struct{}

func WithOfflineNonAcceptanceRecorder(ctx context.Context, recorder func() error) context.Context {
	return context.WithValue(ctx, offlineNonAcceptanceRecorderKey{}, recorder)
}
func RecordOfflineNonAcceptance(ctx context.Context) (bool, error) {
	if recorder, ok := ctx.Value(offlineNonAcceptanceRecorderKey{}).(func() error); ok && recorder != nil {
		if err := recorder(); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}
