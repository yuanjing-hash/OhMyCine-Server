package services

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestArtifactRecoveryDoesNotWaitForBusyScanBoundary(t *testing.T) {
	s, _, _, library, _ := strmManagementFixture(t)
	lock := s.libraries.scanLock(library.ID)
	lock.Lock()
	// No valid execution permit is needed to refuse a busy boundary. Recovery
	// must return before looking up receipts or beginning any observation.
	permit := CatalogPhysicalWritePermit{}
	permit.evidence.LibraryID = library.ID
	done := make(chan error, 1)
	go func() { done <- s.ReconcileSupersededCleanup(context.Background(), permit) }()
	select {
	case err := <-done:
		lock.Unlock()
		if !errors.Is(err, ErrCatalogFence) {
			t.Fatalf("busy boundary error=%v", err)
		}
	case <-time.After(time.Second):
		// Release and join even on regression; do not strand a waiter in tests.
		lock.Unlock()
		<-done
		t.Fatal("recovery waited for the scan lock instead of deferring")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.ReconcileSupersededCleanup(ctx, permit); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled recovery error=%v", err)
	}
}
