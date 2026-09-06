package services

import (
	"encoding/json"
	"strconv"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

type catalogPhysicalOwner struct{ jobID, leaseHash, claimDigest, digest string }

func catalogPhysicalDigest(values ...string) string {
	raw, _ := json.Marshal(values) // []string is always JSON encodable.
	return catalogTokenHash(string(raw))
}

// Concrete adapters bind an existing durable domain owner and its actual queue
// payload. No caller-supplied "is claimed" or "is complete" boolean is trusted.
func catalogPhysicalOwnerTx(tx *gorm.DB, input CatalogPhysicalWriteInput, completed, registering bool) (catalogPhysicalOwner, error) {
	var owner catalogPhysicalOwner
	if input.OwnerID == "" || input.LibraryID == 0 {
		return owner, ErrCatalogInvalid
	}
	var libraryID, actorID uint
	var jobType, payloadField string
	var claimAt *time.Time
	var finished bool
	switch input.OwnerKind {
	case CatalogPhysicalTransfer:
		var row models.TransferTask
		if err := tx.First(&row, "id=?", input.OwnerID).Error; err != nil {
			return owner, err
		}
		libraryID, actorID, owner.jobID = row.LibraryID, row.OwnerID, row.JobID
		jobType, payloadField = "transfer", "transfer_task_id"
		owner.digest = catalogPhysicalDigest(row.DownloadTaskID, row.SourceDataSourceJSON, row.TargetDataSourceJSON, row.RouteKind, strconv.Itoa(row.RouteVersion))
		finished = row.Phase == models.TransferTaskStatusCompleted && row.FinishedAt != nil
	case CatalogPhysicalReorganization:
		var row models.MediaReorganizationTask
		if err := tx.First(&row, "id=?", input.OwnerID).Error; err != nil {
			return owner, err
		}
		libraryID, actorID, owner.jobID = row.LibraryID, row.OwnerID, row.JobID
		jobType, payloadField = JobTypeMediaReorganization, "reorganization_task_id"
		owner.digest = catalogPhysicalDigest(row.PlanJSON, row.TargetIdentityJSON, row.ManagedManifestDigest)
		finished = row.Phase == models.MediaReorganizationPhaseCompleted && row.FinishedAt != nil
	case CatalogPhysicalRepair:
		var row models.MediaLibraryStructureRepair
		if err := tx.First(&row, "id=?", input.OwnerID).Error; err != nil {
			return owner, err
		}
		libraryID, actorID = row.LibraryID, row.OwnerID
		if row.JobID != nil {
			owner.jobID = *row.JobID
		}
		jobType, payloadField = JobTypeMediaLibraryRepair, "repair_id"
		owner.digest = catalogPhysicalDigest(row.PlanJSON, row.RuleFingerprint, row.Scope, row.WorkKey)
		finished = row.Phase == "completed" && row.FinishedAt != nil
		if !completed && owner.jobID == "" {
			if row.Scope != models.MediaLibraryStructureScopeWork {
				return owner, ErrCatalogFence
			}
			if row.Phase != "executing" {
				if row.Phase != "reconciling" && row.Phase != "failed" {
					return owner, ErrCatalogFence
				}
				var prior models.CatalogPhysicalWrite
				if err := tx.Where("library_id=? AND owner_kind=? AND owner_id=? AND state='quiescent'", row.LibraryID, CatalogPhysicalRepair, row.ID).First(&prior).Error; err != nil {
					return owner, err
				}
			}
		}
	case CatalogPhysicalArtifact:
		var row models.MediaArtifactRun
		if err := tx.First(&row, "id=?", input.OwnerID).Error; err != nil {
			return owner, err
		}
		libraryID = row.LibraryID
		if row.JobID != nil {
			owner.jobID = *row.JobID
		}
		jobType, payloadField = "media_artifact", "artifact_run_id"
		owner.digest = catalogPhysicalDigest(row.PolicyJSON, strconv.FormatUint(row.Generation, 10))
		finished = row.Status == models.MediaArtifactStatusCompleted && row.FinishedAt != nil && (row.CleanupStatus == "completed" || row.CleanupStatus == "skipped")
		if !completed && !registering && row.Status != models.MediaArtifactStatusRunning && row.Status != models.MediaArtifactStatusCompleted {
			var prior models.CatalogPhysicalWrite
			if err := tx.Where("library_id=? AND owner_kind=? AND owner_id=? AND state='quiescent'", row.LibraryID, CatalogPhysicalArtifact, row.ID).First(&prior).Error; err != nil {
				return owner, ErrCatalogFence
			}
		}
	case CatalogPhysicalDeletion:
		var row models.MediaCatalogDeletionPreview
		if err := tx.First(&row, "id=?", input.OwnerID).Error; err != nil {
			return owner, err
		}
		libraryID, actorID, claimAt = row.LibraryID, row.ActorID, row.StartedAt
		owner.digest = catalogPhysicalDigest(row.SnapshotJSON, row.EntryDigest, row.WorkKey)
		finished = row.ConsumedAt != nil && row.StartedAt == nil && row.LastErrorCode == ""
	case CatalogPhysicalTransferDeletion:
		var row models.TransferDeletionPreview
		if err := tx.First(&row, "id=?", input.OwnerID).Error; err != nil {
			return owner, err
		}
		libraryID, actorID, claimAt = row.LibraryID, row.ActorID, row.ConsumedAt
		owner.digest = catalogPhysicalDigest(row.TransferTaskID, row.DownloadTaskID, row.Scope, row.SourceManifestDigest, row.ManagedManifestDigest)
		finished = row.CompletedAt != nil && row.LastErrorCode == ""
	default:
		return owner, ErrCatalogInvalid
	}
	if libraryID != input.LibraryID {
		return owner, ErrCatalogFence
	}
	owner.digest = catalogPhysicalDigest(owner.digest, uintID(libraryID), uintID(actorID))
	if completed {
		if !finished {
			return owner, ErrCatalogFence
		}
		return owner, nil
	}
	if owner.jobID != "" {
		if !registering && (input.Job == nil || input.Job.Job.ID != owner.jobID || input.Job.LeaseToken == "") {
			return owner, ErrCatalogFence
		}
		var job models.Job
		if err := tx.First(&job, "id=?", owner.jobID).Error; err != nil {
			return owner, err
		}
		var payload map[string]json.RawMessage
		var ownerID string
		if job.JobType != jobType || json.Unmarshal([]byte(job.PayloadJSON), &payload) != nil || len(payload) != 1 || json.Unmarshal(payload[payloadField], &ownerID) != nil || ownerID != input.OwnerID {
			return owner, ErrCatalogFence
		}
		if registering {
			if input.Job != nil || job.Status != models.JobStatusQueued || job.LeaseTokenHash != "" || job.LeaseExpiresAt != nil {
				return owner, ErrCatalogFence
			}
			return owner, nil
		}
		owner.leaseHash = leaseHash(input.Job.LeaseToken)
		if err := catalogCheckJob(tx, models.CatalogSnapshot{JobID: &owner.jobID, JobLeaseHash: owner.leaseHash}, time.Now().UTC()); err != nil {
			return owner, err
		}
	} else {
		if input.Job != nil || input.ActorID != actorID || actorID == 0 {
			return owner, ErrCatalogFence
		}
		if input.OwnerKind != CatalogPhysicalRepair {
			if registering {
				if claimAt != nil || finished {
					return owner, ErrCatalogFence
				}
				return owner, nil
			}
			if claimAt == nil || input.ClaimAt.IsZero() || !claimAt.Equal(input.ClaimAt) {
				return owner, ErrCatalogFence
			}
			owner.claimDigest = catalogPhysicalDigest(strconv.FormatUint(uint64(actorID), 10), claimAt.UTC().Format(time.RFC3339Nano))
		} else {
			owner.claimDigest = catalogPhysicalDigest(strconv.FormatUint(uint64(actorID), 10), input.OwnerID)
		}
	}
	return owner, nil
}
