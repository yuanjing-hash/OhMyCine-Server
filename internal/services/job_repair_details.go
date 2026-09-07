package services

import (
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	cloudpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"gorm.io/gorm"
)

type JobRepairItem struct {
	Ordinal      int       `json:"ordinal"`
	Action       string    `json:"action"`
	Kind         string    `json:"kind"`
	SourcePath   string    `json:"source_path"`
	TargetPath   string    `json:"target_path"`
	Status       string    `json:"status"`
	ErrorCode    string    `json:"error_code"`
	ErrorMessage string    `json:"error_message"`
	UpdatedAt    time.Time `json:"updated_at"`
}
type JobRepairDetails struct {
	CurrentAction    string           `json:"current_action"`
	CurrentItem      string           `json:"current_item"`
	CurrentBatchSize int              `json:"current_batch_size"`
	List             []JobRepairItem  `json:"list"`
	Total            int64            `json:"total"`
	Page             int              `json:"page"`
	PageSize         int              `json:"page_size"`
	Phase            string           `json:"phase"`
	Counts           map[string]int64 `json:"counts"`
}

// RepairDetails requires both task visibility and access to its library. Only
// allowlisted checkpoint facts leave this boundary; private plans stay private.
func (s *QueueService) RepairDetails(actor Actor, id, status string, page, size int) (JobRepairDetails, error) {
	result := JobRepairDetails{List: []JobRepairItem{}, Counts: map[string]int64{}, Page: page, PageSize: size}
	job, err := s.Get(actor, id)
	if err != nil {
		return result, err
	}
	if size < 1 || size > 200 || page < 1 || page-1 > int(^uint(0)>>1)/size {
		return result, appError(CodeInvalidRequest, "分页参数无效", nil)
	}
	switch status {
	case "", "all", "pending", "running", "succeeded", "failed", "blocked":
	default:
		return result, appError(CodeInvalidRequest, "执行状态无效", nil)
	}
	var repair models.MediaLibraryStructureRepair
	if err := s.db.Select("id", "library_id", "phase", "current_action", "current_item", "current_batch_size").Where("job_id = ?", id).First(&repair).Error; err != nil {
		return result, queueNotFound(err)
	}
	if !actor.CanResource(authz.PermissionMediaLibrariesRead, models.AuthorizationResourceMediaLibrary, uintID(repair.LibraryID)) {
		return result, appError(CodePermissionDenied, "无权查看媒体库执行明细", nil)
	}
	result.Phase = repair.Phase
	if job.Status == "running" {
		var active int64
		if err := s.db.Model(&models.Job{}).Where("id = ? AND status = ? AND lease_expires_at > ?", id, "running", s.clock.Now()).Count(&active).Error; err != nil {
			return result, err
		}
		if active == 1 {
			result.CurrentAction, result.CurrentItem, result.CurrentBatchSize = repair.CurrentAction, jobRepairRelativePath(repair.CurrentItem), repair.CurrentBatchSize
		}
	}
	var counts []struct {
		Status string
		Total  int64
	}
	base := s.db.Model(&models.MediaLibraryStructureRepairItem{}).Where("repair_id = ?", repair.ID)
	if err := base.Session(&gorm.Session{}).Select("status, COUNT(*) AS total").Group("status").Scan(&counts).Error; err != nil {
		return result, err
	}
	for _, count := range counts {
		result.Counts[count.Status] = count.Total
	}
	query := base.Session(&gorm.Session{})
	if status != "" && status != "all" {
		query = query.Where("status = ?", status)
	}
	if err := query.Session(&gorm.Session{}).Count(&result.Total).Error; err != nil {
		return result, err
	}
	var rows []models.MediaLibraryStructureRepairItem
	if err := query.Session(&gorm.Session{}).Order("ordinal").Offset((page - 1) * size).Limit(size).Find(&rows).Error; err != nil {
		return result, err
	}
	for _, row := range rows {
		code, message := jobRepairSafeError(row.ErrorCode)
		result.List = append(result.List, JobRepairItem{Ordinal: row.Ordinal, Action: row.Action, Kind: row.Kind, SourcePath: jobRepairRelativePath(row.SourceRelative), TargetPath: jobRepairRelativePath(row.TargetRelative), Status: row.Status, ErrorCode: code, ErrorMessage: message, UpdatedAt: row.UpdatedAt})
	}
	return result, nil
}
func jobRepairRelativePath(value string) string {
	clean, err := sanitizeTransferRelativePath(value)
	if err != nil {
		return ""
	}
	return clean
}
func jobRepairSafeError(code string) (string, string) {
	switch code {
	case "":
		return "", ""
	case "dependency_failed":
		return code, "依赖的文件操作尚未成功"
	case CodeMediaLibraryStructureBoundaryChanged:
		return code, "执行边界已变化，任务已安全停止"
	case CodeMediaLibraryStructureConflict:
		return code, "目标位置存在冲突"
	case CodeMediaLibraryStructureFileLocked:
		return code, "文件正在被占用"
	case CodeMediaLibraryStructurePermissionDenied:
		return code, "当前文件不可写"
	case cloudpkg.CodeAuthExpired, cloudpkg.CodeCookieInvalid:
		return code, "云盘认证已失效，等待更新登录凭据"
	case cloudpkg.CodeRateLimited:
		return code, "云盘触发风控，已保存进度并等待恢复"
	default:
		return CodeMediaLibraryStructureApplyFailed, "单项文件操作失败，可稍后重试"
	}
}
