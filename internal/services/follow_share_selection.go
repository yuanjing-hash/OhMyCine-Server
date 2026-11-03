package services

import (
	"context"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

var followBareEpisodePattern = regexp.MustCompile(`^[0-9]{1,3}$`)
var followShareCounterPattern = regexp.MustCompile(`\s*[（(][0-9]{1,5}[)）]\s*$`)
var followShareSeasonDirectoryPattern = regexp.MustCompile(`(?i)^(?:season|s)[ ._-]*[0-9]{1,3}$|^第\s*[0-9零〇一二两兩三四五六七八九十百千]{1,12}\s*季$`)
var followShareExtraPattern = regexp.MustCompile(`(?i)(?:^|[ ./_()\[\]-])(?:samples?|extras?|trailers?|interviews?|bonus|bdmv|video_ts)(?:$|[ ./_()\[\]-])|样片|预告|花絮`)

func (s *SiteService) prepareFollowShare(ctx context.Context, actor Actor, result, downloader string, season int, missing []int, _ string) (string, []string, []int, string, error) {
	preview, e := s.PreviewShare(ctx, actor, result, downloader)
	if e != nil {
		return "", nil, nil, "", e
	}
	keep := false
	defer func() {
		if keep {
			return
		}
		s.vaultMu.Lock()
		defer s.vaultMu.Unlock()
		if saved, ok := s.sharePreviews[preview.Token]; ok && saved.ActorID == actor.User.ID && saved.SubmissionDigest == "" {
			delete(s.sharePreviews, preview.Token)
		}
	}()
	s.vaultMu.Lock()
	frozen, ok := s.sharePreviews[preview.Token]
	s.vaultMu.Unlock()
	if !ok {
		return "", nil, nil, "", sharePreviewExpired()
	}
	claim, e := s.sharePreviewClaim(ctx, actor, result)
	if e != nil {
		return "", nil, nil, "", e
	}
	tokens, episodes := selectAnalyzedFollowFiles(preview, frozen.AnalysisFiles, claim.SearchIdentity, season, missing)
	if len(episodes) == 0 {
		return "", nil, nil, "", appError(CodeDownloadSourceInvalid, "分享中没有已核实且符合缺集范围的视频文件", nil)
	}
	if e := s.bindClaimRecognition(result, actor.User.ID, claim.SearchIdentity.TMDBID, "tv", mediaIdentitySourceDirectID, mediaIdentityStatusVerified, false); e != nil {
		return "", nil, nil, "", e
	}
	selection := cloud.ShareSelection{Version: 1}
	for _, token := range tokens {
		entry, ok := frozen.Entries[token]
		if !ok {
			return "", nil, nil, "", sharePreviewExpired()
		}
		selection.Files = append(selection.Files, entry)
	}
	digest, e := selection.Digest()
	if e != nil {
		return "", nil, nil, "", appError(CodeDownloadSourceInvalid, "分享中的文件选择无法安全提交", nil)
	}
	keep = true
	return preview.Token, tokens, episodes, digest, nil
}

func selectAnalyzedFollowFiles(preview SiteSharePreviewSummary, files []siteShareAnalyzedFile, identity *siteShareIdentity, season int, wanted []int) ([]string, []int) {
	selected, episodes := []string{}, []int{}
	if identity == nil || identity.MediaType != "tv" || preview.ShareAnalysis == nil || !preview.ShareAnalysis.Complete {
		return selected, episodes
	}
	missing, covered := map[int]bool{}, map[int]bool{}
	for _, episode := range wanted {
		missing[episode] = true
	}
	stems := map[string][]string{}
	for _, file := range files {
		if file.Group < 0 || file.Group >= len(preview.ShareAnalysis.Groups) {
			continue
		}
		group := preview.ShareAnalysis.Groups[file.Group]
		if group.Status != mediaRecognitionStatusMatched || group.TMDBID == nil || *group.TMDBID != identity.TMDBID || group.MediaType != "tv" || group.Season == nil || *group.Season != season || len(file.Episodes) == 0 {
			continue
		}
		all := true
		for _, episode := range file.Episodes {
			if !missing[episode] {
				all = false
				break
			}
		}
		if !all {
			continue
		}
		selected = append(selected, file.Token)
		for _, episode := range file.Episodes {
			covered[episode] = true
		}
		dir := strings.ToLower(path.Dir(file.Path))
		stems[dir] = append(stems[dir], strings.ToLower(strings.TrimSuffix(path.Base(file.Path), path.Ext(file.Path))))
	}
	for _, entry := range preview.Entries {
		if !entry.IsDir && !isVideoFile(entry.Path) && sidecarBelongsToAcceptedMedia(entry.Path, stems) {
			selected = append(selected, entry.Token)
		}
	}
	for episode := range covered {
		episodes = append(episodes, episode)
	}
	sort.Ints(episodes)
	sort.Strings(selected)
	return selected, episodes
}
