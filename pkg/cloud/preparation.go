package cloud

import "errors"

// preparationFailure identifies a failure before native acquisition dispatch.
// A preparatory directory/seed write can still have its own recovery receipt;
// that receipt does not mean a cloud download exists.
type preparationFailure struct {
	stage string
	cause error
}

func (e *preparationFailure) Error() string            { return "cloud_preparation_failed" }
func (e *preparationFailure) Unwrap() error            { return e.cause }
func (e *preparationFailure) PreparationFailed() bool  { return true }
func (e *preparationFailure) PreparationStage() string { return e.stage }

func WithPreparationFailure(err error, stage string) error {
	if err == nil {
		return nil
	}
	return &preparationFailure{stage: stage, cause: err}
}

func PreparationFailureStage(err error) (string, bool) {
	var failure *preparationFailure
	if !errors.As(err, &failure) {
		return "", false
	}
	return failure.stage, true
}
