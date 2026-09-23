package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/pan115"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/site/builtin"
	"gorm.io/gorm"
)

type SiteSharePreviewEntry struct {
	Token string `json:"token"`
	Path  string `json:"path"`
	Name  string `json:"name"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size"`
}
type SiteSharePreviewSummary struct {
	ShareValidation *SiteShareValidation    `json:"share_validation,omitempty"`
	Token           string                  `json:"token"`
	Entries         []SiteSharePreviewEntry `json:"entries"`
	TotalSize       int64                   `json:"total_size"`
	FileCount       int                     `json:"file_count"`
	ExpiresAt       time.Time               `json:"expires_at"`
}
type siteSharePreview struct {
	ActorID                                      uint
	ResultToken, DownloaderID, ConfigDigest, URL string
	ExpiresAt                                    time.Time
	Entries                                      map[string]cloud.ShareTreeItem
	SubmissionDigest                             string
}

func sharePreviewExpired() error {
	return appError(CodeSiteResultExpired, "分享预览已过期或配置已变化，请重新预览", nil)
}

// Bind only relevant configuration, excluding probe timestamps and health updates.
func sharePreviewConfig(db *gorm.DB, downloaderID string) (string, uint, error) {
	var d models.Downloader
	if err := db.First(&d, "id = ?", downloaderID).Error; err != nil || !d.Enabled || d.Type != models.DownloaderTypePan115Offline || d.StorageID == nil {
		return "", 0, appError(CodeDownloaderUnavailable, "请选择已启用的 115 下载器", nil)
	}
	var storage models.Storage
	if err := db.First(&storage, *d.StorageID).Error; err != nil || !storage.Enabled || storage.Type != models.StorageTypePan115 || storage.ConnectionID == nil {
		return "", 0, appError(CodeDownloaderStorageUnavailable, "下载器的 115 数据源不可用", nil)
	}
	var connection models.Connection
	if err := db.First(&connection, *storage.ConnectionID).Error; err != nil || !connection.Enabled || connection.Provider != cloud.ProviderPan115 {
		return "", 0, appError(CodeConnectionUnavailable, "下载器绑定的 115 连接不可用", nil)
	}
	raw, _ := json.Marshal([]any{d.ID, d.OwnerID, d.ExecutionLocation, d.NodeID, d.ProviderDirectoryID, storage.ID, storage.RootPath, connection.ID, connection.Revision})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), connection.ID, nil
}

func (s *SiteService) sharePreviewClaim(ctx context.Context, actor Actor, token string) (siteResultClaim, error) {
	if !actor.Can(authz.PermissionDiscoveryRead) || !actor.Can(authz.PermissionDownloadsCreate) {
		return siteResultClaim{}, appError(CodePermissionDenied, "无权预览分享内容", nil)
	}
	claim, err := s.resolveAvailableClaim(strings.TrimSpace(token), actor.User.ID)
	if err != nil {
		return claim, err
	}
	if !actor.CanResource(authz.PermissionDiscoveryRead, models.AuthorizationResourceSite, uintID(claim.SiteID)) {
		return claim, appError(CodePermissionDenied, "无权访问这个站点", nil)
	}
	var site models.Site
	if err := s.db.WithContext(ctx).First(&site, claim.SiteID).Error; err != nil {
		return claim, siteNotFound(err)
	}
	def, ok := builtin.DefinitionForKey(site.Kind)
	if !ok || def.SiteType != builtin.SiteTypeCloud {
		return claim, appError(CodeInvalidRequest, "此资源不支持分享预览", nil)
	}
	if !site.Enabled || site.Revision != claim.SiteRevision {
		return claim, sharePreviewExpired()
	}
	return claim, nil
}

func (s *SiteService) acquireSharePreview(ctx context.Context) (func(), error) {
	s.vaultMu.Lock()
	if s.sharePreviewSlots == nil {
		s.sharePreviewSlots = make(chan struct{}, 4)
	}
	slots := s.sharePreviewSlots
	s.vaultMu.Unlock()
	select {
	case slots <- struct{}{}:
		return func() { <-slots }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *SiteService) PreviewShare(ctx context.Context, actor Actor, resultToken, downloaderID string, requests ...RequestContext) (SiteSharePreviewSummary, error) {
	var summary SiteSharePreviewSummary
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	claim, err := s.sharePreviewClaim(ctx, actor, resultToken)
	if err != nil {
		return summary, err
	}
	raw, _, err := pan115.NormalizeShareLink(claim.TorrentID, "")
	if err != nil {
		return summary, appError(CodeDownloadSourceInvalid, "分享地址无效", nil)
	}
	request := RequestContext{}
	if len(requests) > 0 {
		request = requests[0]
	}
	entries, digest, validation, err := s.inspectResultShare(ctx, actor, claim, raw, downloaderID, true, request)
	if err != nil {
		return summary, err
	}
	if _, err = s.sharePreviewClaim(ctx, actor, resultToken); err != nil {
		return summary, err
	}
	current, _, err := sharePreviewConfig(s.db.WithContext(ctx), downloaderID)
	if err != nil || current != digest {
		return summary, sharePreviewExpired()
	}
	expiry := s.now().Add(5 * time.Minute)
	if claim.ExpiresAt.Before(expiry) {
		expiry = claim.ExpiresAt
	}
	frozen := siteSharePreview{ActorID: actor.User.ID, ResultToken: strings.TrimSpace(resultToken), DownloaderID: downloaderID, ConfigDigest: digest, URL: raw, ExpiresAt: expiry, Entries: make(map[string]cloud.ShareTreeItem, len(entries))}
	summary = SiteSharePreviewSummary{ShareValidation: &validation, Token: uuid.NewString(), ExpiresAt: expiry, Entries: make([]SiteSharePreviewEntry, 0, len(entries))}
	sort.Slice(entries, func(i, j int) bool { return entries[i].RelativePath < entries[j].RelativePath })
	for _, item := range entries {
		token := uuid.NewString()
		frozen.Entries[token] = item
		summary.Entries = append(summary.Entries, SiteSharePreviewEntry{Token: token, Path: item.RelativePath, Name: path.Base(item.RelativePath), IsDir: item.IsDir, Size: item.Size})
		if !item.IsDir {
			if summary.TotalSize > math.MaxInt64-item.Size {
				return SiteSharePreviewSummary{}, appError(CodeInvalidRequest, "分享大小超出范围", nil)
			}
			summary.TotalSize += item.Size
			summary.FileCount++
		}
	}
	// Folder sizes are exact descendant totals, never provider estimates.
	for i := range summary.Entries {
		if !summary.Entries[i].IsDir {
			continue
		}
		prefix := summary.Entries[i].Path + "/"
		for _, entry := range summary.Entries {
			if !entry.IsDir && strings.HasPrefix(entry.Path, prefix) {
				summary.Entries[i].Size += entry.Size
			}
		}
	}
	s.vaultMu.Lock()
	defer s.vaultMu.Unlock()
	if s.sharePreviews == nil {
		s.sharePreviews = map[string]siteSharePreview{}
	}
	for token, item := range s.sharePreviews {
		if !item.ExpiresAt.After(s.now()) || (item.ActorID == actor.User.ID && item.ResultToken == resultToken && item.DownloaderID == downloaderID && item.SubmissionDigest == "") {
			delete(s.sharePreviews, token)
		}
	}
	if len(s.sharePreviews) >= 32 {
		return SiteSharePreviewSummary{}, appError(CodeSiteUnavailable, "分享预览较多，请稍后重试", nil)
	}
	s.sharePreviews[summary.Token] = frozen
	return summary, nil
}
func sharePreviewProviderError(err error) error {
	code, _ := cloud.ErrorInfo(err)
	message := "暂时无法读取 115 分享，请稍后重试"
	switch code {
	case cloud.CodeShareExpired:
		message = "分享已失效或已被取消"
	case cloud.CodeSharePassword:
		message = "分享提取码有误，请核对原帖后重新搜索"
	case cloud.CodeAuthExpired, cloud.CodeCookieInvalid:
		message = "115 账号登录已失效，请重新登录后验证分享"
	case cloud.CodeRateLimited:
		message = "115 请求受到限制，正在冷却，请稍后重新验证"
	case cloud.CodeResponseInvalid:
		message = "115 分享响应格式异常，暂时无法验证"
	case cloud.CodeShareInvalid:
		message = "分享地址或文件结构无效，无法安全读取"
	case cloud.CodeShareEmpty:
		message = "分享内没有可读取的文件"
	case cloud.CodeShareTooLarge:
		message = "分享内容过多，暂时无法完整预览，请选择更小的分享"
	}
	return appError(code, message, nil)
}
func (s *SiteService) resolveSharePreview(ctx context.Context, actor Actor, token string) (siteSharePreview, error) {
	s.vaultMu.Lock()
	preview, ok := s.sharePreviews[strings.TrimSpace(token)]
	s.vaultMu.Unlock()
	if !ok || preview.ActorID != actor.User.ID || !preview.ExpiresAt.After(s.now()) {
		return preview, sharePreviewExpired()
	}
	if _, err := s.sharePreviewClaim(ctx, actor, preview.ResultToken); err != nil {
		return preview, err
	}
	if !actor.CanResource(authz.PermissionDownloadsCreate, models.AuthorizationResourceDownloader, preview.DownloaderID) {
		return preview, appError(CodePermissionDenied, "无权使用这个下载器", nil)
	}
	digest, _, err := sharePreviewConfig(s.db.WithContext(ctx), preview.DownloaderID)
	if err != nil || digest != preview.ConfigDigest {
		return preview, sharePreviewExpired()
	}
	return preview, nil
}
func (s *SiteService) RecognizeShareEntry(ctx context.Context, actor Actor, previewToken, entryToken string) (SiteRecognitionSummary, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	preview, err := s.resolveSharePreview(ctx, actor, previewToken)
	if err != nil {
		return SiteRecognitionSummary{}, err
	}
	entry, ok := preview.Entries[entryToken]
	if !ok || entry.IsDir {
		return SiteRecognitionSummary{}, appError(CodeInvalidRequest, "请选择预览中的文件", nil)
	}
	release, err := s.acquireSharePreview(ctx)
	if err != nil {
		return SiteRecognitionSummary{}, err
	}
	defer release()
	claim, err := s.resolveAvailableClaim(preview.ResultToken, actor.User.ID)
	if err != nil {
		return SiteRecognitionSummary{}, err
	}
	claim.Title = path.Base(entry.RelativePath)
	claim.Subtitle = ""
	claim.MediaTypeHint = ""
	return s.recognizeSiteClaim(ctx, actor, claim, "", []recognitionSourceFile{{RelativePath: entry.RelativePath, Size: entry.Size}})
}
func (s *SiteService) resolveShareSelection(ctx context.Context, actor Actor, input SiteDownloadInput, claim siteResultClaim, source DownloadSourceInput) (cloud.ShareSelection, func(*gorm.DB) error, error) {
	selection := cloud.ShareSelection{Version: 1}
	// Download has reserved the result claim already; inspect the immutable preview
	// directly, and validate current configuration again inside the transaction.
	s.vaultMu.Lock()
	preview, ok := s.sharePreviews[input.PreviewToken]
	s.vaultMu.Unlock()
	if !ok || preview.ActorID != actor.User.ID || preview.ResultToken != strings.TrimSpace(input.ResultToken) || preview.DownloaderID != input.DownloaderID || preview.URL != source.URL || !preview.ExpiresAt.After(s.now()) || source.Kind != downloadpkg.SourcePan115Share {
		return selection, nil, sharePreviewExpired()
	}
	if len(input.SelectedEntryTokens) == 0 || len(input.SelectedEntryTokens) > cloud.MaxSharePreviewEntries {
		return selection, nil, appError(CodeInvalidRequest, "请至少选择一个文件", nil)
	}
	selected := map[string]cloud.ShareTreeItem{}
	for _, token := range input.SelectedEntryTokens {
		entry, ok := preview.Entries[token]
		if !ok {
			return selection, nil, appError(CodeInvalidRequest, "文件不属于这次分享预览", nil)
		}
		if !entry.IsDir {
			selected[entry.ID] = entry
			continue
		}
		for _, child := range preview.Entries {
			if !child.IsDir && strings.HasPrefix(child.RelativePath, entry.RelativePath+"/") {
				selected[child.ID] = child
			}
		}
	}
	for _, entry := range selected {
		selection.Files = append(selection.Files, entry)
	}
	sort.Slice(selection.Files, func(i, j int) bool { return selection.Files[i].RelativePath < selection.Files[j].RelativePath })
	if _, err := cloud.EncodeSelectedShareSource(source.URL, selection); err != nil {
		return selection, nil, appError(CodeInvalidRequest, "请选择 1 到 500 个文件；路径清单过长时请减少选择", nil)
	}
	guard := func(tx *gorm.DB) error {
		if !preview.ExpiresAt.After(s.now()) || !claim.ExpiresAt.After(s.now()) {
			return sharePreviewExpired()
		}
		digest, _, err := sharePreviewConfig(tx, preview.DownloaderID)
		if err != nil || digest != preview.ConfigDigest {
			return sharePreviewExpired()
		}
		return nil
	}
	if err := guard(s.db.WithContext(ctx)); err != nil {
		return selection, nil, err
	}
	return selection, guard, nil
}

// Each selected video is its own normal download pipeline. Collections must not
// inherit the single-largest-movie policy of a traditional torrent package.
func selectedSharePackages(selection cloud.ShareSelection) []cloud.ShareSelection {
	groups := []cloud.ShareSelection{}
	for _, file := range selection.Files {
		if isVideoFile(file.RelativePath) {
			groups = append(groups, cloud.ShareSelection{Version: 1, Files: []cloud.ShareTreeItem{file}})
		}
	}
	if len(groups) == 0 {
		return nil
	}
	for _, file := range selection.Files {
		if isVideoFile(file.RelativePath) {
			continue
		}
		target := 0
		for index, group := range groups {
			video := group.Files[0].RelativePath
			stems := map[string][]string{strings.ToLower(path.Dir(video)): {strings.ToLower(strings.TrimSuffix(path.Base(video), path.Ext(video)))}}
			if sidecarBelongsToAcceptedMedia(file.RelativePath, stems) {
				target = index
				break
			}
		}
		groups[target].Files = append(groups[target].Files, file)
	}
	return groups
}
func (s *SiteService) submitShareSelection(ctx context.Context, actor Actor, previewToken string, input SubmitDownloadInput, request RequestContext) (DownloadTaskSummary, error) {
	packages := selectedSharePackages(*input.Source.ShareSelection)
	if len(packages) == 0 {
		return DownloadTaskSummary{}, appError(CodeInvalidRequest, "请至少选择一个视频文件；字幕等附属文件可一并勾选", nil)
	}
	digest, _ := input.Source.ShareSelection.Digest()
	planRaw, _ := json.Marshal([]any{actor.User.ID, input.DownloaderID, input.MediaLibraryID, input.ProfileID, digest})
	planHash := sha256.Sum256(planRaw)
	planDigest := hex.EncodeToString(planHash[:])
	s.vaultMu.Lock()
	preview, ok := s.sharePreviews[previewToken]
	if !ok || (preview.SubmissionDigest != "" && preview.SubmissionDigest != planDigest) {
		s.vaultMu.Unlock()
		return DownloadTaskSummary{}, appError(CodeInvalidRequest, "本次预览已提交其他选择，请保持原选择重试，或重新预览", nil)
	}
	preview.SubmissionDigest = planDigest
	s.sharePreviews[previewToken] = preview
	s.vaultMu.Unlock()
	results := []DownloadTaskSummary{}
	var lastErr error
	pending := 0
	for index, group := range packages {
		if err := ctx.Err(); err != nil {
			lastErr = err
			pending += len(packages) - index
			break
		}
		groupDigest, _ := group.Digest()
		keyHash := sha256.Sum256([]byte("selected-share:" + previewToken + ":" + planDigest + ":" + groupDigest))
		key := hex.EncodeToString(keyHash[:])
		var existing models.DownloadTask
		err := s.db.WithContext(ctx).Where("ingest_source_key = ? AND owner_id = ?", key, actor.User.ID).First(&existing).Error
		if err == nil {
			var job models.Job
			s.db.WithContext(ctx).First(&job, "id = ?", existing.JobID)
			results = append(results, downloadTaskSummary(existing, job.Status))
			continue
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			lastErr = err
			pending++
			continue
		}
		child := input
		child.Source.ShareSelection = &group
		child.DisplayName = path.Base(group.Files[0].RelativePath)
		child.RecognitionOverride = nil
		created, err := s.downloads.submit(ctx, actor.User.ID, child, request, models.DownloadSourceOriginShare, key, "")
		if err != nil {
			lastErr = err
			pending++
			continue
		}
		results = append(results, created)
	}
	if len(results) == 0 {
		return DownloadTaskSummary{}, lastErr
	}
	result := results[0]
	result.SelectionTasks = results
	result.SelectionPending = pending
	if pending > 0 {
		result.SelectionError = "部分内容尚未入队，请保持当前选择重试；已创建的任务不会重复创建"
	}
	return result, nil
}
