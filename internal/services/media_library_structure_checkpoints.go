package services

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	storagefs "github.com/yuanjing-hash/OhMyCine-Server/internal/storage"
	cloudpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type MediaLibraryStructureRepairItemPage struct {
	List     []models.MediaLibraryStructureRepairItem `json:"list"`
	Total    int64                                    `json:"total"`
	Page     int                                      `json:"page"`
	PageSize int                                      `json:"page_size"`
}

func (s *MediaLibraryStructureService) RepairItems(actor Actor, libraryID uint, repairID string, page, pageSize int) (MediaLibraryStructureRepairItemPage, error) {
	if !actor.CanResource(authz.PermissionMediaLibrariesRead, models.AuthorizationResourceMediaLibrary, uintID(libraryID)) {
		return MediaLibraryStructureRepairItemPage{}, appError(CodePermissionDenied, "无权查看目录整理失败项", nil)
	}
	if page < 1 || page > 100000 || pageSize < 1 || pageSize > 200 {
		return MediaLibraryStructureRepairItemPage{}, appError(CodeInvalidRequest, "目录整理失败项分页参数无效", nil)
	}
	var repair models.MediaLibraryStructureRepair
	if err := s.db.Select("id", "library_id").Where("id = ? AND library_id = ?", repairID, libraryID).First(&repair).Error; err != nil {
		return MediaLibraryStructureRepairItemPage{}, mediaLibraryNotFound(err)
	}
	query := s.db.Model(&models.MediaLibraryStructureRepairItem{}).Where("repair_id = ? AND status IN ?", repairID, []string{structureRepairItemFailed, structureRepairItemBlocked})
	result := MediaLibraryStructureRepairItemPage{Page: page, PageSize: pageSize}
	if err := query.Count(&result.Total).Error; err != nil {
		return MediaLibraryStructureRepairItemPage{}, err
	}
	if err := query.Order("ordinal").Offset((page - 1) * pageSize).Limit(pageSize).Find(&result.List).Error; err != nil {
		return MediaLibraryStructureRepairItemPage{}, err
	}
	return result, nil
}

const (
	structureRepairItemPending   = "pending"
	structureRepairItemRunning   = "running"
	structureRepairItemSucceeded = "succeeded"
	structureRepairItemFailed    = "failed"
	structureRepairItemBlocked   = "blocked"
)

type structureRepairExecution struct {
	Plan       StructurePlan
	Succeeded  int
	Failed     int
	Blocked    int
	RetryAt    *time.Time
	GlobalErr  error
	GlobalCode string
}

func structureRepairPathKey(storageType, value string) string {
	clean := safeStructurePath(value)
	if storageType == models.StorageTypeLocal {
		return storagefs.NormalizeForComparison(filepath.FromSlash(clean))
	}
	return strings.ToLower(clean)
}

func structureRepairDependencyOrdinals(storageType string, plan StructurePlan) map[int]int {
	dependencies := make(map[int]int)
	sourceOrdinal := make(map[string]int, len(plan.RecycleItems)+len(plan.Items))
	for index, item := range plan.RecycleItems {
		sourceOrdinal[structureRepairPathKey(storageType, item.SourceRelative)] = index
	}
	moveOffset := len(plan.RecycleItems)
	for index, item := range plan.Items {
		sourceOrdinal[structureRepairPathKey(storageType, item.SourceRelative)] = moveOffset + index
	}
	for index, item := range plan.Items {
		ordinal := moveOffset + index
		if dependency, ok := sourceOrdinal[structureRepairPathKey(storageType, item.TargetRelative)]; ok && dependency != ordinal {
			dependencies[ordinal] = dependency
		}
	}
	return dependencies
}

func (s *MediaLibraryStructureService) structureRepairCheckpointTx(ctx context.Context, repair models.MediaLibraryStructureRepair, claim *ClaimedJob, write func(*gorm.DB) error) error {
	return s.structureCatalogWriteTx(ctx, func(tx *gorm.DB) error {
		if claim != nil {
			if s.queue == nil {
				return ErrCatalogInvalid
			}
			if _, err := s.queue.verifyLease(tx, claim.Job.ID, claim.LeaseToken); err != nil {
				return err
			}
		}
		var current models.MediaLibraryStructureRepair
		if err := tx.Select("id", "job_id", "plan_json", "phase").First(&current, "id = ?", repair.ID).Error; err != nil {
			return err
		}
		if current.PlanJSON != repair.PlanJSON || current.Phase == "completed" {
			return ErrCatalogFence
		}
		if claim != nil && (current.JobID == nil || *current.JobID != claim.Job.ID) {
			return ErrCatalogFence
		}
		return write(tx)
	})
}

func (s *MediaLibraryStructureService) ensureStructureRepairItems(ctx context.Context, repair models.MediaLibraryStructureRepair, plan StructurePlan, storageType string, claim *ClaimedJob) ([]models.MediaLibraryStructureRepairItem, error) {
	now := time.Now().UTC()
	dependencies := structureRepairDependencyOrdinals(storageType, plan)
	rows := make([]models.MediaLibraryStructureRepairItem, 0, len(plan.RecycleItems)+len(plan.Items))
	for index, item := range plan.RecycleItems {
		rows = append(rows, models.MediaLibraryStructureRepairItem{RepairID: repair.ID, Ordinal: index, Action: "recycle", Kind: item.Kind, SourceRelative: safeStructurePath(item.SourceRelative), TargetRelative: safeStructurePath(item.RecycleRelative), Status: structureRepairItemPending, CreatedAt: now, UpdatedAt: now})
	}
	offset := len(plan.RecycleItems)
	for index, item := range plan.Items {
		ordinal := offset + index
		var dependency *int
		if value, ok := dependencies[ordinal]; ok {
			copy := value
			dependency = &copy
		}
		rows = append(rows, models.MediaLibraryStructureRepairItem{RepairID: repair.ID, Ordinal: ordinal, Action: "move", Kind: item.Kind, SourceRelative: safeStructurePath(item.SourceRelative), TargetRelative: safeStructurePath(item.TargetRelative), DependencyOrdinal: dependency, Status: structureRepairItemPending, CreatedAt: now, UpdatedAt: now})
	}
	if err := s.structureRepairCheckpointTx(ctx, repair, claim, func(tx *gorm.DB) error {
		if len(rows) == 0 {
			return nil
		}
		return tx.Clauses(clause.OnConflict{DoNothing: true}).CreateInBatches(rows, 256).Error
	}); err != nil {
		return nil, err
	}
	var persisted []models.MediaLibraryStructureRepairItem
	if err := s.db.Where("repair_id = ?", repair.ID).Order("ordinal").Find(&persisted).Error; err != nil {
		return nil, err
	}
	if len(persisted) != len(rows) {
		return nil, errors.New("structure repair checkpoint set is incomplete")
	}
	for index := range rows {
		expected, actual := rows[index], persisted[index]
		if actual.Ordinal != expected.Ordinal || actual.Action != expected.Action || actual.Kind != expected.Kind || actual.SourceRelative != expected.SourceRelative || actual.TargetRelative != expected.TargetRelative || optionalInt(actual.DependencyOrdinal) != optionalInt(expected.DependencyOrdinal) {
			return nil, errors.New("structure repair checkpoint does not match frozen plan")
		}
	}
	return persisted, nil
}

func optionalInt(value *int) int {
	if value == nil {
		return -1
	}
	return *value
}

func structureRepairFailure(ctx context.Context, err error) (code, message string, global bool, retryAt *time.Time) {
	if err == nil {
		return "", "", false, nil
	}
	// The worker context being revoked is a global stop. A provider request may
	// independently time out while the worker context is still healthy; that is
	// an item-local transient failure and must not abort unrelated items.
	if ctx.Err() != nil || errors.Is(err, ErrCatalogFence) || errors.Is(err, ErrCatalogInvalid) || errors.Is(err, ErrCatalogBudget) {
		return CodeMediaLibraryStructureBoundaryChanged, "执行边界已变化，任务已安全停止", true, nil
	}
	if cloudCode, retryable := cloudpkg.ErrorInfo(err); cloudCode != "" {
		switch cloudCode {
		case cloudpkg.CodeAuthExpired, cloudpkg.CodeCookieInvalid:
			return cloudCode, "云盘认证已失效，任务已安全停止", true, nil
		case cloudpkg.CodeRateLimited:
			if retryable {
				next := time.Now().UTC().Add(time.Minute)
				return cloudCode, "云盘触发风控，已保存进度并等待恢复", true, &next
			}
		}
	}
	if errors.Is(err, errStructureConflict) {
		return CodeMediaLibraryStructureConflict, "目标位置存在冲突", false, nil
	}
	if errors.Is(err, errStructureFileLocked) {
		return CodeMediaLibraryStructureFileLocked, "文件正在被占用", false, nil
	}
	if errors.Is(err, errStructurePermissionDenied) {
		return CodeMediaLibraryStructurePermissionDenied, "当前文件不可写", false, nil
	}
	messageLower := strings.ToLower(err.Error())
	if strings.Contains(messageLower, "escapes library root") || strings.Contains(messageLower, "directory is unsafe") || strings.Contains(messageLower, "connection is unavailable") || strings.Contains(messageLower, "mutation is unavailable") || strings.Contains(messageLower, "recoverable recycle is unavailable") {
		return CodeMediaLibraryStructureBoundaryChanged, "数据源安全边界或能力已变化，任务已停止", true, nil
	}
	return CodeMediaLibraryStructureApplyFailed, "单项文件操作失败，可稍后重试", false, nil
}

func structureRepairCounterColumn(status string) string {
	switch status {
	case structureRepairItemSucceeded:
		return "succeeded_items"
	case structureRepairItemFailed:
		return "failed_items"
	case structureRepairItemBlocked:
		return "blocked_items"
	default:
		return ""
	}
}

// transitionStructureRepairItem performs one bounded checkpoint transaction.
// It updates aggregate counters incrementally; a full-table SUM per item would
// make a 16k-item repair quadratic and is deliberately avoided.
func (s *MediaLibraryStructureService) transitionStructureRepairItem(ctx context.Context, repair models.MediaLibraryStructureRepair, claim *ClaimedJob, ordinal int, from, to string, updates map[string]any) error {
	now := time.Now().UTC()
	updates["status"], updates["updated_at"] = to, now
	return s.structureRepairCheckpointTx(ctx, repair, claim, func(tx *gorm.DB) error {
		changed := tx.Model(&models.MediaLibraryStructureRepairItem{}).Where("repair_id = ? AND ordinal = ? AND status = ?", repair.ID, ordinal, from).Updates(updates)
		if changed.Error != nil {
			return changed.Error
		}
		if changed.RowsAffected != 1 {
			return errors.New("structure repair checkpoint changed concurrently")
		}
		repairUpdates := map[string]any{"updated_at": now}
		if oldColumn := structureRepairCounterColumn(from); oldColumn != "" && oldColumn != structureRepairCounterColumn(to) {
			repairUpdates[oldColumn] = gorm.Expr(oldColumn + " - 1")
		}
		if newColumn := structureRepairCounterColumn(to); newColumn != "" && newColumn != structureRepairCounterColumn(from) {
			repairUpdates[newColumn] = gorm.Expr(newColumn + " + 1")
		}
		if fromCounted, toCounted := structureRepairCounterColumn(from) != "", structureRepairCounterColumn(to) != ""; fromCounted != toCounted {
			if toCounted {
				repairUpdates["processed_items"] = gorm.Expr("processed_items + 1")
			} else {
				repairUpdates["processed_items"] = gorm.Expr("processed_items - 1")
			}
		}
		if err := tx.Model(&models.MediaLibraryStructureRepair{}).Where("id = ?", repair.ID).Updates(repairUpdates).Error; err != nil {
			return err
		}
		return nil
	})
}

func (s *MediaLibraryStructureService) executeStructureRepairItems(ctx context.Context, runtime JobRuntime, repair models.MediaLibraryStructureRepair, plan StructurePlan, boundary StructureBoundary, backend MediaLibraryStructureBackend, claim *ClaimedJob) structureRepairExecution {
	result := structureRepairExecution{Plan: plan}
	result.Plan.Items = nil
	result.Plan.RecycleItems = nil
	rows, err := s.ensureStructureRepairItems(ctx, repair, plan, boundary.Storage.Type, claim)
	if err != nil {
		result.GlobalErr, result.GlobalCode = err, CodeMediaLibraryStructureApplyFailed
		return result
	}
	states := make(map[int]string, len(rows))
	processed := int64(0)
	for _, row := range rows {
		states[row.Ordinal] = row.Status
		if structureRepairCounterColumn(row.Status) != "" {
			processed++
		}
	}
	total := len(rows)
	lastHeartbeat := time.Now()
	transitions := 0
	heartbeat := func(force bool) error {
		if runtime == nil || (!force && transitions%64 != 0 && time.Since(lastHeartbeat) < time.Second) {
			return nil
		}
		total64 := int64(total)
		if err := runtime.Heartbeat(nil, &processed, &total64, nil, nil); err != nil {
			return err
		}
		lastHeartbeat = time.Now()
		return nil
	}
	attempted := make(map[int]bool, len(rows))
	for _, row := range rows {
		if row.Status == structureRepairItemSucceeded {
			attempted[row.Ordinal] = true
		}
	}
	markBlocked := func(row models.MediaLibraryStructureRepairItem, code, message string) error {
		if row.Status != structureRepairItemBlocked {
			if err := s.transitionStructureRepairItem(ctx, repair, claim, row.Ordinal, row.Status, structureRepairItemBlocked, map[string]any{"error_code": code, "error_message": message, "finished_at": time.Now().UTC()}); err != nil {
				return err
			}
			if structureRepairCounterColumn(row.Status) == "" {
				processed++
			}
			transitions++
			if err := heartbeat(false); err != nil {
				return err
			}
		}
		states[row.Ordinal] = structureRepairItemBlocked
		attempted[row.Ordinal] = true
		return nil
	}
	for {
		progressed := false
		for _, row := range rows {
			if attempted[row.Ordinal] {
				continue
			}
			if row.DependencyOrdinal != nil && states[*row.DependencyOrdinal] != structureRepairItemSucceeded {
				if !attempted[*row.DependencyOrdinal] {
					continue
				}
				if err := markBlocked(row, "dependency_failed", "依赖的文件操作尚未成功"); err != nil {
					result.GlobalErr, result.GlobalCode = err, CodeMediaLibraryStructureApplyFailed
					return result
				}
				progressed = true
				continue
			}
			progressed = true
			attempted[row.Ordinal] = true
			var operationErr error
			if row.Action == "recycle" {
				operationErr = backend.Recycle(ctx, boundary, []StructureRecycleItem{plan.RecycleItems[row.Ordinal]}, nil)
			} else {
				operationErr = backend.Apply(ctx, boundary, []StructurePlanItem{plan.Items[row.Ordinal-len(plan.RecycleItems)]}, nil)
			}
			if operationErr != nil {
				code, safeMessage, global, retryAt := structureRepairFailure(ctx, operationErr)
				status := structureRepairItemFailed
				if retryAt != nil {
					status = structureRepairItemPending
				}
				if err := s.transitionStructureRepairItem(ctx, repair, claim, row.Ordinal, row.Status, status, map[string]any{"attempt_count": gorm.Expr("attempt_count + 1"), "error_code": code, "error_message": safeMessage, "started_at": time.Now().UTC(), "finished_at": time.Now().UTC()}); err != nil {
					result.GlobalErr, result.GlobalCode = err, CodeMediaLibraryStructureApplyFailed
					return result
				}
				states[row.Ordinal] = status
				if structureRepairCounterColumn(row.Status) == "" && structureRepairCounterColumn(status) != "" {
					processed++
				}
				if structureRepairCounterColumn(row.Status) != "" && structureRepairCounterColumn(status) == "" {
					processed--
				}
				transitions++
				if heartbeatErr := heartbeat(false); heartbeatErr != nil {
					result.GlobalErr, result.GlobalCode = heartbeatErr, CodeMediaLibraryStructureBoundaryChanged
					return result
				}
				if global {
					result.GlobalErr, result.GlobalCode, result.RetryAt = operationErr, code, retryAt
					return result
				}
				continue
			}
			// Revalidate the lease/fence after the external mutation and before
			// committing success. If it changed, leave the item non-successful so an
			// idempotent retry must reconcile the physical result.
			if operationErr = validateStructureMutation(boundary); operationErr != nil {
				result.GlobalErr, result.GlobalCode = operationErr, CodeMediaLibraryStructureBoundaryChanged
				return result
			}
			if err := s.transitionStructureRepairItem(ctx, repair, claim, row.Ordinal, row.Status, structureRepairItemSucceeded, map[string]any{"attempt_count": gorm.Expr("attempt_count + 1"), "error_code": "", "error_message": "", "started_at": time.Now().UTC(), "finished_at": time.Now().UTC()}); err != nil {
				result.GlobalErr, result.GlobalCode = err, CodeMediaLibraryStructureApplyFailed
				return result
			}
			states[row.Ordinal] = structureRepairItemSucceeded
			if structureRepairCounterColumn(row.Status) == "" {
				processed++
			}
			transitions++
			if err := heartbeat(false); err != nil {
				result.GlobalErr, result.GlobalCode = err, CodeMediaLibraryStructureBoundaryChanged
				return result
			}
		}
		if !progressed {
			for _, row := range rows {
				if attempted[row.Ordinal] {
					continue
				}
				if err := markBlocked(row, "dependency_cycle", "文件操作之间存在循环依赖，无法安全执行"); err != nil {
					result.GlobalErr, result.GlobalCode = err, CodeMediaLibraryStructureApplyFailed
					return result
				}
			}
			break
		}
		if len(attempted) == len(rows) {
			break
		}
	}
	if err := heartbeat(true); err != nil {
		result.GlobalErr, result.GlobalCode = err, CodeMediaLibraryStructureBoundaryChanged
		return result
	}
	for _, row := range rows {
		switch states[row.Ordinal] {
		case structureRepairItemSucceeded:
			result.Succeeded++
			if row.Action == "recycle" {
				result.Plan.RecycleItems = append(result.Plan.RecycleItems, plan.RecycleItems[row.Ordinal])
			} else {
				result.Plan.Items = append(result.Plan.Items, plan.Items[row.Ordinal-len(plan.RecycleItems)])
			}
		case structureRepairItemFailed:
			result.Failed++
		case structureRepairItemBlocked:
			result.Blocked++
		}
	}
	return result
}
