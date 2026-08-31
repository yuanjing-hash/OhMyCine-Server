package updater

import (
	"errors"
	"fmt"
)

const (
	CodeInvalidChannel      = "update_invalid_channel"
	CodeUnsupportedPlatform = "update_unsupported_platform"
	CodeInvalidRelease      = "update_invalid_release"
	CodeNoRelease           = "update_no_release"
	CodeNetwork             = "update_network_error"
	CodeUntrustedSource     = "update_untrusted_source"
	CodeResponseTooLarge    = "update_response_too_large"
	CodeChecksumInvalid     = "update_checksum_invalid"
	CodeChecksumMismatch    = "update_checksum_mismatch"
	CodeArchiveInvalid      = "update_archive_invalid"
	CodeCandidateTooLarge   = "update_candidate_too_large"
	CodePersistence         = "update_persistence_error"
	CodeRevisionConflict    = "update_revision_conflict"
	CodePlanInvalid         = "update_plan_invalid"
	CodeParentExitTimeout   = "update_parent_exit_timeout"
	CodeReplacementFailed   = "update_replacement_failed"
	CodeRestartFailed       = "update_restart_failed"
	CodeHealthCheckFailed   = "update_health_check_failed"
	CodeRollbackFailed      = "update_rollback_failed"
)

// Error exposes a stable, non-sensitive code while retaining an internal cause.
type Error struct {
	Code string
	Err  error
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Err == nil {
		return e.Code
	}
	return fmt.Sprintf("%s: %v", e.Code, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

func coded(code string, err error) error {
	if err == nil {
		err = errors.New("operation failed")
	}
	return &Error{Code: code, Err: err}
}

func ErrorCode(err error) string {
	var updateError *Error
	if errors.As(err, &updateError) {
		return updateError.Code
	}
	return "update_internal_error"
}
