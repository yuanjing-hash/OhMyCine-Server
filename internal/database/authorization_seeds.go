package database

import (
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

// seedNormalUserRole runs in the authorization seed transaction. A custom role
// with the same code is an administrator's template and must stay untouched.
func seedNormalUserRole(tx *gorm.DB) error {
	var role models.Role
	result := tx.Where("code = ?", authz.RoleUser).Limit(1).Find(&role)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 0 && role.Kind != models.RoleKindSystem {
		return nil
	}
	if result.RowsAffected == 0 {
		role = models.Role{Code: authz.RoleUser, Name: "正常用户", Description: "用户", Kind: models.RoleKindSystem, Protected: true, Active: true}
		if err := tx.Create(&role).Error; err != nil {
			return err
		}
	} else if err := tx.Model(&role).Updates(map[string]any{
		"name": "正常用户", "description": "用户", "protected": true, "active": true,
	}).Error; err != nil {
		return err
	}
	if err := tx.Where("role_id = ?", role.ID).Delete(&models.RolePermission{}).Error; err != nil {
		return err
	}
	codes := []string{
		authz.PermissionDashboardRead,
		authz.PermissionDiscoveryRead,
		authz.PermissionDownloadsCreate,
		authz.PermissionDownloadsReadOwn,
		authz.PermissionFollowsCreate,
		authz.PermissionFollowsDeleteOwn,
		authz.PermissionFollowsExecuteOwn,
		authz.PermissionFollowsReadOwn,
		authz.PermissionFollowsUpdateOwn,
		authz.PermissionJobsControlOwn,
		authz.PermissionJobsReadOwn,
		authz.PermissionTransfersReadOwn,
	}
	now := time.Now().UTC()
	grants := make([]models.RolePermission, 0, len(codes))
	for _, code := range codes {
		grants = append(grants, models.RolePermission{RoleID: role.ID, PermissionCode: code, CreatedAt: now})
	}
	return tx.Create(&grants).Error
}
