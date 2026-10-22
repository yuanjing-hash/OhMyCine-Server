package services

import (
	"encoding/json"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

// A synchronous Quark repair borrows only its ORIGINAL transfer job lease.
// Validate the real parent domain row/payload, never invent a repair job payload.
func quarkStructureParentTx(tx *gorm.DB, repair models.MediaLibraryStructureRepair, parentID string) (models.TransferTask, error) {
	var parent models.TransferTask
	if parentID == "" || repair.JobID == nil || repair.Scope != models.MediaLibraryStructureScopeWork {
		return parent, ErrCatalogFence
	}
	if err := tx.First(&parent, "id = ?", parentID).Error; err != nil {
		return parent, err
	}
	if parent.JobID != *repair.JobID || parent.OwnerID != repair.OwnerID || parent.LibraryID != repair.LibraryID {
		return parent, ErrCatalogFence
	}
	var library models.MediaLibrary
	var storage models.Storage
	var job models.Job
	if err := tx.First(&library, repair.LibraryID).Error; err != nil {
		return parent, err
	}
	if err := tx.First(&storage, library.StorageID).Error; err != nil {
		return parent, err
	}
	if storage.Type != models.StorageTypeQuark {
		return parent, ErrCatalogFence
	}
	if err := tx.First(&job, "id = ?", parent.JobID).Error; err != nil {
		return parent, err
	}
	var payload map[string]json.RawMessage
	var id string
	if job.JobType != "transfer" || json.Unmarshal([]byte(job.PayloadJSON), &payload) != nil || len(payload) != 1 || json.Unmarshal(payload["transfer_task_id"], &id) != nil || id != parent.ID {
		return parent, ErrCatalogFence
	}
	return parent, nil
}

func structureRepairOwnerDigest(row models.MediaLibraryStructureRepair) string {
	base := catalogPhysicalDigest(row.PlanJSON, row.RuleFingerprint, row.Scope, row.WorkKey)
	var state structureCatalogRepairState
	if json.Unmarshal([]byte(row.StateJSON), &state) == nil && state.ParentTransferID != "" {
		base = catalogPhysicalDigest(base, "quark_parent_transfer", state.ParentTransferID)
	}
	return base
}
