package services

import (
	"encoding/json"
	"sort"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

type ResourceAccessOption struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Type             string `json:"type"`
	Status           string `json:"status"`
	Deleted          bool   `json:"deleted"`
	EffectiveAllowed bool   `json:"effective_allowed"`
	CanGrant         bool   `json:"can_grant"`
	DenialReason     string `json:"denial_reason"`
}

type ResourceAccessScopeOptions struct {
	Scope        string                 `json:"scope"`
	ResourceType string                 `json:"resource_type"`
	Options      []ResourceAccessOption `json:"options"`
}

type UserResourceAccessOptions struct {
	Revision uint64                       `json:"revision"`
	Scopes   []ResourceAccessScopeOptions `json:"scopes"`
}

func (s *AdminService) resourceAccessActors(tx *gorm.DB, actor Actor, userID uint) (Actor, Actor, error) {
	current, err := s.authz.resolveWithDB(tx, actor.User.ID)
	if err != nil {
		return Actor{}, Actor{}, err
	}
	if !current.Can(authz.PermissionUsersRead) {
		return Actor{}, Actor{}, appError(CodePermissionDenied, "没有查看用户权限的权限", nil)
	}
	var user models.User
	if err := tx.First(&user, userID).Error; err != nil {
		return Actor{}, Actor{}, notFound(err, "用户不存在")
	}
	target, err := s.authz.resolveUserWithDB(tx, userID, false)
	return current, target, err
}

func (s *AdminService) UserResourceAccess(actor Actor, userID uint) (UserResourceAccess, error) {
	var result UserResourceAccess
	err := s.db.Transaction(func(tx *gorm.DB) error {
		_, target, err := s.resourceAccessActors(tx, actor, userID)
		if err != nil {
			return err
		}
		result = resourceAccessDTO(target)
		return nil
	})
	return result, err
}

func (s *AdminService) ReplaceUserResourceAccess(actor Actor, userID uint, input UserResourceAccess, request RequestContext) (UserResourceAccess, error) {
	if actor.User.ID == userID {
		return UserResourceAccess{}, appError(CodeSelfModification, "不能修改当前登录账户的资源权限", nil)
	}
	if input.Revision == 0 || len(input.Policies) != len(resourceAccessScopes) {
		return UserResourceAccess{}, appError(CodeInvalidRequest, "需要当前版本及完整的四类资源权限", nil)
	}
	policies := make(map[string]ResourceAccessPolicy, len(input.Policies))
	for _, policy := range input.Policies {
		normalized, err := normalizeResourceAccessPolicy(policy)
		if err != nil {
			return UserResourceAccess{}, err
		}
		if _, duplicate := policies[normalized.Scope]; duplicate {
			return UserResourceAccess{}, appError(CodeInvalidRequest, "资源权限范围不能重复", nil)
		}
		policies[normalized.Scope] = normalized
	}
	var result UserResourceAccess
	err := s.db.Transaction(func(tx *gorm.DB) error {
		current, before, err := s.resourceAccessActors(tx, actor, userID)
		if err != nil {
			return err
		}
		if !current.Can(authz.PermissionRolesAssign) {
			return appError(CodePermissionDenied, "没有修改用户授权的权限", nil)
		}
		if before.User.IsOwner {
			return appError(CodeOwnerProtected, "实例 owner 的资源权限不能被覆盖", nil)
		}
		if before.User.AuthzVersion != input.Revision {
			return appError(CodeConflict, "用户权限已被其他管理员修改，请重新加载后保存", nil)
		}
		for scope, policy := range policies {
			retained := map[string]bool{}
			for _, id := range before.ResourceAccessPolicies[scope].ResourceIDs {
				retained[id] = true
			}
			for _, rule := range before.ResourceRules {
				if rule.ResourceType == resourceAccessType(scope) {
					retained[rule.ResourceID] = true
				}
			}
			for _, id := range policy.ResourceIDs {
				if retained[id] {
					continue // Existing deleted references may be retained or removed.
				}
				if err := validateAuthorizationResource(tx, resourceAccessType(scope), id); err != nil {
					return err
				}
			}
		}
		after := before
		after.ResourceAccessPolicies = policies
		if err := validateAuthorityExpansion(current, before, after); err != nil {
			return err
		}
		changed := tx.Model(&models.User{}).Where("id = ? AND authz_version = ?", userID, input.Revision).Update("authz_version", gorm.Expr("authz_version + 1"))
		if changed.Error != nil {
			return changed.Error
		}
		if changed.RowsAffected != 1 {
			return appError(CodeConflict, "用户权限版本已变化，请重新加载后保存", nil)
		}
		if err := tx.Where("user_id = ?", userID).Delete(&models.UserResourceAccessPolicy{}).Error; err != nil {
			return err
		}
		now := time.Now().UTC()
		counts := map[string]int{}
		modes := map[string]string{}
		for _, scope := range resourceAccessScopes {
			policy := policies[scope]
			encoded, err := json.Marshal(policy.ResourceIDs)
			if err != nil {
				return err
			}
			row := models.UserResourceAccessPolicy{UserID: userID, Scope: scope, Mode: policy.Mode, ResourceIDsJSON: string(encoded), UpdatedBy: &current.User.ID, CreatedAt: now, UpdatedAt: now}
			if err := tx.Create(&row).Error; err != nil {
				return err
			}
			counts[scope], modes[scope] = len(policy.ResourceIDs), policy.Mode
		}
		if err := s.audit.Record(tx, &current.User.ID, "users.resource_access.replace", "user", uintID(userID), "success", map[string]any{"modes": modes, "selected_counts": counts, "revision": input.Revision + 1}, request); err != nil {
			return err
		}
		after.User.AuthzVersion++
		result = resourceAccessDTO(after)
		return nil
	})
	return result, err
}

type resourceAccessRecord struct {
	ID      string
	Name    string
	Type    string
	Enabled bool
}

func resourceAccessRecords(db *gorm.DB, resourceType string) ([]resourceAccessRecord, error) {
	table, typeColumn := "", ""
	switch resourceType {
	case models.AuthorizationResourceDownloader:
		table, typeColumn = "downloaders", "type"
	case models.AuthorizationResourceSite:
		table, typeColumn = "sites", "kind"
	case models.AuthorizationResourceMediaLibrary:
		table, typeColumn = "media_libraries", "'media_library'"
	}
	result := []resourceAccessRecord{}
	lastID := ""
	for {
		var batch []resourceAccessRecord
		if err := db.Table(table).Select("CAST(id AS TEXT) AS id, name, "+typeColumn+" AS type, enabled").
			Where("CAST(id AS TEXT) > ?", lastID).Order("CAST(id AS TEXT)").Limit(250).Scan(&batch).Error; err != nil {
			return nil, err
		}
		if len(batch) == 0 {
			return result, nil
		}
		result = append(result, batch...)
		if len(result) > 2000 {
			return nil, appError(CodeInvalidRequest, "资源过多，请减少配置后重试", nil)
		}
		lastID = batch[len(batch)-1].ID
	}
}

func scopeOperation(scope string) string {
	switch scope {
	case models.ResourceAccessScopeDownloaderUse, models.ResourceAccessScopeLibraryIngest:
		return authz.PermissionDownloadsCreate
	case models.ResourceAccessScopeSiteSearch:
		return authz.PermissionDiscoveryRead
	default:
		return authz.PermissionMediaLibrariesRead
	}
}

func resourceNamePermission(resourceType string) string {
	switch resourceType {
	case models.AuthorizationResourceDownloader:
		return authz.PermissionDownloadersRead
	case models.AuthorizationResourceSite:
		return authz.PermissionSitesRead
	default:
		return authz.PermissionMediaLibrariesRead
	}
}

func resourceScopeDenial(target Actor, scope, id string) string {
	code, resourceType := scopeOperation(scope), resourceAccessType(scope)
	for _, required := range policyScopes(code, resourceType) {
		if target.ResourceAccessPolicies[required].invalid {
			return "已保存的资源名单格式异常，请管理员重新保存"
		}
	}
	if scope == models.ResourceAccessScopeLibraryIngest && target.libraryIngestAuthority().allows(id) {
		return ""
	}
	if !target.canResourceWithoutPolicy(code, resourceType, id) {
		return "角色或直接授权未允许此操作，已有拒绝规则优先"
	}
	for _, required := range policyScopes(code, resourceType) {
		if !target.ResourceAccessAllows(required, id) {
			if required == models.ResourceAccessScopeLibraryRead && scope == models.ResourceAccessScopeLibraryIngest {
				return "须先允许访问这个媒体库，才能向其入库"
			}
			return "当前资源名单不允许此资源"
		}
	}
	if scope == models.ResourceAccessScopeLibraryIngest && !target.CanResource(authz.PermissionMediaLibrariesRead, resourceType, id) {
		return "须先具有这个媒体库的访问权限，才能向其入库"
	}
	return ""
}

func (s *AdminService) UserResourceAccessOptions(actor Actor, userID uint) (UserResourceAccessOptions, error) {
	var result UserResourceAccessOptions
	err := s.db.Transaction(func(tx *gorm.DB) error {
		current, target, err := s.resourceAccessActors(tx, actor, userID)
		if err != nil {
			return err
		}
		result = UserResourceAccessOptions{Revision: target.User.AuthzVersion, Scopes: make([]ResourceAccessScopeOptions, 0, len(resourceAccessScopes))}
		recordsByType := map[string][]resourceAccessRecord{}
		for _, scope := range resourceAccessScopes {
			resourceType := resourceAccessType(scope)
			records, exists := recordsByType[resourceType]
			if !exists {
				records, err = resourceAccessRecords(tx, resourceType)
				if err != nil {
					return err
				}
				recordsByType[resourceType] = records
			}
			referenced := map[string]bool{}
			for _, id := range target.ResourceAccessPolicies[scope].ResourceIDs {
				referenced[id] = true
			}
			for _, rule := range target.ResourceRules {
				if rule.ResourceType == resourceType {
					referenced[rule.ResourceID] = true
				}
			}
			options := ResourceAccessScopeOptions{Scope: scope, ResourceType: resourceType, Options: []ResourceAccessOption{}}
			for _, record := range records {
				visible := current.CanResource(resourceNamePermission(resourceType), resourceType, record.ID)
				if !visible && !referenced[record.ID] {
					continue
				}
				delete(referenced, record.ID)
				option := ResourceAccessOption{ID: record.ID, Name: "受限资源", Status: "restricted", DenialReason: "操作者无权查看此资源"}
				if visible {
					option.Name, option.Type = record.Name, record.Type
					option.Status = "disabled"
					if record.Enabled {
						option.Status = "enabled"
					}
					option.DenialReason = resourceScopeDenial(target, scope, record.ID)
					option.EffectiveAllowed = option.DenialReason == ""
					option.CanGrant = current.Can(authz.PermissionRolesAssign) && !target.User.IsOwner && current.User.ID != userID &&
						current.CanResource(scopeOperation(scope), resourceType, record.ID)
					if scope == models.ResourceAccessScopeLibraryIngest {
						option.CanGrant = current.Can(authz.PermissionRolesAssign) && !target.User.IsOwner && current.User.ID != userID &&
							current.libraryIngestAuthority().allows(record.ID)
					}
				}
				options.Options = append(options.Options, option)
			}
			for id := range referenced {
				options.Options = append(options.Options, ResourceAccessOption{ID: id, Name: "已删除资源", Status: "deleted", Deleted: true, DenialReason: "资源已删除，可移除已有引用"})
			}
			sort.Slice(options.Options, func(i, j int) bool {
				if options.Options[i].Name == options.Options[j].Name {
					return options.Options[i].ID < options.Options[j].ID
				}
				return options.Options[i].Name < options.Options[j].Name
			})
			result.Scopes = append(result.Scopes, options)
		}
		return nil
	})
	return result, err
}
