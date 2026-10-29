package services

import (
	"context"
	"path"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/resourcecontent"
)

// The actual selected file analyzer is shared with Share preview and Follow.
// A search poster/title supplies context only. It cannot turn a season pack
// into evidence that a requested episode has been selected.
func (s *SiteService) validateUpgradeSource(ctx context.Context, actor Actor, claim siteResultClaim, source DownloadSourceInput, expected *DownloadRecognitionIdentity) error {
	if expected == nil || expected.TMDBID <= 0 {
		return mediaUpgradeSelectionError()
	}
	identity := claim.SearchIdentity
	if identity == nil || identity.TMDBID != expected.TMDBID || identity.MediaType != expected.MediaType {
		return mediaUpgradeSelectionError()
	}
	resolved := ResolvedContentSelection{}
	if source.ShareSelection != nil {
		if err := source.ShareSelection.Validate(); err != nil {
			return mediaUpgradeSelectionError()
		}
		resolved.Share = source.ShareSelection
		for _, file := range source.ShareSelection.Files {
			resolved.Files = append(resolved.Files, SiteSharePreviewEntry{Path: file.RelativePath, Name: path.Base(file.RelativePath), Size: file.Size})
		}
	} else if len(source.Torrent) > 0 {
		manifest, err := resourcecontent.ParseTorrent(source.Torrent, resourcecontent.Limits{})
		if err != nil {
			return mediaUpgradeSelectionError()
		}
		resolved.Manifest = &manifest
		resolved.Torrent = source.ContentSelection
		selected := map[int]bool{}
		if source.ContentSelection != nil {
			if err := source.ContentSelection.ValidateAgainst(manifest); err != nil {
				return mediaUpgradeSelectionError()
			}
			for _, file := range source.ContentSelection.Files {
				selected[file.Index] = true
			}
		}
		for _, file := range manifest.Files {
			if file.Padding || source.ContentSelection != nil && !selected[file.Index] {
				continue
			}
			relative := file.Path
			if manifest.MultiFile {
				relative = path.Join(manifest.Name, file.Path)
			}
			resolved.Files = append(resolved.Files, SiteSharePreviewEntry{Path: relative, Name: path.Base(file.Path), Size: file.Size})
		}
	} else {
		return appError(CodeMediaUpgradeSelection, "请先完整读取实际文件并选择一个对应版本，再洗版", nil)
	}
	videos := 0
	for _, file := range resolved.Files {
		if isVideoFile(file.Path) {
			videos++
		}
	}
	if videos != 1 {
		return mediaUpgradeSelectionError()
	}
	analysis, err := s.ValidateSelectedContent(ctx, actor, claim, resolved, expected)
	if err != nil {
		return err
	}
	if analysis == nil || !analysis.Complete || analysis.VideoCount != 1 || analysis.UnresolvedCount != 0 || len(analysis.Groups) != 1 {
		return mediaUpgradeSelectionError()
	}
	group := analysis.Groups[0]
	if group.Status != mediaRecognitionStatusMatched || group.TMDBID == nil || *group.TMDBID != expected.TMDBID || group.MediaType != expected.MediaType {
		return mediaUpgradeSelectionError()
	}
	if expected.MediaType == "tv" && (!equalInt(group.Season, expected.Season) || len(group.Episodes) != 1 || expected.Episode == nil || group.Episodes[0] != *expected.Episode) {
		return mediaUpgradeSelectionError()
	}
	return nil
}
