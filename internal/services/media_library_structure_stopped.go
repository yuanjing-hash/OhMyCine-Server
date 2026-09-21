package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/database"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

// A paused/failed current attempt may be cancelled after its execution stack
// exited. Reuse scheduler maintenance, not replacement Jobs or a migration.
func (w *MediaLibraryRepairWorker) RecoverStoppedWork(ctx context.Context, after uint64) (uint64, error) {
	var rows []models.CatalogPhysicalWrite
	err := w.service.db.WithContext(ctx).Model(&models.CatalogPhysicalWrite{}).Joins("JOIN jobs j ON j.id=catalog_physical_writes.job_id").Where("catalog_physical_writes.id>? AND owner_kind=? AND state='quiescent' AND runtime_id<>'' AND j.status=? AND j.lease_token_hash='' AND j.lease_expires_at IS NULL", after, CatalogPhysicalRepair, models.JobStatusCancelled).Select("catalog_physical_writes.*").Order("catalog_physical_writes.id").Limit(8).Find(&rows).Error
	if err != nil || len(rows) == 0 {
		return 0, err
	}
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return after, err
		}
		after = row.ID
		err := w.service.recoverStoppedStructureRepair(ctx, row.ID)
		code, message := "structure_cancelled_reconciled", "已取消；文件结果已确认，未继续执行整理"
		if errors.Is(err, errStructureCancelProgress) {
			code, message = "structure_cancelled_verifying", "已取消；正在分批核验文件结果，进度已保存"
			var repair models.MediaLibraryStructureRepair
			if w.service.db.WithContext(ctx).First(&repair, "id=?", row.OwnerID).Error == nil {
				var state structureCatalogRepairState
				if json.Unmarshal([]byte(repair.StateJSON), &state) == nil {
					message = fmt.Sprintf("已取消；正在核验文件结果（已核验 %d 项），进度已保存", state.CancelVerified)
				}
			}
			// Continue this owner next cycle instead of spending a cycle wrapping.
			after = row.ID - 1
		} else if err != nil {
			code, message = "structure_cancelled_outcome_pending", "已取消；部分文件结果尚未确认，请检查数据源连接或原任务中的文件是否发生变化"
		}
		w.service.recordCancelledStructureOutcome(ctx, row.JobID, code, message)
		if err == nil && w.service.queue != nil {
			w.service.queue.wake()
		}
	}
	return after, nil
}

func (s *MediaLibraryStructureService) recordCancelledStructureOutcome(parent context.Context, jobID, code, message string) {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	_ = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		changed := tx.Model(&models.Job{}).Where("id=? AND status=? AND (last_error_code<>? OR last_error_message<>?)", jobID, models.JobStatusCancelled, code, message).Updates(map[string]any{"last_error_code": code, "last_error_message": message, "updated_at": time.Now().UTC()})
		if changed.Error != nil || changed.RowsAffected == 0 {
			return changed.Error
		}
		return recordJobEvent(tx, jobID, "cancel.reconciliation", models.JobStatusCancelled, models.JobStatusCancelled, nil, message, time.Now().UTC())
	})
}

func (s *MediaLibraryStructureService) recoverStoppedStructureRepair(ctx context.Context, id uint64) error {
	var repair models.MediaLibraryStructureRepair
	var plan StructurePlan
	var permit CatalogPhysicalWritePermit
	var boundary StructureBoundary
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		runtimeID, err := database.ExclusiveRuntimeID(tx)
		if err != nil {
			return err
		}
		var proof models.CatalogPhysicalWrite
		if err := tx.First(&proof, id).Error; err != nil {
			return err
		}
		if proof.OwnerKind != CatalogPhysicalRepair || proof.State != "quiescent" || proof.RuntimeID == "" || proof.Revision >= math.MaxInt64 {
			return ErrCatalogFence
		}
		var job models.Job
		if err := tx.First(&job, "id=?", proof.JobID).Error; err != nil {
			return err
		}
		if job.Status != models.JobStatusCancelled || job.LeaseTokenHash != "" || job.LeaseExpiresAt != nil {
			return ErrCatalogFence
		}
		if err := tx.First(&repair, "id=?", proof.OwnerID).Error; err != nil {
			return err
		}
		var state structureCatalogRepairState
		if json.Unmarshal([]byte(repair.StateJSON), &state) != nil || state.Version > 1 || json.Unmarshal([]byte(repair.PlanJSON), &plan) != nil || plan.Version != 1 || plan.LibraryID != repair.LibraryID {
			return ErrCatalogFence
		}
		changed := tx.Model(&models.CatalogPhysicalWrite{}).Where("id=? AND revision=? AND state='quiescent' AND runtime_id=?", proof.ID, proof.Revision, proof.RuntimeID).Updates(map[string]any{"runtime_id": runtimeID, "revision": proof.Revision + 1, "updated_at": time.Now().UTC()})
		if changed.Error != nil {
			return changed.Error
		}
		if changed.RowsAffected != 1 {
			return ErrCatalogFence
		}
		proof.Revision++
		proof.RuntimeID = runtimeID
		permit = CatalogPhysicalWritePermit{evidence: proof}
		if err := validateCancelledStructurePermitTx(tx, permit, repair); err != nil {
			return err
		}
		if err := tx.First(&boundary.Library, repair.LibraryID).Error; err != nil {
			return err
		}
		return tx.First(&boundary.Storage, boundary.Library.StorageID).Error
	})
	if err != nil {
		return err
	}
	backend, err := s.backends.Get(boundary.Storage.Type)
	if err != nil {
		return err
	}
	var state structureCatalogRepairState
	_ = json.Unmarshal([]byte(repair.StateJSON), &state)
	if state.Version == 1 {
		return s.finalizeCancelledCatalogStructureResult(ctx, permit, repair, plan, boundary, backend)
	}
	return s.reconcileCancelledStructureRepair(ctx, permit, repair, plan, boundary, backend)
}
