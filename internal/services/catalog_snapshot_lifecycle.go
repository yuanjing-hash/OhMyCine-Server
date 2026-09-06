package services

import (
	"context"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Seal first freezes writes, validates using a genuine deferred read snapshot,
// then marks ready under the ownership/head fence. Validation failure leaves an
// invisible frozen candidate for diagnostics/abandonment, never a half catalog.
// A restart can call Seal again for the same validating candidate.
func (s *CatalogSnapshotStore) Seal(ctx context.Context, id, token string) error {
	if s.readDB == nil {
		return ErrCatalogInvalid
	}
	err := s.writeCatalogBatch(ctx, func(tx *gorm.DB) error {
		var row models.CatalogSnapshot
		if err := tx.First(&row, "id=?", id).Error; err != nil {
			return err
		}
		state := row.State
		if state != "building" && state != "validating" {
			return ErrCatalogFence
		}
		if _, err := catalogOwnedCandidate(tx, id, token, state); err != nil {
			return err
		}
		return tx.Model(&models.CatalogSnapshot{}).Where("id=?", id).Updates(map[string]any{"state": "validating", "updated_at": time.Now().UTC()}).Error
	})
	if err != nil {
		return err
	}
	if err := s.PrepareCollections(ctx, id, token); err != nil {
		return err
	}
	err = s.readDB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		c, err := catalogOwnedCandidate(tx, id, token, "validating")
		if err != nil {
			return err
		}
		r, err := PinCatalogTx(tx, []uint{c.LibraryID})
		if err != nil {
			return err
		}
		r.legacy = nil
		r.privateLayers = true
		if c.Kind == "base" {
			r.layers = nil
		}
		if c.Kind == "delta" && len(r.layers) > CatalogMaxDeltas {
			return ErrCatalogBudget
		}
		r.layers = append(r.layers, models.CatalogHeadLayer{LibraryID: c.LibraryID, Rank: len(r.layers), SnapshotID: c.ID})
		return validateCatalogCandidate(tx, r, c)
	})
	if err != nil {
		return err
	}
	return s.writeCatalogBatch(ctx, func(tx *gorm.DB) error {
		if _, err := catalogOwnedCandidate(tx, id, token, "validating"); err != nil {
			return err
		}
		return tx.Model(&models.CatalogSnapshot{}).Where("id=?", id).Updates(map[string]any{"state": "ready", "updated_at": time.Now().UTC()}).Error
	})
}

func validateCatalogCandidate(tx *gorm.DB, r *CatalogReader, c models.CatalogSnapshot) error {
	if err := validateCatalogCollectionsPrepared(tx, c); err != nil {
		return err
	}
	checks := []*gorm.DB{
		r.entryIdentities().Select("relative_path").Group("relative_path").Having("COUNT(*)>1"),
		r.Recognitions().Select("source_key").Group("source_key").Having("COUNT(*)>1"),
		r.SourceAssets().Select("relative_path").Group("relative_path").Having("COUNT(*)>1"),
		r.Recognitions().Select("id").Where("NOT json_valid(metadata_json)"),
		r.entryIdentities().Select("id").Where("relative_path='' OR relative_path LIKE '.omc-catalog-anchor/%'"),
		r.SourceAssets().Select("id").Where("relative_path='' OR relative_path LIKE '.omc-catalog-anchor/%'"),
		r.entryIdentities().Select("media_library_entries.id").Joins("LEFT JOIN (?) r ON r.id=media_library_entries.recognition_id AND r.library_id=media_library_entries.library_id", r.Recognitions()).Where("media_library_entries.recognition_id IS NOT NULL AND r.id IS NULL"),
	}
	for _, check := range checks {
		var count int64
		if err := tx.Table("(?) checked", check.Limit(1)).Count(&count).Error; err != nil {
			return err
		}
		if count > 0 {
			return ErrCatalogInvalid
		}
	}
	var count int64
	if err := tx.Raw(`SELECT (SELECT COUNT(*) FROM catalog_entry_facts WHERE snapshot_id=?)+(SELECT COUNT(*) FROM catalog_recognition_facts WHERE snapshot_id=?)+(SELECT COUNT(*) FROM catalog_source_asset_facts WHERE snapshot_id=?)+(SELECT COUNT(*) FROM catalog_collection_member_facts WHERE snapshot_id=?)`, c.ID, c.ID, c.ID, c.ID).Scan(&count).Error; err != nil {
		return err
	}
	if count != c.RowCount {
		return ErrCatalogInvalid
	}
	return nil
}

func (s *CatalogSnapshotStore) RenewCandidate(ctx context.Context, id, token string, duration time.Duration) error {
	if duration <= 0 || duration > 15*time.Minute {
		return ErrCatalogInvalid
	}
	return s.writeCatalogBatch(ctx, func(tx *gorm.DB) error {
		var row models.CatalogSnapshot
		if err := tx.First(&row, "id=?", id).Error; err != nil {
			return err
		}
		if row.State != "building" && row.State != "validating" && row.State != "ready" {
			return ErrCatalogFence
		}
		if _, err := catalogOwnedCandidate(tx, id, token, row.State); err != nil {
			return err
		}
		return tx.Model(&models.CatalogSnapshot{}).Where("id=?", id).Updates(map[string]any{"lease_expires_at": time.Now().UTC().Add(duration), "updated_at": time.Now().UTC()}).Error
	})
}

func (s *CatalogSnapshotStore) Abandon(ctx context.Context, id, token string) error {
	if token == "" {
		return ErrCatalogInvalid
	}
	return s.writeCatalogBatch(ctx, func(tx *gorm.DB) error {
		if preparing, err := catalogConversionPreparingTx(tx, id); err != nil {
			return err
		} else if preparing {
			return ErrCatalogFence
		}
		result := tx.Model(&models.CatalogSnapshot{}).Where("id=? AND owner_token_hash=? AND state IN ?", id, catalogTokenHash(token), []string{"building", "validating", "ready"}).Updates(map[string]any{"state": "abandoned", "updated_at": time.Now().UTC()})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrCatalogFence
		}
		return releaseCatalogCompactionRefsTx(tx, id)
	})
}

// AcquireCatalogReferenceTx rejects GC-marked snapshots in the same immediate
// transaction used by GC marking. No expiration alone authorizes reclamation.
func AcquireCatalogReferenceTx(tx *gorm.DB, snapshotID, ownerKind, ownerID string) error {
	if err := requireCatalogTransaction(tx); err != nil {
		return err
	}
	if snapshotID == "" || ownerID == "" || len(ownerID) > 128 || (ownerKind != "artifact" && ownerKind != "diagnosis" && ownerKind != "repair" && ownerKind != "compaction" && ownerKind != "reorganization") {
		return ErrCatalogInvalid
	}
	var count int64
	if err := tx.Model(&models.CatalogSnapshot{}).Where("id=? AND state=?", snapshotID, "published").Count(&count).Error; err != nil {
		return err
	}
	if count != 1 {
		return ErrCatalogFence
	}
	var libraryID uint
	if err := tx.Model(&models.CatalogSnapshot{}).Where("id=?", snapshotID).Pluck("library_id", &libraryID).Error; err != nil {
		return err
	}
	if err := requireMediaLibraryNotRetiringTx(tx, libraryID); err != nil {
		return err
	}
	return tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&models.CatalogSnapshotReference{SnapshotID: snapshotID, OwnerKind: ownerKind, OwnerID: ownerID, CreatedAt: time.Now().UTC()}).Error
}

func ReleaseCatalogReferenceTx(tx *gorm.DB, snapshotID, ownerKind, ownerID string) error {
	if err := requireCatalogTransaction(tx); err != nil {
		return err
	}
	if snapshotID == "" || ownerKind == "" || ownerID == "" {
		return ErrCatalogInvalid
	}
	return tx.Where("snapshot_id=? AND owner_kind=? AND owner_id=?", snapshotID, ownerKind, ownerID).Delete(&models.CatalogSnapshotReference{}).Error
}

// MarkCatalogGCTx marks a single unreachable retired/abandoned snapshot. It does
// not delete files, identity anchors, or cascade large SQL collections.
func MarkCatalogGCTx(tx *gorm.DB, id string) error {
	if err := requireCatalogTransaction(tx); err != nil {
		return err
	}
	result := tx.Exec(`UPDATE catalog_snapshots SET state='gc',updated_at=? WHERE id=? AND state IN ('published','abandoned') AND NOT EXISTS(SELECT 1 FROM catalog_head_layers WHERE snapshot_id=?) AND NOT EXISTS(SELECT 1 FROM catalog_snapshot_references WHERE snapshot_id=?)`, time.Now().UTC(), id, id, id)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrCatalogFence
	}
	return nil
}

// CollectBatch deletes at most CatalogBatchRows facts from one marked snapshot.
// Return done only after all facts and its metadata are removed. Anchors remain.
func (s *CatalogSnapshotStore) CollectBatch(ctx context.Context, id string) (bool, error) {
	done := false
	err := s.writeCatalogBatch(ctx, func(tx *gorm.DB) error {
		var err error
		done, err = collectCatalogBatchTx(tx, id)
		return err
	})
	return done, err
}

// Retirement shares the same bounded GC primitive but supplies its real job
// lease guard in this transaction, so expired retirement workers cannot write.
func collectCatalogBatchTx(tx *gorm.DB, id string) (bool, error) {
	var row models.CatalogSnapshot
	if err := tx.First(&row, "id=? AND state=?", id, "gc").Error; err != nil {
		return false, err
	}
	for _, table := range []string{"catalog_entry_facts", "catalog_recognition_facts", "catalog_source_asset_facts"} {
		result := tx.Exec("DELETE FROM "+table+" WHERE snapshot_id=? AND id IN (SELECT id FROM "+table+" WHERE snapshot_id=? ORDER BY id LIMIT ?)", id, id, CatalogBatchRows)
		if result.Error != nil {
			return false, result.Error
		}
		if result.RowsAffected > 0 {
			return false, nil
		}
	}
	members := tx.Exec("DELETE FROM catalog_collection_member_facts WHERE snapshot_id=? AND work_key IN (SELECT work_key FROM catalog_collection_member_facts WHERE snapshot_id=? ORDER BY work_key LIMIT ?)", id, id, CatalogBatchRows)
	if members.Error != nil {
		return false, members.Error
	}
	if members.RowsAffected > 0 {
		return false, nil
	}
	if err := tx.Where("snapshot_id=?", id).Delete(&models.CatalogCollectionPreparation{}).Error; err != nil {
		return false, err
	}
	if err := tx.Where("snapshot_id=?", id).Delete(&models.CatalogConversionManifest{}).Error; err != nil {
		return false, err
	}
	if err := tx.Delete(&row).Error; err != nil {
		return false, err
	}
	return true, nil
}
