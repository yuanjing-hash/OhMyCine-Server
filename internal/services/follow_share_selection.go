package services

import (
	"context"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/mediarecognition"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

var followBareEpisodePattern = regexp.MustCompile(`^[0-9]{1,3}$`)
var followShareCounterPattern = regexp.MustCompile(`\s*[（(][0-9]{1,5}[)）]\s*$`)
var followShareSeasonDirectoryPattern = regexp.MustCompile(`(?i)^(?:season|s)[ ._-]*[0-9]{1,3}$|^第\s*[0-9零〇一二两兩三四五六七八九十百千]{1,12}\s*季$`)
var followShareExtraPattern = regexp.MustCompile(`(?i)(?:^|[ ./_()\[\]-])(?:samples?|extras?|trailers?|interviews?|bonus|bdmv|video_ts)(?:$|[ ./_()\[\]-])|样片|预告|花絮`)

// The title is supplied only after the identity-aware search has verified the
// candidate work and season. It provides context for numeric leaf names, never
// proof that the share contains an entire season.
func selectFollowShareFiles(entries []SiteSharePreviewEntry, season int, wanted []int, verifiedTitle ...string) ([]string, []int) {
	missing := map[int]bool{}
	for _, n := range wanted {
		missing[n] = true
	}
	covered := map[int]bool{}
	selected := []string{}
	stems := map[string][]string{}
	for _, entry := range entries {
		if entry.IsDir || !isVideoFile(entry.Path) || followShareExtraPattern.MatchString(entry.Path) || !followEpisodeDeclarationIsUnambiguous(path.Base(entry.Path), season) {
			continue
		}
		// File structure retains all parent-season facts; a caller's season hint
		// must never overwrite a conflicting Season 02 directory.
		structure, structureErr := mediarecognition.Parse(mediarecognition.InputFacts{Files: []mediarecognition.FileFact{{RelativePath: entry.Path}}, SourceKind: mediarecognition.SourceLibraryScan, MediaTypeHint: mediarecognition.MediaTypeTV})
		if structureErr != nil || structure.Structure.HasExtras || structure.Structure.HasBDMV || structure.Structure.HasVideoTS || structure.Episodes.SeasonMin != nil && (*structure.Episodes.SeasonMin != season || structure.Episodes.SeasonMax == nil || *structure.Episodes.SeasonMax != season) {
			continue
		}
		facts := mediarecognition.InputFacts{PackageName: path.Base(entry.Path), SourceKind: mediarecognition.SourceDownload, MediaTypeHint: mediarecognition.MediaTypeTV}
		parsed, e := mediarecognition.Parse(facts)
		if e == nil && parsed.Season == nil {
			facts.SeasonHint = &season
			parsed, e = mediarecognition.Parse(facts)
		}
		if e == nil && parsed.Episodes.EpisodeMin == nil && len(verifiedTitle) == 1 && followBareEpisodePattern.MatchString(strings.TrimSuffix(path.Base(entry.Path), path.Ext(entry.Path))) && followNumericShareContext(entry.Path, verifiedTitle[0], season) {
			// Reuse the recognizer's actual file facts, not digits from the share
			// title, folder counters, years or technical specifications.
			parsed.Episodes = structure.Episodes
			if parsed.Episodes.EpisodeMin == nil || *parsed.Episodes.EpisodeMin < 1 || parsed.Episodes.EpisodeMax == nil || *parsed.Episodes.EpisodeMax > 200 {
				continue
			}
		}
		if e != nil || parsed.Season == nil || *parsed.Season != season || parsed.Episodes.EpisodeMin == nil || parsed.Episodes.EpisodeMax == nil {
			continue
		}
		lo, hi := *parsed.Episodes.EpisodeMin, *parsed.Episodes.EpisodeMax
		if first, last, ok := explicitFollowEpisodeRange(path.Base(entry.Path), season); ok {
			lo, hi = first, last
		}
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

func followNumericShareContext(relative, verifiedTitle string, season int) bool {
	context, err := mediarecognition.Parse(mediarecognition.InputFacts{PackageName: verifiedTitle, SourceKind: mediarecognition.SourceDownload, MediaTypeHint: mediarecognition.MediaTypeTV})
	if err != nil || context.Season == nil || *context.Season != season || !followEpisodeDeclarationIsUnambiguous(verifiedTitle, season) {
		return false
	}
	work := identitySearchKey(context.CanonicalTitle)
	if work == "" {
		return false
	}
	for dir := path.Dir(relative); dir != "." && dir != "/"; dir = path.Dir(dir) {
		name := path.Base(dir)
		if !followEpisodeDeclarationIsUnambiguous(name, season) {
			return false
		}
		// Structural season folders have already been checked above. A different
		// work directory cannot borrow the enclosing search result's identity.
		if followShareSeasonDirectoryPattern.MatchString(name) {
			continue
		}
		parent, err := mediarecognition.Parse(mediarecognition.InputFacts{PackageName: followShareCounterPattern.ReplaceAllString(name, ""), SourceKind: mediarecognition.SourceDownload})
		if err != nil || parent.Season != nil && *parent.Season != season || parent.Episodes.EpisodeMin != nil {
			return false
		}
		key := identitySearchKey(parent.CanonicalTitle)
		if key == "" || key != work {
			return false
		}
	}
	return true
}

func (s *SiteService) prepareFollowShare(ctx context.Context, actor Actor, result, downloader string, season int, missing []int, verifiedTitle string) (string, []string, []int, string, error) {
	preview, e := s.PreviewShare(ctx, actor, result, downloader)
	if e != nil {
		return "", nil, nil, "", e
	}
	tokens, episodes := selectFollowShareFiles(preview.Entries, season, missing, verifiedTitle)
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
