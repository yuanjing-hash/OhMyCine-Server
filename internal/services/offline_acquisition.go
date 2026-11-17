package services

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	cloudpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/resourcecontent"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	CodeOfflineConfirmationRequired = "offline_resource_confirmation_required"
	CodeOfflineDeadlineExceeded     = "offline_deadline_exceeded"
	offlineBudget                   = 30 * time.Minute
	offlineTimeoutMessage           = "云端离线开始后 30 分钟仍未确认所需内容完成，已取消后续获取；保留已下载文件"
)

type OfflineFailureWarning struct {
	Version           int    `json:"version"`
	Reason            string `json:"reason"`
	TimeoutMinutes    int    `json:"timeout_minutes"`
	ConfirmationToken string `json:"confirmation_token"`
}

type OfflineFailureOverride struct {
	Index             int    `json:"index"`
	ConfirmationToken string `json:"confirmation_token"`
}

func offlineAccountNamespace(provider, account string) string {
	if !receiptScalar(provider, 64) || !receiptScalar(account, 512) {
		return ""
	}
	return providerReceiptHash("offline-account-v1\x00" + provider + "\x00" + account)
}

func offlineResourceDigest(source downloadSourceEnvelope) (string, error) {
	identity := resourcecontent.Identity{}
	if source.ContentSelection != nil {
		identity = source.ContentSelection.Identity
	} else if len(source.Torrent) > 0 {
		manifest, err := resourcecontent.ParseTorrent(source.Torrent, resourcecontent.Limits{})
		if err != nil {
			return "", err
		}
		identity = manifest.Identity
	} else if strings.HasPrefix(strings.ToLower(source.URL), "magnet:") {
		_, i, err := resourcecontent.NormalizeMagnet(source.URL)
		if err != nil {
			return "", err
		}
		identity = i
	}
	if identity.V1 != "" || identity.V2 != "" {
		// The existing native/resource guard uses the full v1 hash whenever it
		// is known. Adding a v2 auxiliary topic must not evade that same resource.
		if identity.V1 != "" {
			return providerReceiptHash("offline-resource-v1\x00v1:" + strings.ToLower(identity.V1)), nil
		}
		return providerReceiptHash("offline-resource-v1\x00v2:" + strings.ToLower(identity.V2)), nil
	}
	// Exact normalized HTTP input is intentionally not equivalent to a different
	// signed URL, tracker token, title or search fingerprint.
	return providerReceiptHash("offline-input-v1\x00" + source.Kind + "\x00" + source.URL), nil
}

// A whole-resource handoff may carry a separately verified media goal without
// changing its physical submission mode. Explicit selections keep every leaf;
// Follow's require subtitles are already in its exact selection.
func requiredWholeMediaPlan(manifest resourcecontent.Manifest) *downloadpkg.OfflineRequiredPlan {
	plan := downloadpkg.OfflineRequiredPlan{Version: 1, RootName: manifest.Name, MultiFile: manifest.MultiFile}
	for _, file := range manifest.Files {
		if !file.Padding && isVideoFile(file.Path) {
			plan.Files = append(plan.Files, downloadpkg.OfflineRequiredFile{RelativePath: file.Path, Size: file.Size})
		}
	}
	if _, err := plan.Digest(); err != nil {
		return nil
	}
	return &plan
}

func offlineRequiredPlan(source downloadSourceEnvelope) *downloadpkg.OfflineRequiredPlan {
	if source.ContentSelection == nil && source.OfflineRequired != nil {
		copy := *source.OfflineRequired
		copy.Files = append([]downloadpkg.OfflineRequiredFile(nil), copy.Files...)
		if _, err := copy.Digest(); err == nil {
			return &copy
		}
	}
	if source.ContentSelection == nil || len(source.Torrent) == 0 {
		return nil
	}
	manifest, err := resourcecontent.ParseTorrent(source.Torrent, resourcecontent.Limits{})
	if err != nil || source.ContentSelection.ValidateAgainst(manifest) != nil {
		return nil
	}
	plan := downloadpkg.OfflineRequiredPlan{Version: 1, RootName: manifest.Name, MultiFile: manifest.MultiFile}
	// Preserve an explicit nonmedia selection too. Actual Follow selection has
	// already narrowed videos/paired subtitles through its own policy.
	for _, f := range source.ContentSelection.Files {
		plan.Files = append(plan.Files, downloadpkg.OfflineRequiredFile{RelativePath: f.Path, Size: f.Size})
	}
	if _, err := plan.Digest(); err != nil {
		return nil
	}
	return &plan
}

func offlineSource(source downloadSourceEnvelope, task models.DownloadTask) downloadpkg.Source {
	out := downloadpkg.Source{Kind: source.Kind, URL: source.URL, Torrent: source.Torrent, Filename: source.Filename, ProviderItemID: source.ProviderItemID, ShareSelection: source.ShareSelection, ContentSelection: source.ContentSelection, AllFilesVerified: source.SelectionMode == "verified_all"}
	if task.OfflineRequiredIntentJSON != "" && task.OfflineRequiredIntentJSON != "{}" {
		var plan downloadpkg.OfflineRequiredPlan
		if json.Unmarshal([]byte(task.OfflineRequiredIntentJSON), &plan) == nil {
			if digest, err := plan.Digest(); err == nil && digest == task.OfflineRequiredDigest {
				out.OfflineRequired = &plan
			}
		}
	}
	return out
}

// freezeOfflineAcquisition uses the adapter's actual source contract. Merely
// configuring a cloud downloader does not turn shares/adoption into offline.
func (s *DownloadService) freezeOfflineAcquisition(ctx context.Context, task *models.DownloadTask, record models.Downloader, source downloadSourceEnvelope) error {
	caps, known := s.downloader.registry.Capabilities(record.Type)
	if !known || !caps.NativeOffline {
		return nil
	}
	client, err := s.downloader.clientFor(record)
	if err != nil {
		return err
	}
	op := downloadpkg.ResolveOfflineOperation(client, offlineSource(source, *task))
	if op.Kind != downloadpkg.OfflineOperationCloud {
		return nil
	}
	if op.AccountNamespace == "" || !op.KeepOutputStop {
		return appError(CodeConnectionUnavailable, "离线来源缺少已确认账号或保留文件的原任务取消能力，请检查连接与执行器版本", nil)
	}
	if err := s.downloader.requireOfflineLifecycleNode(record); err != nil {
		return err
	}

	resource, err := offlineResourceDigest(source)
	if err != nil {
		return appError(CodeDownloadSourceInvalid, "离线资源身份无效", err)
	}
	task.OfflineKind, task.OfflineAccountNamespace, task.OfflineResourceDigest = string(op.Kind), op.AccountNamespace, resource
	// Freeze the actual offline output boundary used by the source-aware adapter.
	task.StagingProviderDirectoryID = record.ProviderDirectoryID
	task.StagingStorageID = record.StorageID
	task.OfflineRequiredDigest = firstNonEmpty(source.ContentSelectionDigest, providerReceiptHash("offline-whole-resource-v1"))
	if plan := offlineRequiredPlan(source); plan != nil {
		task.OfflineRequiredDigest, _ = plan.Digest()
		raw, _ := json.Marshal(plan)
		task.OfflineRequiredIntentJSON = string(raw)
	}
	return nil
}

func offlineFailureQuery(tx *gorm.DB, task models.DownloadTask) *gorm.DB {
	return tx.Where("owner_id = ? AND account_namespace = ? AND resource_digest = ? AND required_digest = ? AND resolved_at IS NULL", task.OwnerID, task.OfflineAccountNamespace, task.OfflineResourceDigest, task.OfflineRequiredDigest)
}

func offlineRequestDigest(task models.DownloadTask, input SubmitDownloadInput, authz uint64) string {
	raw, _ := json.Marshal(struct {
		StagingStorage              *uint
		StagingDirectory, Selection string
		Owner                       uint
		Authz                       uint64
		Downloader                  *string
		Account, Resource, Required string
		Target                      *uint
		Profile                     uint
		ProfileRevision             uint64
		Site                        uint
		Route, Source, TargetSource string
		RouteRevision               uint64
		RouteDigest                 string
		Priority                    int
		Preserve                    bool
	}{task.StagingStorageID, task.StagingProviderDirectoryID, func() string {
		if input.Source.ContentSelection != nil {
			digest, _ := input.Source.ContentSelection.Digest()
			return digest
		}
		return ""
	}(), task.OwnerID, authz, task.DownloaderID, task.OfflineAccountNamespace, task.OfflineResourceDigest, task.OfflineRequiredDigest, task.TargetLibraryID, task.ProfileID, task.ProfileRevision, input.SourceSiteID, task.TransferRouteKind, task.SourceDataSourceJSON, task.TargetDataSourceJSON, task.RoutePlanRevision, task.RoutePlanDigest, input.Priority, input.PreserveExistingVersions})
	return providerReceiptHash(string(raw))
}

// prepareOfflineConfirmation is common Submit enforcement, not a UI-only hint.
// Automatic intake can never override, and history is independent of display deletion.
func (s *DownloadService) prepareOfflineConfirmation(ctx context.Context, task *models.DownloadTask, input SubmitDownloadInput) (*models.OfflineConfirmation, *DownloadTaskSummary, error) {
	if task.OfflineKind == "" {
		if input.OfflineFailureOverrideToken != "" {
			return nil, nil, appError(CodeConflict, "离线确认与当前来源不匹配，请重新提交", nil)
		}
		return nil, nil, nil
	}
	actor, err := NewAuthorizationService(s.db).Resolve(task.OwnerID)
	if err != nil {
		return nil, nil, err
	}
	digest := offlineRequestDigest(*task, input, actor.User.AuthzVersion)
	token := strings.TrimSpace(input.OfflineFailureOverrideToken)
	if token != "" {
		if input.FollowSubscriptionID != "" || len(token) != 64 {
			return nil, nil, appError(CodeConflict, "离线确认无效，请重新选择当前资源", nil)
		}
		var challenge models.OfflineConfirmation
		if err := s.db.WithContext(ctx).First(&challenge, "token_hash = ? AND owner_id = ?", leaseHash(token), task.OwnerID).Error; err != nil || challenge.RequestDigest != digest {
			return nil, nil, appError(CodeConflict, "离线确认已失效或来源已变化，请重新提交", nil)
		}
		if challenge.AcceptedTaskID != "" {
			var accepted models.DownloadTask
			var job models.Job
			if err := s.db.First(&accepted, "id = ? AND owner_id = ?", challenge.AcceptedTaskID, task.OwnerID).Error; err != nil {
				return nil, nil, appError(CodeConflict, "已接受的原任务记录不可用，不能重复提交", nil)
			}
			if err := s.db.First(&job, "id = ?", accepted.JobID).Error; err != nil {
				return nil, nil, err
			}
			result := s.observedSubmission(ctx, accepted, job.Status, "reused", input.SourceSiteID)
			return &challenge, &result, nil
		}
		if !challenge.ExpiresAt.After(s.queue.clock.Now()) {
			return nil, nil, appError(CodeConflict, "离线确认已过期，请重新提交当前资源", nil)
		}
		task.OfflineHistoryRevision = challenge.HistoryRevision
		return &challenge, nil, nil
	}
	var failure models.OfflineFailure
	if err := offlineFailureQuery(s.db.WithContext(ctx), *task).Order("id DESC").First(&failure).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil, nil
	} else if err != nil {
		return nil, nil, err
	}
	if input.FollowSubscriptionID != "" {
		return nil, nil, appError(CodeOfflineConfirmationRequired, "这个离线路线此前在 30 分钟内未完成，已跳过并尝试其他路线", nil)
	}
	token, hash, err := newLeaseToken()
	if err != nil {
		return nil, nil, err
	}
	now := s.queue.clock.Now()
	var siteRevision uint64
	if input.SourceSiteID != 0 {
		var site models.Site
		if err := s.db.Select("revision").First(&site, input.SourceSiteID).Error; err != nil {
			return nil, nil, err
		}
		siteRevision = site.Revision
	}
	challenge := models.OfflineConfirmation{IntakeDigest: input.offlineIntakeDigest, AuthzVersion: actor.User.AuthzVersion, SourceSiteID: input.SourceSiteID, SourceSiteRevision: siteRevision, TokenHash: hash, OwnerID: task.OwnerID, RequestDigest: digest, HistoryRevision: failure.ID, CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute)}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Bound abandoned challenges. Accepted identity receipts are retained.
		if err := tx.Where("accepted_task_id = '' AND expires_at <= ?", now).Delete(&models.OfflineConfirmation{}).Error; err != nil {
			return err
		}
		var count int64
		if err := tx.Model(&models.OfflineConfirmation{}).Where("owner_id = ? AND accepted_task_id = ''", task.OwnerID).Count(&count).Error; err != nil {
			return err
		}
		if count >= 64 {
			return appError(CodeConflict, "待确认请求过多，请稍后重新提交", nil)
		}
		return tx.Create(&challenge).Error
	})
	if err != nil {
		return nil, nil, err
	}
	return nil, nil, &AppError{Code: CodeOfflineConfirmationRequired, Message: "这个离线路线此前在 30 分钟内未确认完成。可继续下载这次请求，或选择其他资源", OfflineFailureWarning: &OfflineFailureWarning{Version: 1, Reason: CodeOfflineDeadlineExceeded, TimeoutMinutes: 30, ConfirmationToken: token}}
}

func consumeOfflineConfirmationTx(tx *gorm.DB, task models.DownloadTask, input SubmitDownloadInput, challenge *models.OfflineConfirmation, authz uint64, now time.Time, factory *DownloaderService) error {
	if task.OfflineKind == "" {
		return nil
	}
	if task.DownloaderID == nil {
		return errDownloadIdentityChanged
	}
	var downloader models.Downloader
	if err := tx.First(&downloader, "id = ? AND enabled = ?", *task.DownloaderID, true).Error; err != nil {
		return err
	}
	if downloader.StorageID == nil || task.StagingStorageID == nil || *downloader.StorageID != *task.StagingStorageID || downloader.ProviderDirectoryID != task.StagingProviderDirectoryID {
		return appError(CodeConflict, "离线来源范围已变化，请重新提交", nil)
	}
	currentFactory := *factory
	currentFactory.db = tx
	currentNamespace, err := currentFactory.verifiedOfflineAccount(downloader)
	if err != nil {
		return err
	}
	if currentNamespace != task.OfflineAccountNamespace {
		return appError(CodeConflict, "离线来源账号已变化，请重新提交", nil)
	}
	var latest models.OfflineFailure
	err = offlineFailureQuery(tx, task).Order("id DESC").First(&latest).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		if challenge == nil {
			return nil
		}
		return appError(CodeConflict, "离线失败记录已变化，请重新提交当前资源", nil)
	}
	if err != nil {
		return err
	}
	if challenge == nil || latest.ID != challenge.HistoryRevision || challenge.RequestDigest != offlineRequestDigest(task, input, authz) {
		return appError(CodeConflict, "离线失败记录或当前权限已变化，请重新提交当前资源", nil)
	}
	r := tx.Model(&models.OfflineConfirmation{}).Where("token_hash = ? AND owner_id = ? AND request_digest = ? AND history_revision = ? AND accepted_task_id = '' AND expires_at > ?", challenge.TokenHash, task.OwnerID, challenge.RequestDigest, latest.ID, now).Update("accepted_task_id", task.ID)
	if r.Error != nil {
		return r.Error
	}
	if r.RowsAffected != 1 {
		return appError(CodeConflict, "该离线确认已由另一请求使用，请核对原任务", nil)
	}
	return nil
}

func (w *DownloadWorker) offlineDispatchContext(ctx context.Context, claim ClaimedJob, task *models.DownloadTask) context.Context {
	if task.OfflineKind == "" {
		return ctx
	}
	ctx = cloudpkg.WithOfflineNonAcceptanceRecorder(ctx, func() error { return w.settleOfflineNonAcceptance(ctx, claim, task) })
	return cloudpkg.WithOfflineDispatchRecorder(ctx, func(at time.Time) error {
		return w.service.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if _, err := w.service.queue.verifyLease(tx, task.JobID, claim.LeaseToken); err != nil {
				return err
			}
			var current models.DownloadTask
			if err := tx.First(&current, "id = ?", task.ID).Error; err != nil {
				return err
			}
			if current.Phase == models.DownloadTaskStatusCancelled {
				return context.Canceled
			}
			if current.OfflineStartedAt != nil {
				task.OfflineStartedAt = current.OfflineStartedAt
				task.OfflineDeadlineAt = current.OfflineDeadlineAt
				return nil
			}
			if current.OfflineAccountNamespace != task.OfflineAccountNamespace || current.OfflineResourceDigest != task.OfflineResourceDigest {
				return errDownloadIdentityChanged
			}
			at = at.UTC()
			deadline := at.Add(offlineBudget)
			if err := tx.Model(&current).Updates(map[string]any{"offline_started_at": at, "offline_start_provenance": downloadpkg.OfflineStartDispatch, "offline_deadline_at": deadline, "updated_at": w.service.queue.clock.Now()}).Error; err != nil {
				return err
			}
			task.OfflineStartedAt, task.OfflineDeadlineAt, task.OfflineStartProvenance = &at, &deadline, downloadpkg.OfflineStartDispatch
			return nil
		})
	})
}

// observeOffline serializes cloud-ready and expiry against the original lease.
// Required-file readiness is valid only for the exact frozen intent and a
// complete explicit finalized-object manifest; size/existence never suffice.
func (w *DownloadWorker) observeOffline(ctx context.Context, claim ClaimedJob, task *models.DownloadTask, provider downloadpkg.Task) (bool, error) {
	return w.observeOfflineOwned(ctx, &claim, task, provider)
}

func (w *DownloadWorker) observeOfflineOwned(ctx context.Context, claim *ClaimedJob, task *models.DownloadTask, provider downloadpkg.Task) (bool, error) {
	if task.OfflineKind == "" {
		return false, nil
	}
	o := provider.Offline
	if o == nil || o.Version != 1 || o.Kind != downloadpkg.OfflineOperationCloud {
		return false, nil
	}
	if o.AccountNamespace == "" || o.AccountNamespace != task.OfflineAccountNamespace {
		return false, appError(CodeConnectionUnavailable, "离线原任务账号与当前来源不一致", nil)
	}
	if o.OperationID == "" {
		return false, nil
	}
	if o.DefiniteNonAcceptance {
		return false, w.settleOfflineNonAcceptanceOwned(ctx, claim, task)
	}
	if o.TerminalFailure {
		return false, w.settleOfflineTerminalFailureOwned(ctx, claim, task)
	}
	if o.Ready && o.CompletionMode == downloadpkg.OfflineCompletionRequiredFiles {
		if o.RequiredDigest != task.OfflineRequiredDigest {
			return false, appError(CodeDownloadSourceInvalid, "离线所需文件完成证明不完整", nil)
		}
		if normalizeExecutionLocation(task.ExecutionLocation) == models.NodeLocationRemote {
			// The authenticated Node binds this assertion to the immutable v1
			// operation/required digest before range transport. Its cloud IDs are
			// private and the exported manifest is available only after transport.
			if task.ProtocolVersion != nodeprotocol.VersionV1 || task.OfflineRequiredIntentJSON == "{}" {
				return false, appError(CodeDownloadSourceInvalid, "节点离线所需文件意图不可用", nil)
			}
		} else if o.ReadyManifest == nil || !o.ReadyManifest.Complete {
			return false, appError(CodeDownloadSourceInvalid, "离线所需文件完成证明不完整", nil)
		} else if err := validateRequiredReadyManifest(*task, *o.ReadyManifest); err != nil {
			return false, err
		}
	}
	ready := o.Ready && (o.CompletionMode == downloadpkg.OfflineCompletionAggregate || o.CompletionMode == downloadpkg.OfflineCompletionRequiredFiles)
	err := w.service.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if claim != nil {
			if _, err := w.service.queue.verifyLease(tx, task.JobID, claim.LeaseToken); err != nil {
				return err
			}
		}
		var current models.DownloadTask
		if err := tx.First(&current, "id = ? AND job_id = ?", task.ID, task.JobID).Error; err != nil {
			return err
		}
		if current.Phase == models.DownloadTaskStatusCancelled {
			return context.Canceled
		}
		if current.ProviderTaskID != task.ProviderTaskID || current.OfflineAccountNamespace != task.OfflineAccountNamespace || current.OfflineResourceDigest != task.OfflineResourceDigest || current.OfflineRequiredDigest != task.OfflineRequiredDigest {
			return errDownloadIdentityChanged
		}
		updates := map[string]any{}
		if current.OfflineStartedAt == nil && o.StartedAt != nil && (o.StartProvenance == downloadpkg.OfflineStartDispatch || o.StartProvenance == downloadpkg.OfflineStartAccepted || o.StartProvenance == downloadpkg.OfflineStartProviderAdded) {
			at := o.StartedAt.UTC()
			deadline := at.Add(offlineBudget)
			updates["offline_started_at"], updates["offline_start_provenance"], updates["offline_deadline_at"] = at, o.StartProvenance, deadline
			current.OfflineStartedAt, current.OfflineDeadlineAt, current.OfflineStartProvenance = &at, &deadline, o.StartProvenance
		}
		if ready && current.OfflineReadyAt == nil {
			at := w.service.queue.clock.Now()
			if o.ReadyAt != nil {
				at = o.ReadyAt.UTC()
			}
			updates["offline_ready_at"], updates["offline_completion_mode"] = at, o.CompletionMode
			// Subset early completion retains the original upstream stop intent.
			if o.CompletionMode == downloadpkg.OfflineCompletionRequiredFiles && !provider.Completed && !(o.UpstreamStopped && !o.StopPending) {
				updates["offline_stop_pending"] = true
				current.OfflineStopPending = true
			}
			if o.ReadyManifest != nil && normalizeExecutionLocation(current.ExecutionLocation) == models.NodeLocationServer {
				raw, err := encodeCompletedDownloadManifest(*o.ReadyManifest)
				if err != nil {
					return err
				}
				updates["completed_manifest_json"], updates["scrape_status"], updates["phase"] = raw, "completed_unverified", models.DownloadTaskStatusVerifying
				current.CompletedManifestJSON, current.ScrapeStatus = raw, "completed_unverified"
			}
			if err := releaseDownloadAdmissionTx(tx, current, at); err != nil {
				return err
			}
			if current.OfflineStopPending { // physically running extras still occupy acquisition
				if err := tx.Model(&models.DownloadAdmission{}).Where("acquisition_key = ?", downloadAcquisitionKey(current)).Updates(map[string]any{"state": "held", "released_at": nil}).Error; err != nil {
					return err
				}
			}
			if current.OfflineHistoryRevision != 0 {
				// Success settles the confirmed scope through its observed history
				// revision. Older failures must not revive; newer ones remain valid.
				if err := tx.Model(&models.OfflineFailure{}).Where("id <= ? AND owner_id = ? AND account_namespace = ? AND resource_digest = ? AND required_digest = ? AND resolved_at IS NULL", current.OfflineHistoryRevision, current.OwnerID, current.OfflineAccountNamespace, current.OfflineResourceDigest, current.OfflineRequiredDigest).Update("resolved_at", at).Error; err != nil {
					return err
				}
			}
			current.OfflineReadyAt, current.OfflineCompletionMode = &at, o.CompletionMode
		}
		// An original bound upstream stop may precede Node materialization. It
		// settles cloud occupancy without claiming the later pipeline completed.
		if current.OfflineReadyAt != nil && o.UpstreamStopped && !o.StopPending {
			updates["offline_stop_pending"] = false
			current.OfflineStopPending = false
			if err := releaseDownloadAdmissionTx(tx, current, w.service.queue.clock.Now()); err != nil {
				return err
			}
		}
		if len(updates) > 0 {
			if err := tx.Model(&current).Updates(updates).Error; err != nil {
				return err
			}
		}
		task.OfflineStartedAt, task.OfflineDeadlineAt, task.OfflineStartProvenance = current.OfflineStartedAt, current.OfflineDeadlineAt, current.OfflineStartProvenance
		task.OfflineReadyAt, task.OfflineCompletionMode, task.OfflineStopPending = current.OfflineReadyAt, current.OfflineCompletionMode, current.OfflineStopPending
		if current.CompletedManifestJSON != "" && current.CompletedManifestJSON != "{}" {
			task.CompletedManifestJSON, task.ScrapeStatus = current.CompletedManifestJSON, current.ScrapeStatus
		}
		return nil
	})
	return ready, err
}

func validateRequiredReadyManifest(task models.DownloadTask, manifest downloadpkg.Manifest) error {
	var plan downloadpkg.OfflineRequiredPlan
	if json.Unmarshal([]byte(task.OfflineRequiredIntentJSON), &plan) != nil {
		return appError(CodeDownloadSourceInvalid, "离线所需文件意图不可用", nil)
	}
	digest, err := plan.Digest()
	if err != nil || digest != task.OfflineRequiredDigest || len(plan.Files) != len(manifest.Files) {
		return appError(CodeDownloadSourceInvalid, "离线所需文件完成范围不匹配", nil)
	}
	files := map[string]int64{}
	for _, f := range manifest.Files {
		if f.ProviderItemID == "" || f.Size < 0 {
			return appError(CodeDownloadSourceInvalid, "离线文件完成身份不完整", nil)
		}
		files[f.RelativePath] = f.Size
	}
	for _, f := range plan.Files {
		if size, ok := files[f.RelativePath]; !ok || size != f.Size {
			return appError(CodeDownloadSourceInvalid, "离线所需文件完成范围不匹配", nil)
		}
	}
	return nil
}

// ExpireOfflineDeadlines runs independently of download execution capacity. A
// bounded fresh original-operation read precedes its DB-only terminal decision.
func (w *DownloadWorker) ExpireOfflineDeadlines(ctx context.Context) error {
	now := w.service.queue.clock.Now()
	var due []models.DownloadTask
	if err := w.service.db.WithContext(ctx).Where("offline_kind <> '' AND offline_deadline_at <= ? AND offline_ready_at IS NULL AND offline_settled_at IS NULL AND phase <> ? AND EXISTS (SELECT 1 FROM download_admissions a WHERE a.acquisition_key = 'task:' || download_tasks.id AND a.state = 'held')", now, models.DownloadTaskStatusCancelled).Order("offline_deadline_at,id").Limit(128).Find(&due).Error; err != nil {
		return err
	}
	var wg sync.WaitGroup
	permits := make(chan struct{}, 8)
	errorsOut := make(chan error, len(due))
	for _, task := range due {
		select {
		case permits <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return ctx.Err()
		}
		wg.Add(1)
		go func(task models.DownloadTask) {
			defer wg.Done()
			defer func() { <-permits }()
			{
				read, cancel := context.WithTimeout(ctx, 3*time.Second)
				provider, err := w.offlineDeadlineObservation(read, &task)
				if err == nil {
					_, err = w.observeOfflineOwned(read, nil, &task, provider)
				} else if downloadpkg.DefiniteSubmissionNonAcceptance(err) {
					err = w.settleOfflineNonAcceptanceOwned(read, nil, &task)
				} else if downloadpkg.OfflineTerminalFailure(err) {
					err = w.settleOfflineTerminalFailureOwned(read, nil, &task)
				}
				cancel()
				if task.OfflineReadyAt != nil || task.OfflineSettledAt != nil {
					return
				}
				if errors.Is(err, context.Canceled) && ctx.Err() != nil {
					return
				}
			}
			_, err := w.service.expireOfflineTask(ctx, task, nil)
			if err != nil {
				errorsOut <- err
			}
		}(task)
	}
	wg.Wait()
	close(errorsOut)
	for err := range errorsOut {
		if err != nil {
			return err
		}
	}
	return nil
}

type readOnlyDownloadObservationKey struct{}

// Reconcile a lost original ACK before deciding expiry. This contract is
// read-only: it never invokes Submit, reapplies a wanted set or materializes.
func (w *DownloadWorker) offlineDeadlineObservation(ctx context.Context, task *models.DownloadTask) (downloadpkg.Task, error) {
	ctx = context.WithValue(ctx, readOnlyDownloadObservationKey{}, true)
	client, err := w.service.downloader.clientForDownloadTask(ctx, *task)
	if err != nil {
		return downloadpkg.Task{}, err
	}
	if err := downloadpkg.BindOfflineRequired(client, offlineSource(downloadSourceEnvelope{}, *task).OfflineRequired); err != nil {
		return downloadpkg.Task{}, err
	}
	if task.ProviderTaskID != "" {
		return downloadpkg.ObserveOffline(ctx, client, task.ProviderTaskID)
	}
	plain, err := w.service.credentials.Decrypt(downloadSourcePurpose(task.ID), task.SourceCiphertext)
	if err != nil {
		return downloadpkg.Task{}, err
	}
	var source downloadSourceEnvelope
	if json.Unmarshal([]byte(plain), &source) != nil {
		return downloadpkg.Task{}, appError(CodeDownloadSourceInvalid, "原离线来源记录无效", nil)
	}
	if err := validateStoredContentSelection(source); err != nil {
		return downloadpkg.Task{}, err
	}
	resolver, ok := client.(downloadpkg.SubmissionReconciler)
	if !ok {
		return downloadpkg.Task{}, downloadpkg.Error("downloader_submission_unconfirmed", true, nil)
	}
	provider, err := resolver.ReconcileSubmission(ctx, downloadpkg.SubmitRequest{Source: offlineSource(source, *task), SavePath: task.StagingAbsolutePath, Tag: task.ProviderTag, ProviderDirectoryID: task.StagingProviderDirectoryID})
	if err != nil {
		return provider, err
	}
	if strings.TrimSpace(provider.ID) == "" {
		return provider, downloadpkg.Error("downloader_submission_unconfirmed", true, nil)
	}
	err = w.service.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current models.DownloadTask
		if err := tx.First(&current, "id = ? AND job_id = ?", task.ID, task.JobID).Error; err != nil {
			return err
		}
		if current.Phase == models.DownloadTaskStatusCancelled {
			return context.Canceled
		}
		if current.SourceCiphertext != task.SourceCiphertext || current.OfflineAccountNamespace != task.OfflineAccountNamespace || current.OfflineResourceDigest != task.OfflineResourceDigest || current.OfflineRequiredDigest != task.OfflineRequiredDigest || current.ProviderTaskID != "" && current.ProviderTaskID != provider.ID {
			return errDownloadIdentityChanged
		}
		return tx.Model(&current).Updates(map[string]any{"provider_task_id": provider.ID, "provider_status": safeLabel(provider.Status, 64), "updated_at": w.service.queue.clock.Now()}).Error
	})
	if err == nil {
		task.ProviderTaskID = provider.ID
	}
	return provider, err
}

func (s *DownloadService) expireOfflineTask(ctx context.Context, original models.DownloadTask, claim *ClaimedJob) (bool, error) {
	now := s.queue.clock.Now()
	expired := false
	var changed models.Job
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var task models.DownloadTask
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&task, "id = ? AND job_id = ?", original.ID, original.JobID).Error; err != nil {
			return err
		}
		if claim != nil {
			if _, err := s.queue.verifyLease(tx, task.JobID, claim.LeaseToken); err != nil {
				return err
			}
		}
		if task.Phase == models.DownloadTaskStatusCancelled || task.OfflineKind == "" || task.OfflineReadyAt != nil || task.OfflineSettledAt != nil || task.OfflineStartedAt == nil || task.OfflineDeadlineAt == nil || task.OfflineDeadlineAt.After(now) {
			return nil
		}
		var held int64
		if err := tx.Model(&models.DownloadAdmission{}).Where("acquisition_key = ? AND state = 'held'", downloadAcquisitionKey(task)).Count(&held).Error; err != nil {
			return err
		}
		if held == 0 {
			return nil
		} // authoritative completion/stop already settled
		if task.OfflineAccountNamespace != original.OfflineAccountNamespace || task.OfflineResourceDigest != original.OfflineResourceDigest || task.OfflineRequiredDigest != original.OfflineRequiredDigest || task.SourceCiphertext != original.SourceCiphertext || task.ProviderTaskID != original.ProviderTaskID {
			return errDownloadIdentityChanged
		}
		var job models.Job
		if err := tx.First(&job, "id = ?", task.JobID).Error; err != nil {
			return err
		}
		if err := savePendingDownloadControlTx(tx, &task, &job, false, task.OwnerID, now); err != nil {
			return err
		}
		if err := tx.Model(&task).Updates(map[string]any{"last_error_code": CodeOfflineDeadlineExceeded, "last_error_message": offlineTimeoutMessage, "offline_stop_pending": true}).Error; err != nil {
			return err
		}
		if job.Status == models.JobStatusRunning {
			if err := tx.Model(&job).Updates(map[string]any{"interrupt_status": models.JobStatusCancelled, "cancellation_asked": true}).Error; err != nil {
				return err
			}
		}
		if receiptScalar(task.OfflineAccountNamespace, 128) && receiptScalar(task.OfflineResourceDigest, 64) && receiptScalar(task.OfflineRequiredDigest, 64) {
			failure := models.OfflineFailure{TaskID: task.ID, OwnerID: task.OwnerID, AccountNamespace: task.OfflineAccountNamespace, ResourceDigest: task.OfflineResourceDigest, RequiredDigest: task.OfflineRequiredDigest, Reason: CodeOfflineDeadlineExceeded, StartedAt: *task.OfflineStartedAt, DeadlineAt: *task.OfflineDeadlineAt, CreatedAt: now}
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&failure).Error; err != nil {
				return err
			}
		}
		if err := syncFailedFollowClaims(tx, task.ID, now); err != nil {
			return err
		}
		expired, changed = true, job
		return nil
	})
	if err == nil && expired {
		s.queue.publish(changed, "job.status_changed")
		s.queue.wake()
		if changed.Status == models.JobStatusRunning && s.queue.interrupt != nil {
			s.queue.interrupt(changed.ID, "cancel_pipeline")
		}
	}
	return expired, err
}

func offlineClockWait(task models.DownloadTask) WorkerResult {
	next := time.Now().UTC().Add(time.Minute)
	return WorkerResult{RetryAt: &next, ErrorCode: "offline_clock_unknown", ErrorMessage: "原云端操作缺少可验证的开始时间，保留原任务等待原始回执；不会重新提交"}
}

func (w *DownloadWorker) observationResult(task models.DownloadTask, interval time.Duration) WorkerResult {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	next := w.service.queue.clock.Now().Add(interval)
	if task.OfflineReadyAt == nil && task.OfflineDeadlineAt != nil && task.OfflineDeadlineAt.Before(next) {
		next = *task.OfflineDeadlineAt
	}
	return WorkerResult{RetryAt: &next}
}

func (w *DownloadWorker) settleOfflineNonAcceptance(ctx context.Context, claim ClaimedJob, task *models.DownloadTask) error {
	return w.settleOfflineNonAcceptanceOwned(ctx, &claim, task)
}

// Only a typed adapter proof after its original durable no-task receipt may
// call this boundary. An authentication/quota/error code alone is insufficient.
func (w *DownloadWorker) settleOfflineNonAcceptanceOwned(ctx context.Context, claim *ClaimedJob, task *models.DownloadTask) error {
	return w.settleOfflineTerminalOwned(ctx, claim, task, true)
}
func (w *DownloadWorker) settleOfflineTerminalFailureOwned(ctx context.Context, claim *ClaimedJob, task *models.DownloadTask) error {
	return w.settleOfflineTerminalOwned(ctx, claim, task, false)
}
func (w *DownloadWorker) settleOfflineTerminalOwned(ctx context.Context, claim *ClaimedJob, task *models.DownloadTask, noTask bool) error {
	now := w.service.queue.clock.Now()
	code, message := "downloader_provider_failed", "原云端任务已明确结束且失败，请重新选择资源后提交"
	state := "terminal_failure"
	if noTask {
		code, message = "offline_submission_not_accepted", "原云端提交已明确拒绝且未创建任务，修正原因后可重试下载"
		state = "not_accepted"
	}
	err := w.service.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if claim != nil {
			if _, err := w.service.queue.verifyLease(tx, task.JobID, claim.LeaseToken); err != nil {
				return err
			}
		}
		var current models.DownloadTask
		if err := tx.First(&current, "id = ? AND job_id = ?", task.ID, task.JobID).Error; err != nil {
			return err
		}
		if current.Phase == models.DownloadTaskStatusCancelled || current.OfflineReadyAt != nil {
			return context.Canceled
		}
		if current.ProviderTaskID != task.ProviderTaskID || current.OfflineAccountNamespace != task.OfflineAccountNamespace || current.OfflineResourceDigest != task.OfflineResourceDigest {
			return errDownloadIdentityChanged
		}
		if current.OfflineSettledAt == nil {
			if err := tx.Model(&current).Updates(map[string]any{"offline_settled_at": now, "offline_clock_state": state, "phase": models.DownloadTaskStatusFailed, "last_error_code": code, "last_error_message": message, "finished_at": now, "updated_at": now}).Error; err != nil {
				return err
			}
		}
		current.OfflineStopPending = false
		return releaseDownloadAdmissionTx(tx, current, now)
	})
	if err == nil {
		task.OfflineSettledAt = &now
	}
	return err
}

func (w *DownloadWorker) recoverOfflinePolicy(ctx context.Context, claim ClaimedJob, task *models.DownloadTask, record models.Downloader, client downloadpkg.Client, source downloadpkg.Source) error {
	op := downloadpkg.ResolveOfflineOperation(client, source)
	if task.OfflineKind != "" {
		if op.Kind != downloadpkg.OfflineOperationCloud || op.AccountNamespace != "" && op.AccountNamespace != task.OfflineAccountNamespace {
			return appError(CodeConnectionUnavailable, "原离线来源或账号范围已变化", nil)
		}
		return nil
	}
	if op.Kind != downloadpkg.OfflineOperationCloud {
		return nil
	}
	if normalizeExecutionLocation(task.ExecutionLocation) == models.NodeLocationRemote {
		var count int64
		if err := w.service.db.Model(&models.RemoteOperation{}).Where("task_id = ?", task.ID).Count(&count).Error; err != nil {
			return err
		}
		if count > 0 {
			// Accepted v0 keeps its exact plan/grant and natural read/data flow.
			// It cannot gain a guessed clock or a new upstream-stop permission.
			if task.OfflineClockState == "" {
				if err := w.service.db.Transaction(func(tx *gorm.DB) error {
					if _, err := w.service.queue.verifyLease(tx, task.JobID, claim.LeaseToken); err != nil {
						return err
					}
					return tx.Model(&models.DownloadTask{}).Where("id = ? AND offline_kind = '' AND phase <> ?", task.ID, models.DownloadTaskStatusCancelled).Update("offline_clock_state", "legacy_unavailable").Error
				}); err != nil {
					return err
				}
				task.OfflineClockState = "legacy_unavailable"
			}
			return nil
		}
	}
	legacy := *task
	selectionDigest := ""
	if source.ContentSelection != nil {
		selectionDigest, _ = source.ContentSelection.Digest()
	}
	envelope := downloadSourceEnvelope{Kind: source.Kind, URL: source.URL, Torrent: source.Torrent, Filename: source.Filename, ContentSelection: source.ContentSelection, ContentSelectionDigest: selectionDigest}
	if err := w.service.freezeOfflineAcquisition(ctx, &legacy, record, envelope); err != nil {
		return err
	}
	err := w.service.db.Transaction(func(tx *gorm.DB) error {
		if _, err := w.service.queue.verifyLease(tx, task.JobID, claim.LeaseToken); err != nil {
			return err
		}
		r := tx.Model(&models.DownloadTask{}).Where("id = ? AND phase <> ? AND offline_kind = ''", task.ID, models.DownloadTaskStatusCancelled).Updates(map[string]any{"offline_kind": legacy.OfflineKind, "offline_account_namespace": legacy.OfflineAccountNamespace, "offline_resource_digest": legacy.OfflineResourceDigest, "offline_required_digest": legacy.OfflineRequiredDigest, "offline_required_intent_json": legacy.OfflineRequiredIntentJSON})
		if r.Error != nil {
			return r.Error
		}
		if r.RowsAffected != 1 {
			return errDownloadIdentityChanged
		}
		return nil
	})
	if err == nil {
		*task = legacy
	}
	return err
}

func (w *DownloadWorker) stopOfflineExtras(ctx context.Context, task *models.DownloadTask, client downloadpkg.Client) error {
	if !task.OfflineStopPending || task.OfflineReadyAt == nil || task.Phase == models.DownloadTaskStatusCancelled {
		return nil
	}
	if err := downloadpkg.StopOfflineKeepOutput(ctx, client, task.ProviderTaskID); err != nil {
		return err
	}
	err := w.service.db.Transaction(func(tx *gorm.DB) error {
		var current models.DownloadTask
		if err := tx.First(&current, "id = ? AND job_id = ?", task.ID, task.JobID).Error; err != nil {
			return err
		}
		if current.ProviderTaskID != task.ProviderTaskID || current.OfflineAccountNamespace != task.OfflineAccountNamespace || current.OfflineReadyAt == nil {
			return errDownloadIdentityChanged
		}
		if err := tx.Model(&current).Update("offline_stop_pending", false).Error; err != nil {
			return err
		}
		current.OfflineStopPending = false
		return releaseDownloadAdmissionTx(tx, current, w.service.queue.clock.Now())
	})
	if err == nil {
		task.OfflineStopPending = false
	}
	return err
}

// Goal-complete upstream stop receipts survive the display/Job's completion.
// This does not resume or retry acquisition and never cleans provider output.
func (w *DownloadWorker) RecoverOfflineStops(ctx context.Context) error {
	if err := w.reconcileDownloadAdmissions(ctx); err != nil {
		return err
	}
	var tasks []models.DownloadTask
	if err := w.service.db.WithContext(ctx).Where("offline_stop_pending = ? AND offline_ready_at IS NOT NULL AND phase <> ?", true, models.DownloadTaskStatusCancelled).Order("updated_at,id").Limit(4).Find(&tasks).Error; err != nil {
		return err
	}
	for i := range tasks {
		task := &tasks[i]
		if ctx.Err() != nil {
			return ctx.Err()
		}
		call, cancel := context.WithTimeout(ctx, 3*time.Second)
		call = context.WithValue(call, readOnlyDownloadObservationKey{}, true)
		client, err := w.service.downloader.clientForDownloadTask(call, *task)
		if err == nil {
			err = w.stopOfflineExtras(call, task, client)
		}
		cancel()
		if err != nil {
			_ = w.service.db.Model(&models.DownloadTask{}).Where("id = ? AND offline_stop_pending = ?", task.ID, true).Update("updated_at", w.service.queue.clock.Now()).Error
		}
	}
	return nil
}

func downloadAcquisitionState(task models.DownloadTask, jobStatus string) string {
	if task.Phase == models.DownloadTaskStatusCancelled {
		if task.OfflineStopPending {
			return "cancelled_stop_pending"
		}
		return ""
	}
	if task.Phase == models.DownloadTaskStatusFailed || task.Phase == models.DownloadTaskStatusPaused {
		return ""
	}
	if task.OfflineReadyAt != nil {
		return "cloud_ready"
	}
	if task.OfflineClockState == "legacy_unavailable" {
		return "clock_unknown"
	}
	if task.OfflineKind != "" && task.ProviderTaskID != "" && task.OfflineStartedAt == nil {
		if offlineDispatchWaiting(task.ProviderStatus) {
			return "waiting_capacity"
		}
		return "clock_unknown"
	}
	if task.ProviderStatus == "unavailable" {
		return "reconnecting"
	}
	if task.Phase == models.DownloadTaskStatusCompleted || task.Phase == models.DownloadTaskStatusFailed {
		return ""
	}
	if task.ProviderTaskID != "" && (jobStatus == models.JobStatusRetryWait || jobStatus == models.JobStatusQueued) {
		return "observing"
	}
	if task.ProviderTaskID != "" || task.Phase == models.DownloadTaskStatusSubmitting {
		return "active"
	}
	return ""
}

// These finite adapter states assert that native dispatch has not begun. A
// current source-aware v1 observation may wait here without inventing a clock.
func offlineDispatchWaiting(status string) bool {
	switch status {
	case "accepted", "queued", "waiting_capacity":
		return true
	}
	return false
}
