package services

import (
	"context"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

type DashboardFact struct {
	Label string `json:"label"`
	Value *int64 `json:"value"`
	Unit  string `json:"unit,omitempty"`
	Link  string `json:"link,omitempty"`
}

func (s *AdminService) SetCatalogSnapshotStore(store *CatalogSnapshotStore) {
	s.catalogStore = store
}

// Uses persisted facts only; visiting the dashboard never probes a provider,
// performs a scan or initiates a download. Failure is isolated per section.
func (s *AdminService) DashboardOperations(ctx context.Context, actor Actor) map[string]PlayerOverviewSection[DashboardFact] {
	result := make(map[string]PlayerOverviewSection[DashboardFact])
	db := s.db.WithContext(ctx)
	put := func(key string, loader func() ([]DashboardFact, error)) {
		result[key] = loadPlayerOverviewSection(func() ([]DashboardFact, bool, error) { facts, err := loader(); return facts, false, err })
	}
	fact := func(label string, value int64, link string) DashboardFact {
		return DashboardFact{Label: label, Value: &value, Link: link}
	}
	if actor.Can(authz.PermissionMediaLibrariesRead) {
		put("media-summary", func() ([]DashboardFact, error) {
			facade := &MediaLibraryService{db: s.db, catalogStore: s.catalogStore}
			var facts []DashboardFact
			err := facade.withCatalogReadTx(ctx, func(tx *gorm.DB) error {
				ids, err := facade.authorizedMediaLibraryIDsTx(tx, actor, authz.PermissionMediaLibrariesRead, true)
				if err != nil {
					return err
				}
				var visible []uint
				if len(ids) > 0 {
					if err := tx.Table("media_libraries").Joins("JOIN storages ON storages.id=media_libraries.storage_id").Where("media_libraries.id IN ? AND storages.enabled=?", ids, true).Pluck("media_libraries.id", &visible).Error; err != nil {
						return err
					}
				}
				reader, err := PinCatalogTx(tx, visible)
				if err != nil {
					return err
				}
				var entries, works int64
				if err := reader.Entries().Count(&entries).Error; err != nil {
					return err
				}
				if err := tx.Table("(?) AS works", reader.Entries().Select("library_id,work_key").Group("library_id,work_key")).Count(&works).Error; err != nil {
					return err
				}
				facts = []DashboardFact{fact("可见媒体库", int64(len(visible)), "/discovery/library"), fact("库内作品（按库计数）", works, "/discovery/library"), fact("媒体文件", entries, "/discovery/library")}
				return nil
			})
			return facts, err
		})
	}
	if actor.Can(authz.PermissionStoragesRead) {
		put("storage-summary", func() ([]DashboardFact, error) {
			var rows []models.Storage
			if err := db.Select("id,name,last_probe_free_bytes,last_probe_checked_at").Order("id").Limit(51).Find(&rows).Error; err != nil {
				return nil, err
			}
			facts := make([]DashboardFact, 0, len(rows))
			for _, row := range rows {
				var value *int64
				if row.LastProbeCheckedAt != nil && row.LastProbeFreeBytes != nil && *row.LastProbeFreeBytes <= 1<<63-1 {
					v := int64(*row.LastProbeFreeBytes)
					value = &v
				}
				facts = append(facts, DashboardFact{Label: row.Name + " · 最近检测剩余空间", Value: value, Unit: "bytes", Link: "/system/connections"})
			}
			return facts, nil
		})
		section := result["storage-summary"]
		if len(section.List) > 50 {
			section.List = section.List[:50]
			section.HasMore = true
			result["storage-summary"] = section
		}
	}
	if actor.Can(authz.PermissionConnectionsRead) {
		put("connection-health", func() ([]DashboardFact, error) {
			var groups []struct {
				Status string
				Count  int64
			}
			if err := db.Model(&models.Connection{}).Select("last_health_status AS status,COUNT(*) AS count").Group("last_health_status").Scan(&groups).Error; err != nil {
				return nil, err
			}
			facts := make([]DashboardFact, 0, len(groups))
			for _, group := range groups {
				facts = append(facts, fact("最近检测："+safeLabel(group.Status, 32), group.Count, "/system/connections"))
			}
			return facts, nil
		})
	}
	if actor.Can(authz.PermissionJobsReadAll) || actor.Can(authz.PermissionJobsReadOwn) {
		query, _ := readableJobsQuery(db, actor)
		put("active-tasks", func() ([]DashboardFact, error) {
			var groups []struct {
				Status string
				Count  int64
			}
			if err := query.Select("status,COUNT(*) AS count").Group("status").Scan(&groups).Error; err != nil {
				return nil, err
			}
			facts := make([]DashboardFact, 0, len(groups))
			for _, group := range groups {
				facts = append(facts, fact(group.Status, group.Count, "/automation/tasks?status="+group.Status))
			}
			return facts, nil
		})
	}
	if actor.Can(authz.PermissionSettingsRead) {
		put("scheduler-jobs", func() ([]DashboardFact, error) {
			var enabled, disabled int64
			schedules := db.Model(&models.ScheduleDefinition{})
			if !actor.IsSystemAdmin() {
				schedules = schedules.Where("owner_id = ?", actor.User.ID)
			}
			if err := schedules.Session(&gorm.Session{}).Where("enabled = ?", true).Count(&enabled).Error; err != nil {
				return nil, err
			}
			if err := schedules.Session(&gorm.Session{}).Where("enabled = ?", false).Count(&disabled).Error; err != nil {
				return nil, err
			}
			return []DashboardFact{fact("启用计划", enabled, "/automation/schedules"), fact("停用计划", disabled, "/automation/schedules")}, nil
		})
	}
	if actor.Can(authz.PermissionDownloadsReadAll) || actor.Can(authz.PermissionDownloadsReadOwn) {
		put("download-summary", func() ([]DashboardFact, error) {
			query := db.Model(&models.DownloadTask{}).Joins("JOIN jobs j ON j.id = download_tasks.job_id")
			if !actor.Can(authz.PermissionDownloadsReadAll) {
				query = query.Where("download_tasks.owner_id = ?", actor.User.ID)
			}
			var total int64
			if err := query.Session(&gorm.Session{}).Count(&total).Error; err != nil {
				return nil, err
			}
			var sample struct{ Speed *int64 }
			if err := query.Select("SUM(CASE WHEN j.status = 'running' AND last_sampled_at >= ? THEN download_speed END) AS speed", time.Now().Add(-2*time.Minute)).Scan(&sample).Error; err != nil {
				return nil, err
			}
			return []DashboardFact{fact("下载任务记录", total, "/automation/downloads"), {Label: "两分钟内采样的运行中下载速率（非完整实时总速率）", Value: sample.Speed, Unit: "bytes/s", Link: "/automation/downloads"}}, nil
		})
	}
	return result
}

func readableJobsQuery(db *gorm.DB, actor Actor) (*gorm.DB, error) {
	if !actor.Can(authz.PermissionJobsReadAll) && !actor.Can(authz.PermissionJobsReadOwn) {
		return nil, appError(CodePermissionDenied, "没有查看任务的权限", nil)
	}
	query := db.Model(&models.Job{})
	if !actor.Can(authz.PermissionJobsReadAll) {
		query = query.Where("owner_id = ?", actor.User.ID)
	}
	return query, nil
}
