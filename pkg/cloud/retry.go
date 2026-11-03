package cloud

import (
	"errors"
	"time"
)

// RetryReasonProviderCooldown describes an observed local provider gate. It is
// not a promise that the upstream account will recover at this time.
const RetryReasonProviderCooldown = "provider_cooldown"

type retryNotBeforeError struct {
	error
	until  time.Time
	reason string
}

func (e *retryNotBeforeError) Unwrap() error                       { return e.error }
func (e *retryNotBeforeError) RetryNotBefore() (time.Time, string) { return e.until, e.reason }

// WithRetryNotBefore preserves error identity and adds only allowlisted timing.
// Callers must supply an actual gate deadline, never a guessed retry interval.
func WithRetryNotBefore(err error, until time.Time, reason string) error {
	if err == nil || until.IsZero() || reason != RetryReasonProviderCooldown {
		return err
	}
	return &retryNotBeforeError{error: err, until: until.UTC(), reason: reason}
}

// RetryTiming is optional across providers and across client-safe domain errors.
func RetryTiming(err error) (time.Time, string, bool) {
	var timed interface{ RetryNotBefore() (time.Time, string) }
	if !errors.As(err, &timed) {
		return time.Time{}, "", false
	}
	until, reason := timed.RetryNotBefore()
	if until.IsZero() || reason != RetryReasonProviderCooldown {
		return time.Time{}, "", false
	}
	return until.UTC(), reason, true
}
