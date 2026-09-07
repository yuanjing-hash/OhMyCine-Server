package services

import (
	"context"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	cloud "github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

// Every saved driver is bound to one credential revision. This read-only guard
// covers existing driver holders as well as newly resolved jobs; one confirmed
// authentication failure stops later requests across the connection.
func (s *ConnectionService) guardConnectionCredential(ctx context.Context, id uint, revision uint64) error {
	var current models.Connection
	if err := s.db.WithContext(ctx).Select("id", "revision", "enabled", "last_health_status", "last_health_error_code").First(&current, id).Error; err != nil {
		return cloud.Error(cloud.CodeUnavailable, true, nil)
	}
	if current.Revision != revision || !current.Enabled {
		return cloud.Error(cloud.CodeUnavailable, false, nil)
	}
	if current.LastHealthStatus == "offline" && current.LastHealthErrorCode == cloud.CodeAuthExpired {
		return cloud.Error(cloud.CodeAuthExpired, false, nil)
	}
	return nil
}

// Authentication invalidity is durable; risk control, rate limiting and network
// failures keep their existing adaptive recovery semantics. No late callback
// from an old Cookie can invalidate a successfully committed replacement.
func (s *ConnectionService) recordConnectionCredentialFailure(id uint, revision uint64, err error) {
	code, _ := cloud.ErrorInfo(err)
	if err == nil || code != cloud.CodeAuthExpired {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	now := time.Now().UTC()
	result := s.db.WithContext(ctx).Model(&models.Connection{}).
		Where("id=? AND revision=? AND NOT (last_health_status=? AND last_health_error_code=?)", id, revision, "offline", cloud.CodeAuthExpired).
		Updates(map[string]any{"last_health_status": "offline", "last_health_error_code": cloud.CodeAuthExpired, "last_health_checked_at": now, "updated_at": now})
	if result.Error != nil {
		s.log.Warn().Uint("connection_id", id).Str("error_code", "connection_health_write_failed").Msg("保存连接认证失效状态失败")
	}
}
