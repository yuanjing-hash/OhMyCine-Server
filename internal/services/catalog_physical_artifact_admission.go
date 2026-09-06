package services

import (
	"math"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

// Refresh a registered but NEVER entered artifact owner, inside the same queue
// writer as the policy update. Admitted is positive no-I/O evidence; missing,
// entered, quiescent, failed history and lease expiry are not substitutes.
func refreshAdmittedCatalogArtifactTx(tx *gorm.DB, run models.MediaArtifactRun, proof models.CatalogPhysicalWrite, update func(*gorm.DB) error) error {
	if err := AssertCatalogPhysicalAdmissionTx(tx, run.LibraryID); err != nil {
		return err
	}
	if update == nil || proof.ID == 0 || proof.State != "admitted" || proof.Revision >= math.MaxInt64 || proof.OwnerKind != CatalogPhysicalArtifact || proof.OwnerID != run.ID || proof.LibraryID != run.LibraryID || run.Status != models.MediaArtifactStatusQueued || run.JobID == nil || *run.JobID != proof.JobID {
		return catalogPhysicalUnsettledError()
	}
	input := CatalogPhysicalWriteInput{LibraryID: run.LibraryID, OwnerKind: CatalogPhysicalArtifact, OwnerID: run.ID}
	before, err := catalogPhysicalOwnerTx(tx, input, false, true)
	if err != nil {
		return err
	}
	if before.digest != proof.OwnerDigest || before.jobID != proof.JobID {
		return ErrCatalogFence
	}
	library, storage, profile, err := catalogConversionContextTx(tx, run.LibraryID)
	if err != nil {
		return err
	}
	var epoch uint64
	if err := tx.Model(&models.CatalogHead{}).Select("source_epoch").Where("library_id=?", run.LibraryID).Scan(&epoch).Error; err != nil {
		return err
	}
	if epoch != proof.SourceEpoch || proof.SourceFingerprint != catalogSourceFingerprint(library, storage) || proof.ConfigFingerprint != catalogConfigFingerprint(library, storage, profile) {
		return ErrCatalogFence
	}
	var writeID, cleanupID uint64
	if err := tx.Model(&models.CatalogArtifactWriteReceipt{}).Select("id").Where("physical_write_id=?", proof.ID).Limit(1).Scan(&writeID).Error; err != nil {
		return err
	}
	if err := tx.Table("catalog_artifact_cleanup_claims").Select("artifact_id").Where("physical_write_id=?", proof.ID).Limit(1).Scan(&cleanupID).Error; err != nil {
		return err
	}
	if writeID != 0 || cleanupID != 0 {
		return catalogPhysicalUnsettledError()
	}
	if err := update(tx); err != nil {
		return err
	}
	after, err := catalogPhysicalOwnerTx(tx, input, false, true)
	if err != nil {
		return err
	}
	if after.jobID != proof.JobID {
		return ErrCatalogFence
	}
	result := tx.Model(&models.CatalogPhysicalWrite{}).Where("id=? AND revision=? AND state='admitted' AND owner_digest=? AND job_id=?", proof.ID, proof.Revision, proof.OwnerDigest, proof.JobID).Updates(map[string]any{"revision": proof.Revision + 1, "owner_digest": after.digest, "updated_at": time.Now().UTC()})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrCatalogFence
	}
	return nil
}
