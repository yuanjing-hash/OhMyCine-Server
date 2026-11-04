package services

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

type preparedFollowCandidate struct {
	Candidate                           followCandidate
	Started, Deferred                   bool
	Stage                               string
	ContentSelectionToken, PreviewToken string
	SelectedEntryTokens                 []string
	Recommendation                      SourceRouteRecommendation
	UnconfirmedSubtitles                int
	Err                                 error
}

// Read-only preparation has its own bounded window. Episode claims, persistent
// tasks and executor submissions happen later, serially in deterministic order.
func (w *FollowSearchWorker) prepareFollowCandidates(ctx context.Context, actor Actor, snapshot FollowExecutionSnapshot, candidates []followCandidate, missing map[[2]int]struct{}) []preparedFollowCandidate {
	result := make([]preparedFollowCandidate, len(candidates))
	for index, candidate := range candidates {
		result[index] = preparedFollowCandidate{Candidate: candidate, Deferred: true}
	}
	var wg sync.WaitGroup
	permits := make(chan struct{}, 4)
	for index, candidate := range candidates {
		// Admit in input order. A canceled permit waiter has not inspected a
		// resource and must remain available to the next bounded run.
		select {
		case permits <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return result
		}
		if ctx.Err() != nil {
			<-permits
			break
		}
		wg.Add(1)
		go func(i int, candidate followCandidate) {
			defer wg.Done()
			prepared := preparedFollowCandidate{Candidate: candidate, Started: true, Stage: "content"}
			defer func() {
				prepared.Deferred = prepared.Err != nil && ctx.Err() != nil
				result[i] = prepared
			}()
			defer func() { <-permits }()
			episodes := candidateMissingEpisodes(candidate, missing)
			if len(episodes) == 0 {
				prepared.Err = appError(CodeDownloadSourceInvalid, "该资源不覆盖本次缺集", nil)
				return
			}
			libraryID := snapshot.MediaLibraryID
			// Route eligibility is local/read-only. Reject an unavailable or
			// live-swarm-only route before spending a heavy content read.
			prepared.Stage = "route"
			preflight, err := w.sites.RecommendSiteRoute(ctx, actor, SourceRouteRecommendationInput{ResultToken: candidate.Item.Token, MediaLibraryID: &libraryID})
			preflight = followSeedRoute(preflight, candidate.Item, snapshot)
			if err != nil {
				prepared.Err = err
				return
			}
			if preflight.Recommended == nil {
				prepared.Err = appError(CodeDownloaderUnavailable, "没有符合订阅做种数策略与目标媒体库的可用路线", nil)
				return
			}
			prepared.Stage = "content"
			if candidate.Item.ContentInspection {
				content, err := w.sites.prepareFollowContent(ctx, actor, candidate.Item.Token, candidate.Season, episodes, effectiveSubtitlePolicy(snapshot.Filters.Subtitles))
				if err != nil {
					prepared.Err = err
					return
				}
				prepared.ContentSelectionToken, prepared.UnconfirmedSubtitles = content.SelectionToken, content.UnconfirmedSubtitles
				prepared.Candidate.Episodes, prepared.Candidate.SubtitleRank = content.Episodes, content.SubtitleRank
				prepared.Candidate.Fingerprint = followFingerprint(candidate.SiteID, fmt.Sprintf("%s:%d:%v:%s", candidate.Fingerprint, candidate.Season, content.Episodes, content.Digest), 0, nil)
			}
			prepared.Stage = "route"
			recommendation, err := w.sites.RecommendSiteRoute(ctx, actor, SourceRouteRecommendationInput{ResultToken: candidate.Item.Token, MediaLibraryID: &libraryID, ContentSelectionToken: prepared.ContentSelectionToken})
			recommendation = followSeedRoute(recommendation, candidate.Item, snapshot)
			if err != nil {
				prepared.Err = err
				return
			}
			if recommendation.Recommended == nil {
				prepared.Err = appError(CodeDownloaderUnavailable, "没有可用的下载器与目标媒体库路线", nil)
				return
			}
			prepared.Recommendation = recommendation
			// The executable route was resolved from the frozen source, not the
			// site's display name or category (a PanSou site can also return BT).
			prepared.Candidate.AcquisitionTier = followAcquisitionTier(snapshot.Filters.AcquisitionPreference, recommendation.SourceKind)
			// Compatibility for an old result contract; never use this branch after a
			// new manifest failed. Current native search results advertise inspection.
			if !candidate.Item.ContentInspection && isCloudShareSource(candidate.Item.SourceKind) {
				prepared.Stage = "content"
				var digest string
				prepared.PreviewToken, prepared.SelectedEntryTokens, episodes, digest, prepared.Err = w.sites.prepareFollowShare(ctx, actor, candidate.Item.Token, recommendation.Recommended.DownloaderID, candidate.Season, episodes, candidate.Item.Title)
				if prepared.Err != nil {
					return
				}
				prepared.Candidate.Episodes = episodes
				prepared.Candidate.Fingerprint = followFingerprint(candidate.SiteID, fmt.Sprintf("%s:%d:%v:%s", candidate.Fingerprint, candidate.Season, episodes, digest), 0, nil)
			}
		}(index, candidate)
	}
	wg.Wait()
	return result
}

// The cursor only schedules fresh read-only checks. It carries no source,
// inspection proof, claim or permission authority, and is private Job state.
type followCandidatePosition struct {
	Fingerprint string `json:"fingerprint"`
	Season      int    `json:"season"`
}

type followSearchCheckpoint struct {
	Version          int                       `json:"version"`
	Stage            string                    `json:"stage"`
	Missing          []FollowEpisodeCoordinate `json:"missing"`
	Selected         []string                  `json:"selected,omitempty"`
	PreparationAfter *followCandidatePosition  `json:"preparation_after,omitempty"`
	PreparationScope string                    `json:"preparation_scope"`
}

func (w *FollowSearchWorker) preparationCheckpoint(ctx context.Context, run models.FollowRun, subscription models.FollowSubscription, job models.Job, missing []FollowEpisodeCoordinate) (followSearchCheckpoint, error) {
	scope := inspectionDigest([]any{run.OwnerID, run.SubscriptionID, run.SubscriptionRevision, run.LifecycleRevision, missing})
	var snapshot FollowExecutionSnapshot
	if json.Unmarshal([]byte(run.ExecutionSnapshotJSON), &snapshot) == nil && snapshot.Filters.AcquisitionPreference == FollowAcquisitionTransferFirst {
		// Old cursors were built from BT-first identity pages. Restart that
		// scheduling cycle once at the new share-first boundary. Thereafter the
		// cursor can still progress past failed shares to later source tiers.
		scope = inspectionDigest([]any{scope, "transfer-first-source-order-v2"})
	}
	checkpoint := followSearchCheckpoint{Version: 1, Stage: "search", Missing: missing, PreparationScope: scope}
	reuse := func(raw string) bool {
		var saved followSearchCheckpoint
		if json.Unmarshal([]byte(raw), &saved) == nil && saved.Version == 1 && saved.PreparationScope == scope && saved.PreparationAfter != nil && len(saved.PreparationAfter.Fingerprint) == 64 {
			checkpoint.PreparationAfter = saved.PreparationAfter
			return true
		}
		return false
	}
	if reuse(job.CheckpointJSON) || subscription.LastRunID == nil || *subscription.LastRunID == run.ID {
		return checkpoint, nil
	}
	var previous models.Job
	err := w.follows.db.WithContext(ctx).Model(&models.Job{}).Select("jobs.checkpoint_json").
		Joins("JOIN follow_runs ON follow_runs.job_id = jobs.id").
		Where("follow_runs.id = ? AND follow_runs.subscription_id = ? AND follow_runs.owner_id = ? AND follow_runs.subscription_revision = ? AND follow_runs.lifecycle_revision = ? AND jobs.job_type = ? AND jobs.resource_key = ?", *subscription.LastRunID, run.SubscriptionID, run.OwnerID, run.SubscriptionRevision, run.LifecycleRevision, JobTypeFollowSearch, "follow:"+run.SubscriptionID).Take(&previous).Error
	if err != nil && err != gorm.ErrRecordNotFound {
		return checkpoint, err
	}
	reuse(previous.CheckpointJSON)
	return checkpoint, nil
}

func continueFollowCandidates(pool []followCandidate, after *followCandidatePosition) []followCandidate {
	if after != nil {
		for index, candidate := range pool {
			if candidate.Fingerprint == after.Fingerprint && candidate.Season == after.Season {
				return append(append([]followCandidate(nil), pool[index+1:]...), pool[:index+1]...)
			}
		}
	}
	return pool
}

func followSeederLimited(item SiteSearchResult, snapshot FollowExecutionSnapshot) bool {
	return !isCloudShareSource(item.SourceKind) && snapshot.Filters.MinSeeders > 0 && (item.Seeders == nil || *item.Seeders < snapshot.Filters.MinSeeders)
}

func followSeedRoute(recommendation SourceRouteRecommendation, item SiteSearchResult, snapshot FollowExecutionSnapshot) SourceRouteRecommendation {
	if !followSeederLimited(item, snapshot) {
		return recommendation
	}
	recommendation.Choices = append([]SourceRouteChoice(nil), recommendation.Choices...)
	recommendation.Recommended = nil
	for index := range recommendation.Choices {
		choice := &recommendation.Choices[index]
		if choice.Enabled && !choice.CanTryWithoutSeeders {
			choice.Enabled = false
			choice.ReasonCode, choice.ReasonMessage = "follow_live_seeders_required", "此路线需要满足订阅的实时做种数限制"
		}
		if choice.Enabled && recommendation.Recommended == nil {
			recommendation.Recommended = &SourceRouteSelection{DownloaderID: choice.DownloaderID, MediaLibraryID: choice.MediaLibraryID}
		}
	}
	return recommendation
}

func rejectedFollowContent(err error) bool {
	switch ErrorCode(err) {
	case "resource_content_invalid_metainfo", "resource_content_limit_exceeded", "resource_content_unsafe_path", "resource_content_hash_mismatch", "resource_content_unsupported":
		return true
	}
	return false
}
