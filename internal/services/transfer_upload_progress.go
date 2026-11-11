package services

import (
	"context"
	"encoding/json"
	"math"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
)

func (w *TransferWorker) persistMeasuredUpload(ctx context.Context, task *models.TransferTask, all, batch []models.RemoteUploadFile, response nodeprotocol.StorageActionResponse) error {
	encoded := ""
	if measured := response.UploadProgress; measured != nil {
		var total, batchTotal, previous int64
		batchIDs := make(map[uint]struct{}, len(batch))
		for _, row := range batch {
			batchIDs[row.ID] = struct{}{}
			if row.Size < 0 || batchTotal > math.MaxInt64-row.Size {
				return cloudTransferError(nodeprotocol.ErrorPlanConflict, false, nil)
			}
			batchTotal += row.Size
		}
		for _, row := range all {
			if row.Size < 0 || total > math.MaxInt64-row.Size {
				return cloudTransferError(nodeprotocol.ErrorPlanConflict, false, nil)
			}
			total += row.Size
			if _, current := batchIDs[row.ID]; !current && (row.Status == nodeprotocol.StorageFileCompleted || row.Status == nodeprotocol.StorageFileReused) {
				previous += row.Size
			}
		}
		// Optional observation never changes the eligibility of verified files.
		if measured.HasMeasurements() && measured.Version == 1 && measured.TotalBytes == batchTotal && measured.TransferredBytes >= 0 && measured.TransferredBytes <= batchTotal && !measured.SampledAt.IsZero() && !measured.SampledAt.After(time.Now().Add(time.Minute)) && (measured.UploadSpeedBPS == nil || *measured.UploadSpeedBPS >= 0) {
			progress := *measured
			progress.TotalBytes, progress.TransferredBytes = total, previous+measured.TransferredBytes
			if response.Phase != nodeprotocol.PhaseUploadingTarget || response.ExecutionActive == nil || !*response.ExecutionActive {
				progress.UploadSpeedBPS = nil
			}
			raw, err := json.Marshal(progress)
			if err != nil {
				return err
			}
			encoded = string(raw)
		}
	}
	if err := fenceRemoteUploadClaim(w.service.db.WithContext(ctx).Model(&models.TransferTask{}).Where("id=? AND job_id=? AND history_cleared_at IS NULL", task.ID, task.JobID), ctx).Update("upload_progress_json", encoded).Error; err != nil {
		return err
	}
	task.UploadProgressJSON = encoded
	return nil
}

func uploadHeartbeat(runtime JobRuntime, task models.TransferTask, fallbackProcessed, fallbackTotal int64) error {
	var sample nodeprotocol.StorageUploadProgress
	if json.Unmarshal([]byte(task.UploadProgressJSON), &sample) == nil && sample.HasMeasurements() && sample.Version == 1 && sample.TotalBytes > 0 && sample.TransferredBytes >= 0 && sample.TransferredBytes <= sample.TotalBytes && !sample.SampledAt.IsZero() && !sample.SampledAt.After(time.Now()) {
		progress := float64(sample.TransferredBytes) * 100 / float64(sample.TotalBytes)
		// Job.speed has no sample clock and nil means preserve, not unknown. Keep
		// measured speed exclusively in the versioned Transfer DTO with freshness.
		return runtime.Heartbeat(&progress, &sample.TransferredBytes, &sample.TotalBytes, nil, nil)
	}
	return runtime.Heartbeat(nil, &fallbackProcessed, &fallbackTotal, nil, nil)
}
