package services

import (
	"context"
	"path"
	"sort"
	"strings"
)

type missingContentSelection struct {
	EntryTokens          []string
	Episodes             []int
	SubtitleRank         int
	UnconfirmedSubtitles int
}

// Provider-neutral selection over actual inspected leaves. Advertised title
// ranges and a downloader's file ordering never contribute coordinates.
func selectMissingContent(preview siteContentPreview, identity *siteShareIdentity, season int, wanted []int, policy SubtitlePolicy) missingContentSelection {
	result := missingContentSelection{EntryTokens: []string{}, Episodes: []int{}}
	if identity == nil || identity.MediaType != "tv" || !preview.Summary.Complete || preview.Summary.Analysis == nil || !preview.Summary.Analysis.Complete {
		return result
	}
	missing := map[int]bool{}
	for _, episode := range wanted {
		missing[episode] = true
	}
	facts := map[string]ContentFileFacts{}
	for _, file := range preview.Summary.Files {
		facts[file.Token] = file
	}
	type eligibleVideo struct {
		file      siteShareAnalyzedFile
		rank      int
		confirmed bool
		sidecars  []string
	}
	videos := []eligibleVideo{}
	for _, file := range preview.AnalysisFiles {
		if file.Group < 0 || file.Group >= len(preview.Summary.Analysis.Groups) || len(file.Episodes) == 0 {
			continue
		}
		group := preview.Summary.Analysis.Groups[file.Group]
		if group.Status != mediaRecognitionStatusMatched || group.TMDBID == nil || *group.TMDBID != identity.TMDBID || group.MediaType != "tv" || group.Season == nil || *group.Season != season {
			continue
		}
		allowed := true
		for _, ep := range file.Episodes {
			if !missing[ep] {
				allowed = false
				break
			}
		}
		if !allowed {
			continue
		}
		video := eligibleVideo{file: file, rank: len(policy.Languages)*2 + 2}
		entry := preview.Entries[file.Token]
		stems := map[string][]string{strings.ToLower(path.Dir(entry.Path)): {strings.ToLower(strings.TrimSuffix(path.Base(entry.Path), path.Ext(entry.Path)))}}
		evidence := facts[file.Token].Subtitles
		for token, sidecar := range preview.Entries {
			if !sidecar.IsDir && !isVideoFile(sidecar.Path) && sidecarBelongsToAcceptedMedia(sidecar.Path, stems) {
				video.sidecars = append(video.sidecars, token)
				if contentFileKind(sidecar) == "subtitle" {
					evidence = append(evidence, contentSubtitleEvidence(sidecar)...)
				}
			}
		}
		for _, proof := range evidence {
			for preference, language := range policy.Languages {
				expected := proof.Language
				if proof.Script != "" {
					expected += "-" + proof.Script
				}
				if expected != language {
					if policy.Mode == "prefer" && proof.Language == "zh" && proof.Script == "" && strings.HasPrefix(language, "zh-") {
						score := len(policy.Languages) * 2
						if proof.Source != "sidecar_filename" {
							score++
						}
						if score < video.rank {
							video.rank = score
						}
					}
					continue
				}
				score := preference*2 + 1
				if proof.Source == "sidecar_filename" {
					score = preference * 2
					video.confirmed = true
				}
				if score < video.rank {
					video.rank = score
				}
			}
		}
		if policy.Mode == "require" && !video.confirmed {
			continue
		}
		videos = append(videos, video)
	}
	sort.SliceStable(videos, func(i, j int) bool {
		if policy.Mode != "ignore" && videos[i].rank != videos[j].rank {
			return videos[i].rank < videos[j].rank
		}
		if len(videos[i].file.Episodes) != len(videos[j].file.Episodes) {
			return len(videos[i].file.Episodes) > len(videos[j].file.Episodes)
		}
		return videos[i].file.Path < videos[j].file.Path
	})
	tokens, covered := map[string]bool{}, map[int]bool{}
	for _, video := range videos {
		// A second encode of the same episode is an alternative, not another gap.
		overlaps := false
		for _, episode := range video.file.Episodes {
			if covered[episode] {
				overlaps = true
			}
		}
		if overlaps {
			continue
		}
		tokens[video.file.Token] = true
		for _, token := range video.sidecars {
			tokens[token] = true
		}
		for _, episode := range video.file.Episodes {
			covered[episode] = true
		}
		if policy.Mode != "ignore" {
			result.SubtitleRank += video.rank
			if !video.confirmed {
				result.UnconfirmedSubtitles++
			}
		}
	}
	for token := range tokens {
		result.EntryTokens = append(result.EntryTokens, token)
	}
	for episode := range covered {
		result.Episodes = append(result.Episodes, episode)
	}
	sort.Strings(result.EntryTokens)
	sort.Ints(result.Episodes)
	return result
}

type preparedFollowContent struct {
	SelectionToken                     string
	Episodes                           []int
	Digest                             string
	SelectedBytes                      int64
	SubtitleRank, UnconfirmedSubtitles int
}

func (s *SiteService) prepareFollowContent(ctx context.Context, actor Actor, result string, season int, wanted []int, policy SubtitlePolicy) (preparedFollowContent, error) {
	preview, err := s.PreviewContent(ctx, actor, ContentPreviewInput{ResultToken: result})
	if err != nil {
		return preparedFollowContent{}, err
	}
	keep, receiptToken := false, ""
	defer func() {
		if !keep {
			if receiptToken != "" {
				s.discardContentPreparation(actor, receiptToken)
			}
			s.discardContentPreview(actor, preview.Token)
		}
	}()
	claim, _, err := s.contentClaim(ctx, actor, result)
	if err != nil {
		return preparedFollowContent{}, err
	}
	frozen, err := s.resolveContentPreview(ctx, actor, claim, preview.Token)
	if err != nil {
		return preparedFollowContent{}, err
	}
	selection := selectMissingContent(frozen, claim.SearchIdentity, season, wanted, policy)
	if len(selection.Episodes) == 0 {
		if frozen.Summary.Analysis != nil && frozen.Summary.Analysis.UnresolvedCount == 0 {
			return preparedFollowContent{}, appError(codeContentNoMissingFiles, "已完整检查，实际文件没有符合本次缺集及字幕策略的视频", nil)
		}
		return preparedFollowContent{}, appError(CodeDownloadSourceInvalid, "实际清单中没有已核实且符合缺集及字幕策略的视频文件", nil)
	}
	receipt, err := s.SelectContent(ctx, actor, ContentSelectionInput{ResultToken: result, PreviewToken: preview.Token, SelectedEntryTokens: selection.EntryTokens})
	if err != nil {
		return preparedFollowContent{}, err
	}
	receiptToken = receipt.Token
	resolved, err := s.ResolveContentSelection(ctx, actor, ContentSelectionInput{ResultToken: result, SelectionToken: receipt.Token}, claim)
	if err != nil {
		return preparedFollowContent{}, err
	}
	if _, err := s.ValidateSelectedContent(ctx, actor, claim, resolved, &DownloadRecognitionIdentity{TMDBID: claim.SearchIdentity.TMDBID, MediaType: "tv", Season: &season}); err != nil {
		return preparedFollowContent{}, err
	}
	if err := s.bindClaimRecognition(result, actor.User.ID, claim.SearchIdentity.TMDBID, "tv", mediaIdentitySourceDirectID, mediaIdentityStatusVerified, false); err != nil {
		return preparedFollowContent{}, err
	}
	keep = true
	return preparedFollowContent{SelectionToken: receipt.Token, Episodes: selection.Episodes, Digest: resolved.SelectionDigest, SelectedBytes: resolved.SelectedBytes, SubtitleRank: selection.SubtitleRank, UnconfirmedSubtitles: selection.UnconfirmedSubtitles}, nil
}

// Only abandon a read-only preparation. An acquisition reservation is retained
// until its original outcome is reconciled, even if its caller lost the reply.
func (s *SiteService) discardContentPreparation(actor Actor, token string) {
	state := s.contents()
	state.mu.Lock()
	defer state.mu.Unlock()
	saved, ok := state.selections[token]
	if ok && saved.ActorID == actor.User.ID && saved.SubmissionDigest == "" {
		delete(state.selections, token)
		s.discardContentPreviewLocked(state, actor, saved.PreviewToken)
	}
}

func (s *SiteService) discardContentPreview(actor Actor, token string) {
	state := s.contents()
	state.mu.Lock()
	defer state.mu.Unlock()
	s.discardContentPreviewLocked(state, actor, token)
}
func (s *SiteService) discardContentPreviewLocked(state *siteContentInspection, actor Actor, token string) {
	preview, ok := state.previews[token]
	if !ok || preview.ActorID != actor.User.ID {
		return
	}
	for _, selection := range state.selections {
		if selection.PreviewToken == token && selection.ExpiresAt.After(s.now()) {
			return
		}
	}
	delete(state.previews, token)
}
