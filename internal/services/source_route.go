package services

import (
	"context"
	"strings"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/site/builtin"
)

const (
	RouteSourcePT       = "pt"
	RouteSourceBT       = "bt"
	RouteSource115Share = "115_share"
)

type SourceRouteRecommendationInput struct {
	ResultToken    string `json:"result_token"`
	MediaLibraryID *uint  `json:"media_library_id,omitempty"`
	ExpectedBytes  *int64 `json:"expected_bytes,omitempty"`
}

type SourceRouteSelection struct {
	DownloaderID   string `json:"downloader_id"`
	MediaLibraryID uint   `json:"media_library_id"`
}

type SourceRouteChoice struct {
	DownloaderID   string `json:"downloader_id"`
	DownloaderName string `json:"downloader_name"`
	DownloaderType string `json:"downloader_type"`
	MediaLibraryID uint   `json:"media_library_id"`
	LibraryName    string `json:"library_name"`
	Enabled        bool   `json:"enabled"`
	ReasonCode     string `json:"reason_code"`
	ReasonMessage  string `json:"reason_message"`
	RouteKind      string `json:"route_kind"`
	RouteLabel     string `json:"route_label"`
}

type SourceRouteRecommendation struct {
	SourceKind  string                `json:"source_kind"`
	Recommended *SourceRouteSelection `json:"recommended"`
	Choices     []SourceRouteChoice   `json:"choices"`
}

type FollowRoutePreviewInput struct {
	SiteIDs        []uint `json:"site_ids"`
	MediaLibraryID uint   `json:"media_library_id"`
}

type FollowSiteRoute struct {
	SiteID        uint                  `json:"site_id"`
	SiteName      string                `json:"site_name"`
	SourceKind    string                `json:"source_kind"`
	Recommended   *SourceRouteSelection `json:"recommended"`
	ReasonCode    string                `json:"reason_code"`
	ReasonMessage string                `json:"reason_message"`
}

type FollowRoutePreview struct {
	Routes    []FollowSiteRoute `json:"routes"`
	Available bool              `json:"available"`
}

func routeSourceForSite(site models.Site) (string, error) {
	if site.SourceType == "plugin" && site.Kind == pluginResourceKind {
		return RouteSourceBT, nil
	}
	definition, ok := builtin.DefinitionForKey(site.Kind)
	if !ok {
		return "", appError(CodeSiteKindUnsupported, "站点来源无法确认", nil)
	}
	switch definition.SiteType {
	case builtin.SiteTypePT:
		return RouteSourcePT, nil
	case builtin.SiteTypeBT:
		return RouteSourceBT, nil
	case builtin.SiteTypeCloud:
		return RouteSource115Share, nil
	default:
		return "", appError(CodeSiteKindUnsupported, "站点来源无法确认", nil)
	}
}

func routeSourceDownloadKind(source string) string {
	if source == RouteSource115Share {
		return downloadpkg.SourcePan115Share
	}
	return downloadpkg.SourceTorrent
}

func routeDownloaderApplicable(source string, downloader models.Downloader) bool {
	switch source {
	case RouteSourcePT:
		return downloader.Type == models.DownloaderTypeQBittorrent
	case RouteSource115Share:
		return downloader.Type == models.DownloaderTypePan115Offline
	case RouteSourceBT:
		return downloader.Type == models.DownloaderTypeQBittorrent || downloader.Type == models.DownloaderTypePan115Offline
	default:
		return false
	}
}

// RecommendSiteRoute reads the actor-bound claim without reserving it. Final
// SiteService.Download repeats provenance, permission and target checks.
func (s *SiteService) RecommendSiteRoute(ctx context.Context, actor Actor, input SourceRouteRecommendationInput) (SourceRouteRecommendation, error) {
	claim, err := s.resolveAvailableClaim(strings.TrimSpace(input.ResultToken), actor.User.ID)
	if err != nil {
		return SourceRouteRecommendation{}, err
	}
	var site models.Site
	if err := s.db.WithContext(ctx).Select("revision", "enabled").First(&site, claim.SiteID).Error; err != nil || !site.Enabled || claim.SiteRevision != 0 && claim.SiteRevision != site.Revision {
		return SourceRouteRecommendation{}, appError(CodeSiteResultExpired, "站点配置或搜索结果已失效，请重新搜索", err)
	}
	return s.downloads.RecommendSourceRoute(ctx, actor, claim.SiteID, input.MediaLibraryID, input.ExpectedBytes)
}

func (s *DownloadService) RecommendSourceRoute(ctx context.Context, actor Actor, siteID uint, libraryID *uint, expectedBytes ...*int64) (SourceRouteRecommendation, error) {
	if !actor.Can(authz.PermissionDownloadsCreate) || !actor.Can(authz.PermissionDiscoveryRead) {
		return SourceRouteRecommendation{}, appError(CodePermissionDenied, "无权选择下载路线", nil)
	}
	var size *int64
	if len(expectedBytes) > 0 {
		size = expectedBytes[0]
	}
	if size != nil && *size < 0 {
		return SourceRouteRecommendation{}, appError(CodeInvalidRequest, "预计资源大小无效", nil)
	}
	var site models.Site
	if err := s.db.WithContext(ctx).First(&site, siteID).Error; err != nil || !site.Enabled {
		return SourceRouteRecommendation{}, appError(CodeSiteUnavailable, "站点不存在或已停用", err)
	}
	if !actor.CanResource(authz.PermissionDiscoveryRead, models.AuthorizationResourceSite, uintID(site.ID)) {
		return SourceRouteRecommendation{}, appError(CodePermissionDenied, "无权使用此站点", nil)
	}
	source, err := routeSourceForSite(site)
	if err != nil {
		return SourceRouteRecommendation{}, err
	}
	var downloaders []models.Downloader
	if err := s.db.WithContext(ctx).Where("enabled = ?", true).Order("sort_order, id").Find(&downloaders).Error; err != nil {
		return SourceRouteRecommendation{}, err
	}
	var libraries []models.MediaLibrary
	libraryQuery := s.db.WithContext(ctx).Where("enabled = ?", true)
	if libraryID != nil {
		libraryQuery = libraryQuery.Where("id = ?", *libraryID)
	}
	if err := libraryQuery.Order("sort_order, id").Find(&libraries).Error; err != nil {
		return SourceRouteRecommendation{}, err
	}
	if libraryID != nil && len(libraries) == 0 {
		return SourceRouteRecommendation{}, appError(CodeMediaLibraryStorageUnavailable, "目标媒体库不存在或已停用", nil)
	}
	visibleLibraries := make([]models.MediaLibrary, 0, len(libraries))
	for _, library := range libraries {
		id := uintID(library.ID)
		if actor.CanResource(authz.PermissionDownloadsCreate, models.AuthorizationResourceMediaLibrary, id) && actor.CanResource(authz.PermissionMediaLibrariesRead, models.AuthorizationResourceMediaLibrary, id) {
			visibleLibraries = append(visibleLibraries, library)
		}
	}
	if libraryID != nil && len(visibleLibraries) == 0 {
		return SourceRouteRecommendation{}, appError(CodePermissionDenied, "无权使用这个媒体库", nil)
	}
	visibleDownloaders := make([]models.Downloader, 0, len(downloaders))
	for _, downloader := range downloaders {
		if actor.CanResource(authz.PermissionDownloadsCreate, models.AuthorizationResourceDownloader, downloader.ID) {
			visibleDownloaders = append(visibleDownloaders, downloader)
		}
	}
	if len(downloaders) > 0 && len(visibleDownloaders) == 0 {
		return SourceRouteRecommendation{}, appError(CodePermissionDenied, "没有当前允许使用的下载器", nil)
	}
	if len(libraries) > 0 && len(visibleLibraries) == 0 {
		return SourceRouteRecommendation{}, appError(CodePermissionDenied, "没有当前允许入库的媒体库", nil)
	}
	result := SourceRouteRecommendation{SourceKind: source, Choices: make([]SourceRouteChoice, 0, len(visibleLibraries)*len(visibleDownloaders))}
	choiceByDownloader := make(map[string]map[uint]SourceRouteChoice, len(visibleDownloaders))
	previewSourceKind, previewSiteID := routeSourceDownloadKind(source), &siteID
	if site.SourceType == "plugin" && site.Kind == pluginResourceKind {
		// A plugin resource resolves directly to a magnet. SiteService.Download
		// verifies its saved plugin provenance before accepting a 115 route.
		previewSourceKind, previewSiteID = downloadpkg.SourceURL, nil
	}
	for _, downloader := range visibleDownloaders {
		if !routeDownloaderApplicable(source, downloader) {
			continue
		}
		byLibrary := make(map[uint]SourceRouteChoice, len(visibleLibraries))
		preview, previewErr := s.PreviewRoutes(ctx, actor, DownloadRoutePreviewInput{DownloaderID: downloader.ID, SourceKind: previewSourceKind, SiteID: previewSiteID, ExpectedBytes: size})
		for _, library := range visibleLibraries {
			choice := SourceRouteChoice{DownloaderID: downloader.ID, DownloaderName: downloader.Name, DownloaderType: downloader.Type, MediaLibraryID: library.ID, LibraryName: library.Name}
			if previewErr != nil {
				choice.ReasonCode = ErrorCode(previewErr)
				choice.ReasonMessage = safeErrorMessage(previewErr, "下载器当前无法接收此资源")
			} else {
				for _, option := range preview.Options {
					if option.MediaLibraryID == library.ID {
						choice.Enabled, choice.ReasonCode, choice.ReasonMessage = option.Enabled, option.ReasonCode, option.ReasonMessage
						choice.RouteKind, choice.RouteLabel = option.RouteKind, option.RouteLabel
						break
					}
				}
				if !choice.Enabled && choice.ReasonCode == "" {
					choice.ReasonCode, choice.ReasonMessage = CodeTransferRouteUnsupported, "该媒体库当前无法作为目标"
				}
			}
			byLibrary[library.ID] = choice
		}
		choiceByDownloader[downloader.ID] = byLibrary
	}
	for _, library := range visibleLibraries {
		for _, downloader := range visibleDownloaders {
			if !routeDownloaderApplicable(source, downloader) {
				result.Choices = append(result.Choices, SourceRouteChoice{DownloaderID: downloader.ID, DownloaderName: downloader.Name, DownloaderType: downloader.Type, MediaLibraryID: library.ID, LibraryName: library.Name, ReasonCode: CodeDownloadSourceInvalid, ReasonMessage: "该下载器不支持此站点资源"})
				continue
			}
			result.Choices = append(result.Choices, choiceByDownloader[downloader.ID][library.ID])
		}
	}
	result.Recommended = recommendedSourceRoute(source, visibleLibraries, visibleDownloaders, choiceByDownloader)
	return result, nil
}

func recommendedSourceRoute(source string, libraries []models.MediaLibrary, downloaders []models.Downloader, choices map[string]map[uint]SourceRouteChoice) *SourceRouteSelection {
	if source == RouteSourceBT {
		// For manual intake the first usable target library wins. Within that
		// library, downloader order decides the execution route.
		for _, library := range libraries {
			for _, downloader := range downloaders {
				if routeDownloaderApplicable(source, downloader) && choices[downloader.ID][library.ID].Enabled {
					return &SourceRouteSelection{DownloaderID: downloader.ID, MediaLibraryID: library.ID}
				}
			}
		}
	} else {
		for _, downloader := range downloaders {
			if !routeDownloaderApplicable(source, downloader) {
				continue
			}
			for _, library := range libraries {
				if choices[downloader.ID][library.ID].Enabled {
					return &SourceRouteSelection{DownloaderID: downloader.ID, MediaLibraryID: library.ID}
				}
			}
			// PT and share never advance past the first eligible downloader.
			break
		}
	}
	return nil
}

func (s *FollowService) PreviewSourceRoutes(ctx context.Context, actor Actor, input FollowRoutePreviewInput) (FollowRoutePreview, error) {
	if !actor.Can(authz.PermissionDownloadsCreate) || !actor.Can(authz.PermissionDiscoveryRead) {
		return FollowRoutePreview{}, appError(CodePermissionDenied, "无权预览订阅路线", nil)
	}
	if len(input.SiteIDs) == 0 || len(input.SiteIDs) > 20 || !positiveUints(input.SiteIDs) || input.MediaLibraryID == 0 {
		return FollowRoutePreview{}, appError(CodeInvalidRequest, "订阅路线预览参数无效", nil)
	}
	if s.downloads == nil {
		return FollowRoutePreview{}, appError(CodeDownloaderUnavailable, "下载服务不可用", nil)
	}
	result := FollowRoutePreview{Routes: make([]FollowSiteRoute, 0, len(input.SiteIDs)), Available: true}
	for _, siteID := range uniqueSortedUintsByOrder(input.SiteIDs, 20) {
		var site models.Site
		if err := s.db.WithContext(ctx).First(&site, siteID).Error; err != nil || !site.Enabled {
			return FollowRoutePreview{}, appError(CodeSiteUnavailable, "订阅站点不存在或已停用", err)
		}
		libraryID := input.MediaLibraryID
		recommendation, err := s.downloads.RecommendSourceRoute(ctx, actor, siteID, &libraryID)
		if err != nil {
			return FollowRoutePreview{}, err
		}
		entry := FollowSiteRoute{SiteID: siteID, SiteName: site.Name, SourceKind: recommendation.SourceKind, Recommended: recommendation.Recommended}
		if entry.Recommended == nil {
			result.Available = false
			entry.ReasonCode, entry.ReasonMessage = CodeTransferRouteUnsupported, "该站点没有可用的下载器与目标媒体库路线"
			for _, choice := range recommendation.Choices {
				if routeDownloaderApplicable(recommendation.SourceKind, models.Downloader{Type: choice.DownloaderType}) && choice.ReasonCode != "" {
					entry.ReasonCode, entry.ReasonMessage = choice.ReasonCode, choice.ReasonMessage
					break
				}
			}
		}
		result.Routes = append(result.Routes, entry)
	}
	return result, nil
}
