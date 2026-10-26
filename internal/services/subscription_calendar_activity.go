package services

import (
	"context"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

const maxCalendarActivityClaims = 5000

// Load only the small fields needed for an activity label, in batches from a
// single read snapshot. Never load encrypted sources, manifests or provider IDs.
func (s *SubscriptionCalendarService) episodeActivityFacts(ctx context.Context, actor Actor, subscriptions []string) (map[string]map[[2]int]string, bool, error) {
	result := map[string]map[[2]int]string{}
	if len(subscriptions) == 0 || (!actor.Can(authz.PermissionDownloadsReadOwn) && !actor.Can(authz.PermissionDownloadsReadAll)) {
		return result, false, nil
	}
	var store *CatalogSnapshotStore
	if s.coverage != nil {
		store = s.coverage.catalogStore
	}
	facade := &MediaLibraryService{db: s.db, catalogStore: store}
	bounded := false
	err := facade.withCatalogReadTx(ctx, func(tx *gorm.DB) error {
		claims := make([]models.FollowEpisodeClaim, 0)
		for offset := 0; offset < len(subscriptions); offset += 500 {
			var batch []models.FollowEpisodeClaim
			if err := tx.Select("subscription_id", "season_number", "episode_number", "download_task_id").Where("subscription_id IN ? AND download_task_id IS NOT NULL", subscriptions[offset:min(offset+500, len(subscriptions))]).Order("subscription_id,season_number,episode_number").Limit(maxCalendarActivityClaims + 1 - len(claims)).Find(&batch).Error; err != nil {
				return err
			}
			claims = append(claims, batch...)
			if len(claims) > maxCalendarActivityClaims {
				claims = claims[:maxCalendarActivityClaims]
				bounded = true
				break
			}
		}
		taskIDs := []string{}
		seen := map[string]bool{}
		for _, claim := range claims {
			if claim.DownloadTaskID != nil && !seen[*claim.DownloadTaskID] {
				taskIDs = append(taskIDs, *claim.DownloadTaskID)
				seen[*claim.DownloadTaskID] = true
			}
		}
		tasks := map[string]models.DownloadTask{}
		jobIDs := []string{}
		seenJobs := map[string]bool{}
		for offset := 0; offset < len(taskIDs); offset += 500 {
			var batch []models.DownloadTask
			if err := tx.Select("id", "owner_id", "job_id", "downloader_id", "target_library_id").Where("id IN ?", taskIDs[offset:min(offset+500, len(taskIDs))]).Find(&batch).Error; err != nil {
				return err
			}
			for _, task := range batch {
				if (!actor.Can(authz.PermissionDownloadsReadAll) && task.OwnerID != actor.User.ID) || !taskLibraryVisible(actor, requestedTargetID(task.TargetLibraryID)) || !taskDownloaderVisible(actor, task.DownloaderID) {
					continue
				}
				tasks[task.ID] = task
				if !seenJobs[task.JobID] {
					jobIDs = append(jobIDs, task.JobID)
					seenJobs[task.JobID] = true
				}
			}
		}
		jobs := map[string]string{}
		for offset := 0; offset < len(jobIDs); offset += 500 {
			var batch []models.Job
			if err := tx.Select("id", "status").Where("id IN ?", jobIDs[offset:min(offset+500, len(jobIDs))]).Find(&batch).Error; err != nil {
				return err
			}
			for _, job := range batch {
				jobs[job.ID] = job.Status
			}
		}
		transfers := map[string]models.TransferTask{}
		if actor.Can(authz.PermissionTransfersReadAll) || actor.Can(authz.PermissionTransfersReadOwn) {
			for offset := 0; offset < len(taskIDs); offset += 500 {
				var batch []models.TransferTask
				if err := tx.Select("download_task_id", "owner_id", "library_id", "phase").Where("download_task_id IN ?", taskIDs[offset:min(offset+500, len(taskIDs))]).Find(&batch).Error; err != nil {
					return err
				}
				for _, transfer := range batch {
					if (actor.Can(authz.PermissionTransfersReadAll) || transfer.OwnerID == actor.User.ID) && taskLibraryVisible(actor, transfer.LibraryID) {
						transfers[transfer.DownloadTaskID] = transfer
					}
				}
			}
		}
		for _, claim := range claims {
			if claim.DownloadTaskID == nil {
				continue
			}
			task, ok := tasks[*claim.DownloadTaskID]
			if !ok {
				continue
			}
			state := "none"
			switch jobs[task.JobID] {
			case models.JobStatusRunning, models.JobStatusQueued, models.JobStatusRetryWait:
				state = "downloading"
			case models.JobStatusFailed, models.JobStatusWaitingUserAction:
				state = "needs_attention"
			case models.JobStatusCompleted:
				if transfer, ok := transfers[task.ID]; ok {
					if transfer.Phase == models.TransferTaskStatusFailed {
						state = "needs_attention"
					} else if transfer.Phase != models.TransferTaskStatusCompleted {
						state = "organizing"
					}
				}
			default:
				if _, found := jobs[task.JobID]; !found {
					continue
				}
			}
			if result[claim.SubscriptionID] == nil {
				result[claim.SubscriptionID] = map[[2]int]string{}
			}
			result[claim.SubscriptionID][[2]int{claim.SeasonNumber, claim.EpisodeNumber}] = state
		}
		return nil
	})
	return result, bounded, err
}
