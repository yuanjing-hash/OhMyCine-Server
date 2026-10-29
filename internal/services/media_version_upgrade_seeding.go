package services

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	storagefs "github.com/yuanjing-hash/OhMyCine-Server/internal/storage"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
)

const CodeMediaUpgradeSeedingBusy = "media_upgrade_seeding_source_busy"

// A distinct library hardlink can be recycled without touching a seed path.
// Protect the actual pathname, not inode equivalence or an entire old task.
func (s *MediaVersionUpgradeService) protectUpgradeSeedingSource(ctx context.Context, oldPath string) error {
	var downloads []models.DownloadTask
	if err := s.db.WithContext(ctx).Table("download_tasks AS d").Select("d.*").Joins("JOIN seeding_tasks AS seed ON seed.download_task_id=d.id").Where("seed.phase<>? AND d.staging_absolute_path<>''", models.SeedingTaskStatusCompleted).Limit(1001).Find(&downloads).Error; err != nil {
		return err
	}
	if len(downloads) > 1000 {
		return appError(CodeMediaUpgradeSeedingBusy, "做种源文件范围尚未核实，旧版本保留", nil)
	}
	for _, download := range downloads {
		if normalizeExecutionLocation(download.ExecutionLocation) == models.NodeLocationRemote || !filepath.IsAbs(download.StagingAbsolutePath) || ensureWithin(download.StagingAbsolutePath, oldPath) != nil {
			continue
		}
		var manifest downloadpkg.Manifest
		if json.Unmarshal([]byte(download.CompletedManifestJSON), &manifest) != nil || !manifest.Complete || len(manifest.Files) > 10000 {
			return appError(CodeMediaUpgradeSeedingBusy, "旧版本可能仍是做种源文件，旧版本保留", nil)
		}
		categoryRoot := filepath.Join(download.StagingAbsolutePath, firstNonEmpty(download.StagingCategory, download.ScrapeCategory))
		for _, file := range manifest.Files {
			source, err := resolveManifestSource(categoryRoot, download.StagingAbsolutePath, strings.TrimPrefix(file.RelativePath, "/"))
			if err != nil {
				continue
			}
			if storagefs.NormalizeForComparison(source) == storagefs.NormalizeForComparison(oldPath) {
				return appError(CodeMediaUpgradeSeedingBusy, "旧版本是仍在使用的做种源文件，等待下载器完成后再回收", nil)
			}
		}
	}
	return nil
}
