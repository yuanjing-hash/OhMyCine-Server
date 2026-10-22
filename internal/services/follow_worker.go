package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	serverlog "github.com/yuanjing-hash/OhMyCine-Server/internal/logging"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/mediarecognition"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type FollowEpisodeCoordinate struct {
	Season  int `json:"season"`
	Episode int `json:"episode"`
}

type followCandidate struct {
	SiteID                uint
	SitePriority          int
	Item                  SiteSearchResult
	Season                int
	Episodes              []int
	CompleteSeason        bool
	EpisodeBoundsDeclared bool
	Fingerprint           string
	QualityRank           int
}

type FollowSearchWorker struct {
	follows *FollowService
	sites   *SiteService
}

var followEpisodeRangePattern = regexp.MustCompile(`(?i)S(\d+)E(\d+)(?:[-~～‐‑‒–—](?:S(\d+))?E?|E)(\d+)`)
var followEpisodeMarkerPattern = regexp.MustCompile(`(?i)(?:S\s*\d+\s*[ ._-]*)?E(?:P(?:ISODES?)?)?\s*\d+|\d+x\d+|第\s*[0-9零〇一二两兩三四五六七八九十百千]{1,12}\s*[集话話]|S\s*\d+`)
var followEpisodeContinuationPattern = regexp.MustCompile(`(?i)^\s*[-~～‐‑‒–—]\s*(?:S\d+\s*)?E?\s*\d+(?:[^[:alnum:]]|$)`)
var followCompleteSeasonPattern = regexp.MustCompile(`(?i)(?:complete(?:[ ._-]*(?:season|pack))?|season[ ._-]*pack|全集|全季)`)

func NewFollowSearchWorker(follows *FollowService, sites *SiteService) *FollowSearchWorker {
	return &FollowSearchWorker{follows: follows, sites: sites}
}

func (w *FollowSearchWorker) Run(ctx context.Context, runtime JobRuntime, job ClaimedJob) (result WorkerResult) {
	var payload followJobPayload
	if err := json.Unmarshal([]byte(job.Job.PayloadJSON), &payload); err != nil || payload.RunID == "" || payload.SubscriptionID == "" {
		return WorkerResult{ErrorCode: CodeInvalidRequest, ErrorMessage: "追更任务参数无效"}
	}
	var run models.FollowRun
	if err := w.follows.db.First(&run, "id = ? AND job_id = ?", payload.RunID, job.Job.ID).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			// Deleting a subscription cascades its run records but intentionally
			// leaves unrelated queue history intact. A later claim of that stale
			// Job is a safe no-op, not a retryable failure.
			return WorkerResult{}
		}
		return WorkerResult{ErrorCode: CodeFollowNotFound, ErrorMessage: "追更运行记录不存在"}
	}
	start := w.follows.now()
	started := time.Now()
	stats := &followBusinessStats{sites: map[uint]bool{}}
	ctx = context.WithValue(ctx, followBusinessStatsKey{}, stats)
	ctx = serverlog.WithBusinessFields(ctx, map[string]any{"job_id": job.Job.ID, "user_id": run.OwnerID, "follow_id": run.SubscriptionID, "follow_run_id": run.ID})
	defer func() { w.recordRunEnd(ctx, runtime, run, started, stats, result) }()
	w.record(ctx, "follow.start", "start", "started", "开始本轮追更，先检查当前权限、目标库与已播季集覆盖状态", map[string]any{"trigger": run.Trigger})
	if err := w.follows.db.Model(&run).Updates(map[string]any{"status": models.FollowRunRunning, "started_at": start, "updated_at": start}).Error; err != nil {
		return followPersistFailure()
	}
	w.follows.publish("follow.running", run.OwnerID, run.JobID, models.FollowRunRunning)
	var subscription models.FollowSubscription
	if err := w.follows.db.First(&subscription, "id = ?", payload.SubscriptionID).Error; err != nil {
		return w.stopRun(run, models.FollowRunStale, "follow_deleted", "订阅已删除")
	}
	if !followRunExecutable(subscription, run.LifecycleRevision) {
		return w.stopRun(run, models.FollowRunCancelled, "follow_paused", "订阅已暂停")
	}
	actor, err := w.follows.authorization.Resolve(run.OwnerID)
	if err != nil {
		return w.block(run, subscription, "follow_owner_unavailable", "订阅用户或权限不可用")
	}
	var snapshot FollowExecutionSnapshot
	if err := json.Unmarshal([]byte(run.ExecutionSnapshotJSON), &snapshot); err != nil {
		return w.block(run, subscription, CodeFollowConfigurationInvalid, "订阅执行快照无效")
	}
	if snapshot, _, err = w.follows.validateSnapshotWithRoutes(actor, subscription.TMDBID, snapshot, false); err != nil {
		return w.block(run, subscription, ErrorCode(err), ErrorMessage(err))
	}
	ctx = serverlog.WithBusinessFields(ctx, map[string]any{"library_id": snapshot.MediaLibraryID})
	w.record(ctx, "follow.plan", "coverage", "planned", fmt.Sprintf("追更作品：%s；配置 %d 个站点、%d 个季，单轮最多选择 %d 个资源；先核对缺集后再查询站点", serverlog.BusinessDisplayName(subscription.Title), len(snapshot.SiteIDs), len(snapshot.Seasons), snapshot.MaxResourcesPerRun), map[string]any{"title": subscription.Title, "planned_sites_count": len(snapshot.SiteIDs)})
	coverage, err := w.follows.coverage.Coverage(ctx, actor, "tv", subscription.TMDBID)
	if err != nil {
		return w.block(run, subscription, ErrorCode(err), ErrorMessage(err))
	}
	current, err := w.currentSubscription(subscription.ID)
	if err != nil || !followRunExecutable(current, run.LifecycleRevision) {
		return w.stopRun(run, models.FollowRunCancelled, "follow_inactive", "订阅已暂停或删除")
	}
	if coverage.TV == nil {
		return w.waitForCoverage(run, subscription, snapshot, "媒体库覆盖率暂不可用，将按计划重新检查", 0)
	}
	selectedSeasons := map[int]struct{}{}
	for _, season := range snapshot.Seasons {
		selectedSeasons[season] = struct{}{}
	}
	missing := map[[2]int]struct{}{}
	seenSeasons := map[int]MediaCoverageSeason{}
	target, present, unknown := 0, 0, 0
	for _, season := range coverage.TV.Seasons {
		if _, ok := selectedSeasons[season.SeasonNumber]; !ok {
			continue
		}
		seenSeasons[season.SeasonNumber] = season
		for _, episode := range season.Episodes {
			switch episode.Status {
			case "present":
				target++
				present++
				if err := w.upsertImported(subscription.ID, season.SeasonNumber, episode.EpisodeNumber); err != nil {
					return followPersistFailure()
				}
			case "missing":
				target++
				missing[[2]int{season.SeasonNumber, episode.EpisodeNumber}] = struct{}{}
			case "unknown":
				unknown++
			}
		}
		unknown += season.Counts.Unknown - countEpisodeStatus(season.Episodes, "unknown")
	}
	if len(seenSeasons) != len(selectedSeasons) {
		return w.waitForCoverage(run, subscription, snapshot, "所选季的 TMDB 信息暂不可用，将按计划重新检查", unknown)
	}
	missingBeforeClaims := len(missing)
	missing, err = w.excludeClaimedDownloads(ctx, subscription.ID, missing)
	if err != nil {
		return followPersistFailure()
	}
	missingCoordinates := coordinateList(missing)
	w.record(ctx, "follow.coverage", "coverage", "confirmed", fmt.Sprintf("覆盖检查：已播目标 %d 集，在库 %d 集，已有下载占用 %d 集，明确待补 %d 集，未知 %d 集；本轮待补：%s", target, present, missingBeforeClaims-len(missing), len(missing), unknown, followEpisodeDisplay(missingCoordinates)), map[string]any{"total_count": target, "present_count": present, "active_count": missingBeforeClaims - len(missing), "missing_count": len(missing), "uncertain_count": unknown})
	if len(missingCoordinates) > 0 {
		w.follows.publish("follow.missing_found", run.OwnerID, run.JobID, models.FollowRunRunning)
	}
	missingRaw, _ := json.Marshal(missingCoordinates)
	now := w.follows.now()
	if err := w.follows.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&models.FollowRun{}).Where("id = ?", run.ID).Updates(map[string]any{"missing_snapshot_json": string(missingRaw), "updated_at": now}).Error; err != nil {
			return err
		}
		return tx.Model(&models.FollowSubscription{}).
			Where("id = ? AND revision = ? AND lifecycle_revision = ? AND status <> ?", subscription.ID, run.SubscriptionRevision, run.LifecycleRevision, models.FollowStatusPaused).
			Updates(map[string]any{"progress_target": target, "progress_present": present, "progress_missing": target - present, "last_run_id": run.ID, "last_run_at": now, "updated_at": now}).Error
	}); err != nil {
		return followPersistFailure()
	}
	if len(missing) == 0 {
		if target > present {
			return w.finish(run, subscription, models.FollowRunSubmitted, models.FollowStatusActive, "", "", snapshot, 0, 0, map[string]int{"active_claims": target - present, "coverage_unknown": unknown})
		}
		if unknown > 0 {
			message := "所选季仍有播出信息不完整的剧集，当前没有可独立搜索的明确缺集；等待剧集信息更新，将按计划重新检查"
			if coverage.Freshness.LibraryScanState != "complete" {
				message = "媒体库扫描尚不完整，无法确认未在库剧集是否缺失；等待扫描完成，将按计划重新检查"
			}
			return w.waitForCoverage(run, subscription, snapshot, message, unknown)
		}
		return w.finish(run, subscription, models.FollowRunCompleted, models.FollowStatusCompleted, "", "", snapshot, 0, 0, map[string]int{})
	}
	if unknown > 0 {
		w.record(ctx, "follow.coverage_uncertain", "coverage", "skipped", fmt.Sprintf("另有 %d 集的播出或在库状态尚未确认，本轮跳过这些剧集，只搜索 %d 集明确缺集", unknown, len(missing)), map[string]any{"uncertain_count": unknown, "missing_count": len(missing)})
	}
	progress := float64(20)
	processed, total := int64(1), int64(4)
	_ = runtime.Heartbeat(&progress, &processed, &total, nil, nil)
	_ = runtime.Checkpoint(map[string]any{"stage": "search", "missing": missingCoordinates})
	searchCtx, stopSearch := context.WithTimeoutCause(context.WithValue(ctx, followSearchBudgetKey{}, true), w.sites.searchPolicyValue().Follow, errors.New(CodeSearchBudget))
	candidates, queryNames, filterSummary := w.searchCandidates(searchCtx, actor, subscription, snapshot, missing, seenSeasons)
	if unknown > 0 {
		filterSummary["coverage_unknown"] = unknown
	}
	if searchCtx.Err() != nil && ctx.Err() == nil {
		filterSummary["search_incomplete"]++
	}
	stopSearch()
	if ctx.Err() != nil {
		return w.stopRun(run, models.FollowRunCancelled, "follow_cancelled", "追更任务已取消")
	}
	selected := selectFollowCandidates(candidates, missing, snapshot.MaxResourcesPerRun)
	stats.mu.Lock()
	stats.chosen = len(selected)
	stats.mu.Unlock()
	w.record(ctx, "follow.selection", "selection", "confirmed", fmt.Sprintf("身份与订阅规则过滤后 %d 个可用候选，按缺集覆盖、站点顺序、质量偏好与做种等既有规则选中 %d 个资源", len(candidates), len(selected)), map[string]any{"candidates": len(candidates), "chosen": len(selected), "reason_counts": filterSummary})
	if len(selected) == 0 {
		if filterSummary["site_error"] > 0 || filterSummary["site_incomplete"] > 0 || filterSummary["search_incomplete"] > 0 {
			return w.finish(run, subscription, models.FollowRunFailed, models.FollowStatusActive, "follow_search_incomplete", "本轮部分或全部站点搜索未完成，不能确定没有匹配资源", snapshot, queryNames, len(candidates), filterSummary)
		}
		return w.finish(run, subscription, models.FollowRunNoMatch, models.FollowStatusActive, "", "", snapshot, queryNames, len(candidates), filterSummary)
	}
	progress = 70
	processed = 3
	_ = runtime.Heartbeat(&progress, &processed, &total, nil, nil)
	_ = runtime.Checkpoint(map[string]any{"stage": "submit", "missing": missingCoordinates, "selected": candidateFingerprints(selected)})
	submitted := 0
	submitFailures := 0
	lastSubmitMessage := ""
	for _, candidate := range selected {
		candidateCtx := serverlog.WithBusinessFields(ctx, map[string]any{"site_id": candidate.SiteID, "resource_name": candidate.Item.Title, "season": candidate.Season})
		blocked, gateErr := followReadinessBlocked(w.follows.db, subscription.ID)
		if gateErr != nil {
			return WorkerResult{ErrorCode: CodeConflict, ErrorMessage: "无法确认目标媒体库是否准备好"}
		}
		if blocked {
			w.record(candidateCtx, "follow.wait", "submit", "waiting", "目标媒体库尚未准备好，保留本轮进度，暂不提交资源", map[string]any{"error_code": "media_library_not_ready"})
			next := time.Now().UTC()
			return WorkerResult{RetryAt: &next, ErrorCode: "media_library_not_ready", ErrorMessage: "等待目标媒体库准备好，已保留追更进度"}
		}
		if ctx.Err() != nil {
			return w.stopRun(run, models.FollowRunCancelled, "follow_cancelled", "追更任务已取消")
		}
		current, err := w.currentSubscription(subscription.ID)
		if err != nil || !followRunExecutable(current, run.LifecycleRevision) {
			return w.stopRun(run, models.FollowRunCancelled, "follow_inactive", "订阅已暂停或删除")
		}
		episodes := candidateMissingEpisodes(candidate, missing)
		if len(episodes) == 0 {
			w.record(candidateCtx, "follow.coverage_changed", "selection", "skipped", "该资源已不覆盖本轮剩余缺集，未提交下载", nil)
			continue
		}
		libraryID := snapshot.MediaLibraryID
		// Search may outlive an authorization edit. Each new resource submission
		// uses the owner's current authority; accepted tasks retain their plan.
		actor, err = w.follows.authorization.Resolve(run.OwnerID)
		if err != nil {
			return w.block(run, subscription, CodePermissionDenied, "订阅用户或权限不可用")
		}
		recommendation, routeErr := w.sites.RecommendSiteRoute(ctx, actor, SourceRouteRecommendationInput{ResultToken: candidate.Item.Token, MediaLibraryID: &libraryID})
		if routeErr != nil || recommendation.Recommended == nil {
			submitFailures++
			filterSummary["submit_failed"]++
			lastSubmitMessage = "没有可用的下载器与目标媒体库路线"
			if routeErr != nil {
				lastSubmitMessage = safeErrorMessage(routeErr, lastSubmitMessage)
			}
			w.record(candidateCtx, "follow.route_unavailable", "route", "failed", "资源未提交：没有可用的下载器与目标媒体库路线", map[string]any{"error_code": ErrorCode(routeErr)})
			continue
		}
		previewToken := ""
		var selectedEntryTokens []string
		if isCloudShareSource(candidate.Item.SourceKind) {
			var selectionDigest string
			previewToken, selectedEntryTokens, episodes, selectionDigest, err = w.sites.prepareFollowShare(ctx, actor, candidate.Item.Token, recommendation.Recommended.DownloaderID, candidate.Season, episodes)
			if err != nil {
				submitFailures++
				filterSummary["share_selection_failed"]++
				lastSubmitMessage = safeErrorMessage(err, "无法核实分享中的本次缺集文件")
				continue
			}
			candidate.Fingerprint = followFingerprint(candidate.SiteID, fmt.Sprintf("%s:%d:%v:%s", candidate.Fingerprint, candidate.Season, episodes, selectionDigest), 0, nil)
		}
		claimed, err := w.reserveEpisodes(run, subscription.ID, candidate, episodes)
		if err != nil || !claimed {
			reason, outcome, message := "active_claim", "skipped", "所选季集已有任务占用，未重复提交该资源"
			if err != nil {
				reason, outcome, message = "reservation_failed", "failed", "无法保留本轮季集占用，未提交该资源"
				stats.mu.Lock()
				stats.reservationFailures++
				stats.mu.Unlock()
			}
			w.record(candidateCtx, "follow.reservation", "reserve", outcome, message, map[string]any{"reason": reason, "episode_count": len(episodes)})
			continue
		}
		season := candidate.Season
		var singleEpisode *int
		if len(episodes) == 1 {
			value := episodes[0]
			singleEpisode = &value
		}
		downloaders := []string{recommendation.Recommended.DownloaderID}
		if recommendation.SourceKind == RouteSourceBT {
			for _, choice := range recommendation.Choices {
				if choice.Enabled && choice.MediaLibraryID == libraryID && choice.DownloaderID != downloaders[0] {
					downloaders = append(downloaders, choice.DownloaderID)
				}
			}
		}
		var download DownloadTaskSummary
		for index, downloaderID := range downloaders {
			downloaderName, libraryName, routeLabel := "所选下载器", "所选媒体库", "已校验路线"
			for _, choice := range recommendation.Choices {
				if choice.DownloaderID == downloaderID && choice.MediaLibraryID == libraryID {
					downloaderName, libraryName, routeLabel = choice.DownloaderName, choice.LibraryName, choice.RouteLabel
					break
				}
			}
			submitCtx := serverlog.WithBusinessFields(candidateCtx, map[string]any{"downloader_id": downloaderID, "downloader_name": downloaderName, "library_name": libraryName})
			w.record(submitCtx, "follow.route", "route", "planned", fmt.Sprintf("选择资源 %s，下载器：%s，目标媒体库：%s；%s，覆盖本轮 %d 集", serverlog.BusinessDisplayName(candidate.Item.Title), serverlog.BusinessDisplayName(downloaderName), serverlog.BusinessDisplayName(libraryName), serverlog.BusinessDisplayName(routeLabel), len(episodes)), map[string]any{"episode_count": len(episodes)})
			download, err = w.sites.Download(submitCtx, actor, SiteDownloadInput{PreviewToken: previewToken, SelectedEntryTokens: selectedEntryTokens, ResultToken: candidate.Item.Token, DownloaderID: downloaderID, MediaLibraryID: &libraryID, Priority: snapshot.DownloadPriority, FollowSubscriptionID: subscription.ID, FollowResourceFingerprint: candidate.Fingerprint, Season: &season, Episode: singleEpisode, BeforeSubmit: func() error {
				current, currentErr := w.currentSubscription(subscription.ID)
				if currentErr != nil || !followRunExecutable(current, run.LifecycleRevision) {
					return appError(CodeConflict, "订阅已暂停或删除", currentErr)
				}
				return nil
			}, BeforePersist: func(tx *gorm.DB) error {
				current, currentErr := w.follows.LockCurrentSubscription(tx, subscription.ID)
				if currentErr != nil || !followRunExecutable(current, run.LifecycleRevision) {
					return appError(CodeConflict, "订阅已暂停或删除", currentErr)
				}
				return nil
			}}, RequestContext{})
			if err == nil || index+1 == len(downloaders) || !isDefinitiveNoTaskRouteError(ErrorCode(err)) {
				break
			}
			w.record(submitCtx, "follow.route_fallback", "submit", "retry", "公开 BT 资源被明确拒绝且未创建任务，按已有规则尝试下一兼容下载器", map[string]any{"error_code": ErrorCode(err)})
		}
		if err != nil {
			w.releaseEpisodes(subscription.ID, run.ID, candidate.Fingerprint)
			current, currentErr := w.currentSubscription(subscription.ID)
			if currentErr != nil || !followRunExecutable(current, run.LifecycleRevision) {
				return w.stopRun(run, models.FollowRunCancelled, "follow_inactive", "订阅已暂停或删除")
			}
			if ctx.Err() != nil {
				return w.stopRun(run, models.FollowRunCancelled, "follow_cancelled", "追更任务已取消")
			}
			filterSummary["submit_failed"]++
			submitFailures++
			lastSubmitMessage = safeErrorMessage(err, "资源提交失败")
			w.record(candidateCtx, "follow.submit_failed", "submit", "failed", "资源未能提交；本条失败不会阻塞其他独立资源，未对不确定结果自动更换下载器", map[string]any{"error_code": ErrorCode(err)})
			continue
		}
		if err := w.attachDownload(subscription.ID, run.ID, candidate.Fingerprint, download.ID); err != nil {
			return followPersistFailure()
		}
		w.follows.publish("follow.download_submitted", run.OwnerID, run.JobID, models.FollowRunSubmitted)
		submitted++
		w.record(candidateCtx, "follow.submitted", "submit", "accepted", "下载请求已接受，后续下载与入库请查看关联任务；本轮未提前确认入库完成", map[string]any{"related_task_id": download.ID, "related_job_id": download.JobID, "action": download.submissionOutcome})
	}
	if submitted == 0 {
		if submitFailures > 0 {
			return w.finish(run, subscription, models.FollowRunFailed, models.FollowStatusActive, "follow_submit_failed", lastSubmitMessage, snapshot, queryNames, len(candidates), filterSummary)
		}
		return w.finish(run, subscription, models.FollowRunNoMatch, models.FollowStatusActive, "", "", snapshot, queryNames, len(candidates), filterSummary)
	}
	if submitFailures > 0 {
		return w.finish(run, subscription, models.FollowRunSubmitted, models.FollowStatusActive, "follow_submit_partial", lastSubmitMessage, snapshot, queryNames, len(candidates), filterSummary, submitted)
	}
	return w.finish(run, subscription, models.FollowRunSubmitted, models.FollowStatusActive, "", "", snapshot, queryNames, len(candidates), filterSummary, submitted)
}

func isDefinitiveNoTaskRouteError(code string) bool {
	switch code {
	case CodeDownloaderUnavailable, CodeDownloaderStorageUnavailable, CodeDownloadSourceInvalid, CodeDownloadStagingRequired, CodeDownloadStagingUnavailable, CodeMediaLibraryStorageUnavailable, CodeMediaLibraryProfileUnavailable, CodeMediaLibraryPathInvalid, CodeTransferRouteUnsupported:
		return true
	default:
		return false
	}
}

func (w *FollowSearchWorker) searchCandidates(ctx context.Context, actor Actor, subscription models.FollowSubscription, snapshot FollowExecutionSnapshot, missing map[[2]int]struct{}, coverage map[int]MediaCoverageSeason) ([]followCandidate, int, map[string]int) {
	result := []followCandidate{}
	summary := map[string]int{}
	queryNames := 0
	seen := map[string]struct{}{}
	for sitePriority, siteID := range snapshot.SiteIDs {
		for _, season := range snapshot.Seasons {
			if ctx.Err() != nil {
				summary["search_incomplete"]++
				return result, queryNames, summary
			}
			hasMissing := false
			for key := range missing {
				if key[0] == season {
					hasMissing = true
					break
				}
			}
			if !hasMissing {
				continue
			}
			searchCtx := serverlog.WithBusinessFields(ctx, map[string]any{"site_id": siteID, "season": season})
			grouped, err := w.sites.SearchMediaIdentity(searchCtx, actor, MediaIdentitySearchInput{MediaType: "tv", TMDBID: subscription.TMDBID, Season: &season, Page: 1, SiteID: &siteID, ClientCapabilities: []string{MixedDiscoveryCapability}, Refresh: true})
			if err != nil {
				summary["site_error"]++
				w.record(searchCtx, "follow.search_preflight_failed", "search", "failed", "该站搜索未能完成预检或执行，不把它计为零结果", map[string]any{"error_code": ErrorCode(err)})
				continue
			}
			if queryNames == 0 {
				queryNames = len(grouped.QueryNames)
			}
			for _, group := range grouped.Groups {
				groupCtx := serverlog.WithBusinessFields(searchCtx, map[string]any{"site_name": group.SiteName})
				rejections := newFollowRejections()
				if group.Partial {
					summary["site_incomplete"]++
				}
				if group.ErrorCount > 0 {
					summary["site_error"] += group.ErrorCount
				}
				if group.Status != "success" {
					if group.ErrorCount == 0 {
						summary["site_error"]++
					}
					continue
				}
				for _, item := range group.Items {
					candidate, reason, ok := buildFollowCandidate(item, group.SiteID, sitePriority, season, snapshot)
					if !ok {
						summary[reason]++
						rejections.add(reason, item.Title)
						continue
					}
					candidate, ok = constrainFollowCandidateCoverage(candidate, coverage[season])
					if !ok {
						summary["episode_not_confirmed"]++
						rejections.add("episode_not_confirmed", item.Title)
						continue
					}
					candidateKey := candidate.Fingerprint
					if isCloudShareSource(item.SourceKind) {
						candidateKey = fmt.Sprintf("%s:%d", candidateKey, season)
					}
					if _, duplicate := seen[candidateKey]; duplicate {
						summary["duplicate"]++
						rejections.add("duplicate", item.Title)
						continue
					}
					seen[candidateKey] = struct{}{}
					covers := false
					for _, episode := range candidate.Episodes {
						if _, ok := missing[[2]int{season, episode}]; ok {
							covers = true
							break
						}
					}
					if !covers {
						summary["no_missing_coverage"]++
						rejections.add("no_missing_coverage", item.Title)
						continue
					}
					result = append(result, candidate)
				}
				rejections.record(groupCtx, w.record)
			}
		}
	}
	sort.SliceStable(result, func(i, j int) bool { return followCandidateLess(result[i], result[j]) })
	return result, queryNames, summary
}

func buildFollowCandidate(item SiteSearchResult, siteID uint, sitePriority, season int, snapshot FollowExecutionSnapshot) (followCandidate, string, bool) {
	parsed, err := mediarecognition.Parse(mediarecognition.InputFacts{PackageName: item.Title, SourceKind: mediarecognition.SourceDownload, MediaTypeHint: mediarecognition.MediaTypeTV, SeasonHint: &season})
	if err != nil || parsed.Season == nil || *parsed.Season != season || !followEpisodeDeclarationIsUnambiguous(item.Title, season) {
		return followCandidate{}, "episode_unproven", false
	}
	min, max := parsed.Episodes.EpisodeMin, parsed.Episodes.EpisodeMax
	completeSeason := followCompleteSeasonPattern.MatchString(item.Title)
	if episodeMin, episodeMax, ok := explicitFollowEpisodeRange(item.Title, season); ok {
		min, max = &episodeMin, &episodeMax
	}
	boundsDeclared := min != nil && max != nil
	if !boundsDeclared && completeSeason {
		episodeMin, episodeMax := 1, 200
		min, max = &episodeMin, &episodeMax
	}
	if min == nil || max == nil || *min < 1 || *max < *min || *max-*min > 200 {
		return followCandidate{}, "episode_unproven", false
	}
	episodes := make([]int, 0, *max-*min+1)
	for value := *min; value <= *max; value++ {
		episodes = append(episodes, value)
	}
	text := strings.ToLower(item.Title + " " + item.Subtitle + " " + strings.Join(item.Tags, " "))
	spec := item.Specifications
	if !containsEvery(text, snapshot.Filters.IncludeKeywords) {
		return followCandidate{}, "include_keyword", false
	}
	if containsAny(text, snapshot.Filters.ExcludeKeywords) {
		return followCandidate{}, "exclude_keyword", false
	}
	if len(snapshot.Filters.Resolutions) > 0 && !matchesAllowed(spec.Resolution, snapshot.Filters.Resolutions) {
		return followCandidate{}, "resolution", false
	}
	if len(snapshot.Filters.VideoCodecs) > 0 && !matchesAllowed(spec.VideoCodec, snapshot.Filters.VideoCodecs) {
		return followCandidate{}, "video_codec", false
	}
	if len(snapshot.Filters.Qualities) > 0 && !matchesAnyAllowed([]string{item.Quality, spec.Source}, snapshot.Filters.Qualities) {
		return followCandidate{}, "quality", false
	}
	if len(snapshot.Filters.ReleaseGroups) > 0 && !matchesAllowed(spec.ReleaseGroup, snapshot.Filters.ReleaseGroups) {
		return followCandidate{}, "release_group", false
	}
	if matchesAllowed(spec.ReleaseGroup, snapshot.Filters.ExcludeReleaseGroups) {
		return followCandidate{}, "excluded_release_group", false
	}
	if !isCloudShareSource(item.SourceKind) && snapshot.Filters.MinSeeders > 0 && (item.Seeders == nil || *item.Seeders < snapshot.Filters.MinSeeders) {
		return followCandidate{}, "seeders", false
	}
	if (snapshot.Filters.MinSizeBytes != nil || snapshot.Filters.MaxSizeBytes != nil) && item.SizeBytes <= 0 {
		return followCandidate{}, "size", false
	}
	if snapshot.Filters.MinSizeBytes != nil && item.SizeBytes < *snapshot.Filters.MinSizeBytes {
		return followCandidate{}, "size", false
	}
	if snapshot.Filters.MaxSizeBytes != nil && item.SizeBytes > *snapshot.Filters.MaxSizeBytes {
		return followCandidate{}, "size", false
	}
	if snapshot.Filters.MaxAgeHours != nil && (item.Published == nil || time.Since(*item.Published) > time.Duration(*snapshot.Filters.MaxAgeHours)*time.Hour) {
		return followCandidate{}, "age", false
	}
	fingerprint := item.ResourceFingerprint
	if fingerprint == "" {
		fingerprint = followFingerprint(siteID, item.Title, item.SizeBytes, item.Published)
	}
	return followCandidate{SiteID: siteID, SitePriority: sitePriority, Item: item, Season: season, Episodes: episodes, CompleteSeason: completeSeason, EpisodeBoundsDeclared: boundsDeclared, Fingerprint: fingerprint, QualityRank: qualityPreferenceRank(item, snapshot)}, "", true
}

func explicitFollowEpisodeRange(title string, season int) (int, int, bool) {
	match := followEpisodeRangePattern.FindStringSubmatch(title)
	if len(match) != 5 {
		return 0, 0, false
	}
	matchedSeason, seasonErr := strconv.Atoi(match[1])
	first, firstErr := strconv.Atoi(match[2])
	last, lastErr := strconv.Atoi(match[4])
	if match[3] != "" {
		lastSeason, err := strconv.Atoi(match[3])
		if err != nil || lastSeason != season {
			return 0, 0, false
		}
	}
	return first, last, seasonErr == nil && firstErr == nil && lastErr == nil && matchedSeason == season
}

// The general recognizer intentionally projects one episode for some compound
// names. Automation must reject extra declarations it cannot bound instead of
// treating that first episode as the complete contents of a torrent or file.
func followEpisodeDeclarationIsUnambiguous(title string, season int) bool {
	if span := followEpisodeRangePattern.FindStringIndex(title); span != nil {
		if _, _, ok := explicitFollowEpisodeRange(title, season); !ok {
			return false
		}
		if followEpisodeContinuationPattern.MatchString(title[span[1]:]) {
			return false
		}
		return !followEpisodeMarkerPattern.MatchString(title[:span[0]] + " " + title[span[1]:])
	}
	markers := followEpisodeMarkerPattern.FindAllStringIndex(title, -1)
	if len(markers) > 1 {
		return false
	}
	return len(markers) == 0 || !followEpisodeContinuationPattern.MatchString(title[markers[0][1]:])
}

// Torrent submissions cannot select individual files here. A range must not
// smuggle unknown/future episodes alongside a known missing one. Cloud shares
// instead prove and select each actual missing file in prepareFollowShare.
func constrainFollowCandidateCoverage(candidate followCandidate, coverage MediaCoverageSeason) (followCandidate, bool) {
	if isCloudShareSource(candidate.Item.SourceKind) {
		return candidate, true
	}
	confirmed := make(map[int]struct{}, len(coverage.Episodes))
	for _, episode := range coverage.Episodes {
		if episode.Status == "present" || episode.Status == "missing" {
			confirmed[episode.EpisodeNumber] = struct{}{}
		}
	}
	// An explicit bound remains evidence even beside a complete-season label;
	// replacing it first would silently erase out-of-snapshot episodes.
	if !candidate.CompleteSeason || candidate.EpisodeBoundsDeclared {
		for _, episode := range candidate.Episodes {
			if _, ok := confirmed[episode]; !ok {
				return followCandidate{}, false
			}
		}
	}
	if candidate.CompleteSeason {
		if coverage.Counts.Unknown > 0 || coverage.Counts.Future > 0 || len(confirmed) == 0 || len(confirmed) != coverage.Counts.Total {
			return followCandidate{}, false
		}
		candidate.Episodes = make([]int, 0, len(confirmed))
		for episode := range confirmed {
			candidate.Episodes = append(candidate.Episodes, episode)
		}
		sort.Ints(candidate.Episodes)
	}
	return candidate, true
}

func selectFollowCandidates(candidates []followCandidate, missing map[[2]int]struct{}, limit int) []followCandidate {
	remaining := map[[2]int]struct{}{}
	for key := range missing {
		remaining[key] = struct{}{}
	}
	pool := append([]followCandidate(nil), candidates...)
	selected := []followCandidate{}
	for len(remaining) > 0 && len(selected) < limit {
		best := -1
		bestCoverage := 0
		bestExcess := 0
		for index, candidate := range pool {
			coverage := 0
			for _, episode := range candidate.Episodes {
				if _, ok := remaining[[2]int{candidate.Season, episode}]; ok {
					coverage++
				}
			}
			excess := len(candidate.Episodes) - coverage
			if coverage > bestCoverage || (coverage == bestCoverage && coverage > 0 && (best < 0 || excess < bestExcess || excess == bestExcess && followCandidateLess(candidate, pool[best]))) {
				best, bestCoverage, bestExcess = index, coverage, excess
			}
		}
		if best < 0 || bestCoverage == 0 {
			break
		}
		chosen := pool[best]
		selected = append(selected, chosen)
		for _, episode := range chosen.Episodes {
			delete(remaining, [2]int{chosen.Season, episode})
		}
		pool = append(pool[:best], pool[best+1:]...)
	}
	return selected
}

func followCandidateLess(left, right followCandidate) bool {
	if left.SitePriority != right.SitePriority {
		return left.SitePriority < right.SitePriority
	}
	if left.QualityRank != right.QualityRank {
		return left.QualityRank < right.QualityRank
	}
	ls, rs := -1, -1
	if left.Item.Seeders != nil {
		ls = *left.Item.Seeders
	}
	if right.Item.Seeders != nil {
		rs = *right.Item.Seeders
	}
	if ls != rs {
		return ls > rs
	}
	if compared := compareOptionalTime(left.Item.Published, right.Item.Published); compared != 0 {
		return compared > 0
	}
	return left.Fingerprint < right.Fingerprint
}

func (w *FollowSearchWorker) reserveEpisodes(run models.FollowRun, subscriptionID string, candidate followCandidate, episodes []int) (bool, error) {
	now := w.follows.now()
	reserved := false
	err := w.follows.db.Transaction(func(tx *gorm.DB) error {
		record, err := w.follows.LockCurrentSubscription(tx, subscriptionID)
		if err != nil {
			return err
		}
		if !followRunExecutable(record, run.LifecycleRevision) {
			return appError(CodeConflict, "订阅已暂停", nil)
		}
		existing := make(map[int]models.FollowEpisodeClaim, len(episodes))
		for _, episode := range episodes {
			var claim models.FollowEpisodeClaim
			err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&claim, "subscription_id = ? AND season_number = ? AND episode_number = ?", subscriptionID, candidate.Season, episode).Error
			if err == nil && claim.DownloadTaskID != nil {
				return nil
			}
			if err != nil && err != gorm.ErrRecordNotFound {
				return err
			}
			if err == nil {
				existing[episode] = claim
			}
		}
		for _, episode := range episodes {
			_, exists := existing[episode]
			claim := models.FollowEpisodeClaim{SubscriptionID: subscriptionID, SeasonNumber: candidate.Season, EpisodeNumber: episode, State: "queued", RunID: &run.ID, ResourceFingerprint: candidate.Fingerprint, UpdatedAt: now}
			if !exists {
				if err := tx.Create(&claim).Error; err != nil {
					return err
				}
			} else {
				if err := tx.Model(&models.FollowEpisodeClaim{}).Where("subscription_id = ? AND season_number = ? AND episode_number = ?", subscriptionID, candidate.Season, episode).Updates(map[string]any{"state": "queued", "run_id": run.ID, "download_task_id": nil, "resource_fingerprint": candidate.Fingerprint, "updated_at": now}).Error; err != nil {
					return err
				}
			}
			reserved = true
		}
		return nil
	})
	return reserved, err
}
func (w *FollowSearchWorker) releaseEpisodes(subscriptionID, runID, fingerprint string) {
	_ = w.follows.db.Model(&models.FollowEpisodeClaim{}).Where("subscription_id = ? AND run_id = ? AND resource_fingerprint = ? AND download_task_id IS NULL", subscriptionID, runID, fingerprint).Updates(map[string]any{"state": "failed", "updated_at": w.follows.now()}).Error
}
func (w *FollowSearchWorker) attachDownload(subscriptionID, runID, fingerprint, downloadID string) error {
	return w.follows.db.Model(&models.FollowEpisodeClaim{}).Where("subscription_id = ? AND run_id = ? AND resource_fingerprint = ?", subscriptionID, runID, fingerprint).Updates(map[string]any{"state": "downloading", "download_task_id": downloadID, "updated_at": w.follows.now()}).Error
}
func (w *FollowSearchWorker) upsertImported(subscriptionID string, season, episode int) error {
	now := w.follows.now()
	return w.follows.db.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "subscription_id"}, {Name: "season_number"}, {Name: "episode_number"}}, DoUpdates: clause.Assignments(map[string]any{"state": "imported", "updated_at": now})}).Create(&models.FollowEpisodeClaim{SubscriptionID: subscriptionID, SeasonNumber: season, EpisodeNumber: episode, State: "imported", UpdatedAt: now}).Error
}

func (w *FollowSearchWorker) excludeClaimedDownloads(ctx context.Context, subscriptionID string, missing map[[2]int]struct{}) (map[[2]int]struct{}, error) {
	var claims []models.FollowEpisodeClaim
	if err := w.follows.db.Where("subscription_id = ? AND download_task_id IS NOT NULL", subscriptionID).Find(&claims).Error; err != nil {
		return nil, err
	}
	for _, claim := range claims {
		var task models.DownloadTask
		if err := w.follows.db.First(&task, "id = ?", *claim.DownloadTaskID).Error; err != nil {
			if err != gorm.ErrRecordNotFound {
				return nil, err
			}
			continue
		}
		if task.Phase != models.DownloadTaskStatusFailed && task.Phase != models.DownloadTaskStatusCancelled {
			if _, needed := missing[[2]int{claim.SeasonNumber, claim.EpisodeNumber}]; needed {
				fields := map[string]any{"related_task_id": task.ID, "related_job_id": task.JobID, "season": claim.SeasonNumber}
				if task.DownloaderID != nil {
					fields["downloader_id"] = *task.DownloaderID
				}
				w.record(ctx, "follow.active_claim", "coverage", "skipped", fmt.Sprintf("S%02dE%02d 已由原下载任务占用，本轮不重复下载", claim.SeasonNumber, claim.EpisodeNumber), fields)
			}
			delete(missing, [2]int{claim.SeasonNumber, claim.EpisodeNumber})
		} else {
			if err := w.follows.db.Model(&claim).Updates(map[string]any{"state": "failed", "download_task_id": nil, "updated_at": w.follows.now()}).Error; err != nil {
				return nil, err
			}
		}
	}
	return missing, nil
}

func (w *FollowSearchWorker) currentSubscription(id string) (models.FollowSubscription, error) {
	var record models.FollowSubscription
	err := w.follows.db.First(&record, "id = ?", id).Error
	return record, err
}
func (w *FollowSearchWorker) block(run models.FollowRun, subscription models.FollowSubscription, code, message string) WorkerResult {
	return w.finish(run, subscription, models.FollowRunFailed, models.FollowStatusBlocked, code, message, FollowExecutionSnapshot{}, 0, 0, map[string]int{})
}

func (w *FollowSearchWorker) waitForCoverage(run models.FollowRun, subscription models.FollowSubscription, snapshot FollowExecutionSnapshot, message string, unknown int) WorkerResult {
	// Uncertain metadata/absence is temporary. Keep the normal schedule active
	// without claiming a successful search or marking the subscription complete.
	summary := map[string]int{}
	if unknown > 0 {
		summary["coverage_unknown"] = unknown
	}
	return w.finish(run, subscription, models.FollowRunFailed, models.FollowStatusActive, "follow_coverage_unknown", message, snapshot, 0, 0, summary)
}
func (w *FollowSearchWorker) stopRun(run models.FollowRun, status, code, message string) WorkerResult {
	now := w.follows.now()
	if err := w.follows.db.Model(&run).Updates(map[string]any{"status": status, "error_code": code, "error_message": message, "finished_at": now, "updated_at": now}).Error; err != nil {
		return followPersistFailure()
	}
	w.follows.publish("follow.cancelled", run.OwnerID, run.JobID, status)
	return WorkerResult{}
}
func (w *FollowSearchWorker) finish(run models.FollowRun, subscription models.FollowSubscription, runStatus, subscriptionStatus, code, message string, snapshot FollowExecutionSnapshot, queryNames, candidates int, summary map[string]int, selected ...int) WorkerResult {
	now := w.follows.now()
	next := now.Add(time.Duration(snapshot.Schedule.Minutes) * time.Minute)
	if snapshot.Schedule.Minutes == 0 {
		var stored FollowExecutionSnapshot
		if json.Unmarshal([]byte(run.ExecutionSnapshotJSON), &stored) == nil {
			next = now.Add(time.Duration(stored.Schedule.Minutes) * time.Minute)
		}
	}
	summaryRaw, _ := json.Marshal(summary)
	selectedCount := 0
	if len(selected) > 0 {
		selectedCount = selected[0]
	}
	err := w.follows.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&models.FollowRun{}).Where("id = ?", run.ID).Updates(map[string]any{"status": runStatus, "searched_names_count": queryNames, "candidates": candidates, "selected": selectedCount, "filter_summary_json": string(summaryRaw), "error_code": code, "error_message": message, "finished_at": now, "updated_at": now}).Error; err != nil {
			return err
		}
		if err := tx.Model(&models.FollowSubscription{}).Where("id = ?", subscription.ID).Updates(map[string]any{"last_run_id": run.ID, "last_run_at": now, "updated_at": now}).Error; err != nil {
			return err
		}
		return tx.Model(&models.FollowSubscription{}).
			Where("id = ? AND revision = ? AND lifecycle_revision = ? AND status <> ?", subscription.ID, run.SubscriptionRevision, run.LifecycleRevision, models.FollowStatusPaused).
			Updates(map[string]any{"status": subscriptionStatus, "next_run_at": next, "last_error_code": code, "last_error_message": message, "updated_at": now}).Error
	})
	if err != nil {
		return followPersistFailure()
	}
	eventType := map[string]string{models.FollowRunNoMatch: "follow.no_match", models.FollowRunCompleted: "follow.completed", models.FollowRunFailed: "follow.blocked"}[runStatus]
	if runStatus == models.FollowRunFailed && subscriptionStatus == models.FollowStatusActive {
		eventType = "follow.submit_failed"
		if code == "follow_search_incomplete" {
			eventType = "follow.search_failed"
		} else if code == "follow_coverage_unknown" {
			eventType = "follow.coverage_waiting"
		}
	}
	if eventType != "" {
		w.follows.publish(eventType, run.OwnerID, run.JobID, runStatus)
	}
	return WorkerResult{}
}

func coordinateList(missing map[[2]int]struct{}) []FollowEpisodeCoordinate {
	result := make([]FollowEpisodeCoordinate, 0, len(missing))
	for key := range missing {
		result = append(result, FollowEpisodeCoordinate{Season: key[0], Episode: key[1]})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Season != result[j].Season {
			return result[i].Season < result[j].Season
		}
		return result[i].Episode < result[j].Episode
	})
	return result
}
func countEpisodeStatus(items []MediaCoverageEpisode, status string) int {
	count := 0
	for _, item := range items {
		if item.Status == status {
			count++
		}
	}
	return count
}
func containsEvery(text string, values []string) bool {
	for _, value := range values {
		if !strings.Contains(text, strings.ToLower(value)) {
			return false
		}
	}
	return true
}
func containsAny(text string, values []string) bool {
	for _, value := range values {
		if strings.Contains(text, strings.ToLower(value)) {
			return true
		}
	}
	return false
}
func matchesAllowed(value string, allowed []string) bool {
	if len(allowed) == 0 {
		return false
	}
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return false
	}
	for _, item := range allowed {
		normalized := strings.ToLower(strings.TrimSpace(item))
		if normalized == "" {
			continue
		}
		if value == normalized || strings.Contains(value, normalized) || strings.Contains(normalized, value) {
			return true
		}
	}
	return false
}
func matchesAnyAllowed(values, allowed []string) bool {
	for _, value := range values {
		if matchesAllowed(value, allowed) {
			return true
		}
	}
	return false
}
func qualityPreferenceRank(item SiteSearchResult, snapshot FollowExecutionSnapshot) int {
	preference := func(values []string, actual ...string) int {
		if len(values) == 0 {
			return 0
		}
		for index, value := range values {
			if matchesAnyAllowed(actual, []string{value}) {
				return index
			}
		}
		return len(values)
	}
	resolution := preference(snapshot.Filters.Resolutions, item.Specifications.Resolution)
	codec := preference(snapshot.Filters.VideoCodecs, item.Specifications.VideoCodec)
	quality := preference(snapshot.Filters.Qualities, item.Quality, item.Specifications.Source)
	return resolution*1024 + codec*32 + quality
}
func followFingerprint(siteID uint, title string, size int64, published *time.Time) string {
	stamp := ""
	if published != nil {
		stamp = published.UTC().Format(time.RFC3339Nano)
	}
	sum := sha256.Sum256([]byte(strconv.FormatUint(uint64(siteID), 10) + "\x00" + strings.ToLower(strings.Join(strings.Fields(title), " ")) + "\x00" + strconv.FormatInt(size, 10) + "\x00" + stamp))
	return hex.EncodeToString(sum[:])
}
func candidateMissingEpisodes(candidate followCandidate, missing map[[2]int]struct{}) []int {
	result := []int{}
	for _, episode := range candidate.Episodes {
		if _, ok := missing[[2]int{candidate.Season, episode}]; ok {
			result = append(result, episode)
		}
	}
	return result
}
func candidateFingerprints(candidates []followCandidate) []string {
	result := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		result = append(result, candidate.Fingerprint)
	}
	return result
}

func followRunExecutable(subscription models.FollowSubscription, lifecycleRevision uint64) bool {
	return subscription.Status != models.FollowStatusPaused && subscription.LifecycleRevision == lifecycleRevision
}

func followPersistFailure() WorkerResult {
	return WorkerResult{ErrorCode: "follow_state_persist_failed", ErrorMessage: "追更状态保存失败"}
}
