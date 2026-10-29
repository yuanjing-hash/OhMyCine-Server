package services

import (
	"context"
	"fmt"
	"sync"
)

type preparedFollowCandidate struct {
	Candidate                           followCandidate
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
	var wg sync.WaitGroup
	permits := make(chan struct{}, 4)
	for index, candidate := range candidates {
		wg.Add(1)
		go func(i int, candidate followCandidate) {
			defer wg.Done()
			prepared := preparedFollowCandidate{Candidate: candidate}
			defer func() { result[i] = prepared }()
			select {
			case permits <- struct{}{}:
			case <-ctx.Done():
				prepared.Err = ctx.Err()
				return
			}
			defer func() { <-permits }()
			episodes := candidateMissingEpisodes(candidate, missing)
			if len(episodes) == 0 {
				prepared.Err = appError(CodeDownloadSourceInvalid, "该资源不覆盖本次缺集", nil)
				return
			}
			libraryID := snapshot.MediaLibraryID
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
			recommendation, err := w.sites.RecommendSiteRoute(ctx, actor, SourceRouteRecommendationInput{ResultToken: candidate.Item.Token, MediaLibraryID: &libraryID, ContentSelectionToken: prepared.ContentSelectionToken})
			if err != nil {
				prepared.Err = err
				return
			}
			if recommendation.Recommended == nil {
				prepared.Err = appError(CodeDownloaderUnavailable, "没有可用的下载器与目标媒体库路线", nil)
				return
			}
			prepared.Recommendation = recommendation
			// Compatibility for an old result contract; never use this branch after a
			// new manifest failed. Current native search results advertise inspection.
			if !candidate.Item.ContentInspection && isCloudShareSource(candidate.Item.SourceKind) {
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
