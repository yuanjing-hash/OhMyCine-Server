package services

import (
	"context"
	"errors"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

type CatalogMaintenanceResult struct {
	Abandoned, Marked, Collected, Batches int
}

// Maintain is a bounded background tick, never a startup catalog sweep. Queue
// scheduling owns retries. It can run under disk pressure: no preparation space
// gate, VACUUM, file deletion, or arbitrary reference expiration is involved.
func (s *CatalogSnapshotStore) Maintain(ctx context.Context, maxBatches int) (CatalogMaintenanceResult, error) {
	var result CatalogMaintenanceResult
	if maxBatches < 1 || maxBatches > 16 {
		return result, ErrCatalogBudget
	}
	// The durable admission cap makes this query bounded even after a crash.
	var candidates []models.CatalogSnapshot
	if err := s.readDB.WithContext(ctx).Where("state IN ('building','validating','ready') AND lease_expires_at<=?", time.Now().UTC()).Limit(CatalogMaxPreparations).Find(&candidates).Error; err != nil {
		return result, err
	}
	for _, candidate := range candidates {
		if result.Batches >= maxBatches {
			return result, nil
		}
		err := s.writeCatalogBatch(ctx, func(tx *gorm.DB) error {
			var current models.CatalogSnapshot
			if err := tx.First(&current, "id=?", candidate.ID).Error; err != nil {
				return err
			}
			now := time.Now().UTC()
			if current.State != candidate.State || current.LeaseExpiresAt.After(now) || current.OwnerTokenHash != candidate.OwnerTokenHash {
				return ErrCatalogFence
			}
			if preparing, err := catalogConversionPreparingTx(tx, current.ID); err != nil {
				return err
			} else if preparing {
				// Conversion recovery needs its exact manifest/owner proof to
				// restore legacy mode or resume. A timer cannot discard it.
				return ErrCatalogFence
			}
			if current.JobID != nil {
				// A still-running current owner retains references even if its
				// candidate lease expired. Only revoked/expired job authority
				// permits recovery to abandon its immutable candidate.
				var live int64
				if err := tx.Model(&models.Job{}).Where("id=? AND status=? AND lease_token_hash=? AND lease_expires_at>?", *current.JobID, "running", current.JobLeaseHash, now).Count(&live).Error; err != nil {
					return err
				}
				if live > 0 {
					return ErrCatalogFence
				}
			}
			if err := tx.Model(&current).Updates(map[string]any{"state": "abandoned", "updated_at": now}).Error; err != nil {
				return err
			}
			// These exact owner IDs belong only to this revoked candidate.
			return releaseCatalogCompactionRefsTx(tx, current.ID)
		})
		result.Batches++
		if err == nil {
			result.Abandoned++
		} else if !errors.Is(err, ErrCatalogFence) && !errors.Is(err, gorm.ErrRecordNotFound) {
			return result, err
		}
	}
	for result.Batches < maxBatches {
		var pending models.CatalogSnapshot
		err := s.readDB.WithContext(ctx).Select("id").Where("state=?", "gc").Order("updated_at,id").First(&pending).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// Eligibility search is deferred; mark rechecks exact reachability
			// and references in its short immediate transaction.
			err = s.readDB.WithContext(ctx).Select("id").Where("state IN ('published','abandoned') AND NOT EXISTS(SELECT 1 FROM catalog_head_layers l WHERE l.snapshot_id=catalog_snapshots.id) AND NOT EXISTS(SELECT 1 FROM catalog_snapshot_references r WHERE r.snapshot_id=catalog_snapshots.id)").Order("updated_at,id").First(&pending).Error
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return result, nil
			}
			if err != nil {
				return result, err
			}
			err = s.writeCatalogBatch(ctx, func(tx *gorm.DB) error { return MarkCatalogGCTx(tx, pending.ID) })
			result.Batches++
			if errors.Is(err, ErrCatalogFence) {
				continue
			}
			if err != nil {
				return result, err
			}
			result.Marked++
			continue
		}
		if err != nil {
			return result, err
		}
		done, err := s.CollectBatch(ctx, pending.ID)
		result.Batches++
		if errors.Is(err, gorm.ErrRecordNotFound) || errors.Is(err, ErrCatalogFence) {
			continue
		}
		if err != nil {
			return result, err
		}
		if done {
			result.Collected++
		}
	}
	return result, nil
}
