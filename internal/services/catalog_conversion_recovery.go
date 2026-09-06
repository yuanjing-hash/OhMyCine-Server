package services

import (
	"context"
	"errors"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

// Recovery is not a public admin bypass. Its caller must establish authority
// and drain through Guard. Only the exact revoked durable conversion owner can
// be abandoned; a still-live worker, unrelated job, or published head refuses.
type catalogConversionRecoveryInput struct {
	LibraryID uint
	JobID     string
	Guard     func(*gorm.DB, uint) error
}

func (s *CatalogSnapshotStore) abortCatalogConversion(ctx context.Context, input catalogConversionRecoveryInput) error {
	if input.LibraryID == 0 || input.JobID == "" || input.Guard == nil {
		return ErrCatalogInvalid
	}
	return s.writeCatalogBatch(ctx, func(tx *gorm.DB) error {
		if err := assertCatalogConversionDrainedTx(tx, input.LibraryID); err != nil {
			return err
		}
		if err := input.Guard(tx, input.LibraryID); err != nil {
			return err
		}
		var job models.Job
		if err := tx.First(&job, "id=?", input.JobID).Error; err != nil {
			return err
		}
		var payload catalogConversionPayload
		if job.JobType != catalogConversionJobType || job.ResourceKey != mediaArtifactResourceKey(input.LibraryID) || decodeStrictJSON(job.PayloadJSON, &payload) != nil || payload.Version != 1 || payload.LibraryID != input.LibraryID {
			return ErrCatalogFence
		}
		var head models.CatalogHead
		if err := tx.First(&head, "library_id=?", input.LibraryID).Error; err != nil {
			return err
		}
		if head.Mode != "converting" || head.Revision != 0 {
			return ErrCatalogFence
		}
		var candidates []models.CatalogSnapshot
		if err := tx.Where("library_id=? AND source_epoch=? AND state IN ('building','validating','ready')", input.LibraryID, head.SourceEpoch).Limit(2).Find(&candidates).Error; err != nil {
			return err
		}
		if len(candidates) != 1 {
			return ErrCatalogFence
		}
		candidate := candidates[0]
		if candidate.Kind != "base" || candidate.JobID == nil || *candidate.JobID != input.JobID || candidate.JobLeaseHash == "" || candidate.PublishedRevision != 0 {
			return ErrCatalogFence
		}
		now := time.Now().UTC()
		if err := catalogCheckJob(tx, candidate, now); err == nil {
			return ErrCatalogFence
		} else if !errors.Is(err, ErrCatalogFence) {
			return err
		}
		var manifest models.CatalogConversionManifest
		if err := tx.First(&manifest, "snapshot_id=?", candidate.ID).Error; err != nil {
			return err
		}
		var published int64
		if err := tx.Model(&models.CatalogSnapshot{}).Where("library_id=? AND published_revision>0", input.LibraryID).Count(&published).Error; err != nil {
			return err
		}
		if published != 0 {
			return ErrCatalogFence
		}
		if err := tx.Model(&candidate).Updates(map[string]any{"state": "abandoned", "updated_at": now}).Error; err != nil {
			return err
		}
		// Isolate old conversion identity mappings without rewriting legacy rows
		// or unfreezing the original manifest. New conversion starts from scratch.
		return tx.Model(&head).Updates(map[string]any{"mode": "legacy", "source_epoch": head.SourceEpoch + 1, "updated_at": now}).Error
	})
}

func catalogConversionPreparingTx(tx *gorm.DB, snapshotID string) (bool, error) {
	var count int64
	err := tx.Table("catalog_conversion_manifests m").Joins("JOIN catalog_snapshots s ON s.id=m.snapshot_id").Joins("JOIN catalog_heads h ON h.library_id=s.library_id AND h.source_epoch=s.source_epoch AND h.mode='converting'").Where("m.snapshot_id=?", snapshotID).Count(&count).Error
	return count != 0, err
}
