package services

import (
	"context"
	"errors"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"gorm.io/gorm"
)

const capacityUnsupported = "storage_capacity_unsupported"

type StorageCapacity struct {
	ID         uint       `json:"id"`
	Name       string     `json:"name"`
	Enabled    bool       `json:"enabled"`
	Status     string     `json:"status"`
	FreeBytes  *uint64    `json:"free_bytes"`
	TotalBytes *uint64    `json:"total_bytes"`
	SampledAt  *time.Time `json:"sampled_at"`
	CheckedAt  *time.Time `json:"checked_at"`
	ErrorCode  string     `json:"error_code,omitempty"`
}

func storageCapacity(row models.Storage, now time.Time) StorageCapacity {
	if row.LastCapacitySourceFingerprint != catalogSourceFingerprint(models.MediaLibrary{}, row) {
		status := "not_sampled"
		if !row.Enabled {
			status = "disabled"
		}
		return StorageCapacity{ID: row.ID, Name: row.Name, Enabled: row.Enabled, Status: status}
	}
	status := row.LastCapacityStatus
	if status == "" {
		status = "not_sampled"
	}
	if !row.Enabled {
		status = "disabled"
	}
	if status == "ok" && (row.LastCapacitySampledAt == nil || now.Sub(*row.LastCapacitySampledAt) > time.Hour) {
		status = "stale"
	}
	if status == "refreshing" && row.LastCapacityCheckedAt != nil && now.Sub(*row.LastCapacityCheckedAt) > 30*time.Second {
		status = "error"
	}
	return StorageCapacity{ID: row.ID, Name: row.Name, Enabled: row.Enabled, Status: status,
		FreeBytes: row.LastCapacityFreeBytes, TotalBytes: row.LastCapacityTotalBytes,
		SampledAt: row.LastCapacitySampledAt, CheckedAt: row.LastCapacityCheckedAt, ErrorCode: row.LastCapacityErrorCode}
}

// RefreshCapacity is an explicit, bounded measurement, independent of the root
// readability probe. A failed measurement retains the last successful sample.
func (s *StorageService) RefreshCapacity(ctx context.Context, actor Actor, id uint, request RequestContext) (StorageCapacity, error) {
	if !actor.Can(authz.PermissionStoragesRead) || !actor.Can(authz.PermissionStoragesTest) {
		return StorageCapacity{}, appError(CodePermissionDenied, "无权更新存储容量", nil)
	}
	var row models.Storage
	if err := s.db.WithContext(ctx).First(&row, id).Error; err != nil {
		return StorageCapacity{}, storageNotFound(err)
	}
	if !row.Enabled {
		return storageCapacity(row, time.Now().UTC()), appError(CodeConflict, "数据源已停用", nil)
	}
	now := time.Now().UTC()
	if row.LastCapacitySourceFingerprint == catalogSourceFingerprint(models.MediaLibrary{}, row) && row.LastCapacityCheckedAt != nil && now.Sub(*row.LastCapacityCheckedAt) < time.Minute {
		return storageCapacity(row, now), appError(CodeConflict, "容量刚刚检测过，请一分钟后重试", nil)
	}
	scope := catalogSourceFingerprint(models.MediaLibrary{}, row)
	reservation := map[string]any{"last_capacity_checked_at": now, "last_capacity_source_fingerprint": scope, "last_capacity_status": "refreshing", "last_capacity_error_code": ""}
	if row.LastCapacitySourceFingerprint != scope {
		reservation["last_capacity_free_bytes"], reservation["last_capacity_total_bytes"], reservation["last_capacity_sampled_at"] = nil, nil, nil
	}
	var reserved int64
	err := s.writeConfiguration(ctx, func(tx *gorm.DB) error {
		result := tx.Model(&models.Storage{}).Where("id = ? AND updated_at = ? AND catalog_connection_epoch = ? AND (last_capacity_source_fingerprint <> ? OR last_capacity_checked_at IS NULL OR last_capacity_checked_at <= ?)", id, row.UpdatedAt, row.CatalogConnectionEpoch, scope, now.Add(-time.Minute)).UpdateColumns(reservation)
		reserved = result.RowsAffected
		return result.Error
	})
	if err != nil {
		return StorageCapacity{}, err
	}
	if reserved != 1 {
		return StorageCapacity{}, appError(CodeConflict, "容量正在更新或数据源已变化，请稍后重试", nil)
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	status, code := "ok", ""
	var free, total *uint64
	var connection *models.Connection
	if row.Type == models.StorageTypeLocal {
		probe := s.driver.ProbeRoot(row.RootPath)
		if probe.FreeBytes == nil || probe.TotalBytes == nil || probe.ErrorCode != "" {
			status, code = "error", "storage_capacity_unknown"
		} else {
			free, total = probe.FreeBytes, probe.TotalBytes
		}
	} else if row.ConnectionID != nil && s.connections != nil {
		current, driver, err := s.connections.cachedDriver(*row.ConnectionID)
		connection = &current
		if err != nil {
			status, code = "error", CodeConnectionUnavailable
		} else if reader, ok := driver.(cloud.QuotaReader); !ok {
			status, code = "unsupported", capacityUnsupported
		} else if quota, err := reader.Quota(cloud.WithReadClass(ctx, cloud.ReadClassInteractive)); err != nil {
			status, code = "error", "storage_capacity_unavailable"
			providerCode, _ := cloud.ErrorInfo(err)
			switch providerCode {
			case cloud.CodeAuthExpired, cloud.CodeCookieInvalid:
				code = "storage_capacity_authorization"
			case cloud.CodeRateLimited:
				code = "storage_capacity_rate_limited"
			}
			if errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
				code = "storage_capacity_timeout"
			}
		} else if quota.TotalBytes == 0 || quota.UsedBytes > quota.TotalBytes {
			status, code = "error", "storage_capacity_invalid"
		} else {
			remaining, capacity := quota.TotalBytes-quota.UsedBytes, quota.TotalBytes
			free, total = &remaining, &capacity
		}
	} else {
		status, code = "unsupported", capacityUnsupported
	}
	// The request may have timed out; use a short independent context only for
	// recording its bounded result, never for extending provider work.
	finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer finishCancel()
	err = s.writeConfiguration(finishCtx, func(tx *gorm.DB) error {
		query := tx.Model(&models.Storage{}).Where("id = ? AND updated_at = ? AND catalog_connection_epoch = ? AND last_capacity_checked_at = ? AND last_capacity_source_fingerprint = ?", id, row.UpdatedAt, row.CatalogConnectionEpoch, now, scope)
		if connection != nil && connection.ID != 0 {
			query = query.Where("EXISTS (SELECT 1 FROM connections WHERE id = ? AND revision = ? AND account_id = ? AND enabled = ?)", connection.ID, connection.Revision, connection.AccountID, true)
		}
		updates := map[string]any{"last_capacity_status": status, "last_capacity_error_code": code}
		if status == "ok" {
			updates["last_capacity_free_bytes"], updates["last_capacity_total_bytes"], updates["last_capacity_sampled_at"] = free, total, time.Now().UTC()
		}
		result := query.UpdateColumns(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return appError(CodeConflict, "数据源或账号已变化，请重新更新容量", nil)
		}
		if s.audit != nil {
			return s.audit.Record(tx, &actor.User.ID, "storage.capacity", "storage", uintID(id), status, map[string]any{"error_code": code}, request)
		}
		return nil
	})
	if err != nil {
		return StorageCapacity{}, err
	}
	var saved models.Storage
	if err := s.db.WithContext(finishCtx).First(&saved, id).Error; err != nil {
		return StorageCapacity{}, err
	}
	return storageCapacity(saved, time.Now().UTC()), nil
}
