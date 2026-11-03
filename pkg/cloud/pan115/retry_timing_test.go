package pan115

import (
	"context"
	"errors"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"testing"
	"time"
)

func TestCircuitRefusalCarriesActualDeadlineWithoutSDKCall(t *testing.T) {
	now := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	c := newMutationTestClient(nil)
	c.now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		c.recordOutcome(errors.New("HTTP 429"))
	}
	calls := 0
	err := c.waitAndCall(context.Background(), c.listRate, func() error { calls++; return nil })
	var timing interface{ RetryNotBefore() (time.Time, string) }
	if !errors.As(err, &timing) {
		t.Fatalf("real gate deadline lost: %v", err)
	}
	deadline, reason := timing.RetryNotBefore()
	if !deadline.Equal(now.Add(5*time.Minute)) || reason != "provider_cooldown" || calls != 0 || !errors.Is(err, errCircuitOpen) {
		t.Fatalf("deadline=%s reason=%s calls=%d", deadline, reason, calls)
	}
}

func TestProviderOutcomeCarriesBackoffButDoesNotChangeUnrelatedErrors(t *testing.T) {
	now := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	c := newMutationTestClient(nil)
	c.now = func() time.Time { return now }
	calls := 0
	err := c.waitAndCall(context.Background(), c.listRate, func() error { calls++; return errors.New("HTTP 429") })
	until, reason, ok := cloud.RetryTiming(mapError(err))
	if !ok || !until.Equal(now.Add(2*time.Second)) || reason != cloud.RetryReasonProviderCooldown || calls != 1 {
		t.Fatalf("first outcome timing=%s %s %t calls=%d", until, reason, ok, calls)
	}
	if code, retryable := cloud.ErrorInfo(mapError(err)); code != cloud.CodeRateLimited || !retryable {
		t.Fatal("public error contract changed")
	}
	if _, _, ok := cloud.RetryTiming(c.withRecoveryTiming(cloud.Error(cloud.CodeNotFound, false, nil))); ok {
		t.Fatal("unrelated error got cooldown")
	}
	c.recordOutcome(nil)
	if !c.backoffTil.Equal(until) {
		t.Fatal("late success erased gate")
	}
}
