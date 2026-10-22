package services

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

// pluginResourceLifecycle is evaluated before resolving and again inside the
// new download's persistence transaction. A final enabled=true alone cannot
// distinguish disable/re-enable or an active-package/connection replacement.
func pluginResourceLifecycle(db *gorm.DB, expected models.Site, pluginID, version string) (string, error) {
	var site models.Site
	if err := db.Select("id", "revision", "enabled", "kind", "source_type", "plugin_id", "plugin_connection_id").First(&site, expected.ID).Error; err != nil || !site.Enabled || site.Kind != pluginResourceKind || site.SourceType != "plugin" || site.PluginConnectionID != expected.PluginConnectionID || site.PluginID != pluginID {
		return "", appError(CodeSiteResultExpired, "资源站配置已失效，请重新搜索", err)
	}
	var connection models.PluginConnection
	if err := db.Select("id", "plugin_id", "enabled", "revision", "credential_version", "resource_type").First(&connection, "id = ?", site.PluginConnectionID).Error; err != nil || !connection.Enabled || connection.PluginID != pluginID || connection.ResourceType != "bt_resource" {
		return "", appError(CodeSiteResultExpired, "资源站连接已变化，请重新搜索", err)
	}
	var installation models.PluginInstallation
	if err := db.Select("plugin_id", "active_package_id", "status", "revision", "runtime_generation").First(&installation, "plugin_id = ?", pluginID).Error; err != nil || installation.Status != models.PluginInstallationEnabled {
		return "", appError(CodeSiteResultExpired, "资源站插件已停用或卸载，请重新搜索", err)
	}
	var pkg models.PluginPackage
	if err := db.Select("id", "plugin_id", "version").First(&pkg, installation.ActivePackageID).Error; err != nil || pkg.PluginID != pluginID || pkg.Version != version {
		return "", appError(CodeSiteResultExpired, "资源站插件版本已变化，请重新搜索", err)
	}
	raw, _ := json.Marshal(struct {
		Site         models.Site
		Connection   models.PluginConnection
		Installation models.PluginInstallation
		Package      models.PluginPackage
	}{site, connection, installation, pkg})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}
