package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"gorm.io/gorm"
)

type cloudObservationDirectoryReader struct{ driver cloud.Driver }

func NewCloudObservationReader(driver cloud.Driver) ObservationDirectoryReader {
	return &cloudObservationDirectoryReader{driver: driver}
}
func (r *cloudObservationDirectoryReader) ObserveDirectory(ctx context.Context, _ models.ProviderObservationScope, id string, offset int64, limit int) (ObservationDirectoryPage, error) {
	if r.driver == nil {
		return ObservationDirectoryPage{}, ErrObservationUnavailable
	}
	page, err := r.driver.List(cloud.WithReadClass(ctx, cloud.ReadClassBackground), id, cloud.PageRequest{Offset: offset, Limit: int64(limit)})
	if err != nil {
		return ObservationDirectoryPage{}, err
	}
	out := ObservationDirectoryPage{Offset: page.Offset, HasMore: page.HasMore, Total: page.Total, Items: make([]ObservationFact, 0, len(page.Items))}
	for _, item := range page.Items {
		out.Items = append(out.Items, ObservationFact{ID: item.ID, ParentID: item.ParentID, Name: item.Name, IsDir: item.IsDir, Size: item.Size, ModifiedAt: item.ModifiedAt, ContentVersion: item.Revision, Digest: item.SHA1})
	}
	return out, nil
}

// ClaimIntake creates the normal ingestion task and its deduplication claim in
// one DB transaction. enqueue only persists the existing task/job; it must not
// perform cloud operations. Disabled intake never adopts an externally added
// final-library file. Ordinary final-root observations are read-only indexing.
func (s *ProviderObservationService) ClaimIntake(ctx context.Context, scopeID, resourceID string, enqueue func(*gorm.DB, models.ProviderObservationItem) (string, error)) (string, error) {
	if enqueue == nil {
		return "", ErrObservationUnavailable
	}
	var taskID string
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var err error
		taskID, err = s.claimIntakeTx(tx, scopeID, resourceID, enqueue)
		return err
	})
	return taskID, err
}

func (s *ProviderObservationService) claimIntakeTx(tx *gorm.DB, scopeID, resourceID string, enqueue func(*gorm.DB, models.ProviderObservationItem) (string, error)) (string, error) {
	var taskID string
	err := func() error {
		var scope models.ProviderObservationScope
		if err := tx.First(&scope, "id = ?", scopeID).Error; err != nil {
			return err
		}
		if scope.Kind != "intake" || !scope.AutoIngest {
			return ErrObservationUnavailable
		}
		if err := s.validScopeTx(tx, scope); err != nil {
			return err
		}
		pending, err := s.scopeHasPendingWritesTx(tx, scopeID)
		if err != nil {
			return err
		}
		if pending {
			return ErrObservationDeferred
		}
		var candidate models.ProviderObservationItem
		if err = observationCurrent(tx, scopeID).First(&candidate, "resource_id = ?", resourceID).Error; err != nil {
			return err
		}
		if candidate.ParentID != scope.RootID || candidate.Excluded || candidate.MissingSince != nil || candidate.DownlistedAt != nil || candidate.StableObservations < 2 || s.now().Sub(candidate.StableSince) < time.Minute || observationExcluded(candidate.Name, true) {
			return ErrObservationDeferred
		}
		if candidate.IsDir {
			if err = s.stableIntakeTreeTx(tx, scope, candidate); err != nil {
				return err
			}
		}
		sum := sha256.Sum256([]byte(scope.ID + "\x00" + scope.Fence + "\x00" + candidate.ResourceID))
		id := hex.EncodeToString(sum[:])
		var existing models.ProviderObservationIntakeClaim
		err = tx.First(&existing, "id = ?", id).Error
		if err == nil {
			taskID = existing.TaskID
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		taskID, err = enqueue(tx, candidate)
		if err != nil {
			return err
		}
		if taskID == "" {
			return ErrObservationInvalid
		}
		claim := models.ProviderObservationIntakeClaim{ID: id, ScopeID: scope.ID, ResourceID: candidate.ResourceID, Fingerprint: candidate.Fingerprint, Fence: scope.Fence, TaskID: taskID, CreatedAt: s.now()}
		return tx.Create(&claim).Error
	}()
	return taskID, err
}

func (s *ProviderObservationService) stableIntakeTreeTx(tx *gorm.DB, scope models.ProviderObservationScope, candidate models.ProviderObservationItem) error {
	queue := []models.ProviderObservationItem{candidate}
	seen := map[string]bool{candidate.ResourceID: true}
	count := 0
	for len(queue) > 0 {
		dir := queue[0]
		queue = queue[1:]
		if dir.LastCompleteAt == nil || dir.ActiveBatchID != "" || dir.PathRepairPending || dir.LastCompleteAt.Before(dir.StableSince.Add(time.Minute)) {
			return ErrObservationDeferred
		}
		var children []models.ProviderObservationItem
		if err := observationCurrent(tx, scope.ID).Where("parent_id = ?", dir.ResourceID).Limit(10001 - count).Find(&children).Error; err != nil {
			return err
		}
		for _, child := range children {
			count++
			if count > 10000 {
				return ErrObservationDeferred
			}
			if child.Excluded || child.MissingSince != nil || child.DownlistedAt != nil || child.StableObservations < 2 || s.now().Sub(child.StableSince) < time.Minute {
				return ErrObservationDeferred
			}
			if child.IsDir {
				if seen[child.ResourceID] {
					return ErrObservationInvalid
				}
				seen[child.ResourceID] = true
				queue = append(queue, child)
			}
		}
	}
	return nil
}
