package services

import (
	"sort"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

type AuthorizationService struct{ db *gorm.DB }

func NewAuthorizationService(db *gorm.DB) *AuthorizationService { return &AuthorizationService{db: db} }

func (s *AuthorizationService) Resolve(userID uint) (Actor, error) {
	return s.resolveWithDB(s.db, userID)
}

func (s *AuthorizationService) resolveWithDB(db *gorm.DB, userID uint) (Actor, error) {
	var user models.User
	if err := db.First(&user, userID).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return Actor{}, appError(CodeNotAuthenticated, "登录会话无效", err)
		}
		return Actor{}, err
	}
	if user.Status != models.UserStatusActive {
		return Actor{}, appError(CodeNotAuthenticated, "账户已停用", nil)
	}
	var roles []models.Role
	if err := db.Table("roles").
		Joins("JOIN user_roles ON user_roles.role_id = roles.id").
		Where("user_roles.user_id = ? AND roles.active = ?", userID, true).
		Order("roles.code").Find(&roles).Error; err != nil {
		return Actor{}, err
	}
	permissions := map[string]struct{}{}
	roleCodes := make([]string, 0, len(roles))
	administrator := false
	roleIDs := make([]uint, 0, len(roles))
	for _, role := range roles {
		roleCodes = append(roleCodes, role.Code)
		roleIDs = append(roleIDs, role.ID)
		administrator = administrator || role.Code == authz.RoleAdministrator
	}
	if administrator {
		codes, err := authz.Codes()
		if err != nil {
			return Actor{}, err
		}
		for _, code := range codes {
			permissions[code] = struct{}{}
		}
	} else if len(roleIDs) > 0 {
		var codes []string
		if err := db.Table("role_permissions").Where("role_id IN ?", roleIDs).Distinct().Pluck("permission_code", &codes).Error; err != nil {
			return Actor{}, err
		}
		for _, code := range codes {
			permissions[code] = struct{}{}
		}
	}
	var persistedRules []models.UserAuthorizationRule
	if err := db.Where("user_id = ?", userID).Order("permission_code, resource_type, resource_id, effect").Find(&persistedRules).Error; err != nil {
		return Actor{}, err
	}
	resourceRules := make([]AuthorizationRule, 0, len(persistedRules))
	deniedPermissions := map[string]struct{}{}
	for _, rule := range persistedRules {
		if rule.ResourceType == "" {
			if rule.Effect == models.AuthorizationEffectDeny {
				delete(permissions, rule.PermissionCode)
				deniedPermissions[rule.PermissionCode] = struct{}{}
			} else {
				permissions[rule.PermissionCode] = struct{}{}
			}
			continue
		}
		resourceRules = append(resourceRules, AuthorizationRule{PermissionCode: rule.PermissionCode, Effect: rule.Effect, ResourceType: rule.ResourceType, ResourceID: rule.ResourceID})
	}
	sort.Strings(roleCodes)
	return Actor{User: user, RoleCodes: roleCodes, Permissions: permissions, DeniedPermissions: deniedPermissions, ResourceRules: resourceRules}, nil
}

func subset(requested []string, allowed map[string]struct{}) bool {
	for _, code := range requested {
		if _, ok := allowed[code]; !ok {
			return false
		}
	}
	return true
}
