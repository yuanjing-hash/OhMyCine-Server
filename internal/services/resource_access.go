package services

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

const maxResourceAccessIDs = 1000

type ResourceAccessPolicy struct {
	Scope       string   `json:"scope"`
	Mode        string   `json:"mode"`
	ResourceIDs []string `json:"resource_ids"`
	invalid     bool
}

type UserResourceAccess struct {
	Revision uint64                 `json:"revision"`
	Policies []ResourceAccessPolicy `json:"policies"`
}

var resourceAccessScopes = []string{
	models.ResourceAccessScopeDownloaderUse, models.ResourceAccessScopeSiteSearch,
	models.ResourceAccessScopeLibraryRead, models.ResourceAccessScopeLibraryIngest,
}

func resourceAccessType(scope string) string {
	switch scope {
	case models.ResourceAccessScopeDownloaderUse:
		return models.AuthorizationResourceDownloader
	case models.ResourceAccessScopeSiteSearch:
		return models.AuthorizationResourceSite
	case models.ResourceAccessScopeLibraryRead, models.ResourceAccessScopeLibraryIngest:
		return models.AuthorizationResourceMediaLibrary
	default:
		return ""
	}
}

func canonicalResourceID(resourceType, id string) string {
	if resourceType != models.AuthorizationResourceMediaLibrary && resourceType != models.AuthorizationResourceSite {
		return id
	}
	for _, digit := range id {
		if digit < '0' || digit > '9' {
			return ""
		}
	}
	value, err := strconv.ParseUint(id, 10, 64)
	if err != nil || value == 0 {
		return ""
	}
	return strconv.FormatUint(value, 10)
}

func policyScopes(code, resourceType string) []string {
	switch resourceType {
	case models.AuthorizationResourceDownloader:
		return []string{models.ResourceAccessScopeDownloaderUse}
	case models.AuthorizationResourceSite:
		return []string{models.ResourceAccessScopeSiteSearch}
	case models.AuthorizationResourceMediaLibrary:
		if code == authz.PermissionDownloadsCreate {
			return []string{models.ResourceAccessScopeLibraryRead, models.ResourceAccessScopeLibraryIngest}
		}
		return []string{models.ResourceAccessScopeLibraryRead}
	default:
		return nil
	}
}

// ResourceAccessAllows evaluates only the restrictive resource list. Callers
// must also check the function permission or use CanResource.
func (a Actor) ResourceAccessAllows(scope, resourceID string) bool {
	resourceType := resourceAccessType(scope)
	if resourceType == "" {
		return false
	}
	resourceID = canonicalResourceID(resourceType, resourceID)
	if resourceID == "" {
		return false
	}
	policy, exists := a.ResourceAccessPolicies[scope]
	if !exists {
		return true
	}
	if policy.invalid {
		return false
	}
	switch policy.Mode {
	case models.ResourceAccessModeAll:
		return len(policy.ResourceIDs) == 0
	case models.ResourceAccessModeAllowlist, models.ResourceAccessModeDenylist:
		found := false
		for _, id := range policy.ResourceIDs {
			if canonicalResourceID(resourceType, id) == resourceID {
				found = true
				break
			}
		}
		return found == (policy.Mode == models.ResourceAccessModeAllowlist)
	default:
		return false
	}
}

func (a Actor) resourcePoliciesAllow(code, resourceType, resourceID string) bool {
	if code == authz.PermissionDownloadsCreate && resourceType == models.AuthorizationResourceMediaLibrary &&
		!a.canResourceWithoutPolicy(authz.PermissionMediaLibrariesRead, resourceType, resourceID) {
		return false
	}
	for _, scope := range policyScopes(code, resourceType) {
		if !a.ResourceAccessAllows(scope, resourceID) {
			return false
		}
	}
	return true
}

// CanIngestLibrary rejects a target that the user cannot read, even when a
// legacy scoped download grant exists for it.
func (a Actor) CanIngestLibrary(resourceID string) bool {
	return a.CanResource(authz.PermissionMediaLibrariesRead, models.AuthorizationResourceMediaLibrary, resourceID) &&
		a.CanResource(authz.PermissionDownloadsCreate, models.AuthorizationResourceMediaLibrary, resourceID)
}

func normalizeResourceAccessPolicy(policy ResourceAccessPolicy) (ResourceAccessPolicy, error) {
	if resourceAccessType(policy.Scope) == "" {
		return ResourceAccessPolicy{}, appError(CodeInvalidRequest, "包含未知资源权限范围", nil)
	}
	if policy.Mode != models.ResourceAccessModeAll && policy.Mode != models.ResourceAccessModeAllowlist && policy.Mode != models.ResourceAccessModeDenylist {
		return ResourceAccessPolicy{}, appError(CodeInvalidRequest, "资源权限模式无效", nil)
	}
	if len(policy.ResourceIDs) > maxResourceAccessIDs {
		return ResourceAccessPolicy{}, appError(CodeInvalidRequest, "每个资源名单最多允许 1000 项", nil)
	}
	if policy.Mode == models.ResourceAccessModeAll && len(policy.ResourceIDs) != 0 {
		return ResourceAccessPolicy{}, appError(CodeInvalidRequest, "全部开放模式不能包含资源名单", nil)
	}
	ids := make(map[string]struct{}, len(policy.ResourceIDs))
	for _, id := range policy.ResourceIDs {
		if id == "" || len(id) > 128 || strings.TrimSpace(id) != id || strings.ContainsAny(id, "\x00\r\n\t") {
			return ResourceAccessPolicy{}, appError(CodeInvalidRequest, "资源标识无效", nil)
		}
		id = canonicalResourceID(resourceAccessType(policy.Scope), id)
		if id == "" {
			return ResourceAccessPolicy{}, appError(CodeInvalidRequest, "资源标识无效", nil)
		}
		ids[id] = struct{}{}
	}
	policy.ResourceIDs = make([]string, 0, len(ids))
	for id := range ids {
		policy.ResourceIDs = append(policy.ResourceIDs, id)
	}
	sort.Strings(policy.ResourceIDs)
	policy.invalid = false
	return policy, nil
}

func loadResourceAccessPolicies(db *gorm.DB, userID uint) (map[string]ResourceAccessPolicy, error) {
	var rows []models.UserResourceAccessPolicy
	if err := db.Where("user_id = ?", userID).Limit(len(resourceAccessScopes) + 1).Find(&rows).Error; err != nil {
		return nil, err
	}
	policies := make(map[string]ResourceAccessPolicy, len(rows))
	for _, row := range rows {
		if resourceAccessType(row.Scope) == "" || len(rows) > len(resourceAccessScopes) {
			// An unknown persisted scope cannot be silently ignored as an open policy.
			for _, scope := range resourceAccessScopes {
				policies[scope] = ResourceAccessPolicy{Scope: scope, Mode: models.ResourceAccessModeAllowlist, ResourceIDs: []string{}, invalid: true}
			}
			return policies, nil
		}
		policy := ResourceAccessPolicy{Scope: row.Scope, Mode: row.Mode}
		decodeErr := json.Unmarshal([]byte(row.ResourceIDsJSON), &policy.ResourceIDs)
		normalized, validationErr := normalizeResourceAccessPolicy(policy)
		if decodeErr != nil || validationErr != nil || policy.ResourceIDs == nil {
			policies[row.Scope] = ResourceAccessPolicy{Scope: row.Scope, Mode: models.ResourceAccessModeAllowlist, ResourceIDs: []string{}, invalid: true}
			continue
		}
		policies[row.Scope] = normalized
	}
	return policies, nil
}

func resourceAccessDTO(actor Actor) UserResourceAccess {
	result := UserResourceAccess{Revision: actor.User.AuthzVersion, Policies: make([]ResourceAccessPolicy, 0, len(resourceAccessScopes))}
	for _, scope := range resourceAccessScopes {
		policy, exists := actor.ResourceAccessPolicies[scope]
		if !exists {
			policy = ResourceAccessPolicy{Scope: scope, Mode: models.ResourceAccessModeAll, ResourceIDs: []string{}}
		}
		policy.ResourceIDs = append([]string{}, policy.ResourceIDs...)
		result.Policies = append(result.Policies, policy)
	}
	return result
}

// resourceAuthoritySet represents all possible stable IDs, including resources
// not created yet. Values differ from fallback only at explicitly named IDs.
type resourceAuthoritySet struct {
	fallback bool
	values   map[string]bool
}

func (s resourceAuthoritySet) allows(id string) bool {
	if value, exists := s.values[id]; exists {
		return value
	}
	return s.fallback
}

func intersectResourceAuthority(first, second resourceAuthoritySet) resourceAuthoritySet {
	result := resourceAuthoritySet{fallback: first.fallback && second.fallback, values: map[string]bool{}}
	for _, set := range []resourceAuthoritySet{first, second} {
		for id := range set.values {
			value := first.allows(id) && second.allows(id)
			if value != result.fallback {
				result.values[id] = value
			}
		}
	}
	return result
}

func unionResourceAuthority(first, second resourceAuthoritySet) resourceAuthoritySet {
	result := resourceAuthoritySet{fallback: first.fallback || second.fallback, values: map[string]bool{}}
	for _, set := range []resourceAuthoritySet{first, second} {
		for id := range set.values {
			value := first.allows(id) || second.allows(id)
			if value != result.fallback {
				result.values[id] = value
			}
		}
	}
	return result
}

func (a Actor) resourcePolicyAuthority(scope string) resourceAuthoritySet {
	policy, exists := a.ResourceAccessPolicies[scope]
	if !exists {
		return resourceAuthoritySet{fallback: true}
	}
	set := resourceAuthoritySet{values: map[string]bool{}}
	if policy.invalid {
		return set
	}
	switch policy.Mode {
	case models.ResourceAccessModeAll:
		set.fallback = len(policy.ResourceIDs) == 0
	case models.ResourceAccessModeDenylist:
		set.fallback = true
		fallthrough
	case models.ResourceAccessModeAllowlist:
		for _, id := range policy.ResourceIDs {
			id = canonicalResourceID(resourceAccessType(scope), id)
			if id != "" {
				set.values[id] = !set.fallback
			}
		}
	}
	return set
}

func (a Actor) resourceAuthority(code, resourceType string) resourceAuthoritySet {
	set := resourceAuthoritySet{fallback: a.Can(code), values: map[string]bool{}}
	if _, denied := a.DeniedPermissions[code]; denied {
		return resourceAuthoritySet{}
	}
	deniedIDs := map[string]bool{}
	for _, rule := range a.ResourceRules {
		if rule.PermissionCode != code || rule.ResourceType != resourceType {
			continue
		}
		id := canonicalResourceID(resourceType, rule.ResourceID)
		if id == "" {
			continue
		}
		if rule.Effect == models.AuthorizationEffectAllow {
			set.values[id] = true
		} else if rule.Effect == models.AuthorizationEffectDeny {
			deniedIDs[id] = true
		}
	}
	for id := range deniedIDs {
		set.values[id] = false
	}
	for _, scope := range policyScopes(code, resourceType) {
		set = intersectResourceAuthority(set, a.resourcePolicyAuthority(scope))
	}
	if code == authz.PermissionDownloadsCreate && resourceType == models.AuthorizationResourceMediaLibrary {
		set = intersectResourceAuthority(set, a.resourceAuthority(authz.PermissionMediaLibrariesRead, resourceType))
	}
	return set
}

// Library ingestion includes controlled organization of existing files as well
// as downloads. Repair operators do not need a download-create permission.
func (a Actor) libraryIngestAuthority() resourceAuthoritySet {
	read := a.resourceAuthority(authz.PermissionMediaLibrariesRead, models.AuthorizationResourceMediaLibrary)
	result := a.resourceAuthority(authz.PermissionDownloadsCreate, models.AuthorizationResourceMediaLibrary)
	canOrganize := (a.Can(authz.PermissionJobsControlOwn) || a.Can(authz.PermissionJobsControlAll)) &&
		(a.Can(authz.PermissionTransfersReadOwn) || a.Can(authz.PermissionTransfersReadAll))
	if canOrganize {
		result = unionResourceAuthority(result, intersectResourceAuthority(read, a.resourcePolicyAuthority(models.ResourceAccessScopeLibraryIngest)))
	}
	return result
}

func resourceExpansionAllowed(before, after, granter resourceAuthoritySet) bool {
	if after.fallback && !before.fallback && !granter.fallback {
		return false
	}
	for _, set := range []resourceAuthoritySet{before, after, granter} {
		for id := range set.values {
			if after.allows(id) && !before.allows(id) && !granter.allows(id) {
				return false
			}
		}
	}
	return true
}

// Compare the resource operations the application actually performs, plus any
// explicit legacy rule pairs. Unrelated global capabilities such as users.read
// do not acquire a fictitious downloader/site/library universe.
func authorityResourceTypes(code string, actors ...Actor) []string {
	types := map[string]bool{}
	switch code {
	case authz.PermissionDownloadersRead, authz.PermissionDownloadersUpdate, authz.PermissionDownloadersDelete, authz.PermissionDownloadersTest:
		types[models.AuthorizationResourceDownloader] = true
	case authz.PermissionSitesRead, authz.PermissionSitesUpdate, authz.PermissionSitesDelete, authz.PermissionSitesTest, authz.PermissionDiscoveryRead:
		types[models.AuthorizationResourceSite] = true
	case authz.PermissionMediaLibrariesRead, authz.PermissionMediaLibrariesUpdate, authz.PermissionMediaLibrariesDelete,
		authz.PermissionMediaLibrariesMediaDelete, authz.PermissionMediaLibrariesScan, authz.PermissionMediaServersRefresh,
		authz.PermissionSTRMRunsRead, authz.PermissionSTRMRunsCreate, authz.PermissionSTRMCleanup:
		types[models.AuthorizationResourceMediaLibrary] = true
	case authz.PermissionDownloadsCreate:
		types[models.AuthorizationResourceDownloader] = true
		types[models.AuthorizationResourceMediaLibrary] = true
	case authz.PermissionConnectionsSecretsExport:
		types[models.AuthorizationResourceDownloader] = true
		types[models.AuthorizationResourceSite] = true
	}
	for _, actor := range actors {
		for _, rule := range actor.ResourceRules {
			if rule.PermissionCode == code {
				types[rule.ResourceType] = true
			}
		}
	}
	result := make([]string, 0, len(types))
	for _, resourceType := range []string{models.AuthorizationResourceDownloader, models.AuthorizationResourceSite, models.AuthorizationResourceMediaLibrary} {
		if types[resourceType] {
			result = append(result, resourceType)
		}
	}
	return result
}

// validateAuthorityExpansion checks only newly effective grants, allowing a
// limited administrator to tighten a user that was previously broader.
func validateAuthorityExpansion(granter, before, after Actor) error {
	if granter.User.IsOwner {
		return nil
	}
	codes, err := authz.Codes()
	if err != nil {
		return err
	}
	for _, code := range codes {
		if after.Can(code) && !before.Can(code) && !granter.Can(code) {
			return appError(CodePrivilegeEscalation, "不能授予操作者自己没有的功能权限", nil)
		}
		for _, resourceType := range authorityResourceTypes(code, granter, before, after) {
			if !resourceExpansionAllowed(before.resourceAuthority(code, resourceType), after.resourceAuthority(code, resourceType), granter.resourceAuthority(code, resourceType)) {
				return appError(CodePrivilegeEscalation, "不能扩大到操作者无权使用的资源范围", nil)
			}
		}
	}
	if !resourceExpansionAllowed(before.libraryIngestAuthority(), after.libraryIngestAuthority(), granter.libraryIngestAuthority()) {
		return appError(CodePrivilegeEscalation, "不能扩大到操作者无权入库或整理的媒体库范围", nil)
	}
	return nil
}
