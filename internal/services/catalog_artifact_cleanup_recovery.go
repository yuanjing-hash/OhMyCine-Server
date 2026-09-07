package services

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

type artifactCleanupPermitContextKey struct{}

func withArtifactCleanupPermit(ctx context.Context, permit CatalogPhysicalWritePermit) context.Context {
	return context.WithValue(ctx, artifactCleanupPermitContextKey{}, permit)
}

type mediaArtifactCleanupRecovery interface {
	ReconcileSupersededCleanup(context.Context, CatalogPhysicalWritePermit) error
}

// Writer admission and cleanup claims use the same transaction-local target
// boundary. A claim still blocks even if its artifact status was changed.
func assertArtifactCleanupTargetResolvedTx(tx *gorm.DB, artifact models.MediaArtifact) error {
	var unresolved uint
	if err := tx.Model(&models.MediaArtifact{}).Select("id").Where("library_id = ? AND target_kind = ? AND relative_path = ?", artifact.LibraryID, artifact.TargetKind, artifact.RelativePath).Where("status = ? OR EXISTS (SELECT 1 FROM catalog_artifact_cleanup_claims c WHERE c.artifact_id = media_artifacts.id)", models.MediaArtifactStatusCleanup).Limit(1).Scan(&unresolved).Error; err != nil {
		return err
	}
	if unresolved != 0 {
		return catalogPhysicalUnsettledError()
	}
	return nil
}

func artifactCleanupManifestDigest(artifact models.MediaArtifact) (string, error) {
	artifact.Status = models.MediaArtifactStatusCleanup
	// Status updates change UpdatedAt; all ownership/content fields remain exact.
	artifact.UpdatedAt = time.Time{}
	raw, err := encodeCatalogArtifactMetadata(artifact)
	return catalogTokenHash(raw), err
}

func (s *STRMManagementService) recordArtifactCleanupClaimTx(tx *gorm.DB, plan artifactCleanupPlan, artifact models.MediaArtifact) error {
	var existing models.CatalogArtifactCleanupClaim
	err := tx.First(&existing, "artifact_id = ?", artifact.ID).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	digest, err := artifactCleanupManifestDigest(artifact)
	if err != nil {
		return err
	}
	var current models.MediaArtifact
	if err := tx.First(&current, artifact.ID).Error; err != nil {
		return err
	}
	currentDigest, err := artifactCleanupManifestDigest(current)
	if err != nil || currentDigest != digest || current.Status != artifact.Status {
		return ErrCatalogFence
	}
	if artifact.Status == models.MediaArtifactStatusCleanup {
		// Never invent ownership for historical interrupted cleanup or transfer an
		// existing marker to a newer automatic execution. Manual exact retries
		// preserve prior evidence until the manifest has actually been removed.
		if existing.ArtifactID != 0 && (existing.LibraryID != artifact.LibraryID || existing.ManifestDigest != digest) {
			return ErrCatalogFence
		}
		if plan.Automatic && (plan.PhysicalPermit == nil || existing.OwnerRunID == nil || existing.PhysicalWriteID == nil || *existing.PhysicalWriteID != plan.PhysicalPermit.evidence.ID || *existing.OwnerRunID != plan.PhysicalPermit.evidence.OwnerID) {
			return ErrCatalogFence
		}
		if plan.Automatic {
			proof, run, err := catalogArtifactPermitTx(tx, *plan.PhysicalPermit)
			if err != nil {
				return err
			}
			if proof.State != "entered" || plan.Run == nil || plan.Run.ID != run.ID || proof.LibraryID != artifact.LibraryID {
				return ErrCatalogFence
			}
			if err := catalogCheckJob(tx, models.CatalogSnapshot{JobID: &proof.JobID, JobLeaseHash: proof.JobLeaseHash}, time.Now().UTC()); err != nil {
				return err
			}
		}
		return nil
	}
	if existing.ArtifactID != 0 {
		return ErrCatalogFence
	}
	var generator models.MediaArtifactRun
	if err := tx.First(&generator, "id = ? AND library_id = ?", artifact.RunID, artifact.LibraryID).Error; err != nil {
		return err
	}
	target, ok := plan.Targets[artifact.ID]
	if !ok || target.RootIdentity == "" {
		return ErrCatalogFence
	}
	claim := models.CatalogArtifactCleanupClaim{ArtifactID: artifact.ID, LibraryID: artifact.LibraryID, OriginalStatus: artifact.Status, ManifestDigest: digest, RootIdentity: target.RootIdentity, GeneratorPolicyDigest: catalogTokenHash(generator.PolicyJSON)}
	if plan.Automatic && plan.PhysicalPermit != nil {
		proof, run, err := catalogArtifactPermitTx(tx, *plan.PhysicalPermit)
		if err != nil {
			return err
		}
		if proof.State != "entered" || plan.Run == nil || plan.Run.ID != run.ID || proof.LibraryID != artifact.LibraryID {
			return ErrCatalogFence
		}
		if err := catalogCheckJob(tx, models.CatalogSnapshot{JobID: &proof.JobID, JobLeaseHash: proof.JobLeaseHash}, time.Now().UTC()); err != nil {
			return err
		}
		claim.OwnerRunID, claim.PhysicalWriteID, claim.PermitRevision = &run.ID, &proof.ID, proof.Revision
	}
	// Manual/legacy direct cleanup has no run attribution. Only the actual
	// worker's opaque permit can authorize later automatic reconciliation.
	return tx.Create(&claim).Error
}

// ReconcileSupersededCleanup only observes files and settles exact bookkeeping.
// It must never remove a file or prune a directory for an obsolete generation.
func (s *STRMManagementService) ReconcileSupersededCleanup(ctx context.Context, permit CatalogPhysicalWritePermit) error {
	if s == nil || s.libraries == nil {
		return ErrCatalogFence
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Maintenance must not queue behind a long scan: its context cannot cancel
	// Mutex.Lock, and that would stall other libraries and Scheduler.Close.
	// Keep the same boundary, but defer recovery with all evidence intact when
	// another operation owns it. The existing worker/maintenance retries later.
	lock := s.libraries.scanLock(permit.evidence.LibraryID)
	if !lock.TryLock() {
		return ErrCatalogFence
	}
	defer lock.Unlock()
	if err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		proof, _, err := catalogArtifactPermitTx(tx, permit)
		if err != nil {
			return err
		}
		if proof.State != "quiescent" {
			return ErrCatalogFence
		}
		return nil
	}); err != nil {
		return err
	}
	var after uint
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var claims []models.CatalogArtifactCleanupClaim
		if err := s.db.WithContext(ctx).Where("library_id = ? AND physical_write_id = ? AND owner_run_id = ? AND artifact_id > ?", permit.evidence.LibraryID, permit.evidence.ID, permit.evidence.OwnerID, after).Order("artifact_id").Limit(CatalogBatchRows).Find(&claims).Error; err != nil {
			return err
		}
		if len(claims) == 0 {
			return nil
		}
		for _, claim := range claims {
			var artifact models.MediaArtifact
			var policy mediaArtifactPolicy
			if err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
				var err error
				artifact, policy, err = validateArtifactCleanupRecoveryTx(tx, permit, claim)
				return err
			}); err != nil {
				return err
			}
			observed, err := inspectArtifactReceiptFile(ctx, policy.ProjectionRoot, claim.RootIdentity, artifact.RelativePath)
			if err != nil {
				return err
			}
			if observed.Exists && (len(artifact.ContentFingerprint) != 64 || observed.Fingerprint != artifact.ContentFingerprint) {
				return ErrCatalogFence
			}
			if err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
				if _, _, err := validateArtifactCleanupRecoveryTx(tx, permit, claim); err != nil {
					return err
				}
				var result *gorm.DB
				if observed.Exists {
					result = tx.Model(&models.MediaArtifact{}).Where("id = ? AND status = ? AND active = ?", artifact.ID, models.MediaArtifactStatusCleanup, false).Update("status", claim.OriginalStatus)
				} else {
					result = tx.Where("id = ? AND status = ? AND active = ?", artifact.ID, models.MediaArtifactStatusCleanup, false).Delete(&models.MediaArtifact{})
				}
				if result.Error != nil {
					return result.Error
				}
				if result.RowsAffected != 1 {
					return ErrCatalogFence
				}
				return tx.Where("artifact_id = ?", artifact.ID).Delete(&models.CatalogArtifactCleanupClaim{}).Error
			}); err != nil {
				return err
			}
			after = claim.ArtifactID
		}
	}
}

func validateArtifactCleanupRecoveryTx(tx *gorm.DB, permit CatalogPhysicalWritePermit, expected models.CatalogArtifactCleanupClaim) (models.MediaArtifact, mediaArtifactPolicy, error) {
	var artifact models.MediaArtifact
	var policy mediaArtifactPolicy
	proof, _, err := catalogArtifactPermitTx(tx, permit)
	if err != nil {
		return artifact, policy, err
	}
	if proof.State != "quiescent" {
		return artifact, policy, ErrCatalogFence
	}
	// No physical operation or readiness publication is allowed here. Like
	// receipt reconciliation, exact original quiescent evidence remains usable
	// after cancellation/revocation; demanding a live lease would strand it.
	var claim models.CatalogArtifactCleanupClaim
	if err := tx.First(&claim, "artifact_id = ?", expected.ArtifactID).Error; err != nil {
		return artifact, policy, err
	}
	if claim.OwnerRunID == nil || claim.PhysicalWriteID == nil || *claim.OwnerRunID != proof.OwnerID || *claim.PhysicalWriteID != proof.ID || claim.LibraryID != proof.LibraryID || claim.PermitRevision == 0 || claim.PermitRevision > proof.Revision || claim.ManifestDigest != expected.ManifestDigest || claim.OriginalStatus != expected.OriginalStatus || claim.RootIdentity != expected.RootIdentity || claim.GeneratorPolicyDigest != expected.GeneratorPolicyDigest || claim.OriginalStatus == "" || claim.OriginalStatus == models.MediaArtifactStatusCleanup {
		return artifact, policy, ErrCatalogFence
	}
	if err := tx.First(&artifact, claim.ArtifactID).Error; err != nil {
		return artifact, policy, err
	}
	digest, err := artifactCleanupManifestDigest(artifact)
	if err != nil || artifact.LibraryID != proof.LibraryID || !artifact.Managed || artifact.Active || artifact.Status != models.MediaArtifactStatusCleanup || digest != claim.ManifestDigest {
		return artifact, policy, ErrCatalogFence
	}
	var generator models.MediaArtifactRun
	if err := tx.First(&generator, "id = ? AND library_id = ?", artifact.RunID, artifact.LibraryID).Error; err != nil {
		return artifact, policy, err
	}
	if catalogTokenHash(generator.PolicyJSON) != claim.GeneratorPolicyDigest || json.Unmarshal([]byte(generator.PolicyJSON), &policy) != nil || policy.LibraryID != artifact.LibraryID || policy.TargetKind != artifact.TargetKind || !cleanupArtifactPathAllowed(artifact, policy) || (policy.ProjectionRootIdentity != "" && policy.ProjectionRootIdentity != claim.RootIdentity) {
		return artifact, policy, ErrCatalogFence
	}
	var unresolved uint64
	if err := tx.Model(&models.CatalogArtifactWriteReceipt{}).Select("id").Where("library_id = ? AND target_kind = ? AND relative_path = ? AND phase IN ?", artifact.LibraryID, artifact.TargetKind, artifact.RelativePath, []string{"prepared", "conflict"}).Limit(1).Scan(&unresolved).Error; err != nil {
		return artifact, policy, err
	}
	if unresolved != 0 {
		return artifact, policy, ErrCatalogFence
	}
	return artifact, policy, nil
}
