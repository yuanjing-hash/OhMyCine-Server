package services

import (
	"encoding/json"
	"errors"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

// Refresh a short-lived confirmation without replacing the unresolved physical
// owner. Only the same actor and immutable source facts can resume quiescent
// execution; a new preview is not authority to discard an earlier failure.
func saveCatalogDeletionPreviewTx(tx *gorm.DB, preview *models.MediaCatalogDeletionPreview, snapshot mediaCatalogDeletionSnapshot) error {
	var prior models.MediaCatalogDeletionPreview
	err := tx.Table("media_catalog_deletion_previews p").Select("p.*").Joins("JOIN catalog_physical_writes w ON w.owner_kind=? AND w.owner_id=p.id", CatalogPhysicalDeletion).Where("p.library_id=? AND p.work_key=? AND w.state IN ?", preview.LibraryID, preview.WorkKey, []string{"entered", "quiescent"}).Order("p.created_at,p.id").Take(&prior).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return RegisterCatalogPhysicalOwnerTx(tx, CatalogPhysicalWriteInput{LibraryID: preview.LibraryID, OwnerKind: CatalogPhysicalDeletion, OwnerID: preview.ID, ActorID: preview.ActorID}, func(tx *gorm.DB) error { return tx.Create(preview).Error })
	}
	if err != nil {
		return err
	}
	var evidence models.CatalogPhysicalWrite
	if err := tx.First(&evidence, "owner_kind=? AND owner_id=?", CatalogPhysicalDeletion, prior.ID).Error; err != nil {
		return err
	}
	var old mediaCatalogDeletionSnapshot
	if evidence.State != "quiescent" || prior.ActorID != preview.ActorID || prior.ConsumedAt != nil || prior.EntryDigest != preview.EntryDigest || prior.StorageType != preview.StorageType || json.Unmarshal([]byte(prior.SnapshotJSON), &old) != nil || !sameDeletionRecoveryFence(old.CatalogFence, snapshot.CatalogFence) || old.BoundaryDigest != snapshot.BoundaryDigest || old.LibraryRootID != snapshot.LibraryRootID {
		return catalogPhysicalUnsettledError()
	}
	updates := map[string]any{"token_hash": preview.TokenHash, "expires_at": preview.ExpiresAt, "started_at": nil, "updated_at": preview.UpdatedAt}
	if err := tx.Model(&prior).Updates(updates).Error; err != nil {
		return err
	}
	prior.TokenHash = preview.TokenHash
	prior.ExpiresAt = preview.ExpiresAt
	prior.UpdatedAt = preview.UpdatedAt
	prior.StartedAt = nil
	*preview = prior
	return nil
}

func saveTransferDeletionPreviewTx(tx *gorm.DB, preview *models.TransferDeletionPreview) error {
	if !deletionIncludesLibrary(preview.Scope) {
		return tx.Create(preview).Error
	}
	var prior models.TransferDeletionPreview
	err := tx.Table("transfer_deletion_previews p").Select("p.*").Joins("JOIN catalog_physical_writes w ON w.owner_kind=? AND w.owner_id=p.id", CatalogPhysicalTransferDeletion).Where("p.transfer_task_id=? AND w.state IN ?", preview.TransferTaskID, []string{"entered", "quiescent"}).Order("p.created_at,p.id").Take(&prior).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return RegisterCatalogPhysicalOwnerTx(tx, CatalogPhysicalWriteInput{LibraryID: preview.LibraryID, OwnerKind: CatalogPhysicalTransferDeletion, OwnerID: preview.ID, ActorID: preview.ActorID}, func(tx *gorm.DB) error { return tx.Create(preview).Error })
	}
	if err != nil {
		return err
	}
	var evidence models.CatalogPhysicalWrite
	if err := tx.First(&evidence, "owner_kind=? AND owner_id=?", CatalogPhysicalTransferDeletion, prior.ID).Error; err != nil {
		return err
	}
	var old, next transferDeletionState
	if evidence.State != "quiescent" || prior.ActorID != preview.ActorID || prior.CompletedAt != nil || prior.LibraryID != preview.LibraryID || prior.Scope != preview.Scope || prior.DownloadTaskID != preview.DownloadTaskID || prior.IdentityRevision != preview.IdentityRevision || prior.SourceManifestDigest != preview.SourceManifestDigest || prior.ManagedManifestDigest != preview.ManagedManifestDigest || json.Unmarshal([]byte(prior.StateJSON), &old) != nil || json.Unmarshal([]byte(preview.StateJSON), &next) != nil || !sameDeletionRecoveryFence(old.CatalogFence, next.CatalogFence) || old.CatalogDigest != next.CatalogDigest || old.CatalogAssetDigest != next.CatalogAssetDigest {
		return catalogPhysicalUnsettledError()
	}
	updates := map[string]any{"token_hash": preview.TokenHash, "expires_at": preview.ExpiresAt, "consumed_at": nil, "updated_at": preview.UpdatedAt, "transfer_job_revision": preview.TransferJobRevision, "download_job_revision": preview.DownloadJobRevision, "seeding_job_revision": preview.SeedingJobRevision}
	if err := tx.Model(&prior).Updates(updates).Error; err != nil {
		return err
	}
	prior.TokenHash = preview.TokenHash
	prior.ExpiresAt = preview.ExpiresAt
	prior.UpdatedAt = preview.UpdatedAt
	prior.ConsumedAt = nil
	prior.TransferJobRevision = preview.TransferJobRevision
	prior.DownloadJobRevision = preview.DownloadJobRevision
	prior.SeedingJobRevision = preview.SeedingJobRevision
	*preview = prior
	return nil
}

func sameDeletionRecoveryFence(a, b *catalogDeletionFence) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
