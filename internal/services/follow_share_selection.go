package services

import (
	"context"
	"path"
	"sort"
	"strings"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/mediarecognition"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

// selectFollowShareFiles proves coverage from individual file names. A share
// title is insufficient evidence that every episode in a season already exists.
func selectFollowShareFiles(entries []SiteSharePreviewEntry, season int, wanted []int) ([]string, []int) {
	missing := map[int]bool{}
	for _, n := range wanted {
		missing[n] = true
	}
	covered := map[int]bool{}
	selected := []string{}
	stems := map[string][]string{}
	for _, entry := range entries {
		if entry.IsDir || !isVideoFile(entry.Path) {
			continue
		}
		facts := mediarecognition.InputFacts{PackageName: path.Base(entry.Path), SourceKind: mediarecognition.SourceDownload, MediaTypeHint: mediarecognition.MediaTypeTV}
		parsed, e := mediarecognition.Parse(facts)
		if e == nil && parsed.Season == nil {
			facts.SeasonHint = &season
			parsed, e = mediarecognition.Parse(facts)
		}
		if e != nil || parsed.Season == nil || *parsed.Season != season || parsed.Episodes.EpisodeMin == nil || parsed.Episodes.EpisodeMax == nil {
			continue
		}
		lo, hi := *parsed.Episodes.EpisodeMin, *parsed.Episodes.EpisodeMax
		if lo < 1 || hi < lo || hi-lo > 200 {
			continue
		}
		all := true
		for ep := lo; ep <= hi; ep++ {
			if !missing[ep] {
				all = false
				break
			}
		}
		if !all {
			continue
		}
		selected = append(selected, entry.Token)
		for ep := lo; ep <= hi; ep++ {
			covered[ep] = true
		}
		dir := strings.ToLower(path.Dir(entry.Path))
		stems[dir] = append(stems[dir], strings.ToLower(strings.TrimSuffix(path.Base(entry.Path), path.Ext(entry.Path))))
	}
	for _, entry := range entries {
		if !entry.IsDir && !isVideoFile(entry.Path) && sidecarBelongsToAcceptedMedia(entry.Path, stems) {
			selected = append(selected, entry.Token)
		}
	}
	episodes := []int{}
	for n := range covered {
		episodes = append(episodes, n)
	}
	sort.Ints(episodes)
	sort.Strings(selected)
	return selected, episodes
}

func (s *SiteService) prepareFollowShare(ctx context.Context, actor Actor, result, downloader string, season int, missing []int) (string, []string, []int, string, error) {
	preview, e := s.PreviewShare(ctx, actor, result, downloader)
	if e != nil {
		return "", nil, nil, "", e
	}
	tokens, episodes := selectFollowShareFiles(preview.Entries, season, missing)
	if len(episodes) == 0 {
		return "", nil, nil, "", appError(CodeDownloadSourceInvalid, "分享中没有已核实且符合缺集范围的视频文件", nil)
	}
	s.vaultMu.Lock()
	frozen, ok := s.sharePreviews[preview.Token]
	s.vaultMu.Unlock()
	if !ok {
		return "", nil, nil, "", sharePreviewExpired()
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
	return preview.Token, tokens, episodes, digest, nil
}
