package services

import (
	"context"
	"errors"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

const mediaChangeDispatchBatch = 100

// DispatchBatch atomically advances at most 100 target rows and its private
// cursor. Commit-before-wake, interruption, retention and superseding revisions
// cannot lose the newest desired refresh. Queue creation happens after commit.
func (s *MediaChangeService) DispatchBatch(ctx context.Context) (bool, error) {
	var marker models.MediaChangeDispatch
	found := false
	completed := false
	err := withBackgroundTransaction(ctx, s.db, s.writeAdmission, func(tx *gorm.DB) error {
		if err := tx.Order("updated_at,library_id").First(&marker).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		} else if err != nil {
			return err
		}
		found = true
		var ids []uint
		if err := tx.Model(&models.MediaServerRefreshTarget{}).
			Where("library_id=? AND id>? AND enabled=? AND desired_revision<?", marker.LibraryID, marker.AfterTargetID, true, marker.Revision).
			Order("id").Limit(mediaChangeDispatchBatch).Pluck("id", &ids).Error; err != nil {
			return err
		}
		if len(ids) == 0 {
			completed = true
			return tx.Delete(&marker).Error
		}
		if err := tx.Model(&models.MediaServerRefreshTarget{}).Where("id IN ? AND desired_revision<?", ids, marker.Revision).
			Updates(map[string]any{"desired_revision": marker.Revision, "updated_at": time.Now().UTC()}).Error; err != nil {
			return err
		}
		return tx.Model(&marker).Updates(map[string]any{"after_target_id": ids[len(ids)-1], "updated_at": time.Now().UTC()}).Error
	})
	if err != nil || !found {
		return found, err
	}
	s.mu.Lock()
	handler := s.onReady
	s.mu.Unlock()
	if completed && handler != nil {
		handler(marker.LibraryID, marker.Revision)
	}
	return true, nil
}

// Run is a single cancellable lifecycle worker. Recovery is periodic because
// process death after target advancement but before queue insertion is valid.
// onError receives only the error; callers must log a safe code, not SQL/paths.
func (s *MediaChangeService) Run(ctx context.Context, recoverPending func() error, onError func(error)) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	report := func(err error) {
		if err != nil && ctx.Err() == nil && onError != nil {
			onError(err)
		}
	}
	for ctx.Err() == nil {
		wake := s.Wakeups()
		more, err := s.DispatchBatch(ctx)
		if err != nil {
			report(err)
		}
		if more && err == nil {
			// Yield outside SQLite; no sleeps while a transaction owns the writer.
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Millisecond):
			}
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-wake:
		case <-ticker.C:
			if recoverPending != nil {
				report(recoverPending())
			}
			s.prune()
		}
	}
}
