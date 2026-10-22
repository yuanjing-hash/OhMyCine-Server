package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	sitepkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/site"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/site/pansou"
)

const MixedDiscoveryCapability = "mixed_discovery_v1"

func mixedDiscovery(capabilities []string) bool {
	for _, value := range capabilities {
		if value == MixedDiscoveryCapability {
			return true
		}
	}
	return false
}

func discoveryAuthorizationScope(actor Actor) string {
	// This is private cache/claim binding, not a public permission inventory.
	raw, _ := json.Marshal(struct {
		ID                  uint
		Permissions, Denied map[string]struct{}
		Rules               []AuthorizationRule
		Policies            map[string]ResourceAccessPolicy
	}{actor.User.ID, actor.Permissions, actor.DeniedPermissions, actor.ResourceRules, actor.ResourceAccessPolicies})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (s *SiteService) panSouAutomaticTypes(actor Actor) ([]string, error) {
	providers := map[string]bool{"magnet": true}
	var downloaders []models.Downloader
	if err := s.db.Where("enabled = ?", true).Find(&downloaders).Error; err != nil {
		return nil, err
	}
	for _, row := range downloaders {
		if !actor.CanResource(authz.PermissionDownloadsCreate, models.AuthorizationResourceDownloader, row.ID) {
			continue
		}
		provider := ""
		if row.Type == models.DownloaderTypePan115Offline {
			provider = "115"
		}
		if row.Type == models.DownloaderTypeQuarkShare {
			provider = "quark"
		}
		if provider == "" || row.StorageID == nil {
			continue
		}
		var storage models.Storage
		if err := s.db.First(&storage, *row.StorageID).Error; err != nil || !storage.Enabled || storage.ConnectionID == nil {
			continue
		}
		var connection models.Connection
		if err := s.db.First(&connection, *storage.ConnectionID).Error; err == nil && connection.Enabled && panSouProviderName(connection.Provider) == provider {
			providers[provider] = true
		}
	}
	var libraries []models.MediaLibrary
	if err := s.db.Where("enabled = ?", true).Find(&libraries).Error; err != nil {
		return nil, err
	}
	for _, row := range libraries {
		if !actor.CanResource(authz.PermissionMediaLibrariesRead, models.AuthorizationResourceMediaLibrary, uintID(row.ID)) && !actor.CanIngestLibrary(uintID(row.ID)) {
			continue
		}
		var storage models.Storage
		if err := s.db.First(&storage, row.StorageID).Error; err != nil || !storage.Enabled || storage.ConnectionID == nil {
			continue
		}
		var connection models.Connection
		if err := s.db.First(&connection, *storage.ConnectionID).Error; err == nil && connection.Enabled && panSouProviderName(connection.Provider) != "" {
			providers[panSouProviderName(connection.Provider)] = true
		}
	}
	// Connection managers may search their configured accounts before creating
	// a first library. Ordinary users never need this management permission.
	if actor.Can(authz.PermissionConnectionsRead) {
		var connections []models.Connection
		if err := s.db.Where("enabled = ? AND provider IN ?", true, []string{models.ConnectionProviderPan115, models.ConnectionProviderQuark}).Find(&connections).Error; err != nil {
			return nil, err
		}
		for _, row := range connections {
			providers[panSouProviderName(row.Provider)] = true
		}
	}
	types := make([]string, 0, len(providers))
	for provider := range providers {
		types = append(types, provider)
	}
	sort.Strings(types)
	return types, nil
}

func panSouProviderName(provider string) string {
	switch provider {
	case models.ConnectionProviderPan115:
		return "115"
	case models.ConnectionProviderQuark:
		return "quark"
	}
	return ""
}

func (s *SiteService) SavedPanSouScope(ctx context.Context, actor Actor, id uint, refresh bool) (sitepkg.ChannelScope, error) {
	if !actor.CanResource(authz.PermissionSitesRead, models.AuthorizationResourceSite, uintID(id)) {
		return sitepkg.ChannelScope{}, appError(CodePermissionDenied, "无权读取这个站点配置", nil)
	}
	var record models.Site
	if err := s.db.First(&record, id).Error; err != nil {
		return sitepkg.ChannelScope{}, siteNotFound(err)
	}
	return s.PanSouScope(ctx, actor, PanSouScopeInput{SiteID: &id, Revision: record.Revision, Refresh: refresh})
}

func (s *SiteService) panSouQuery(actor Actor, record models.Site, config sitepkg.Config, input SiteSearchInput) (sitepkg.Query, error) {
	q := sitepkg.Query{Keyword: input.Keyword, MediaType: input.MediaType, Year: input.Year, TMDBID: input.TMDBID, Page: input.Page, SnapshotID: input.SnapshotID, Refresh: input.Refresh}
	cfg, err := pansou.NormalizeConfig(config.Cloud)
	if err != nil {
		return q, appError(CodeInvalidRequest, "PanSou配置无效", nil)
	}
	q.TypePolicy = input.TypePolicy
	if q.TypePolicy == "" {
		q.TypePolicy = cfg.TypePolicy
	}
	q.CloudTypes = input.CloudTypes
	if q.CloudTypes == nil {
		q.CloudTypes = cfg.CloudTypes
	}
	if !mixedDiscovery(input.ClientCapabilities) {
		q.TypePolicy = "manual"
		q.CloudTypes = []string{"115"}
	} else if q.TypePolicy == "auto" {
		q.CloudTypes, err = s.panSouAutomaticTypes(actor)
		if err != nil {
			return q, err
		}
	}
	q.ScopeKey = uintID(actor.User.ID) + ":" + uintID(record.ID) + ":" + stringifyRevision(record.Revision) + ":" + discoveryAuthorizationScope(actor) + ":" + strings.Join(input.ClientCapabilities, ",")
	return q, nil
}

func stringifyRevision(revision uint64) string { raw, _ := json.Marshal(revision); return string(raw) }

type PanSouScopeInput struct {
	SiteID      *uint                `json:"site_id,omitempty"`
	Revision    uint64               `json:"revision,omitempty"`
	BaseURL     string               `json:"base_url"`
	CloudConfig *sitepkg.CloudConfig `json:"cloud_config"`
	Username    *string              `json:"username,omitempty"`
	Password    *string              `json:"password,omitempty"`
	Refresh     bool                 `json:"refresh,omitempty"`
}

func (s *SiteService) PanSouScope(ctx context.Context, actor Actor, input PanSouScopeInput) (sitepkg.ChannelScope, error) {
	if input.SiteID == nil && !actor.Can(authz.PermissionSitesCreate) {
		return sitepkg.ChannelScope{}, appError(CodePermissionDenied, "无权检测PanSou配置", nil)
	}
	var saved models.Site
	var credential siteCredentialEnvelope
	if input.SiteID != nil {
		if !actor.CanResource(authz.PermissionSitesRead, models.AuthorizationResourceSite, uintID(*input.SiteID)) {
			return sitepkg.ChannelScope{}, appError(CodePermissionDenied, "无权读取这个站点配置", nil)
		}
		if err := s.db.First(&saved, *input.SiteID).Error; err != nil {
			return sitepkg.ChannelScope{}, siteNotFound(err)
		}
		if saved.Kind != pansou.Kind {
			return sitepkg.ChannelScope{}, appError(CodeInvalidRequest, "这个站点不支持PanSou频道检测", nil)
		}
		if input.Revision != 0 && input.Revision != saved.Revision {
			return sitepkg.ChannelScope{}, appError(CodeConflict, "站点配置已变化，请重新加载", nil)
		}
		if input.BaseURL == "" {
			input.BaseURL = saved.BaseURL
		}
		if input.CloudConfig == nil {
			input.CloudConfig = cloudSiteConfig(saved)
		}
		// A changed candidate URL must never receive stored credentials. An exact
		// saved revision is mandatory when a draft asks to reuse them.
		if input.BaseURL == saved.BaseURL && input.Revision == saved.Revision {
			var err error
			credential, err = s.decryptCredential(saved)
			if err != nil {
				return sitepkg.ChannelScope{}, err
			}
		}
	}
	base, err := normalizeSiteBaseURL(pansou.Kind, input.BaseURL)
	if err != nil {
		return sitepkg.ChannelScope{}, err
	}
	if input.Username != nil {
		credential.Username = *input.Username
	}
	if input.Password != nil {
		credential.Password = *input.Password
	}
	cfg, err := normalizeCloudSite(pansou.Kind, input.CloudConfig, &credential)
	if err != nil {
		return sitepkg.ChannelScope{}, err
	}
	resolver, ok := s.adapters[pansou.Kind].(sitepkg.ChannelScopeResolver)
	if !ok {
		return sitepkg.ChannelScope{}, appError(CodeSiteUnavailable, "PanSou频道检测暂不可用", nil)
	}
	scope, resolveErr := resolver.ResolveChannelScope(ctx, sitepkg.Config{BaseURL: base, Cloud: cfg, Username: credential.Username, Password: credential.Password, Timeout: 15 * time.Second}, input.Refresh)
	if resolveErr != nil {
		if scope.DefaultErrorCode != "" {
			return scope, nil
		}
		return scope, siteAdapterError(resolveErr, "PanSou频道范围检测失败")
	}
	return scope, nil
}

type SiteResultLink struct {
	URL        string    `json:"url"`
	ResultType string    `json:"result_type"`
	ExpiresAt  time.Time `json:"expires_at"`
}

func (s *SiteService) ResultLink(ctx context.Context, actor Actor, token string) (SiteResultLink, error) {
	claim, err := s.resolveAvailableClaim(strings.TrimSpace(token), actor.User.ID)
	if err != nil {
		return SiteResultLink{}, err
	}
	if !actor.CanResource(authz.PermissionDiscoveryRead, models.AuthorizationResourceSite, uintID(claim.SiteID)) {
		return SiteResultLink{}, appError(CodePermissionDenied, "无权查看这个搜索结果", nil)
	}
	var row models.Site
	if err := s.db.WithContext(ctx).First(&row, claim.SiteID).Error; err != nil || !row.Enabled || row.Revision != claim.SiteRevision || row.Kind != pansou.Kind {
		return SiteResultLink{}, appError(CodeSiteResultExpired, "站点配置或搜索结果已失效，请重新搜索", nil)
	}
	if claim.AuthorizationScope != "" && claim.AuthorizationScope != discoveryAuthorizationScope(actor) {
		return SiteResultLink{}, appError(CodeSiteResultExpired, "权限范围已变化，请重新搜索", nil)
	}
	kind, normalized, _, err := pansou.NormalizeResource(claim.ResultType, claim.TorrentID, "")
	if err != nil {
		return SiteResultLink{}, appError(CodeDownloadSourceInvalid, "该结果不是受支持的公开资源链接", nil)
	}
	return SiteResultLink{normalized, kind, claim.ExpiresAt}, nil
}
