package services

import (
	"context"
	"testing"
)

func runCompletedDeletionTransfer(t *testing.T, f cloudTransferFixture) WorkerResult {
	t.Helper()
	if err := f.service.Enqueue(f.download, f.manifest); err != nil {
		t.Fatal(err)
	}
	claim, err := f.queue.Claim([]string{"transfer"})
	if err != nil || claim == nil {
		t.Fatalf("claim %v", err)
	}
	result := NewTransferWorker(f.service).Run(context.Background(), workerRuntime{queue: f.queue, job: *claim}, *claim)
	if result.ErrorCode == "" {
		if err := f.queue.Complete(claim.Job.ID, claim.LeaseToken); err != nil {
			t.Fatal(err)
		}
	}
	return result
}
