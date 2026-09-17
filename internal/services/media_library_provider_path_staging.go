package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path"
	"sort"
	"strings"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/medialibrary"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Stage immutable pages before entering the catalog publication writer. Only
// the independently retained batch header is activated by the commit fence.
func stageProviderPaths(ctx context.Context, db *gorm.DB, library models.MediaLibrary, storage models.Storage, result medialibrary.Result, run models.MediaLibraryScanRun) error {
	if storage.Type != models.StorageTypePan115 || run.StartedAt.IsZero() {
		return nil
	}
	if run.ID == 0 {
		return ErrCatalogFence
	}
	fingerprint := catalogSourceFingerprint(library, storage)
	byID := map[string]models.MediaLibraryProviderPath{}
	add := func(id, parent, relative string, dir bool) {
		if id == "" || relative == "" || !strings.HasPrefix(relative, "/") || path.Clean(relative) != relative || relative == "/" || strings.ContainsAny(relative, "\\\x00\r\n") {
			return
		}
		byID[id] = models.MediaLibraryProviderPath{ScanRunID: run.ID, LibraryID: library.ID, SourceFingerprint: fingerprint, ProviderID: id, ParentProviderID: parent, RelativePath: relative, IsDir: dir, ObservedAt: run.StartedAt.UTC()}
	}
	if !result.Partial || result.Scoped {
		for _, d := range result.Directories {
			add(d.ID, d.ParentID, d.RelativePath, true)
		}
	}
	for _, f := range result.Files {
		add(f.ProviderID, "", f.RelativePath, false)
	}
	for _, a := range result.Assets {
		add(a.ProviderID, a.ParentProviderID, a.RelativePath, false)
	}
	rows := make([]models.MediaLibraryProviderPath, 0, len(byID))
	for _, row := range byID {
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ProviderID < rows[j].ProviderID })
	hash := sha256.New()
	for _, row := range rows {
		data, _ := json.Marshal([]any{row.ProviderID, row.ParentProviderID, row.RelativePath, row.IsDir})
		hash.Write(data)
	}
	manifest := hex.EncodeToString(hash.Sum(nil))
	batch := models.MediaLibraryProviderPathBatch{ScanRunID: run.ID, LibraryID: library.ID, SourceFingerprint: fingerprint, ManifestFingerprint: manifest, ObservedAt: run.StartedAt.UTC()}
	if err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&batch).Error; err != nil {
			return err
		}
		var existing models.MediaLibraryProviderPathBatch
		if err := tx.First(&existing, "scan_run_id = ?", run.ID).Error; err != nil {
			return err
		}
		if existing.LibraryID != library.ID || existing.SourceFingerprint != fingerprint || existing.ManifestFingerprint != manifest || !existing.ObservedAt.Equal(run.StartedAt) {
			return ErrCatalogFence
		}
		return nil
	}); err != nil {
		return err
	}
	for start := 0; start < len(rows); start += 100 {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			return tx.Clauses(clause.OnConflict{DoNothing: true}).Create(rows[start:min(start+100, len(rows))]).Error
		}); err != nil {
			return err
		}
	}
	return nil
}

// Latest PUBLISHED fact wins for each stable ID. Failed or cancelled staging
// never shadows older authority; clearing scan history does not remove headers.
func publishedProviderPaths(tx *gorm.DB, libraryID uint, fingerprint string) *gorm.DB {
	return tx.Model(&models.MediaLibraryProviderPath{}).Table("media_library_provider_paths AS paths").Select("paths.*").Where("paths.library_id = ? AND paths.source_fingerprint = ?", libraryID, fingerprint).
		Where("EXISTS (SELECT 1 FROM media_library_provider_path_batches batch WHERE batch.scan_run_id = paths.scan_run_id AND batch.published_at IS NOT NULL)").
		Where(`NOT EXISTS (SELECT 1 FROM media_library_provider_paths newer JOIN media_library_provider_path_batches batch ON batch.scan_run_id = newer.scan_run_id AND batch.published_at IS NOT NULL WHERE newer.library_id = paths.library_id AND newer.source_fingerprint = paths.source_fingerprint AND newer.provider_id = paths.provider_id AND newer.scan_run_id > paths.scan_run_id)`)
}
