package services

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

// A user retry after durable proof of no native task creates a distinct
// acquisition. The old operation, immutable clock and provider receipt remain
// attached to the failed historical task. Unknown writes and accepted terminal
// tasks never enter this path; normal Retry retains their existing fences.
func (s *DownloadService) replaceRejectedOfflineRetry(tx *gorm.DB, job models.Job, now time.Time) (*models.Job, error) {
	if job.JobType != "download" {
		return nil, nil
	}
	var original models.DownloadTask
	if err := tx.First(&original, "job_id = ?", job.ID).Error; err != nil {
		return nil, err
	}
	if original.OfflineKind == "" || original.OfflineClockState != "not_accepted" || original.OfflineSettledAt == nil || original.OfflineReadyAt != nil || original.OfflineStopPending {
		return nil, nil
	}
	var checkpoint map[string]json.RawMessage
	if err := json.Unmarshal([]byte(job.CheckpointJSON), &checkpoint); err != nil {
		return nil, appError(CodeQueueStateConflict, "原任务重试记录无效", err)
	}
	if checkpoint == nil {
		checkpoint = make(map[string]json.RawMessage)
	}
	var replacementID string
	if raw := checkpoint["offline_retry_task_id"]; len(raw) != 0 {
		if json.Unmarshal(raw, &replacementID) != nil || replacementID == "" {
			return nil, appError(CodeQueueStateConflict, "原任务重试记录无效", nil)
		}
		var accepted models.DownloadTask
		var acceptedJob models.Job
		if err := tx.First(&accepted, "id = ? AND owner_id = ?", replacementID, original.OwnerID).Error; err != nil {
			return nil, appError(CodeQueueStateConflict, "已接受的重试记录不可用，不能重复提交", err)
		}
		if err := tx.First(&acceptedJob, "id = ?", accepted.JobID).Error; err != nil {
			return nil, err
		}
		return &acceptedJob, nil
	}
	if original.DownloaderID == nil || original.BTSelectionGroupID != "" {
		return nil, appError(CodeQueueStateConflict, "原来源需要重新选择资源后提交", nil)
	}
	var record models.Downloader
	if err := tx.First(&record, "id = ? AND enabled = ?", *original.DownloaderID, true).Error; err != nil {
		return nil, appError(CodeDownloaderUnavailable, "原下载器不可用", err)
	}
	if record.Type != original.ProviderType || normalizeExecutionLocation(record.ExecutionLocation) != normalizeExecutionLocation(original.ExecutionLocation) || !sameOptionalString(record.NodeID, original.NodeID) {
		return nil, appError(CodeQueueStateConflict, "原下载路线已变化，请重新选择资源后提交", nil)
	}
	factory := *s.downloader
	factory.db = tx
	if err := consumeOfflineConfirmationTx(tx, original, SubmitDownloadInput{}, nil, 0, now, &factory); err != nil {
		return nil, appError(CodeQueueStateConflict, "原来源账号、目录或离线历史已变化，请重新选择资源并确认本次下载", err)
	}
	if normalizeExecutionLocation(record.ExecutionLocation) == models.NodeLocationRemote {
		if err := factory.requireOfflineLifecycleNode(record); err != nil {
			return nil, err
		}
		_, revision, digest, err := nodeTaskRoutePlan(tx, record, original.StagingAbsolutePath, requestedTargetID(original.TargetLibraryID))
		if err != nil {
			return nil, err
		}
		if revision != original.RoutePlanRevision || digest != original.RoutePlanDigest {
			return nil, appError(CodeQueueStateConflict, "原节点路线已变化，请重新选择资源后提交", nil)
		}
	}
	plain, err := s.credentials.Decrypt(downloadSourcePurpose(original.ID), original.SourceCiphertext)
	if err != nil {
		return nil, appError(CodeConnectionUnavailable, "原下载来源记录不可用", err)
	}
	var source downloadSourceEnvelope
	if json.Unmarshal([]byte(plain), &source) != nil || validateStoredContentSelection(source) != nil || original.ContentSelectionRequired && source.ContentSelection == nil {
		return nil, appError(CodeDownloadSourceInvalid, "原下载来源或选集记录无效", nil)
	}
	resource, err := offlineResourceDigest(source)
	if err != nil || resource != original.OfflineResourceDigest {
		return nil, appError(CodeDownloadSourceInvalid, "原下载资源身份已变化", err)
	}
	var payload downloadJobPayload
	if json.Unmarshal([]byte(job.PayloadJSON), &payload) != nil || payload.DownloadTaskID != original.ID {
		return nil, appError(CodeQueueStateConflict, "原下载任务关联无效", nil)
	}
	replacement := original
	replacement.ID, replacement.JobID = uuid.NewString(), uuid.NewString()
	replacement.ProviderTag = "omc-" + replacement.ID
	replacement.ProviderTaskID, replacement.ProviderOutputID, replacement.ProviderStatus = "", "", ""
	replacement.OfflineSettledAt, replacement.OfflineStartedAt, replacement.OfflineDeadlineAt, replacement.OfflineReadyAt = nil, nil, nil, nil
	replacement.OfflineClockState, replacement.OfflineStartProvenance, replacement.OfflineCompletionMode = "", "", ""
	replacement.OfflineStopPending, replacement.OfflineHistoryRevision = false, 0
	replacement.Progress, replacement.BytesCompleted, replacement.BytesTotal = nil, nil, nil
	replacement.DownloadSpeed, replacement.UploadSpeed, replacement.ETASeconds, replacement.LastSampledAt = nil, nil, nil, nil
	replacement.Phase, replacement.LastErrorCode, replacement.LastErrorMessage = models.DownloadTaskStatusQueued, "", ""
	replacement.ScrapeStatus, replacement.CompletedManifestJSON, replacement.ProviderMetadataJSON, replacement.StagingCategory = "", "{}", "", ""
	replacement.ManifestFileCount = 0
	replacement.HistoryClearedAt, replacement.FinishedAt = nil, nil
	replacement.CreatedAt, replacement.UpdatedAt = now, now
	replacement.SourceCiphertext, err = s.credentials.Encrypt(downloadSourcePurpose(replacement.ID), plain)
	if err != nil {
		return nil, err
	}
	payload.DownloadTaskID, payload.ControlIntent = replacement.ID, ""
	rawPayload, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	var maxPosition int64
	if err := tx.Model(&models.Job{}).Where("job_type = ? AND priority = ?", job.JobType, job.Priority).Select("COALESCE(MAX(lane_position),0)").Scan(&maxPosition).Error; err != nil {
		return nil, err
	}
	next := models.Job{ID: replacement.JobID, OwnerID: job.OwnerID, CreatedByKind: job.CreatedByKind, JobType: job.JobType, Priority: job.Priority, LanePosition: maxPosition + 1000, Revision: 1, Status: models.JobStatusQueued, DisplayName: job.DisplayName, Provider: record.Type, ResourceKey: downloadQueueResourceKey(record), Generation: 1, PayloadJSON: string(rawPayload), CheckpointJSON: "{}", CreatedAt: now, UpdatedAt: now}
	checkpoint["offline_retry_task_id"], _ = json.Marshal(replacement.ID)
	rawCheckpoint, err := json.Marshal(checkpoint)
	if err != nil {
		return nil, err
	}
	cas := tx.Model(&models.Job{}).Where("id = ? AND status = ? AND revision = ?", job.ID, models.JobStatusFailed, job.Revision).Updates(map[string]any{"checkpoint_json": string(rawCheckpoint), "revision": job.Revision + 1, "updated_at": now})
	if cas.Error != nil {
		return nil, cas.Error
	}
	if cas.RowsAffected != 1 {
		return nil, appError(CodeQueueStateConflict, "原任务状态已变化，请刷新后重试", nil)
	}
	if original.FollowSubscriptionID != "" {
		if err := tx.Model(&models.DownloadTask{}).Where("id = ?", original.ID).Updates(map[string]any{"follow_subscription_id": "", "follow_resource_fingerprint": ""}).Error; err != nil {
			return nil, err
		}
	}
	if err := tx.Create(&next).Error; err != nil {
		return nil, err
	}
	if err := tx.Create(&replacement).Error; err != nil {
		return nil, err
	}
	if err := moveRejectedOfflineIntentsTx(tx, original, replacement, now); err != nil {
		return nil, err
	}
	if err := recordJobEvent(tx, next.ID, "created", "", models.JobStatusQueued, next.OwnerID, "", now); err != nil {
		return nil, err
	}
	return &next, nil
}

func moveRejectedOfflineIntentsTx(tx *gorm.DB, original, replacement models.DownloadTask, now time.Time) error {
	if original.FollowSubscriptionID != "" {
		var claims []models.FollowEpisodeClaim
		if err := tx.Where("subscription_id = ? AND resource_fingerprint = ?", original.FollowSubscriptionID, original.FollowResourceFingerprint).Find(&claims).Error; err != nil {
			return err
		}
		for _, claim := range claims {
			if claim.State == "imported" || claim.DownloadTaskID != nil && *claim.DownloadTaskID != original.ID {
				return appError(CodeQueueStateConflict, "原追更集数已由其他任务接管，请刷新后选择资源", nil)
			}
			if err := tx.Model(&claim).Updates(map[string]any{"state": "downloading", "download_task_id": replacement.ID, "updated_at": now}).Error; err != nil {
				return err
			}
		}
	}
	if err := tx.Model(&models.MediaAcquisition{}).Where("owner_id = ? AND download_task_id = ?", original.OwnerID, original.ID).Updates(map[string]any{"download_task_id": replacement.ID, "stage": "download", "status": models.DownloadTaskStatusQueued, "last_error_code": "", "revision": gorm.Expr("revision + 1"), "updated_at": now}).Error; err != nil {
		return err
	}
	var upgrades []models.MediaVersionUpgrade
	if err := tx.Where("owner_id = ? AND download_task_id = ?", original.OwnerID, original.ID).Find(&upgrades).Error; err != nil {
		return err
	}
	for _, upgrade := range upgrades {
		if upgrade.State != "submitted" && !(upgrade.State == "failed" && upgrade.ErrorCode == "media_upgrade_replacement_failed") || upgrade.TransferTaskID != "" || upgrade.RecycleReceiptID != "" {
			return appError(CodeQueueStateConflict, "洗版流程已变化，请从原洗版页面重新选择资源", nil)
		}
		snapshot, err := decodeMediaUpgradeSnapshot(upgrade.SnapshotJSON)
		if err != nil {
			return err
		}
		if err := (&MediaVersionUpgradeService{}).validateSnapshotTx(tx, upgrade, snapshot); err != nil {
			return err
		}
		var parent models.Job
		if err := tx.First(&parent, "id = ?", upgrade.JobID).Error; err != nil {
			return err
		}
		// A running observer owns a cached old download identity. Let its bounded
		// pass settle before moving the parent, rather than racing that writer.
		if parent.Status == models.JobStatusRunning || parent.InterruptStatus != "" {
			return appError(CodeQueueStateConflict, "洗版状态正在核对，请稍后重试", nil)
		}
		if parent.Status == models.JobStatusCancelled || parent.Status == models.JobStatusCompleted {
			return appError(CodeQueueStateConflict, "洗版任务已结束，请从原洗版页面重新选择资源", nil)
		}
		if err := tx.Model(&upgrade).Where("download_task_id = ? AND revision = ?", original.ID, upgrade.Revision).Updates(map[string]any{"download_task_id": replacement.ID, "state": "submitted", "stage": "download_import", "error_code": "", "next_attempt_at": nil, "finished_at": nil, "revision": gorm.Expr("revision + 1"), "updated_at": now}).Error; err != nil {
			return err
		}
		if parent.Status != models.JobStatusPaused {
			if err := tx.Model(&parent).Updates(map[string]any{"status": models.JobStatusQueued, "next_attempt_at": nil, "finished_at": nil, "last_error_code": "", "last_error_message": "", "failure_retry_count": 0, "revision": gorm.Expr("revision + 1"), "updated_at": now}).Error; err != nil {
				return err
			}
		}
	}
	return nil
}

func sameOptionalString(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return strings.TrimSpace(*a) == strings.TrimSpace(*b)
}
