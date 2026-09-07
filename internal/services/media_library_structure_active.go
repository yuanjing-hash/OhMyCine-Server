package services

import (
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

// Both repair submission paths must agree with readiness about cancelled,
// unentered owners. Otherwise a new submission returns the cancelled Job or
// reports a nonexistent active repair even after diagnosis is unblocked.
func findActiveStructureRepair(db *gorm.DB, libraryID uint, scope, workKey string) (models.MediaLibraryStructureRepair, error) {
	var repair models.MediaLibraryStructureRepair
	err := db.Table("media_library_structure_repairs AS r").Select("r.*").
		Joins("JOIN media_libraries AS l ON l.id=r.library_id").
		Where("r.library_id=? AND r.scope=? AND r.work_key=? AND (("+readinessRepairActiveSQL+") OR (r.phase='failed' AND r.succeeded_items>0 AND (r.failed_items>0 OR r.blocked_items>0)))", libraryID, scope, workKey).
		Order("r.created_at DESC").Take(&repair).Error
	return repair, err
}
