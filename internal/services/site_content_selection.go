package services

import (
	"context"
	"math"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/resourcecontent"
	"gorm.io/gorm"
)

func (s *SiteService) ResolveContentSelection(ctx context.Context, actor Actor, input ContentSelectionInput, claim siteResultClaim) (ResolvedContentSelection, error) {
	var selected ResolvedContentSelection
	actor, err := s.inspectionActor(ctx, actor)
	if err != nil {
		return selected, err
	}
	if !actor.Can(authz.PermissionDownloadsCreate) {
		return selected, appError(CodePermissionDenied, "无权创建文件获取选择", nil)
	}
	if input.SelectionToken != "" {
		state := s.contents()
		state.mu.Lock()
		saved, ok := state.selections[input.SelectionToken]
		state.mu.Unlock()
		if !ok || saved.ActorID != actor.User.ID || !saved.ExpiresAt.After(s.now()) || saved.ResultToken != strings.TrimSpace(input.ResultToken) {
			return selected, contentInspectionExpired()
		}
		input.PreviewToken, input.SelectedEntryTokens = saved.PreviewToken, append([]string(nil), saved.EntryTokens...)
	}
	preview, err := s.resolveContentPreview(ctx, actor, claim, input.PreviewToken)
	if err != nil {
		return selected, err
	}
	if preview.ResultToken != strings.TrimSpace(input.ResultToken) || !preview.Summary.Complete || len(input.SelectedEntryTokens) == 0 || len(input.SelectedEntryTokens) > 500 {
		return selected, contentInspectionExpired()
	}
	byToken := map[string]SiteSharePreviewEntry{}
	for _, token := range input.SelectedEntryTokens {
		entry, ok := preview.Entries[token]
		if !ok {
			return selected, appError(CodeInvalidRequest, "文件不属于这次内容预览", nil)
		}
		if !entry.IsDir {
			byToken[token] = entry
			continue
		}
		for childToken, child := range preview.Entries {
			if !child.IsDir && strings.HasPrefix(child.Path, entry.Path+"/") {
				byToken[childToken] = child
			}
		}
	}
	if len(byToken) == 0 || len(byToken) > 500 {
		return selected, appError(CodeInvalidRequest, "请选择 1 到 500 个实际文件", nil)
	}
	tokens := make([]string, 0, len(byToken))
	for token := range byToken {
		tokens = append(tokens, token)
	}
	sort.Slice(tokens, func(i, j int) bool { return byToken[tokens[i]].Path < byToken[tokens[j]].Path })
	selected.SourceDigest = preview.SourceDigest
	selected.OriginalMagnet = preview.Read.OriginalMagnet
	if preview.Read.Manifest != nil {
		manifest := *preview.Read.Manifest
		metainfo, err := resourcecontent.BuildMetainfo(manifest)
		if err != nil {
			return selected, resourceContentError(err)
		}
		manifest.MetainfoBytes = metainfo
		selected.Manifest = &manifest
		selected.Torrent = &resourcecontent.Selection{Version: resourcecontent.SelectionVersion, Identity: manifest.Identity}
	} else {
		selected.Share = &cloud.ShareSelection{Version: 1}
	}
	for _, token := range tokens {
		entry := byToken[token]
		if entry.Size < 0 || selected.SelectedBytes > math.MaxInt64-entry.Size {
			return ResolvedContentSelection{}, appError(CodeInvalidRequest, "选中文件大小超出范围", nil)
		}
		selected.SelectedBytes += entry.Size
		selected.Files = append(selected.Files, entry)
		if selected.Torrent != nil {
			file, ok := preview.TorrentFiles[token]
			if !ok || file.Padding {
				return ResolvedContentSelection{}, contentInspectionExpired()
			}
			selected.Torrent.Files = append(selected.Torrent.Files, resourcecontent.SelectedFile{Index: file.Index, Path: file.Path, Size: file.Size})
		} else {
			file, ok := preview.Native[token]
			if !ok || file.IsDir {
				return ResolvedContentSelection{}, contentInspectionExpired()
			}
			selected.Share.Files = append(selected.Share.Files, file)
		}
	}
	if selected.Torrent != nil {
		if err := selected.Torrent.ValidateAgainst(*selected.Manifest); err != nil {
			return ResolvedContentSelection{}, contentInspectionExpired()
		}
		selected.AllFilesVerified = downloadpkg.SelectionCoversAll(*selected.Manifest, *selected.Torrent)
		selected.SelectionDigest, err = selected.Torrent.Digest()
	} else {
		selected.SelectionDigest, err = selected.Share.Digest()
	}
	if err != nil {
		return ResolvedContentSelection{}, appError(CodeInvalidRequest, "选中文件清单无效", nil)
	}
	selected.Guard = func(tx *gorm.DB) error {
		if !preview.ExpiresAt.After(s.now()) || !claim.ExpiresAt.After(s.now()) {
			return contentInspectionExpired()
		}
		current, err := NewAuthorizationService(tx).resolveWithDB(tx, actor.User.ID)
		if err != nil || current.User.AuthzVersion != actor.User.AuthzVersion || inspectionAuthorizationScope(current) != inspectionAuthorizationScope(actor) || !current.Can(authz.PermissionDownloadsCreate) {
			return contentInspectionExpired()
		}
		var site models.Site
		if tx.First(&site, claim.SiteID).Error != nil || !site.Enabled || contentSourceDigest(claim, site) != preview.SourceDigest {
			return contentInspectionExpired()
		}
		lifecycle, err := s.contentPluginLifecycle(tx, actor, claim, site)
		if err != nil || lifecycle != preview.Read.PluginLifecycle {
			return contentInspectionExpired()
		}
		if preview.Scope != nil {
			scope, err := s.providerInspectionScopeWithDB(tx, actor, claim, preview.Scope.PreferredDownloader)
			if err != nil || scope.Digest != preview.Scope.Digest {
				return contentInspectionExpired()
			}
		}
		return nil
	}
	if err := selected.Guard(s.db.WithContext(ctx)); err != nil {
		return ResolvedContentSelection{}, err
	}
	return selected, nil
}

func (s *SiteService) SelectContent(ctx context.Context, actor Actor, input ContentSelectionInput) (ContentSelectionSummary, error) {
	var summary ContentSelectionSummary
	actor, err := s.inspectionActor(ctx, actor)
	if err != nil {
		return summary, err
	}
	claim, _, err := s.contentClaim(ctx, actor, input.ResultToken)
	if err != nil {
		return summary, err
	}
	selection, err := s.ResolveContentSelection(ctx, actor, input, claim)
	if err != nil {
		return summary, err
	}
	preview, err := s.resolveContentPreview(ctx, actor, claim, input.PreviewToken)
	if err != nil {
		return summary, err
	}
	summary = ContentSelectionSummary{Token: "selection-" + uuid.NewString(), PreviewToken: input.PreviewToken, SelectedEntryTokens: append([]string(nil), input.SelectedEntryTokens...), FileCount: len(selection.Files), TotalSize: selection.SelectedBytes, SelectedBytes: selection.SelectedBytes}
	for _, file := range selection.Files {
		if contentFileKind(file) == "video" {
			summary.VideoCount++
		}
	}
	state := s.contents()
	state.mu.Lock()
	defer state.mu.Unlock()
	for token, saved := range state.selections {
		if !saved.ExpiresAt.After(s.now()) {
			delete(state.selections, token)
		}
	}
	if len(state.selections) >= 128 {
		return ContentSelectionSummary{}, appError(CodeSiteUnavailable, "有效选择较多，请稍后重试", nil)
	}
	state.selections[summary.Token] = siteContentSelection{ActorID: actor.User.ID, ResultToken: strings.TrimSpace(input.ResultToken), PreviewToken: input.PreviewToken, EntryTokens: append([]string(nil), input.SelectedEntryTokens...), ExpiresAt: preview.ExpiresAt}
	return summary, nil
}

func (s *SiteService) reserveContentSubmission(actor Actor, token, digest string) error {
	state := s.contents()
	state.mu.Lock()
	defer state.mu.Unlock()
	saved, ok := state.selections[token]
	if !ok || saved.ActorID != actor.User.ID || !saved.ExpiresAt.After(s.now()) || saved.SubmissionDigest != "" && saved.SubmissionDigest != digest {
		return contentInspectionExpired()
	}
	saved.SubmissionDigest = digest
	state.selections[token] = saved
	return nil
}

func (s *SiteService) bindExpectedContentSelection(ctx context.Context, actor Actor, claim siteResultClaim, token string, expected siteShareIdentity, input SiteDownloadInput) error {
	if strings.TrimSpace(input.ResultToken) != token {
		return contentInspectionExpired()
	}
	selected, err := s.ResolveContentSelection(ctx, actor, ContentSelectionInput{ResultToken: token, SelectionToken: input.ContentSelectionToken}, claim)
	if err != nil {
		return err
	}
	actual := claim
	actual.SearchIdentity = &expected
	if _, err = s.ValidateSelectedContent(ctx, actor, actual, selected, &DownloadRecognitionIdentity{TMDBID: expected.TMDBID, MediaType: expected.MediaType, Season: cloneInt(expected.Season)}); err != nil {
		return err
	}
	if err = selected.Guard(s.db.WithContext(ctx)); err != nil {
		return err
	}
	return s.bindClaimRecognition(token, actor.User.ID, expected.TMDBID, expected.MediaType, mediaIdentitySourceDirectID, mediaIdentityStatusVerified, false)
}

// ValidateSelectedContent corroborates only actual selected leaves against a
// verified intended work. It does not supply episode facts from an advertisement.
// Washing may additionally require exactly one video and one exact coordinate.
func (s *SiteService) ValidateSelectedContent(ctx context.Context, actor Actor, claim siteResultClaim, selected ResolvedContentSelection, expected *DownloadRecognitionIdentity) (*SiteShareAnalysis, error) {
	if expected == nil || expected.TMDBID <= 0 {
		return nil, appError(CodeSiteResultIdentityMismatch, "缺少已核实的目标作品", nil)
	}
	identity := claim.SearchIdentity
	if identity == nil || identity.TMDBID != expected.TMDBID || identity.MediaType != expected.MediaType {
		return nil, appError(CodeSiteResultIdentityMismatch, "目标作品与结果身份不一致", nil)
	}
	analysis, _ := s.analyzeShareEntries(ctx, actor, claim, selected.Files)
	if analysis == nil || !analysis.Complete || analysis.VideoCount == 0 || analysis.UnresolvedCount != 0 {
		return analysis, appError(CodeSiteResultIdentityMismatch, "所选实际视频的作品或季集尚未核实", nil)
	}
	for _, group := range analysis.Groups {
		if group.Status != mediaRecognitionStatusMatched || group.TMDBID == nil || *group.TMDBID != expected.TMDBID || group.MediaType != expected.MediaType {
			return analysis, appError(CodeSiteResultIdentityMismatch, "所选文件包含其他作品", nil)
		}
		if expected.MediaType == "tv" && expected.Season != nil && !equalInt(group.Season, expected.Season) {
			return analysis, appError(CodeSiteResultIdentityMismatch, "所选视频包含其他季", nil)
		}
	}
	return analysis, nil
}
