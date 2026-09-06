package services

import (
	"errors"
	"math"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/database"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

const (
	CatalogPhysicalTransfer         = "transfer"
	CatalogPhysicalRepair           = "repair"
	CatalogPhysicalReorganization   = "reorganization"
	CatalogPhysicalArtifact         = "artifact"
	CatalogPhysicalDeletion         = "catalog_deletion"
	CatalogPhysicalTransferDeletion = "transfer_deletion"
)

// Call inside the SAME writer as domain enqueue/claim. Enter must additionally
// run immediately before external mutation; admission alone is not a permit.
func AssertCatalogPhysicalAdmissionTx(tx *gorm.DB, libraryID uint) error {
	if err := requireCatalogTransaction(tx); err != nil {
		return err
	}
	if libraryID == 0 {
		return ErrCatalogInvalid
	}
	if err := requireMediaLibraryNotRetiringTx(tx, libraryID); err != nil {
		return err
	}
	var library models.MediaLibrary
	if err := tx.Select("id").First(&library, libraryID).Error; err != nil {
		return err
	}
	var head models.CatalogHead
	err := tx.Select("mode").Where("library_id=?", libraryID).First(&head).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if head.Mode != "legacy" && head.Mode != "versioned" {
		return ErrCatalogFence
	}
	return nil
}

type CatalogPhysicalWriteInput struct {
	LibraryID              uint
	OwnerKind              string
	OwnerID                string
	Job                    *ClaimedJob
	ArtifactReceiptVersion uint
	// Non-queue owners must match their durable domain claim. Repair uses its
	// existing owner ID; synchronous deletion additionally supplies claim time.
	ActorID uint
	ClaimAt time.Time
}

// Opaque process-held receipt, not a transferable/reclaimable lease. Never
// reconstruct a permit from a domain terminal status or from age alone.
type CatalogPhysicalWritePermit struct {
	evidence models.CatalogPhysicalWrite
}

// EnterCatalogPhysicalWriteTx records pre-physical intent transactionally with
// the exclusive converter/retirement gate. Entered evidence is never reclaimed
// by lease expiry. Quiescent evidence can resume ONLY the same frozen owner;
// quiescence acknowledges execution exit, not physical or catalog completion.
func EnterCatalogPhysicalWriteTx(tx *gorm.DB, input CatalogPhysicalWriteInput) (CatalogPhysicalWritePermit, error) {
	var permit CatalogPhysicalWritePermit
	if err := AssertCatalogPhysicalAdmissionTx(tx, input.LibraryID); err != nil {
		return permit, err
	}
	owner, err := catalogPhysicalOwnerTx(tx, input, false, false)
	if err != nil {
		return permit, err
	}
	runtimeID, err := database.ExclusiveRuntimeID(tx)
	if err != nil {
		return permit, err
	}
	library, storage, profile, err := catalogConversionContextTx(tx, input.LibraryID)
	if err != nil {
		return permit, err
	}
	if !storage.Enabled {
		return permit, ErrCatalogFence
	}
	// Profile notification is bounded and happens after its global row commits.
	// Do not enter with old materialized templates in that short propagation gap;
	// otherwise notification itself would invalidate an in-flight recovery fence.
	if library.ProfileRevision != profile.Revision {
		organization, err := storedProfileOrganizationConfig(profile)
		if err != nil {
			return permit, err
		}
		if library.MovieDirectoryTemplate != organization.MovieDirectoryTemplate || library.MovieFilenameTemplate != organization.MovieFilenameTemplate || library.TVDirectoryTemplate != organization.TVDirectoryTemplate || library.TVFilenameTemplate != organization.TVFilenameTemplate {
			return permit, ErrCatalogFence
		}
	}
	var epoch uint64
	if err := tx.Model(&models.CatalogHead{}).Select("source_epoch").Where("library_id=?", input.LibraryID).Scan(&epoch).Error; err != nil {
		return permit, err
	}
	var existing models.CatalogPhysicalWrite
	err = tx.Where("owner_kind=? AND owner_id=?", input.OwnerKind, input.OwnerID).First(&existing).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return permit, err
	}
	if err == nil && ((existing.State != "settled" && existing.State != "quiescent" && existing.State != "admitted") || existing.LibraryID != input.LibraryID || existing.Revision >= math.MaxInt64) {
		return permit, catalogPhysicalUnsettledError()
	}
	if input.ArtifactReceiptVersion > 1 || (input.ArtifactReceiptVersion != 0 && input.OwnerKind != CatalogPhysicalArtifact) {
		return permit, ErrCatalogInvalid
	}
	if input.OwnerKind == CatalogPhysicalArtifact {
		var other uint64
		if err := tx.Model(&models.CatalogPhysicalWrite{}).Select("id").Where("library_id=? AND owner_kind=? AND owner_id<>? AND state IN ('entered','quiescent')", input.LibraryID, CatalogPhysicalArtifact, input.OwnerID).Limit(1).Scan(&other).Error; err != nil {
			return permit, err
		}
		if other != 0 {
			return permit, catalogPhysicalUnsettledError()
		}
	}
	if (existing.State == "quiescent" || existing.State == "admitted") && (existing.OwnerDigest != owner.digest || existing.JobID != owner.jobID || existing.SourceFingerprint != catalogSourceFingerprint(library, storage) || existing.ConfigFingerprint != catalogConfigFingerprint(library, storage, profile) || existing.SourceEpoch != epoch) {
		return permit, ErrCatalogFence
	}
	now := time.Now().UTC()
	row := models.CatalogPhysicalWrite{ID: existing.ID, LibraryID: input.LibraryID, OwnerKind: input.OwnerKind, OwnerID: input.OwnerID, Revision: existing.Revision + 1, State: "entered", JobID: owner.jobID, JobLeaseHash: owner.leaseHash, RuntimeID: runtimeID, ClaimDigest: owner.claimDigest, OwnerDigest: owner.digest, SourceFingerprint: catalogSourceFingerprint(library, storage), ConfigFingerprint: catalogConfigFingerprint(library, storage, profile), SourceEpoch: epoch, EnteredAt: now, UpdatedAt: now}
	row.ArtifactReceiptVersion = input.ArtifactReceiptVersion
	if input.OwnerKind == CatalogPhysicalArtifact && existing.ID == 0 {
		// An untracked historical run cannot prove no replacement occurred.
		row.ArtifactReceiptVersion = 0
	}
	if existing.State == "quiescent" {
		row.ArtifactReceiptVersion = existing.ArtifactReceiptVersion
	}
	if existing.ID == 0 {
		err = tx.Create(&row).Error
	} else {
		result := tx.Model(&models.CatalogPhysicalWrite{}).Where("id=? AND state=? AND revision=?", existing.ID, existing.State, existing.Revision).Select("*").Updates(&row)
		err = result.Error
		if err == nil && result.RowsAffected != 1 {
			err = ErrCatalogFence
		}
	}
	if err != nil {
		return permit, err
	}
	permit.evidence = row
	return permit, nil
}

// Settle is called only after all external calls returned and in the exact
// transaction committing their verified domain/catalog outcome. It is NOT a
// deferred cleanup and must not run after an error/cancellation as a courtesy.
// The original live queue lease and permit revision are both required.
func SettleCatalogPhysicalWriteTx(tx *gorm.DB, permit CatalogPhysicalWritePermit, claim *ClaimedJob) error {
	if err := requireCatalogTransaction(tx); err != nil {
		return err
	}
	proof := permit.evidence
	if proof.ID == 0 || proof.Revision == 0 {
		return ErrCatalogInvalid
	}
	if err := requireCatalogPhysicalPermitRuntimeTx(tx, proof); err != nil {
		return err
	}
	var current models.CatalogPhysicalWrite
	if err := tx.First(&current, proof.ID).Error; err != nil {
		return err
	}
	if current.LibraryID != proof.LibraryID || current.OwnerKind != proof.OwnerKind || current.OwnerID != proof.OwnerID || current.Revision != proof.Revision || current.JobID != proof.JobID || current.JobLeaseHash != proof.JobLeaseHash || current.ClaimDigest != proof.ClaimDigest || current.OwnerDigest != proof.OwnerDigest || current.RuntimeID != proof.RuntimeID {
		return ErrCatalogFence
	}
	if current.State == "settled" {
		return nil
	}
	if current.State != "entered" {
		return ErrCatalogFence
	}
	if proof.JobID != "" {
		if claim == nil || claim.Job.ID != proof.JobID || leaseHash(claim.LeaseToken) != proof.JobLeaseHash {
			return ErrCatalogFence
		}
		if err := catalogCheckJob(tx, models.CatalogSnapshot{JobID: &proof.JobID, JobLeaseHash: proof.JobLeaseHash}, time.Now().UTC()); err != nil {
			return err
		}
	} else if claim != nil {
		return ErrCatalogFence
	}
	owner, err := catalogPhysicalOwnerTx(tx, CatalogPhysicalWriteInput{LibraryID: proof.LibraryID, OwnerKind: proof.OwnerKind, OwnerID: proof.OwnerID, Job: claim}, true, false)
	if err != nil {
		return err
	}
	if owner.digest != proof.OwnerDigest || owner.jobID != proof.JobID {
		return ErrCatalogFence
	}
	if proof.OwnerKind == CatalogPhysicalArtifact {
		if err := assertCatalogArtifactReceiptsResolvedTx(tx, proof); err != nil {
			return err
		}
	}
	now := time.Now().UTC()
	result := tx.Model(&models.CatalogPhysicalWrite{}).Where("id=? AND revision=? AND state='entered' AND job_lease_hash=? AND claim_digest=?", proof.ID, proof.Revision, proof.JobLeaseHash, proof.ClaimDigest).Updates(map[string]any{"state": "settled", "settled_at": now, "updated_at": now})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrCatalogFence
	}
	return nil
}

// QuiesceCatalogPhysicalWriteTx may run only after ALL synchronous physical
// calls have returned and spawned work has joined. Unlike Settle, this preserves
// unresolved recovery and still blocks conversion/retirement. The process-held
// original permit is sufficient even after a queue lease was revoked; no domain
// success, candidate publication or source checkpoint is written by this ACK.
func QuiesceCatalogPhysicalWriteTx(tx *gorm.DB, permit CatalogPhysicalWritePermit) error {
	if err := requireCatalogTransaction(tx); err != nil {
		return err
	}
	proof := permit.evidence
	if proof.ID == 0 || proof.Revision == 0 {
		return ErrCatalogInvalid
	}
	if err := requireCatalogPhysicalPermitRuntimeTx(tx, proof); err != nil {
		return err
	}
	var row models.CatalogPhysicalWrite
	if err := tx.First(&row, proof.ID).Error; err != nil {
		return err
	}
	if row.LibraryID != proof.LibraryID || row.OwnerKind != proof.OwnerKind || row.OwnerID != proof.OwnerID || row.Revision != proof.Revision || row.JobID != proof.JobID || row.JobLeaseHash != proof.JobLeaseHash || row.ClaimDigest != proof.ClaimDigest || row.OwnerDigest != proof.OwnerDigest || row.RuntimeID != proof.RuntimeID {
		return ErrCatalogFence
	}
	if row.State == "settled" || row.State == "quiescent" {
		return nil
	}
	if row.State != "entered" {
		return ErrCatalogFence
	}
	result := tx.Model(&models.CatalogPhysicalWrite{}).Where("id=? AND revision=? AND state='entered' AND job_lease_hash=? AND claim_digest=?", proof.ID, proof.Revision, proof.JobLeaseHash, proof.ClaimDigest).Updates(map[string]any{"state": "quiescent", "updated_at": time.Now().UTC()})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrCatalogFence
	}
	return nil
}

func requireCatalogPhysicalPermitRuntimeTx(tx *gorm.DB, proof models.CatalogPhysicalWrite) error {
	runtimeID, err := database.ExclusiveRuntimeID(tx)
	if err != nil {
		return err
	}
	if runtimeID != proof.RuntimeID {
		return ErrCatalogFence
	}
	return nil
}

func catalogPhysicalUnsettledError() error {
	return appError(CodeConflict, "媒体库存在尚未确认完成的文件操作，请先恢复原任务后重试", nil)
}
