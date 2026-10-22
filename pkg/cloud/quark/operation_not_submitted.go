package quark

// OperationNotSubmittedError is positive evidence from the native preflight:
// no receipt was created and no mutation was dispatched for this invocation.
// A timeout or an error after dispatch must never use this type.
type OperationNotSubmittedError struct{ Err error }

func (e *OperationNotSubmittedError) Error() string { return "quark_operation_not_submitted" }
func (e *OperationNotSubmittedError) Unwrap() error { return e.Err }
