package services

import (
	"context"
	"strings"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

// revokeQuarkObservationSTRMTx closes playback eligibility in the same commit
// that publishes a confirmed observation removal. Physical cleanup still uses
// the precise manifest workset/outbox; an inactive row is not a deleted receipt.
// This is deliberately not a full-catalog sweep or a change to legacy scans.
func revokeQuarkObservationSTRMTx(ctx context.Context, tx *gorm.DB, library models.MediaLibrary, run *models.MediaLibraryScanRun, changes CatalogArtifactChangeSet) error {
	if run == nil || run.Kind != "provider_observation" {
		return nil
	}
	scope, ok := providerChangeScopeFromContext(ctx)
	if !ok || scope.VerifiedResult == nil || len(scope.VerifiedResult.DeletedProviderIDs) == 0 {
		return nil
	}
	if err := requireCatalogTransaction(tx); err != nil {
		return err
	}
	var storage models.Storage
	if err := tx.First(&storage, library.StorageID).Error; err != nil {
		return err
	}
	if storage.Type != models.StorageTypeQuark {
		return nil
	}
	ack, _ := ctx.Value(providerObservationCommitContextKey{}).(func(*gorm.DB) error)
	if ack == nil || run.LibraryID != library.ID || !scope.VerifiedResult.Partial || !scope.VerifiedResult.Scoped {
		return ErrObservationInvalid
	}
	// PublishPending supplies at most 250 exact changes. Refuse an oversized
	// final commit rather than silently revoke a prefix or scan the whole library.
	if len(scope.VerifiedResult.DeletedProviderIDs) > CatalogBatchRows {
		return ErrCatalogBudget
	}
	providerIDs := make([]string, 0, len(scope.VerifiedResult.DeletedProviderIDs))
	seen := make(map[string]bool, len(scope.VerifiedResult.DeletedProviderIDs))
	for _, id := range scope.VerifiedResult.DeletedProviderIDs {
		if strings.TrimSpace(id) == "" {
			return ErrObservationInvalid
		}
		if !seen[id] {
			seen[id] = true
			providerIDs = append(providerIDs, id)
		}
	}
	var artifacts []models.MediaArtifact
	if err := tx.Where("library_id = ? AND provider_item_id IN ? AND managed = ? AND active = ? AND kind = ? AND target_kind = ?", library.ID, providerIDs, true, true, models.MediaArtifactKindSTRM, models.MediaArtifactTargetLocalProjection).
		Order("id").Limit(CatalogBatchRows + 1).Find(&artifacts).Error; err != nil {
		return err
	}
	if len(artifacts) > CatalogBatchRows {
		return ErrCatalogBudget
	}
	if len(artifacts) == 0 {
		return nil
	}
	reader, err := PinCatalogTx(tx, []uint{library.ID})
	if err != nil {
		return err
	}
	var remaining []models.MediaLibraryEntry
	if err := reader.Entries().Where("library_id = ? AND provider_id IN ?", library.ID, providerIDs).Limit(1).Find(&remaining).Error; err != nil {
		return err
	}
	if len(remaining) != 0 {
		return ErrCatalogFence
	}
	prepared := make(map[uint]bool, len(changes.Manifests))
	for _, id := range changes.Manifests {
		prepared[id] = true
	}
	var profile models.MediaClassificationProfile
	if err := tx.First(&profile, library.ProfileID).Error; err != nil {
		return err
	}
	owners := make(map[string]bool)
	ids := make([]uint, 0, len(artifacts))
	for _, artifact := range artifacts {
		if !prepared[artifact.ID] {
			return ErrCatalogFence
		}
		if !owners[artifact.RunID] {
			if err := validateDeletedArtifactOwnerTx(tx, library, storage, profile, artifact.RunID); err != nil {
				return err
			}
			owners[artifact.RunID] = true
		}
		ids = append(ids, artifact.ID)
	}
	result := tx.Model(&models.MediaArtifact{}).Where("library_id = ? AND id IN ? AND managed = ? AND active = ? AND kind = ? AND target_kind = ?", library.ID, ids, true, true, models.MediaArtifactKindSTRM, models.MediaArtifactTargetLocalProjection).
		Updates(map[string]any{"active": false, "updated_at": time.Now().UTC()})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != int64(len(ids)) {
		return ErrCatalogFence
	}
	return nil
}
