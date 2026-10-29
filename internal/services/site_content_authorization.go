package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"gorm.io/gorm"
)

// ProviderInspectionScope is a private, single-share read grant. Its association
// is selected from current server records, never a client-supplied Connection ID.
// It conveys no write/receive/root-directory or credential-export authority.
type ProviderInspectionScope struct {
	ConnectionID                         uint
	Provider, Digest, AuthorizationScope string
	PreferredDownloader                  string
}

func contentInspectionExpired() error {
	return appError(CodeSiteResultExpired, "内容检查已过期或授权范围已变化，请重新读取", nil)
}

func (s *SiteService) inspectionActor(ctx context.Context, actor Actor) (Actor, error) {
	current, err := NewAuthorizationService(s.db).resolveWithDB(s.db.WithContext(ctx), actor.User.ID)
	if err != nil {
		return Actor{}, err
	}
	if current.User.AuthzVersion != actor.User.AuthzVersion || inspectionAuthorizationScope(current) != inspectionAuthorizationScope(actor) {
		return Actor{}, contentInspectionExpired()
	}
	if !current.Can(authz.PermissionDiscoveryRead) {
		return Actor{}, appError(CodePermissionDenied, "无权读取资源内容", nil)
	}
	// Preserve the original request/claim encoding after proving it is the same
	// effective authority. nil and empty collections do not change permission.
	return actor, nil
}

func inspectionAuthorizationScope(actor Actor) string {
	if len(actor.Permissions) == 0 {
		actor.Permissions = nil
	}
	if len(actor.DeniedPermissions) == 0 {
		actor.DeniedPermissions = nil
	}
	if len(actor.ResourceRules) == 0 {
		actor.ResourceRules = nil
	} else {
		actor.ResourceRules = append([]AuthorizationRule(nil), actor.ResourceRules...)
		sort.Slice(actor.ResourceRules, func(i, j int) bool {
			left, right := actor.ResourceRules[i], actor.ResourceRules[j]
			return left.PermissionCode+":"+left.ResourceType+":"+left.ResourceID+":"+left.Effect < right.PermissionCode+":"+right.ResourceType+":"+right.ResourceID+":"+right.Effect
		})
	}
	if len(actor.ResourceAccessPolicies) == 0 {
		actor.ResourceAccessPolicies = nil
	}
	return discoveryAuthorizationScope(actor)
}

func (s *SiteService) contentClaim(ctx context.Context, actor Actor, token string) (siteResultClaim, models.Site, error) {
	claim, err := s.resolveAvailableClaim(strings.TrimSpace(token), actor.User.ID)
	if err != nil {
		return claim, models.Site{}, err
	}
	record, err := s.validateContentClaim(ctx, actor, claim)
	return claim, record, err
}

func (s *SiteService) validateContentClaim(ctx context.Context, actor Actor, claim siteResultClaim) (models.Site, error) {
	var record models.Site
	if claim.ActorID != actor.User.ID || !claim.ExpiresAt.After(s.now()) || !actor.CanResource(authz.PermissionDiscoveryRead, models.AuthorizationResourceSite, uintID(claim.SiteID)) {
		return record, contentInspectionExpired()
	}
	if claim.AuthorizationScope != "" && claim.AuthorizationScope != discoveryAuthorizationScope(actor) {
		return record, contentInspectionExpired()
	}
	if err := s.db.WithContext(ctx).First(&record, claim.SiteID).Error; err != nil || !record.Enabled || claim.SiteRevision != 0 && record.Revision != claim.SiteRevision {
		return record, contentInspectionExpired()
	}
	if record.SourceType == "plugin" {
		if available, code, message := s.pluginResourceSearchAvailability(record); !available {
			return record, appError(code, message, nil)
		}
		if _, err := s.contentPluginLifecycle(s.db.WithContext(ctx), actor, claim, record); err != nil {
			return record, err
		}
	}
	return record, nil
}

func (s *SiteService) contentPluginLifecycle(db *gorm.DB, actor Actor, claim siteResultClaim, record models.Site) (string, error) {
	if record.SourceType != "plugin" {
		return "", nil
	}
	var saved models.PluginResourceClaim
	if db.Where("id = ? AND owner_id = ? AND site_id = ? AND consumed_at IS NULL AND expires_at > ?", claim.PluginClaimID, actor.User.ID, record.ID, s.now()).First(&saved).Error != nil || saved.ResourceID != claim.TorrentID || saved.PluginID != record.PluginID || saved.PluginConnectionID != record.PluginConnectionID {
		return "", contentInspectionExpired()
	}
	lifecycle, err := pluginResourceLifecycle(db, record, saved.PluginID, saved.PluginVersion)
	if err != nil || claim.PluginLifecycle != "" && lifecycle != claim.PluginLifecycle {
		return "", contentInspectionExpired()
	}
	return lifecycle, nil
}

func inspectionDigest(value any) string {
	raw, _ := json.Marshal(value)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (s *SiteService) providerInspectionScope(ctx context.Context, actor Actor, claim siteResultClaim, preferred string) (ProviderInspectionScope, error) {
	return s.providerInspectionScopeWithDB(s.db.WithContext(ctx), actor, claim, preferred)
}

func (s *SiteService) providerInspectionScopeWithDB(db *gorm.DB, actor Actor, claim siteResultClaim, preferred string) (ProviderInspectionScope, error) {
	var empty ProviderInspectionScope
	if !actor.Can(authz.PermissionDownloadsCreate) {
		return empty, appError(CodePermissionDenied, "无权读取网盘分享", nil)
	}
	provider := map[string]string{RouteSource115Share: models.ConnectionProviderPan115, RouteSourceQuarkShare: models.ConnectionProviderQuark}[claimCloudSource(claim)]
	if provider != models.ConnectionProviderPan115 && provider != models.ConnectionProviderQuark {
		return empty, appError(CodeDownloadSourceInvalid, "该来源尚不支持内容检查", nil)
	}
	type candidate struct {
		connection  models.Connection
		association any
	}
	var candidates []candidate
	addStorage := func(storageID uint, association any) {
		var storage models.Storage
		if db.First(&storage, storageID).Error != nil || !storage.Enabled || storage.Type != provider || storage.ConnectionID == nil {
			return
		}
		var connection models.Connection
		if db.First(&connection, *storage.ConnectionID).Error != nil || !connection.Enabled || connection.Provider != provider {
			return
		}
		candidates = append(candidates, candidate{connection, []any{association, storage.ID, storage.RootPath, storage.Enabled}})
	}
	if preferred != "" {
		var downloader models.Downloader
		if !actor.CanResource(authz.PermissionDownloadsCreate, models.AuthorizationResourceDownloader, preferred) {
			return empty, appError(CodePermissionDenied, "无权使用这个读取关联", nil)
		}
		if db.First(&downloader, "id = ?", preferred).Error != nil || downloader.StorageID == nil || !routeDownloaderApplicable(claimCloudSource(claim), downloader) {
			return empty, appError(CodeDownloadSourceInvalid, "读取关联与分享来源不一致", nil)
		}
		addStorage(*downloader.StorageID, []any{"downloader", downloader.ID, downloader.OwnerID, downloader.Enabled, downloader.Type, downloader.ExecutionLocation, downloader.NodeID})
	} else {
		if actor.Can(authz.PermissionConnectionsRead) {
			var connections []models.Connection
			if err := db.Where("enabled = ? AND provider = ?", true, provider).Order("id").Find(&connections).Error; err != nil {
				return empty, err
			}
			for _, connection := range connections {
				candidates = append(candidates, candidate{connection, []any{"connection", connection.ID}})
			}
		}
		var downloaders []models.Downloader
		if err := db.Order("sort_order, id").Find(&downloaders).Error; err != nil {
			return empty, err
		}
		for _, downloader := range downloaders {
			if downloader.StorageID != nil && routeDownloaderApplicable(claimCloudSource(claim), downloader) && actor.CanResource(authz.PermissionDownloadsCreate, models.AuthorizationResourceDownloader, downloader.ID) {
				addStorage(*downloader.StorageID, []any{"downloader", downloader.ID, downloader.OwnerID, downloader.Enabled, downloader.Type, downloader.ExecutionLocation, downloader.NodeID})
			}
		}
		var libraries []models.MediaLibrary
		if err := db.Where("enabled = ?", true).Order("sort_order, id").Find(&libraries).Error; err != nil {
			return empty, err
		}
		for _, library := range libraries {
			if actor.CanResource(authz.PermissionMediaLibrariesRead, models.AuthorizationResourceMediaLibrary, uintID(library.ID)) {
				addStorage(library.StorageID, []any{"library", library.ID, library.RelativeRoot})
			}
		}
	}
	if len(candidates) == 0 {
		return empty, appError(CodePermissionDenied, "没有获准读取该分享的同网盘账号关联，请联系管理员授权", nil)
	}
	selected := candidates[0]
	scope := ProviderInspectionScope{ConnectionID: selected.connection.ID, Provider: provider, AuthorizationScope: discoveryAuthorizationScope(actor), PreferredDownloader: preferred}
	scope.Digest = inspectionDigest([]any{scope.ConnectionID, selected.connection.Revision, selected.connection.AccountID, scope.Provider, scope.AuthorizationScope, selected.association})
	return scope, nil
}

func (s *SiteService) validateInspectionScope(ctx context.Context, actor Actor, claim siteResultClaim, scope ProviderInspectionScope) error {
	currentActor, err := s.inspectionActor(ctx, actor)
	if err != nil {
		return err
	}
	if _, err = s.validateContentClaim(ctx, currentActor, claim); err != nil {
		return err
	}
	current, err := s.providerInspectionScope(ctx, currentActor, claim, scope.PreferredDownloader)
	if err != nil || current.Digest != scope.Digest {
		return contentInspectionExpired()
	}
	return nil
}

func (s *SiteService) inspectionDriver(scope ProviderInspectionScope) (cloud.ShareBrowseDriver, error) {
	if s.downloads == nil || s.downloads.downloader == nil || s.downloads.downloader.connections == nil {
		return nil, appError(CodeConnectionUnavailable, "网盘读取服务不可用", nil)
	}
	_, driver, err := s.downloads.downloader.connections.cachedDriver(scope.ConnectionID)
	if err != nil {
		return nil, err
	}
	browse, ok := driver.(cloud.ShareBrowseDriver)
	if !ok {
		return nil, appError(CodeDownloadSourceInvalid, "该账号尚不支持分享目录读取", nil)
	}
	return browse, nil
}
