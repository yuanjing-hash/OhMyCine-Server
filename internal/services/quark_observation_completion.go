package services

import (
	"context"
	"errors"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

// completedScopeTx rechecks durable coverage, not the preloaded progress DTO.
// An old complete round, an active reader or unpublished positive facts cannot
// establish the library's first complete-source baseline.
func (s *ProviderObservationService) completedScopeTx(tx *gorm.DB, expected models.ProviderObservationScope) (models.ProviderObservationScope, bool, error) {
	var scope models.ProviderObservationScope
	if err := tx.First(&scope, "id = ?", expected.ID).Error; err != nil {
		return scope, false, err
	}
	if scope.Kind != "library" || scope.LibraryID != expected.LibraryID || scope.ConnectionID != expected.ConnectionID || scope.Fence != expected.Fence || scope.AccountKey != expected.AccountKey || scope.RootID != expected.RootID || scope.CoverageRound != expected.CoverageRound || !scope.CoverageStartedAt.Equal(expected.CoverageStartedAt) || scope.RequestedScanRunID != expected.RequestedScanRunID {
		return scope, false, ErrObservationFence
	}
	if err := s.validScopeTx(tx, scope); err != nil {
		return scope, false, err
	}
	if scope.LastCompleteAt == nil || scope.LastCompleteAt.Before(scope.CoverageStartedAt) || scope.LastErrorCode != "" {
		return scope, false, nil
	}
	var pending int64
	if err := tx.Model(&models.ProviderObservationBatch{}).Where("scope_id = ? AND (revision > published_revision OR state IN ?)", scope.ID, []string{"reading", "confirming"}).Count(&pending).Error; err != nil || pending != 0 {
		return scope, false, err
	}
	if err := observationCurrent(tx, scope.ID).Model(&models.ProviderObservationItem{}).Where("excluded = ? AND (path_repair_pending = ? OR active_batch_id <> '' OR (is_dir = ? AND absent_parent_round = 0 AND completed_round < queue_round))", false, true, true).Count(&pending).Error; err != nil || pending != 0 {
		return scope, false, err
	}
	if err := tx.Model(&models.ProviderObservationAccountBudget{}).Where("account_key = ? AND last_scope_id = ? AND lease_token <> '' AND lease_until > ?", scope.AccountKey, scope.ID, s.now()).Count(&pending).Error; err != nil || pending != 0 {
		return scope, false, err
	}
	writes, err := s.scopeHasPendingWritesTx(tx, scope.ID)
	return scope, !writes && err == nil, err
}

// completeQuarkObservation attaches full-scope evidence to the current catalog
// without publishing an empty delta. In particular, a previously finished 1.1.94
// summary scan can recover its missing baseline/followup in this transaction.
func (s *MediaLibraryService) completeQuarkObservation(ctx context.Context, expected models.ProviderObservationScope) error {
	if s.observations == nil || s.catalogStore == nil || s.catalogScanCommit == nil {
		return ErrObservationUnavailable
	}
	return s.catalogStore.writeCatalogBatch(ctx, func(tx *gorm.DB) error {
		scope, complete, err := s.observations.completedScopeTx(tx, expected)
		if err != nil || !complete {
			return err
		}
		var library models.MediaLibrary
		var storage models.Storage
		var profile models.MediaClassificationProfile
		var head models.CatalogHead
		var summary models.MediaLibraryScanRun
		if err := tx.First(&library, scope.LibraryID).Error; err != nil {
			return err
		}
		if err := tx.First(&storage, library.StorageID).Error; err != nil {
			return err
		}
		if err := tx.First(&profile, library.ProfileID).Error; err != nil {
			return err
		}
		if err := tx.First(&head, "library_id = ?", library.ID).Error; err != nil {
			return err
		}
		if err := tx.First(&summary, scope.RequestedScanRunID).Error; err != nil {
			return err
		}
		if head.Mode != "versioned" || head.SourceFingerprint != catalogSourceFingerprint(library, storage) || head.ConfigFingerprint != catalogConfigFingerprint(library, storage, profile) || summary.LibraryID != library.ID || summary.Kind != "provider_observation" {
			return ErrObservationFence
		}
		// The scope's binding is the original enumeration authority. Once this
		// helper attaches catalog evidence, ordinary no-op progress reuses it.
		if summary.CatalogPublishedAt == nil && summary.SourceFingerprint != scope.Fence {
			return ErrObservationFence
		}
		var followup models.CatalogScanFollowup
		followupErr := tx.First(&followup, "library_id = ?", library.ID).Error
		if followupErr != nil && !errors.Is(followupErr, gorm.ErrRecordNotFound) {
			return followupErr
		}
		if library.BaselineGeneration == 0 {
			// Fresh empty/filtered sources have no content publication owner. Do
			// not replace a pending owner with the summary's zero work counters.
			if followupErr == nil && (followup.RecognitionPending || followup.ArtifactPending || followup.ArtworkPending) {
				return ErrObservationDeferred
			}
			summary.Generation = max(uint64(1), library.DirtyGeneration+1)
			summary.Partial = false
			library.DirtyGeneration = summary.Generation
			summary.SourceFingerprint = mediaLibraryScanSourceFingerprint(library, storage, profile)
			if err := s.finishCatalogScanTx(tx, profile, &summary, CatalogScanPublication{Head: head, NoContentChange: true}, s.catalogScanCommit); err != nil {
				return err
			}
		} else {
			// Positive pages already own recognition/artifacts. Preserve their
			// receipt, run, generation and pending flags; add only scope coverage.
			if followupErr != nil {
				return ErrCatalogFence
			}
			if _, _, err := s.validateCatalogFollowupTx(tx, followup); err != nil {
				return err
			}
			if !followup.CompleteObserved {
				if err := tx.Model(&models.CatalogScanFollowup{}).Where("library_id = ? AND receipt_id = ?", library.ID, followup.ReceiptID).Updates(map[string]any{"complete_observed": true, "diagnosis_pending": s.structure != nil, "updated_at": time.Now().UTC()}).Error; err != nil {
					return err
				}
			}
			if summary.CatalogPublishedAt == nil || summary.Generation != library.BaselineGeneration || summary.Partial {
				now := time.Now().UTC()
				summary.Generation, summary.SourceFingerprint = library.BaselineGeneration, mediaLibraryScanSourceFingerprint(library, storage, profile)
				summary.Status, summary.Phase, summary.Partial, summary.ErrorCode = "success", "completed", false, ""
				summary.FinishedAt, summary.CatalogPublishedAt = scope.LastCompleteAt, &now
				if err := tx.Save(&summary).Error; err != nil {
					return err
				}
			}
		}
		// Quark's supervisor uses observations rather than the filesystem
		// listener handoff. Its successful source status must leave initializing.
		return tx.Model(&models.MediaLibrary{}).Where("id = ?", library.ID).Updates(map[string]any{"status": models.MediaLibraryStatusListening, "last_scan_at": scope.LastCompleteAt, "last_successful_scan_at": scope.LastCompleteAt, "profile_revision": profile.Revision, "reclassification_due": false, "status_error_code": "", "next_retry_at": nil}).Error
	})
}
