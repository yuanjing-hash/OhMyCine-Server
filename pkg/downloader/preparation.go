package downloader

import (
	"context"
	"errors"
)

// PreparationController distinguishes a failed preparation from a dispatched
// native transfer. The caller must quiesce the original submit worker first.
// With deleteData=false it only reads original authority; true may remove only
// that request's owned preparation workspace. Unsupported/entered transfers
// return false so their existing cancellation path remains authoritative.
type PreparationController interface {
	AbandonPreparation(context.Context, SubmitRequest, bool) (bool, error)
}

// PreparationReleaser retires the cancelled original preparation and releases
// only its private resource ownership. Unlike inspection, it must run under
// the original control worker's exclusive lease after submit has quiesced.
type PreparationReleaser interface {
	ReleasePreparation(context.Context, SubmitRequest) error
}

func PreparationFailed(err error) bool {
	var failure interface{ PreparationFailed() bool }
	return errors.As(err, &failure) && failure.PreparationFailed()
}
