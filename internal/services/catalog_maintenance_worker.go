package services

import (
	"context"
	"time"
)

// RunMaintenance isolates potentially long compaction preparation from change
// delivery and artifact/refresh recovery. Each write inside remains admitted and
// bounded; compaction never runs in a user's manual-save request.
func (s *CatalogSnapshotStore) RunMaintenance(ctx context.Context, onError func(error)) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	var after uint
	var stagingAfter uint
	report := func(err error) {
		if err != nil && ctx.Err() == nil && onError != nil {
			onError(err)
		}
	}
	for ctx.Err() == nil {
		_, err := s.Maintain(ctx, 4)
		report(err)
		nextStaging, _, err := s.CleanupLegacyScanStaging(ctx, stagingAfter, time.Now().UTC())
		stagingAfter = nextStaging
		report(err)
		next, _, err := s.CompactNextDue(ctx, after)
		after = next
		report(err)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
