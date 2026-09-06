package services

import (
	"errors"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/database"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

func catalogPhysicalOwnerModel(kind string) (any, error) {
	switch kind {
	case CatalogPhysicalTransfer:
		return &models.TransferTask{}, nil
	case CatalogPhysicalRepair:
		return &models.MediaLibraryStructureRepair{}, nil
	case CatalogPhysicalReorganization:
		return &models.MediaReorganizationTask{}, nil
	case CatalogPhysicalArtifact:
		return &models.MediaArtifactRun{}, nil
	case CatalogPhysicalDeletion:
		return &models.MediaCatalogDeletionPreview{}, nil
	case CatalogPhysicalTransferDeletion:
		return &models.TransferDeletionPreview{}, nil
	default:
		return nil, ErrCatalogInvalid
	}
}

// RegisterCatalogPhysicalOwnerTx wraps the ACTUAL FIRST domain Create in its
// enqueue/preview transaction. Its admitted proof means the row did not exist
// before this transaction and no physical execution has entered. It must never
// be retrofitted onto an old failed/cancelled/retried owner. The callback performs
// database creation only, not external I/O. Queued Jobs are validated durably.
func RegisterCatalogPhysicalOwnerTx(tx *gorm.DB, input CatalogPhysicalWriteInput, create func(*gorm.DB) error) error {
	if err := AssertCatalogPhysicalAdmissionTx(tx, input.LibraryID); err != nil {
		return err
	}
	if create == nil || input.OwnerID == "" {
		return ErrCatalogInvalid
	}
	model, err := catalogPhysicalOwnerModel(input.OwnerKind)
	if err != nil {
		return err
	}
	err = tx.First(model, "id=?", input.OwnerID).Error
	if err == nil {
		return ErrCatalogFence
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	if err := create(tx); err != nil {
		return err
	}
	owner, err := catalogPhysicalOwnerTx(tx, input, false, true)
	if err != nil {
		return err
	}
	library, storage, profile, err := catalogConversionContextTx(tx, input.LibraryID)
	if err != nil {
		return err
	}
	runtimeID, err := database.ExclusiveRuntimeID(tx)
	if err != nil {
		return err
	}
	var epoch uint64
	if err := tx.Model(&models.CatalogHead{}).Select("source_epoch").Where("library_id=?", input.LibraryID).Scan(&epoch).Error; err != nil {
		return err
	}
	now := time.Now().UTC()
	return tx.Create(&models.CatalogPhysicalWrite{LibraryID: input.LibraryID, OwnerKind: input.OwnerKind, OwnerID: input.OwnerID, Revision: 1, State: "admitted", JobID: owner.jobID, RuntimeID: runtimeID, ClaimDigest: owner.claimDigest, OwnerDigest: owner.digest, SourceEpoch: epoch, SourceFingerprint: catalogSourceFingerprint(library, storage), ConfigFingerprint: catalogConfigFingerprint(library, storage, profile), EnteredAt: now, UpdatedAt: now}).Error
}
