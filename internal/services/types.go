package services

import (
	"sort"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

// Actor is the authenticated authorization context used by policy-aware services.
type Actor struct {
	User              models.User
	RoleCodes         []string
	Permissions       map[string]struct{}
	DeniedPermissions map[string]struct{}
	ResourceRules     []AuthorizationRule
}

type AuthorizationRule struct {
	PermissionCode string `json:"permission_code"`
	Effect         string `json:"effect"`
	ResourceType   string `json:"resource_type"`
	ResourceID     string `json:"resource_id"`
}

func (a Actor) Can(code string) bool {
	if _, denied := a.DeniedPermissions[code]; denied {
		return false
	}
	_, ok := a.Permissions[code]
	return ok
}

func (a Actor) CanResource(code, resourceType, resourceID string) bool {
	if _, denied := a.DeniedPermissions[code]; denied {
		return false
	}
	allowed := a.Can(code)
	for _, rule := range a.ResourceRules {
		if rule.PermissionCode != code || rule.ResourceType != resourceType || rule.ResourceID != resourceID {
			continue
		}
		if rule.Effect == models.AuthorizationEffectDeny {
			return false
		}
		if rule.Effect == models.AuthorizationEffectAllow {
			allowed = true
		}
	}
	return allowed
}

// HasPermission reports whether the actor may have a permission globally or
// for at least one resource. HTTP middleware uses it for early rejection;
// resource-owning services still call CanResource with the exact ID.
func (a Actor) HasPermission(code string) bool {
	if a.Can(code) {
		return true
	}
	if _, denied := a.DeniedPermissions[code]; denied {
		return false
	}
	for _, rule := range a.ResourceRules {
		if rule.PermissionCode == code && rule.Effect == models.AuthorizationEffectAllow {
			return true
		}
	}
	return false
}

func (a Actor) IsSystemAdmin() bool { return a.Can(authz.PermissionSystemAdmin) }

func (a Actor) SortedPermissions() []string {
	codes := make([]string, 0, len(a.Permissions))
	seen := make(map[string]struct{}, len(a.Permissions)+len(a.ResourceRules))
	for code := range a.Permissions {
		codes = append(codes, code)
		seen[code] = struct{}{}
	}
	for _, rule := range a.ResourceRules {
		if rule.Effect != models.AuthorizationEffectAllow || !a.HasPermission(rule.PermissionCode) {
			continue
		}
		if _, exists := seen[rule.PermissionCode]; exists {
			continue
		}
		seen[rule.PermissionCode] = struct{}{}
		codes = append(codes, rule.PermissionCode)
	}
	sort.Strings(codes)
	return codes
}

type RequestContext struct {
	RequestID string
	IPHint    string
}

type CurrentUser struct {
	ID          uint     `json:"id"`
	Username    string   `json:"username"`
	DisplayName string   `json:"display_name"`
	Status      string   `json:"status"`
	IsOwner     bool     `json:"is_owner"`
	Roles       []string `json:"roles"`
	Permissions []string `json:"permissions"`
}

func CurrentUserFromActor(actor Actor) CurrentUser {
	roles := append([]string(nil), actor.RoleCodes...)
	sort.Strings(roles)
	return CurrentUser{
		ID: actor.User.ID, Username: actor.User.Username, DisplayName: actor.User.DisplayName,
		Status: actor.User.Status, IsOwner: actor.User.IsOwner, Roles: roles,
		Permissions: actor.SortedPermissions(),
	}
}
