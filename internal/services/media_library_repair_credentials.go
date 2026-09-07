package services

import (
	"context"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
	"time"
)

// Queue claim checks the persisted credential state, so this single release
// does not repeatedly consume attempts while the connection is expired.
func (s *MediaLibraryStructureService) waitRepairCredentials(ctx context.Context, repair models.MediaLibraryStructureRepair, claim *ClaimedJob) WorkerResult {
	now := time.Now().UTC()
	if err := s.structureRepairCheckpointTx(ctx, repair, claim, func(tx *gorm.DB) error {
		return tx.Model(&models.MediaLibraryStructureRepair{}).Where("id=? AND plan_json=? AND phase<>?", repair.ID, repair.PlanJSON, "completed").Updates(map[string]any{"phase": "queued", "last_error_code": "pan115_auth_expired", "updated_at": now}).Error
	}); err != nil {
		return WorkerResult{ErrorCode: CodeMediaLibraryStructureApplyFailed, ErrorMessage: "无法保存等待登录凭据的整理进度"}
	}
	return WorkerResult{RetryAt: &now, ErrorCode: "pan115_auth_expired", ErrorMessage: "等待更新登录凭据，已保存整理进度"}
}
