package services

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

const (
	sourcePurposeCleanup = "source_cleanup"
	sourcePurposeSeeding = "seeding"
)

// sourceDisposition describes the logical source lifecycle, independently of
// the local/Node data plane. A physical copy used to deliver a Move does not
// create a seeding obligation. Cloud uploads never create one either.
type sourceDisposition struct {
	Purpose      string
	CloudUpload  bool
	RetainSource bool
}

type sourceNativeTaskMissingKey struct{}

type sourceCleanupOutcome struct {
	nativeMissing  bool
	sharedDeferred bool
}

func (outcome sourceCleanupOutcome) context(ctx context.Context) context.Context {
	return context.WithValue(ctx, sourceNativeTaskMissingKey{}, outcome.nativeMissing)
}

func downloadSourceDisposition(download models.DownloadTask) (sourceDisposition, error) {
	cloudUpload := false
	legacySource := download.SourceDataSourceJSON == "" || download.SourceDataSourceJSON == "{}"
	legacyTarget := download.TargetDataSourceJSON == "" || download.TargetDataSourceJSON == "{}"
	if download.TransferRouteVersion == 0 && legacySource && legacyTarget {
		// Legacy rows already froze the target Storage type. Do not infer a
		// remote target from Node placement, a name, or a filesystem path.
		cloudUpload = download.TargetStorageType != "" && download.TargetStorageType != models.StorageTypeLocal
	} else {
		if download.TransferRouteVersion != models.TransferRouteVersionCurrent {
			return sourceDisposition{}, errors.New("source disposition route version is invalid")
		}
		source, err := decodeDataSourceIdentity(download.SourceDataSourceJSON)
		if err != nil {
			return sourceDisposition{}, err
		}
		target, err := decodeDataSourceIdentity(download.TargetDataSourceJSON)
		if err != nil {
			return sourceDisposition{}, err
		}
		if selectTransferRoute(source, target) != download.TransferRouteKind {
			return sourceDisposition{}, errors.New("source disposition route is inconsistent")
		}
		cloudUpload = download.TransferRouteKind == models.TransferRouteCrossSource && target.Kind == models.DataSourceKindProvider
	}
	result := sourceDisposition{Purpose: sourcePurposeSeeding, CloudUpload: cloudUpload}
	if download.TransferMode == models.MediaLibraryTransferMove || cloudUpload {
		result.Purpose = sourcePurposeCleanup
	}
	result.RetainSource = download.TransferMode == models.MediaLibraryTransferSymlink || cloudUpload && download.TransferMode != models.MediaLibraryTransferMove
	return result, nil
}

func (disposition sourceDisposition) immediate() bool {
	return disposition.Purpose == sourcePurposeCleanup
}

func (disposition sourceDisposition) allowsWholePackageDelete(download models.DownloadTask) bool {
	if disposition.RetainSource {
		return false
	}
	// A same-source local Move has already moved its selected bytes. Remove
	// only its stale native task; the original frozen manifest owns leftovers.
	return download.TransferMode == models.MediaLibraryTransferCopy || download.TransferMode == models.MediaLibraryTransferMove && (disposition.CloudUpload || normalizeExecutionLocation(download.ExecutionLocation) == models.NodeLocationRemote)
}

type sourceCleanupConfirmation struct {
	Version         int    `json:"version"`
	DownloadTaskID  string `json:"download_task_id"`
	TransferTaskID  string `json:"transfer_task_id"`
	ManagedRevision uint64 `json:"managed_revision"`
}

// An old Transfer CleanupCompleted could mean only that Server's receiving
// workspace was removed. Bind a new native source acknowledgement to the
// existing private Job checkpoint, atomically with its transfer result. A late
// missing/protected observation must never downgrade this confirmed result.
func (s *TransferService) recordSourceCleanup(task models.TransferTask, download models.DownloadTask, confirmed bool, updates map[string]any) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		var seed models.SeedingTask
		err := tx.First(&seed, "download_task_id = ?", download.ID).Error
		if errors.Is(err, gorm.ErrRecordNotFound) && !confirmed {
			return tx.Model(&models.TransferTask{}).Where("id = ?", task.ID).Updates(updates).Error
		}
		if err != nil {
			return err
		}
		var job models.Job
		if err := tx.First(&job, "id = ?", seed.JobID).Error; err != nil {
			return err
		}
		if seed.OwnerID != download.OwnerID || task.OwnerID != download.OwnerID || job.JobType != "seeding" || job.OwnerID == nil || *job.OwnerID != download.OwnerID {
			return errors.New("source cleanup confirmation binding is invalid")
		}
		checkpoint := map[string]json.RawMessage{}
		if len(job.CheckpointJSON) > 64*1024 || json.Unmarshal([]byte(job.CheckpointJSON), &checkpoint) != nil || checkpoint == nil {
			return errors.New("source cleanup checkpoint is invalid")
		}
		var prior sourceCleanupConfirmation
		_ = json.Unmarshal(checkpoint["source_cleanup_confirmed"], &prior)
		if !confirmed && prior.Version == 1 && prior.DownloadTaskID == download.ID && prior.TransferTaskID == task.ID && prior.ManagedRevision == task.ManagedRevision {
			return nil
		}
		if confirmed {
			value, _ := json.Marshal(sourceCleanupConfirmation{Version: 1, DownloadTaskID: download.ID, TransferTaskID: task.ID, ManagedRevision: task.ManagedRevision})
			checkpoint["source_cleanup_confirmed"] = value
			raw, err := json.Marshal(checkpoint)
			if err != nil || len(raw) > 64*1024 {
				return errors.New("source cleanup checkpoint is invalid")
			}
			if err := tx.Model(&job).Update("checkpoint_json", string(raw)).Error; err != nil {
				return err
			}
		}
		return tx.Model(&models.TransferTask{}).Where("id = ?", task.ID).Updates(updates).Error
	})
}
