package services

import (
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func taskLibraryVisible(actor Actor, id uint) bool {
	return id == 0 || actor.CanResource(authz.PermissionMediaLibrariesRead, models.AuthorizationResourceMediaLibrary, uintID(id))
}

func taskDownloaderVisible(actor Actor, id *string) bool {
	if id == nil {
		return true
	}
	if !actor.ResourceAccessAllows(models.ResourceAccessScopeDownloaderUse, *id) {
		return false
	}
	for _, rule := range actor.ResourceRules {
		if rule.ResourceType == models.AuthorizationResourceDownloader && rule.ResourceID == *id && rule.Effect == models.AuthorizationEffectDeny && (rule.PermissionCode == authz.PermissionDownloadersRead || rule.PermissionCode == authz.PermissionDownloadsCreate || rule.PermissionCode == authz.PermissionDownloadsReadOwn || rule.PermissionCode == authz.PermissionDownloadsReadAll) {
			return false
		}
	}
	return true
}

func explicitResourceDeny(actor Actor, permission, resourceType, id string) bool {
	if _, denied := actor.DeniedPermissions[permission]; denied {
		return true
	}
	for _, rule := range actor.ResourceRules {
		if rule.PermissionCode == permission && rule.ResourceType == resourceType && rule.ResourceID == id && rule.Effect == models.AuthorizationEffectDeny {
			return true
		}
	}
	return false
}

// Keep the accepted task's non-sensitive lifecycle available after revocation.
// Its source, target, recognition and route are no longer available to browse.
func downloadSummaryForActor(actor Actor, record models.DownloadTask, item DownloadTaskSummary) DownloadTaskSummary {
	if taskLibraryVisible(actor, requestedTargetID(record.TargetLibraryID)) && taskDownloaderVisible(actor, record.DownloaderID) {
		return item
	}
	return safeDownloadTaskSummary(item)
}

func safeDownloadTaskSummary(item DownloadTaskSummary) DownloadTaskSummary {
	return DownloadTaskSummary{
		ID: item.ID, JobID: item.JobID, OwnerID: item.OwnerID, DisplayName: "资源权限受限的下载任务",
		JobStatus: item.JobStatus, ProviderStatus: item.ProviderStatus, Phase: item.Phase,
		Progress: item.Progress, BytesCompleted: item.BytesCompleted, BytesTotal: item.BytesTotal,
		DownloadSpeed: item.DownloadSpeed, UploadSpeed: item.UploadSpeed, ETASeconds: item.ETASeconds,
		LastSampledAt: item.LastSampledAt, LastErrorCode: item.LastErrorCode,
		CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt, FinishedAt: item.FinishedAt,
		TransferPhase: item.TransferPhase, TransferTaskID: item.TransferTaskID,
		TransferJobID: item.TransferJobID, TransferJobStatus: item.TransferJobStatus,
		SeedingTaskID: item.SeedingTaskID, SeedingJobID: item.SeedingJobID,
		SeedingJobStatus: item.SeedingJobStatus, SeedingPhase: item.SeedingPhase, LifecycleScope: item.LifecycleScope,
	}
}

func safeTransferSummary(item TransferSummary) TransferSummary {
	return TransferSummary{ID: item.ID, OwnerID: item.OwnerID, DownloadTaskID: item.DownloadTaskID, JobID: item.JobID,
		DisplayName: "资源权限受限的整理任务", Phase: item.Phase, JobStatus: item.JobStatus, RetryAt: item.RetryAt,
		ProcessedFiles: item.ProcessedFiles, TotalFiles: item.TotalFiles, LastErrorCode: item.LastErrorCode,
		CleanupStatus: item.CleanupStatus, CleanupRemoved: item.CleanupRemoved, CleanupErrorCode: item.CleanupErrorCode,
		CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt, FinishedAt: item.FinishedAt}
}

func safeSeedingSummary(item SeedingTaskSummary) SeedingTaskSummary {
	return SeedingTaskSummary{ID: item.ID, JobID: item.JobID, JobStatus: item.JobStatus, DownloadTaskID: item.DownloadTaskID, OwnerID: item.OwnerID,
		DisplayName: "资源权限受限的做种任务", Phase: item.Phase, Ratio: item.Ratio, SeededSeconds: item.SeededSeconds, UploadedBytes: item.UploadedBytes,
		LastSampledAt: item.LastSampledAt, LastErrorCode: item.LastErrorCode, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt, FinishedAt: item.FinishedAt}
}

func followSummaryForActor(actor Actor, record models.FollowSubscription) (FollowSummary, error) {
	item, err := followSummary(record)
	if err != nil {
		return item, err
	}
	allowed := make([]uint, 0, len(item.Snapshot.SiteIDs))
	for _, id := range item.Snapshot.SiteIDs {
		if actor.CanResource(authz.PermissionDiscoveryRead, models.AuthorizationResourceSite, uintID(id)) {
			allowed = append(allowed, id)
		}
	}
	if len(allowed) != len(item.Snapshot.SiteIDs) {
		item.LastErrorMessage = ""
	}
	item.Snapshot.SiteIDs = allowed
	if !taskLibraryVisible(actor, item.Snapshot.MediaLibraryID) {
		item.Snapshot.MediaLibraryID = 0
		item.Title, item.PosterRef = "资源权限受限的订阅", ""
		item.LastErrorMessage = ""
	}
	return item, nil
}
