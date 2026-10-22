package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	serverlog "github.com/yuanjing-hash/OhMyCine-Server/internal/logging"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/mediarecognition"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/metadata/tmdb"
	sitepkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/site"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/site/builtin"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/site/pansou"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

type MediaIdentitySearchInput struct {
	MediaType          string
	TMDBID             int64
	Season             *int
	Page               int
	SiteID             *uint
	SiteIDs            []uint
	ClientCapabilities []string
	TypePolicy         string
	CloudTypes         []string
	SnapshotID         string
	Refresh            bool
	ProgressVersion    int
}

type MediaIdentitySearchResult struct {
	MediaType  string            `json:"media_type"`
	TMDBID     int64             `json:"tmdb_id"`
	Title      string            `json:"title"`
	Year       *int              `json:"year,omitempty"`
	QueryNames []tmdb.SearchName `json:"query_names"`
	Groups     []SiteSearchGroup `json:"groups"`
}

// SearchMediaIdentity verifies a stable TMDB identity and aggregates the same
// bounded multilingual name set across configured sites. The browser receives
// only safe display names and ordinary actor-bound opaque result tokens.
func (s *SiteService) SearchMediaIdentity(ctx context.Context, actor Actor, input MediaIdentitySearchInput) (MediaIdentitySearchResult, error) {
	result := MediaIdentitySearchResult{}
	groups := []SiteSearchGroup{}
	input.ProgressVersion = 0 // JSON and Follow receive terminal groups only.
	err := s.SearchMediaIdentityEach(ctx, actor, input, func(metadata MediaIdentitySearchResult) {
		result = metadata
	}, func(group SiteSearchGroup) {
		groups = append(groups, group)
	})
	if err != nil && (ErrorCode(err) != CodeSearchBudget || len(groups) == 0) {
		return MediaIdentitySearchResult{}, err
	}
	priorities := s.sitePriorityMap(input.SiteID, input.SiteIDs)
	sort.SliceStable(groups, func(left, right int) bool {
		leftPriority, rightPriority := priorities[groups[left].SiteID], priorities[groups[right].SiteID]
		if leftPriority != rightPriority {
			return leftPriority < rightPriority
		}
		return groups[left].SiteID < groups[right].SiteID
	})
	result.Groups = groups
	return result, nil
}

// SearchMediaIdentityEach shares the same preflight and per-site aggregation
// with the JSON endpoint while allowing SSE callers to receive one complete
// site group as soon as that site finishes all bounded aliases. Callbacks are
// serialized, so HTTP writers never receive concurrent writes.
func (s *SiteService) SearchMediaIdentityEach(ctx context.Context, actor Actor, input MediaIdentitySearchInput, emitMetadata func(MediaIdentitySearchResult), emit func(SiteSearchGroup)) error {
	return s.SearchMediaIdentityEachProgress(ctx, actor, input, emitMetadata, emit, nil)
}

func (s *SiteService) SearchMediaIdentityEachProgress(ctx context.Context, actor Actor, input MediaIdentitySearchInput, emitMetadata func(MediaIdentitySearchResult), emit func(SiteSearchGroup), emitProgress func(SiteSearchProgress)) error {
	ctx, cancel := s.searchRunContext(ctx)
	defer cancel()
	ctx = context.WithValue(ctx, detailedSearchKey{}, input.ProgressVersion == 2)
	if !actor.HasPermission(authz.PermissionDiscoveryRead) {
		return appError(CodePermissionDenied, "无权搜索种子资源", nil)
	}
	input.MediaType = strings.ToLower(strings.TrimSpace(input.MediaType))
	if input.TMDBID <= 0 || (input.MediaType != "movie" && input.MediaType != "tv") || s.metadata == nil {
		return appError(CodeInvalidRequest, "TMDB 搜索身份无效", nil)
	}
	if input.Season != nil && (input.MediaType != "tv" || *input.Season < 0 || *input.Season > 200) {
		return appError(CodeInvalidRequest, "资源搜索季数无效", nil)
	}
	if input.Page == 0 {
		input.Page = 1
	}
	if input.Page < 1 || input.Page > 20 {
		return appError(CodeInvalidRequest, "种子资源搜索页码无效", nil)
	}
	if input.ProgressVersion == 2 && emitProgress != nil {
		emitProgress(SiteSearchProgress{ProgressVersion: 2, Phase: "preparing_media"})
	}
	client, err := s.metadata.Client()
	if err != nil {
		return appError(CodeTMDBUnavailable, "TMDB 详情服务暂时不可用", nil)
	}
	verified, names, err := client.IdentitySearchNames(ctx, input.MediaType, input.TMDBID, "zh-CN", tmdb.DefaultIdentitySearchNameLimit)
	if err != nil {
		return appError(tmdb.ErrorCode(err), "TMDB 作品身份无法解析", nil)
	}
	if len(names) == 0 {
		return appError(CodeTMDBUnavailable, "TMDB 作品没有可用搜索名称", nil)
	}
	metadata := MediaIdentitySearchResult{MediaType: input.MediaType, TMDBID: input.TMDBID, Title: verified.Title, Year: cloneInt(verified.ReleaseYear), QueryNames: names, Groups: []SiteSearchGroup{}}
	ctx = context.WithValue(ctx, searchPageKey{}, input.Page)
	records, err := s.searchSiteRecords(actor, input.SiteID, input.SiteIDs)
	if err != nil {
		return err
	}
	if emitMetadata != nil {
		emitMetadata(metadata)
	}
	return s.runSiteSearches(ctx, records, func(siteCtx context.Context, record models.Site) SiteSearchGroup {
		return s.searchMediaIdentitySite(siteCtx, actor, input, verified, names, record)
	}, emit, emitProgress)
}

func (s *SiteService) searchMediaIdentitySite(ctx context.Context, actor Actor, input MediaIdentitySearchInput, verified tmdb.Match, names []tmdb.SearchName, record models.Site) SiteSearchGroup {
	if record.Kind == pansou.Kind {
		return s.searchPanSouIdentitySite(ctx, actor, input, verified, names, record)
	}
	return s.searchMediaIdentitySitePage(ctx, actor, input, verified, names, record)
}

func (s *SiteService) searchMediaIdentitySitePage(ctx context.Context, actor Actor, input MediaIdentitySearchInput, verified tmdb.Match, names []tmdb.SearchName, record models.Site) SiteSearchGroup {
	var target *SiteSearchGroup
	privateKeys := make(map[string]int)
	ctx = serverlog.WithBusinessFields(ctx, map[string]any{"site_id": record.ID, "site_name": record.Name, "user_id": actor.User.ID})
	if input.Season != nil {
		ctx = serverlog.WithBusinessFields(ctx, map[string]any{"season": *input.Season})
	}
	if err := s.prepareSiteSearch(ctx, record); err != nil {
		return searchPreparationFailure(record, input.Page, len(names), err)
	}
	for queryIndex, name := range names {
		if ctx.Err() != nil {
			break
		}
		reportSearchQuery(ctx, queryIndex+1, len(names), name.Value)
		group := s.searchSite(ctx, actor, record, SiteSearchInput{ClientCapabilities: input.ClientCapabilities, TypePolicy: input.TypePolicy, CloudTypes: input.CloudTypes, Refresh: input.Refresh, ProgressVersion: input.ProgressVersion, Keyword: name.Value, MediaType: input.MediaType, SearchBy: "title", Year: identitySearchYear(input.MediaType, verified.ReleaseYear), TMDBID: &input.TMDBID, Page: input.Page, SiteID: &record.ID})
		aliasCtx := serverlog.WithBusinessFields(ctx, map[string]any{"query_name": name.Value})
		rejections := newFollowRejections()
		kept, replaced := 0, 0
		if target == nil {
			copy := group
			copy.Items = []SiteSearchResult{}
			copy.Skipped = 0
			copy.HasNext = false
			copy.QueriesTotal = len(names)
			target = &copy
		}
		target.HasNext = target.HasNext || group.HasNext
		target.Skipped += group.Skipped
		target.Partial = target.Partial || group.Partial
		target.Truncated = target.Truncated || group.Truncated
		if group.Completion != "" {
			target.Completion = group.Completion
		}
		for _, warning := range group.Warnings {
			present := false
			for _, saved := range target.Warnings {
				if saved.Code == warning.Code {
					present = true
					break
				}
			}
			if !present {
				target.Warnings = append(target.Warnings, warning)
			}
		}
		if group.Partial {
			target.Partial, target.IncompleteReason = true, group.IncompleteReason
		}
		if group.Status == "success" {
			target.QueriesCompleted++
			target.Status, target.ErrorCode = "success", ""
		} else {
			target.ErrorCount++
			if group.QueryAttempted {
				target.QueriesFailed++
			}
			target.IncompleteReason = group.ErrorCode
			if target.Status != "success" {
				target.Status, target.ErrorCode = group.Status, group.ErrorCode
			}
		}
		for _, item := range group.Items {
			claim, claimErr := s.resolveAvailableClaim(item.Token, actor.User.ID)
			if claimErr != nil {
				rejections.add("claim_unavailable", item.Title)
				continue
			}
			cloudShare := isCloudShareSource(item.SourceKind)
			if !cloudShare && !mediaIdentityResultMatches(item.Title, names, input.MediaType, verified.ReleaseYear, input.Season) {
				s.deleteClaim(item.Token)
				target.Skipped++
				rejections.add("identity_mismatch", item.Title)
				continue
			}
			identity := claim.TorrentID
			if (isCloudShareSource(item.SourceKind) || item.SourceKind == RouteSourceBT) && item.ResourceFingerprint != "" {
				identity = item.ResourceFingerprint
			}
			key := strconv.FormatUint(uint64(group.SiteID), 10) + ":" + identity
			index, duplicate := privateKeys[key]
			if duplicate && (!isCloudShareSource(item.SourceKind) || compareOptionalTime(item.Published, target.Items[index].Published) <= 0) {
				s.deleteClaim(item.Token)
				rejections.add("duplicate", item.Title)
				continue
			}
			var bindErr error
			if cloudShare {
				bindErr = s.bindShareSearchIdentity(item.Token, actor.User.ID, siteShareIdentity{TMDBID: input.TMDBID, MediaType: input.MediaType, Title: verified.Title, Names: names, Season: cloneInt(input.Season), Year: cloneInt(verified.ReleaseYear), SeasonCount: verified.Snapshot.SeasonCount})
			} else {
				bindErr = s.bindClaimRecognition(item.Token, actor.User.ID, input.TMDBID, input.MediaType, mediaIdentitySourceDirectID, mediaIdentityStatusVerified, false)
			}
			if bindErr != nil {
				s.deleteClaim(item.Token)
				rejections.add("identity_bind_failed", item.Title)
				continue
			}
			item.MatchedName = name.Value
			item.ResourceFingerprint = privateResultFingerprint(group.SiteID, identity)
			if duplicate {
				// Another language query may find a newer post for the same evolving share.
				s.deleteClaim(target.Items[index].Token)
				target.Items[index] = item
				replaced++
			} else {
				privateKeys[key] = len(target.Items)
				target.Items = append(target.Items, item)
				kept++
			}
		}
		rejections.record(aliasCtx, s.recordSearch)
		if group.Status == "success" {
			s.recordSearch(aliasCtx, "site.identity.finish", "identity", "confirmed", fmt.Sprintf("名称“%s”返回 %d 条可用结果，作品与季集验证、去重后新增 %d 条，替换同分享较旧帖子 %d 条，当前站点合计 %d 条", serverlog.BusinessDisplayName(name.Value), len(group.Items), kept, replaced, len(target.Items)), map[string]any{"result_count": len(group.Items), "identity_kept": kept, "updated_count": replaced, "deduplicated_count": len(target.Items)})
		}
		// PanSou builds one bounded multi-alias snapshot across its cached pages.
		// Internal pages are not the requested client page and must not be emitted
		// as cumulative SSE rows. The coordinator publishes its final projection.
		if record.Kind != pansou.Kind {
			snapshot := cloneSearchGroup(*target)
			sortIdentitySearchItems(snapshot.Items)
			if !publishSearchSnapshot(ctx, snapshot) {
				target.Partial, target.IncompleteReason = true, CodeSearchResultLimit
				break
			}
		}
		if group.ErrorCode == CodeSearchRateWait || group.ErrorCode == CodeSearchCancelled || ctx.Err() != nil {
			break
		}
	}
	if target == nil {
		definition, _ := builtin.DefinitionForKey(record.Kind)
		return SiteSearchGroup{SiteID: record.ID, SiteName: record.Name, SiteType: definition.SiteType, Status: "error", ErrorCode: CodeSiteUnavailable, Page: input.Page, Items: []SiteSearchResult{}}
	}
	if record.Kind == pansou.Kind && target.ErrorCount > 0 && target.Status == "success" {
		target.Partial = true
		target.Warnings = append(target.Warnings, sitepkg.SearchWarning{Code: "pansou_alias_search_incomplete", Message: "部分作品名称搜索未完成，已保留成功返回的结果"})
	}
	if target.QueriesCompleted < target.QueriesTotal {
		target.Partial = target.QueriesCompleted > 0
		if ctx.Err() != nil {
			target.IncompleteReason = searchContextError(ctx, CodeSearchSiteBudget)
		}
		if target.IncompleteReason == "" {
			target.IncompleteReason = CodeSearchSiteBudget
		}
	}
	sortIdentitySearchItems(target.Items)
	return *target
}

func privateResultFingerprint(siteID uint, torrentID string) string {
	sum := sha256.Sum256([]byte(strconv.FormatUint(uint64(siteID), 10) + "\x00" + torrentID))
	return hex.EncodeToString(sum[:])
}

func sortIdentitySearchItems(items []SiteSearchResult) {
	sort.SliceStable(items, func(left, right int) bool {
		leftSeeders, rightSeeders := intValue(items[left].Seeders), intValue(items[right].Seeders)
		if leftSeeders != rightSeeders {
			return leftSeeders > rightSeeders
		}
		if compared := compareOptionalTime(items[left].Published, items[right].Published); compared != 0 {
			return compared > 0
		}
		if items[left].SizeBytes != items[right].SizeBytes {
			return items[left].SizeBytes > items[right].SizeBytes
		}
		return items[left].Title < items[right].Title
	})
}

func identitySearchYear(mediaType string, year *int) *int {
	if !strings.EqualFold(strings.TrimSpace(mediaType), "movie") {
		return nil
	}
	return cloneInt(year)
}

func mediaIdentityResultMatches(title string, names []tmdb.SearchName, mediaType string, year, season *int) bool {
	parsed, err := mediarecognition.Parse(mediarecognition.InputFacts{PackageName: title, SourceKind: mediarecognition.SourceDownload})
	if err != nil {
		return false
	}
	candidate := identitySearchKey(parsed.CanonicalTitle)
	matched := false
	for _, name := range names {
		key := identitySearchKey(name.Value)
		if key != "" && (candidate == key || strings.Contains(candidate, key) || strings.Contains(key, candidate)) {
			matched = true
			break
		}
	}
	if !matched {
		return false
	}
	// For TV, TMDB supplies the work's first-air year while release titles may
	// contain a season year, episode year, encode year, or no year at all. It is
	// useful metadata for display but cannot be a hard identity gate. Movie
	// release years remain a bounded disambiguator.
	if strings.EqualFold(strings.TrimSpace(mediaType), "movie") && year != nil && parsed.Year != nil && absInt(*year-*parsed.Year) > 1 {
		return false
	}
	return season == nil || parsed.Season != nil && *season == *parsed.Season
}

func identitySearchKey(value string) string {
	value = cases.Fold().String(norm.NFKC.String(strings.TrimSpace(value)))
	return strings.Map(func(character rune) rune {
		if unicode.IsLetter(character) || unicode.IsNumber(character) {
			return character
		}
		return -1
	}, value)
}

func (s *SiteService) deleteClaim(token string) {
	s.vaultMu.Lock()
	if claim, ok := s.vault[token]; ok && !claim.Published && !claim.InFlight {
		delete(s.vault, token)
	}
	s.vaultMu.Unlock()
}

func (s *SiteService) sitePriorityMap(siteID *uint, siteIDs []uint) map[uint]int {
	var records []models.Site
	query := s.db.Select("id", "priority")
	if siteID != nil {
		query = query.Where("id = ?", *siteID)
	} else if len(siteIDs) > 0 {
		query = query.Where("id IN ?", siteIDs)
	}
	_ = query.Find(&records).Error
	result := make(map[uint]int, len(records))
	for _, record := range records {
		result[record.ID] = record.Priority
	}
	return result
}

func intValue(value *int) int {
	if value == nil {
		return -1
	}
	return *value
}

func compareOptionalTime(left, right *time.Time) int {
	if left == nil && right == nil {
		return 0
	}
	if left == nil {
		return -1
	}
	if right == nil {
		return 1
	}
	if left.UnixNano() < right.UnixNano() {
		return -1
	}
	if left.UnixNano() > right.UnixNano() {
		return 1
	}
	return 0
}

func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}
