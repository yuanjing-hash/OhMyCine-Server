package services

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

func offlineSiteIntakeDigest(input SiteDownloadInput) string {
	selected := append([]string(nil), input.SelectedEntryTokens...)
	sort.Strings(selected)
	raw, _ := json.Marshal(struct {
		Result, Selection, Preview, Mode, Downloader, Context string
		Entries                                               []string
		Library                                               *uint
		Profile                                               uint
		Priority                                              int
		Season, Episode                                       *int
		Preserve                                              bool
		Replacement                                           *DownloadRecognitionIdentity
		ExpectedID                                            *int64
		ExpectedType                                          string
	}{strings.TrimSpace(input.ResultToken), input.ContentSelectionToken, input.PreviewToken, input.ContentMode, strings.TrimSpace(input.DownloaderID), input.offlineIntakeContext, selected, input.MediaLibraryID, input.ProfileID, input.Priority, input.Season, input.Episode, input.PreserveExistingVersions, input.ReplacementIdentity, input.ExpectedTMDBID, strings.ToLower(strings.TrimSpace(input.ExpectedMediaType))})
	return providerReceiptHash(string(raw))
}

// This is only an accepted receipt read. It neither reopens a consumed Site
// claim nor authorizes a new task. Unaccepted challenges follow the normal full
// Site/source/selection validation and transactional common Submit path.
func (s *DownloadService) replayOfflineIntake(ctx context.Context, actor Actor, token, intakeDigest string) (DownloadTaskSummary, bool, error) {
	if len(token) != 64 {
		return DownloadTaskSummary{}, false, nil
	}
	var confirmation models.OfflineConfirmation
	err := s.db.WithContext(ctx).First(&confirmation, "token_hash = ? AND owner_id = ?", leaseHash(token), actor.User.ID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return DownloadTaskSummary{}, false, nil
	}
	if err != nil {
		return DownloadTaskSummary{}, false, err
	}
	if confirmation.AcceptedTaskID == "" {
		return DownloadTaskSummary{}, false, nil
	}
	stale := func() (DownloadTaskSummary, bool, error) {
		return DownloadTaskSummary{}, true, appError(CodeConflict, "原离线确认与当前请求或资源范围不匹配，请查看已接受的原任务", nil)
	}
	if confirmation.IntakeDigest == "" || confirmation.IntakeDigest != intakeDigest {
		return stale()
	}
	current, err := NewAuthorizationService(s.db).Resolve(actor.User.ID)
	if err != nil {
		return DownloadTaskSummary{}, true, err
	}
	if current.User.AuthzVersion != confirmation.AuthzVersion {
		return stale()
	}
	var task models.DownloadTask
	if err := s.db.WithContext(ctx).First(&task, "id = ? AND owner_id = ?", confirmation.AcceptedTaskID, actor.User.ID).Error; err != nil {
		return stale()
	}
	if task.DownloaderID == nil || !current.HasPermission(authz.PermissionDownloadsCreate) || !current.CanResource(authz.PermissionDownloadsCreate, models.AuthorizationResourceDownloader, *task.DownloaderID) || task.TargetLibraryID != nil && !current.CanIngestLibrary(uintID(*task.TargetLibraryID)) || confirmation.SourceSiteID != 0 && !current.CanResource(authz.PermissionDiscoveryRead, models.AuthorizationResourceSite, uintID(confirmation.SourceSiteID)) {
		return DownloadTaskSummary{}, true, appError(CodePermissionDenied, "当前权限不允许读取这次离线确认的原任务", nil)
	}
	var downloader models.Downloader
	if err := s.db.WithContext(ctx).First(&downloader, "id = ? AND enabled = ?", *task.DownloaderID, true).Error; err != nil {
		return stale()
	}
	if downloader.Type != task.ProviderType || downloader.StorageID == nil || task.StagingStorageID == nil || *downloader.StorageID != *task.StagingStorageID || downloader.ProviderDirectoryID != task.StagingProviderDirectoryID {
		return stale()
	}
	namespace, err := s.downloader.verifiedOfflineAccount(downloader)
	if err != nil {
		return DownloadTaskSummary{}, true, err
	}
	if namespace != task.OfflineAccountNamespace {
		return stale()
	}
	if confirmation.SourceSiteID != 0 {
		var site models.Site
		if err := s.db.Select("revision", "enabled").First(&site, confirmation.SourceSiteID).Error; err != nil || !site.Enabled || site.Revision != confirmation.SourceSiteRevision {
			return stale()
		}
	}
	var profile models.MediaClassificationProfile
	if err := s.db.Select("revision").First(&profile, task.ProfileID).Error; err != nil || profile.Revision != task.ProfileRevision {
		return stale()
	}
	if task.TargetLibraryID != nil {
		var library models.MediaLibrary
		if err := s.db.First(&library, *task.TargetLibraryID).Error; err != nil || !library.Enabled {
			return stale()
		}
		var source downloadSourceEnvelope
		plain, err := s.credentials.Decrypt(downloadSourcePurpose(task.ID), task.SourceCiphertext)
		if err != nil || json.Unmarshal([]byte(plain), &source) != nil {
			return stale()
		}
		target, _, err := s.previewDownloadTarget(ctx, downloader, library, source.Kind)
		if err != nil || target.LibraryID != *task.TargetLibraryID || task.TargetStorageID == nil || target.StorageID != *task.TargetStorageID || target.ProviderRootID != task.TargetProviderRootID || target.StorageRoot != task.TargetStorageRoot || target.RelativeRoot != task.TargetRelativeRoot || target.SourceDataSourceJSON != task.SourceDataSourceJSON || target.TargetDataSourceJSON != task.TargetDataSourceJSON || target.RouteKind != task.TransferRouteKind || target.RouteVersion != task.TransferRouteVersion {
			return stale()
		}
	}
	if normalizeExecutionLocation(task.ExecutionLocation) == models.NodeLocationRemote {
		_, revision, digest, err := nodeTaskRoutePlan(s.db, downloader, task.StagingAbsolutePath, requestedTargetID(task.TargetLibraryID))
		if err != nil || revision != task.RoutePlanRevision || digest != task.RoutePlanDigest {
			return stale()
		}
	}
	var job models.Job
	if err := s.db.First(&job, "id = ?", task.JobID).Error; err != nil {
		return stale()
	}
	return s.observedSubmission(ctx, task, job.Status, "reused", confirmation.SourceSiteID), true, nil
}
