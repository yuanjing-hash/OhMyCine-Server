package quark

import (
	"errors"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

// OperationError identifies a fixed phase of a native operation.
// The cause is for typed classification only: Error never renders the cause,
// which can include a private database or transport error.
type OperationError struct {
	Stage string
	Cause error
}

func (e *OperationError) Error() string { return "quark_operation_failed" }
func (e *OperationError) Unwrap() error { return e.Cause }

func operationError(stage string, err error) error {
	if err == nil {
		return nil
	}
	var nested *OperationError
	if errors.As(err, &nested) {
		return err
	}
	return &OperationError{Stage: stage, Cause: err}
}

// Preserve safe provider numbers when a write has an uncertain result. The
// operation remains unknown: these diagnostics do not authorize resubmission.
func unknownUploadOutcome(err error) error {
	f := &Failure{Code: cloud.CodeMutationUnknown, UnknownOutcome: true}
	var cause *Failure
	if errors.As(err, &cause) {
		f.HTTPStatus, f.Errno = cause.HTTPStatus, cause.Errno
	}
	return f
}
