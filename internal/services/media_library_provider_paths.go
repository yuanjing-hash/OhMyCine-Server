package services

import (
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/medialibrary"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

// Called in the catalog publication transaction, after its source/head guard.
// Keep identity history after deletion: artifact-only retries need the original
// path, not a guess based on the formatted STRM filename.
func persistProviderPathsTx(tx *gorm.DB, library models.MediaLibrary, storage models.Storage, result medialibrary.Result, observed time.Time, scanRunID uint) error {
	if storage.Type != models.StorageTypePan115 || observed.IsZero() {
		return nil
	}
	updated := tx.Model(&models.MediaLibraryProviderPathBatch{}).Where("scan_run_id = ? AND library_id = ? AND source_fingerprint = ?", scanRunID, library.ID, catalogSourceFingerprint(library, storage)).Update("published_at", time.Now().UTC())
	if updated.Error != nil {
		return updated.Error
	}
	if updated.RowsAffected != 1 {
		return ErrCatalogFence
	}
	return nil
}

// Run only after cleanup completion commits. Each page repeats the proof guard
// in a separate short transaction; no scan/artifact completion holds all rows.
func resolveCoveredProviderDeletions(db *gorm.DB, libraryID, scanRunID uint, guard func(*gorm.DB) (bool, error)) error {
	for after := uint(0); ; {
		previous := after
		if err := db.Transaction(func(tx *gorm.DB) error {
			ok, err := guard(tx)
			if err != nil || !ok {
				return err
			}
			return resolveCoveredProviderDeletionPageTx(tx, libraryID, scanRunID, &after)
		}); err != nil {
			return err
		}
		if after == previous {
			return nil
		}
	}
}

// A partial batch and an obsolete generation cannot settle ambiguous notices.
func resolveCoveredProviderDeletionPageTx(tx *gorm.DB, libraryID uint, scanRunID uint, after *uint) error {
	var scan models.MediaLibraryScanRun
	if scanRunID == 0 {
		return nil
	}
	if err := tx.Where("id = ? AND library_id = ?", scanRunID, libraryID).First(&scan).Error; err != nil {
		return err
	}
	if (scan.Kind != "full" && scan.Kind != "strm_full_manual" && scan.Kind != "initial") || scan.Partial || scan.CatalogPublishedAt == nil || scan.StartedAt.IsZero() || scan.Generation == 0 {
		return nil
	}
	var library models.MediaLibrary
	var storage models.Storage
	var profile models.MediaClassificationProfile
	if err := tx.First(&library, libraryID).Error; err != nil {
		return err
	}
	if err := tx.First(&storage, library.StorageID).Error; err != nil {
		return err
	}
	if err := tx.First(&profile, library.ProfileID).Error; err != nil {
		return err
	}
	if library.BaselineGeneration != scan.Generation {
		return nil
	}
	old := library
	old.DirtyGeneration = scan.Generation - 1
	if scan.SourceFingerprint == "" || mediaLibraryScanSourceFingerprint(old, storage, profile) != scan.SourceFingerprint {
		return nil
	}
	return resolveCoveredProviderEventsPageTx(tx, libraryID, catalogSourceFingerprint(library, storage), scan.StartedAt, "complete_source_and_artifact_audit", scan.Kind != "initial", after)
}

func resolveCoveredProviderEventsPageTx(tx *gorm.DB, libraryID uint, source string, cutoff time.Time, reason string, manual bool, after *uint) error {
	var rows []models.MediaLibraryProviderEvent
	if err := tx.Where("library_id = ? AND resolution_code = ? AND processed_at IS NULL AND id > ?", libraryID, providerEventNeedsReview, *after).Order("id").Limit(250).Find(&rows).Error; err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}
	ids := []uint{}
	superseded := []uint{}
	inboxIDs := make([]uint, 0, len(rows))
	for _, row := range rows {
		inboxIDs = append(inboxIDs, row.InboxEventID)
	}
	var inboxRows []models.ProviderEvent
	if err := tx.Where("id IN ?", inboxIDs).Find(&inboxRows).Error; err != nil {
		return err
	}
	inboxes := make(map[uint]models.ProviderEvent, len(inboxRows))
	for _, inbox := range inboxRows {
		inboxes[inbox.ID] = inbox
	}
	for _, row := range rows {
		if !row.CreatedAt.Before(cutoff) || (row.SourceFingerprint != "" && row.SourceFingerprint != source) {
			continue
		}
		inbox, exists := inboxes[row.InboxEventID]
		if !exists || !inbox.EventTime.Before(cutoff) {
			continue
		}
		if row.SourceFingerprint == source {
			ids = append(ids, row.ID)
		} else if manual {
			superseded = append(superseded, row.ID)
		}
	}
	if len(ids) > 0 {
		if err := tx.Model(&models.MediaLibraryProviderEvent{}).Where("library_id = ? AND id IN ? AND resolution_code = ? AND processed_at IS NULL", libraryID, ids, providerEventNeedsReview).Updates(map[string]any{"resolution_code": "reconciled_by_scan", "resolution_reason": reason, "processed_at": time.Now().UTC(), "updated_at": time.Now().UTC()}).Error; err != nil {
			return err
		}
	}
	if len(superseded) > 0 {
		if err := tx.Model(&models.MediaLibraryProviderEvent{}).Where("library_id = ? AND id IN ? AND source_fingerprint = '' AND resolution_code = ? AND processed_at IS NULL", libraryID, superseded, providerEventNeedsReview).Updates(map[string]any{"resolution_code": "superseded_by_manual_audit", "resolution_reason": "obsolete_hint_after_complete_manual_audit", "processed_at": time.Now().UTC(), "updated_at": time.Now().UTC()}).Error; err != nil {
			return err
		}
	}
	*after = rows[len(rows)-1].ID
	return nil
}
