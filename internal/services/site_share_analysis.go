package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/mediarecognition"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/metadata/tmdb"
)

// Complete describes directory enumeration, independently of TMDB recognition.
// Episodes are actual video-file facts; the post's advertised range is never
// copied here. A sparse array must remain sparse in every projection.
type SiteShareAnalysis struct {
	Status          string                   `json:"status"`
	Complete        bool                     `json:"complete"`
	Groups          []SiteShareAnalysisGroup `json:"groups"`
	VideoCount      int                      `json:"video_count"`
	IgnoredCount    int                      `json:"ignored_count"`
	UnresolvedCount int                      `json:"unresolved_count"`
	Message         string                   `json:"message"`
	ErrorCode       string                   `json:"error_code,omitempty"`
	CheckedAt       time.Time                `json:"checked_at"`
	ExpiresAt       *time.Time               `json:"expires_at,omitempty"`
	DownloaderID    string                   `json:"downloader_id,omitempty"`
}

type SiteShareAnalysisGroup struct {
	Title          string `json:"title"`
	MediaType      string `json:"media_type"`
	TMDBID         *int64 `json:"tmdb_id,omitempty"`
	Year           *int   `json:"year,omitempty"`
	Season         *int   `json:"season,omitempty"`
	Episodes       []int  `json:"episodes"`
	VideoCount     int    `json:"video_count"`
	Status         string `json:"status"`
	IdentityStatus string `json:"identity_status,omitempty"`
	ErrorCode      string `json:"error_code,omitempty"`
	PosterURL      string `json:"poster_url,omitempty"`
}

// A verified search identity supplies aliases and season context, not evidence
// that any result title or any share file belongs to that work.
type siteShareIdentity struct {
	TMDBID           int64
	MediaType, Title string
	Names            []tmdb.SearchName
	Season           *int
	Year             *int
	SeasonCount      int
}

type siteShareAnalyzedFile struct {
	Token, Path, Title string
	Year, Season       *int
	Episodes           []int
	Group              int
}

func (s *SiteService) bindContentSearchIdentity(token string, actorID uint, identity siteShareIdentity) error {
	s.vaultMu.Lock()
	defer s.vaultMu.Unlock()
	claim, ok := s.vault[token]
	if !ok || claim.ActorID != actorID || claim.InFlight || !claim.ExpiresAt.After(s.now()) {
		return sharePreviewExpired()
	}
	identity.Names = append([]tmdb.SearchName(nil), identity.Names...)
	claim.SearchIdentity = &identity
	s.vault[token] = claim
	return nil
}

func shareAnalysisIdentityDigest(claim siteResultClaim) string {
	var manualID *int64
	manualType := ""
	if claim.RecognitionLocked {
		manualID, manualType = claim.ManualTMDBID, claim.ManualMediaType
	}
	raw, _ := json.Marshal([]any{claim.Title, claim.MediaTypeHint, claim.SearchIdentity, manualID, manualType})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (s *SiteService) acquireShareAnalysis(ctx context.Context, key string) (func(), error) {
	for {
		s.vaultMu.Lock()
		if s.shareAnalysisFlights == nil {
			s.shareAnalysisFlights = map[string]chan struct{}{}
		}
		pending, exists := s.shareAnalysisFlights[key]
		if !exists {
			if len(s.shareAnalysisFlights) >= 32 {
				s.vaultMu.Unlock()
				return nil, appError(CodeSiteUnavailable, "分享分析较多，请稍后重试", nil)
			}
			pending = make(chan struct{})
			s.shareAnalysisFlights[key] = pending
			s.vaultMu.Unlock()
			return func() { s.vaultMu.Lock(); delete(s.shareAnalysisFlights, key); close(pending); s.vaultMu.Unlock() }, nil
		}
		s.vaultMu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-pending:
		}
	}
}

func (s *SiteService) shareAnalysisTree(ctx context.Context, actor Actor, claim siteResultClaim, raw, downloader string, reuse bool, request RequestContext) ([]cloud.ShareTreeItem, string, SiteShareValidation, *siteSharePreview, error) {
	if reuse {
		digest, _, err := sharePreviewConfig(s.db.WithContext(ctx), downloader)
		if err != nil {
			return nil, "", SiteShareValidation{}, nil, err
		}
		scope := discoveryAuthorizationScope(actor)
		s.vaultMu.Lock()
		var cached *siteSharePreview
		for _, preview := range s.sharePreviews {
			if preview.ActorID == actor.User.ID && preview.URL == raw && preview.DownloaderID == downloader && preview.ConfigDigest == digest && preview.AuthorizationScope == scope && preview.ExpiresAt.After(s.now()) && preview.Summary.ShareAnalysis != nil {
				if cached == nil || preview.ExpiresAt.After(cached.ExpiresAt) {
					copy := preview
					cached = &copy
				}
			}
		}
		s.vaultMu.Unlock()
		if cached != nil {
			entries := make([]cloud.ShareTreeItem, 0, len(cached.Entries))
			for _, entry := range cached.Entries {
				entries = append(entries, entry)
			}
			return entries, digest, *cached.Summary.ShareValidation, cached, nil
		}
	}
	entries, digest, validation, err := s.inspectResultShare(ctx, actor, claim, raw, downloader, true, request)
	return entries, digest, validation, nil, err
}

func shareTitleMatchesIdentity(title string, identity *siteShareIdentity) bool {
	if identity == nil {
		return false
	}
	key := identitySearchKey(title)
	if key == "" {
		return false
	}
	if key == identitySearchKey(identity.Title) {
		return true
	}
	for _, name := range identity.Names {
		if key == identitySearchKey(name.Value) {
			return true
		}
	}
	return false
}

func shareTitlesAgree(left, right string, identity *siteShareIdentity) bool {
	return identitySearchKey(left) == identitySearchKey(right) || shareTitleMatchesIdentity(left, identity) && shareTitleMatchesIdentity(right, identity)
}

func shareGroupMatchesIdentity(group SiteShareAnalysisGroup, identity *siteShareIdentity) bool {
	if !shareTitleMatchesIdentity(group.Title, identity) || group.MediaType != identity.MediaType {
		return false
	}
	// Keep the existing movie-year tolerance. A TV first-air year is not the
	// release year of every later season and cannot be used as that gate.
	return group.MediaType != "movie" || group.Year == nil || identity.Year == nil || absInt(*group.Year-*identity.Year) <= 1
}

var sharePackageRangeSuffix = regexp.MustCompile(`(?i)(?:\s*[\[【(（]\s*(?:\d{1,3}\s*[-~–]\s*\d{1,3}|(?:全\s*)?\d{1,3}\s*集(?:全)?|全集)\s*[\]】)）])+\s*$`)
var shareTechnicalDirectory = regexp.MustCompile(`(?i)^(?:[\[\] _.,+()-]*(?:2160p|1080p|720p|480p|4k|hevc|av1|h[ .]?26[45]|x26[45]|bluray|bdrip|web[- ]?dl|webrip|remux|aac|dts|flac|10bit|8bit|hdr10|hdr|dv))+[\[\] _.,+()-]*$`)

func genericShareDirectory(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "movies", "movie", "tv", "series", "collection", "collections", "video", "videos", "电影", "电视剧", "剧集", "合集", "视频":
		return true
	}
	return shareTechnicalDirectory.MatchString(name)
}

func stripShareDirectoryCounter(name string) string {
	// A real parent may describe an entire package. Its range is not an episode
	// declaration for every leaf, and must not hide the parent's actual work name.
	name = strings.TrimSpace(sharePackageRangeSuffix.ReplaceAllString(name, ""))
	match := followShareCounterPattern.FindString(name)
	if match == "" {
		return name
	}
	number, _ := strconv.Atoi(strings.Trim(strings.TrimSpace(match), "()（）"))
	if number >= 1888 && number <= 2200 {
		return name
	}
	return followShareCounterPattern.ReplaceAllString(name, "")
}

func shareFileFacts(entry SiteSharePreviewEntry, claim siteResultClaim) (siteShareAnalyzedFile, bool) {
	file := siteShareAnalyzedFile{Token: entry.Token, Path: entry.Path, Group: -1}
	if entry.IsDir || !isVideoFile(entry.Path) || followShareExtraPattern.MatchString(entry.Path) {
		return file, false
	}
	structure, err := mediarecognition.Parse(mediarecognition.InputFacts{Files: []mediarecognition.FileFact{{RelativePath: entry.Path}}, SourceKind: mediarecognition.SourceLibraryScan})
	if err != nil || structure.Structure.HasExtras || structure.Structure.HasBDMV || structure.Structure.HasVideoTS {
		return file, false
	}
	base := path.Base(entry.Path)
	parsed, err := mediarecognition.Parse(mediarecognition.InputFacts{PackageName: base, SourceKind: mediarecognition.SourceDownload})
	if err != nil {
		return file, false
	}
	file.Title, file.Year, file.Season = parsed.CanonicalTitle, cloneInt(parsed.Year), cloneInt(parsed.Season)
	if file.Year == nil {
		file.Year = cloneInt(parsed.SeasonYear)
	}
	// Orphaned separators before the extension do not change a bare episode
	// number ("14..mp4"). Trim only the suffix, preserving internal decimal or
	// range separators and the original path. The existing numeric bounds and
	// real-parent/identity/season checks below still supply the authority.
	stem := strings.TrimRight(strings.TrimSuffix(base, path.Ext(base)), " ._-")
	// A bare number cannot establish a movie/work identity (including "1917").
	_, numericErr := strconv.Atoi(stem)
	numeric := numericErr == nil
	if numeric {
		file.Title, file.Year, file.Season = "", nil, nil
		number, _ := strconv.Atoi(stem)
		if followBareEpisodePattern.MatchString(stem) && number >= 1 && number <= 200 {
			file.Episodes = []int{number}
		}
	}
	for dir := path.Dir(entry.Path); dir != "." && dir != "/"; dir = path.Dir(dir) {
		name := path.Base(dir)
		if genericShareDirectory(name) {
			continue
		}
		// Count suffixes in actual directory names are not episode declarations.
		name = stripShareDirectoryCounter(name)
		parent, err := mediarecognition.Parse(mediarecognition.InputFacts{PackageName: name, SourceKind: mediarecognition.SourceDownload})
		if err != nil {
			return file, false
		}
		if parent.Season != nil {
			if file.Season != nil && *file.Season != *parent.Season {
				return file, false
			}
			file.Season = cloneInt(parent.Season)
		}
		if followShareSeasonDirectoryPattern.MatchString(name) {
			continue
		}
		if parent.CanonicalTitle == "" || parent.Episodes.EpisodeMin != nil {
			return file, false
		}
		if file.Title == "" {
			file.Title = parent.CanonicalTitle
		} else if !shareTitlesAgree(file.Title, parent.CanonicalTitle, claim.SearchIdentity) {
			return file, false
		}
		if file.Year == nil {
			file.Year = cloneInt(parent.Year)
			if file.Year == nil {
				file.Year = cloneInt(parent.SeasonYear)
			}
		}
	}
	// Numeric leaves need an actual work directory. A search/post title alone
	// cannot lend its identity to arbitrary root-level numbered files.
	if file.Title == "" {
		return file, false
	}
	if file.Season == nil && shareTitleMatchesIdentity(file.Title, claim.SearchIdentity) {
		file.Season = cloneInt(claim.SearchIdentity.Season)
		if file.Season == nil && claim.SearchIdentity.SeasonCount == 1 {
			one := 1
			file.Season = &one
		}
	}
	if numeric {
		number, _ := strconv.Atoi(stem)
		if !followBareEpisodePattern.MatchString(stem) || number < 1 || number > 200 {
			return file, false
		}
		file.Episodes = []int{number}
	} else if parsed.Episodes.EpisodeMin != nil && parsed.Episodes.EpisodeMax != nil {
		if file.Season == nil || !followEpisodeDeclarationIsUnambiguous(base, *file.Season) {
			return file, false
		}
		lo, hi := *parsed.Episodes.EpisodeMin, *parsed.Episodes.EpisodeMax
		if first, last, ok := explicitFollowEpisodeRange(base, *file.Season); ok {
			lo, hi = first, last
		}
		if lo < 1 || hi < lo || hi-lo > 200 {
			return file, false
		}
		for ep := lo; ep <= hi; ep++ {
			file.Episodes = append(file.Episodes, ep)
		}
	}
	return file, true
}

func (s *SiteService) analyzeShareEntries(ctx context.Context, actor Actor, claim siteResultClaim, entries []SiteSharePreviewEntry) (*SiteShareAnalysis, []siteShareAnalyzedFile) {
	analysis := &SiteShareAnalysis{Status: "complete", Complete: true, Groups: []SiteShareAnalysisGroup{}, Message: "已完整读取分享，集数来自实际视频文件"}
	files := []siteShareAnalyzedFile{}
	byKey := map[string]int{}
	groupFiles := [][]recognitionSourceFile{}
	groupEpisodes := []map[int]bool{}
	for _, entry := range entries {
		if entry.IsDir {
			continue
		}
		if !isVideoFile(entry.Path) || followShareExtraPattern.MatchString(entry.Path) {
			analysis.IgnoredCount++
			continue
		}
		analysis.VideoCount++
		file, ok := shareFileFacts(entry, claim)
		if !ok {
			analysis.UnresolvedCount++
			continue
		}
		season := -1
		if file.Season != nil {
			season = *file.Season
		}
		key := fmt.Sprintf("%s:%v:%d", identitySearchKey(file.Title), intValue(file.Year), season)
		index, exists := byKey[key]
		if !exists {
			if len(analysis.Groups) >= 32 {
				analysis.UnresolvedCount++
				continue
			}
			index = len(analysis.Groups)
			byKey[key] = index
			mediaType := "movie"
			if len(file.Episodes) > 0 || file.Season != nil {
				mediaType = "tv"
			}
			analysis.Groups = append(analysis.Groups, SiteShareAnalysisGroup{Title: file.Title, MediaType: mediaType, Year: file.Year, Season: file.Season, Episodes: []int{}, Status: mediaRecognitionStatusUnrecognized})
			groupFiles = append(groupFiles, nil)
			groupEpisodes = append(groupEpisodes, map[int]bool{})
		}
		newEpisodes := 0
		for _, ep := range file.Episodes {
			if !groupEpisodes[index][ep] {
				newEpisodes++
			}
		}
		if len(groupEpisodes[index])+newEpisodes > 2000 {
			analysis.UnresolvedCount++
			continue
		}
		for _, ep := range file.Episodes {
			groupEpisodes[index][ep] = true
		}
		file.Group = index
		files = append(files, file)
		group := &analysis.Groups[index]
		group.VideoCount++
		group.Episodes = append(group.Episodes, file.Episodes...)
		if len(groupFiles[index]) < 8 {
			groupFiles[index] = append(groupFiles[index], recognitionSourceFile{RelativePath: entry.Path, Size: entry.Size})
		}
	}
	for i := range analysis.Groups {
		group := &analysis.Groups[i]
		sort.Ints(group.Episodes)
		unique := group.Episodes[:0]
		for _, episode := range group.Episodes {
			if len(unique) == 0 || unique[len(unique)-1] != episode {
				unique = append(unique, episode)
			}
		}
		group.Episodes = unique
	}
	s.recognizeContentGroups(ctx, actor, claim, analysis.Groups, groupFiles)
	for i := range analysis.Groups {
		group := &analysis.Groups[i]
		if claim.RecognitionLocked && claim.ManualTMDBID != nil && group.TMDBID != nil && (*group.TMDBID != *claim.ManualTMDBID || group.MediaType != claim.ManualMediaType) {
			group.Status, group.ErrorCode = mediaRecognitionStatusUnrecognized, CodeSiteResultIdentityMismatch
		}
		if group.Status != mediaRecognitionStatusMatched || group.MediaType == "tv" && (group.Season == nil || len(group.Episodes) == 0) {
			analysis.UnresolvedCount += group.VideoCount
		}
	}
	return analysis, files
}

func (s *SiteService) automaticShareReceiver(ctx context.Context, actor Actor, claim siteResultClaim) (string, error) {
	if !actor.Can(authz.PermissionDownloadsCreate) {
		return "", appError(CodePermissionDenied, "没有分享读取权限，无法验证实际文件", nil)
	}
	if s.downloads == nil || s.downloads.downloader == nil {
		return "", appError(CodeDownloaderUnavailable, "下载服务不可用", nil)
	}
	var receivers []models.Downloader
	if err := s.db.WithContext(ctx).Where("enabled = ?", true).Order("sort_order, id").Find(&receivers).Error; err != nil {
		return "", appError(CodeDownloaderUnavailable, "无法读取分享接收器配置", nil)
	}
	for _, receiver := range receivers {
		if !routeDownloaderApplicable(claimCloudSource(claim), receiver) || !actor.CanResource(authz.PermissionDownloadsCreate, models.AuthorizationResourceDownloader, receiver.ID) {
			continue
		}
		if _, err := s.downloads.downloader.configuredShareReceiver(ctx, receiver); err == nil {
			return receiver.ID, nil
		}
	}
	return "", appError(CodeDownloaderUnavailable, "没有当前可用的同网盘接收器，尚未读取分享文件", nil)
}

func (s *SiteService) recognizeShareResult(ctx context.Context, actor Actor, claim siteResultClaim, token string) (SiteRecognitionSummary, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	result := SiteRecognitionSummary{EngineVersion: mediarecognition.EngineVersion, Title: claim.Title, Status: mediaRecognitionStatusUnrecognized}
	unavailable := func(err error) (SiteRecognitionSummary, error) {
		if ctx.Err() != nil {
			return SiteRecognitionSummary{}, ctx.Err()
		}
		result.ErrorCode = ErrorCode(err)
		result.ShareAnalysis = &SiteShareAnalysis{Status: "unavailable", Groups: []SiteShareAnalysisGroup{}, Message: safeErrorMessage(err, "分享文件未能完整读取"), ErrorCode: ErrorCode(err), CheckedAt: s.now()}
		return result, nil
	}
	if _, err := s.sharePreviewClaim(ctx, actor, token); err != nil {
		return unavailable(err)
	}
	downloader, err := s.automaticShareReceiver(ctx, actor, claim)
	if err != nil {
		return unavailable(err)
	}
	preview, err := s.previewShare(ctx, actor, token, downloader, true, RequestContext{})
	if err != nil {
		return unavailable(err)
	}
	result.ShareAnalysis = preview.ShareAnalysis
	// Mixed work/season summaries stay grouped. Never bind an entire collection
	// to the first recognized file or manufacture an aggregate episode range.
	if len(preview.ShareAnalysis.Groups) == 1 && preview.ShareAnalysis.UnresolvedCount == 0 {
		group := preview.ShareAnalysis.Groups[0]
		result.Title, result.Status, result.IdentityStatus, result.TMDBID = group.Title, group.Status, group.IdentityStatus, cloneInt64(group.TMDBID)
		result.MediaType, result.Year, result.PosterURL = group.MediaType, cloneInt(group.Year), group.PosterURL
		if group.Status == mediaRecognitionStatusMatched && group.TMDBID != nil {
			if err := s.bindClaimRecognition(token, actor.User.ID, *group.TMDBID, group.MediaType, mediaIdentitySourceDirectID, mediaIdentityStatusVerified, false); err != nil {
				return SiteRecognitionSummary{}, err
			}
		}
	}
	if _, err := s.resolveSharePreview(ctx, actor, preview.Token); err != nil {
		return SiteRecognitionSummary{}, err
	}
	return result, nil
}

// A poster identifies the intended work, not every file in a mixed share.
// Validate only the caller's exact frozen selection and keep the original
// preview/identity digest intact; overwriting it would retire those tokens.
func (s *SiteService) bindExpectedShareSelection(ctx context.Context, actor Actor, claim siteResultClaim, token string, expected siteShareIdentity, input SiteDownloadInput) error {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	if strings.TrimSpace(input.ResultToken) != token {
		return sharePreviewExpired()
	}
	preview, err := s.resolveSharePreview(ctx, actor, input.PreviewToken)
	if err != nil {
		return err
	}
	if preview.Summary.ShareAnalysis == nil || !preview.Summary.ShareAnalysis.Complete {
		return sharePreviewExpired()
	}
	raw, provider, _, err := normalizeCloudShare(claim.TorrentID)
	if err != nil || provider+"_share" != claimCloudSource(claim) {
		return sharePreviewExpired()
	}
	selection, guard, err := s.resolveShareSelection(ctx, actor, input, claim, DownloadSourceInput{Kind: provider + "_share", URL: raw})
	if err != nil {
		return err
	}
	entries := make([]SiteSharePreviewEntry, 0, len(selection.Files))
	for _, file := range selection.Files {
		entries = append(entries, SiteSharePreviewEntry{Path: file.RelativePath, Name: path.Base(file.RelativePath), Size: file.Size})
	}
	// The same real-file analyzer is used by cards, preview and Follow. Expected
	// aliases may corroborate actual file/parent names; unrelated leaves cannot
	// acquire the target identity merely because it was supplied by the client.
	actual := claim
	actual.SearchIdentity = &expected
	analysis, _ := s.analyzeShareEntries(ctx, actor, actual, entries)
	if err := ctx.Err(); err != nil {
		return err
	}
	if analysis.VideoCount == 0 || analysis.UnresolvedCount != 0 || len(analysis.Groups) == 0 {
		return appError(CodeSiteResultIdentityMismatch, "所选视频尚未核实为当前作品，请检查具体文件", nil)
	}
	for _, group := range analysis.Groups {
		if group.Status != mediaRecognitionStatusMatched || group.TMDBID == nil || *group.TMDBID != expected.TMDBID || group.MediaType != expected.MediaType {
			return appError(CodeSiteResultIdentityMismatch, "所选视频包含其他作品，请只选择当前作品的文件", nil)
		}
	}
	if _, err := s.resolveSharePreview(ctx, actor, input.PreviewToken); err != nil {
		return err
	}
	if err := guard(s.db.WithContext(ctx)); err != nil {
		return err
	}
	return s.bindClaimRecognition(token, actor.User.ID, expected.TMDBID, expected.MediaType, mediaIdentitySourceDirectID, mediaIdentityStatusVerified, false)
}
