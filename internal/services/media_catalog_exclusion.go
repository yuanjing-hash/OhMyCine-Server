package services

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type CatalogExclusionItem struct {
	ID          string    `json:"exclusion_id"`
	LibraryID   uint      `json:"library_id"`
	LibraryName string    `json:"library_name"`
	Title       string    `json:"title"`
	Kind        string    `json:"kind"`
	EntryCount  int       `json:"entry_count"`
	ExcludedAt  time.Time `json:"excluded_at"`
}

type CatalogExclusionPage struct {
	List     []CatalogExclusionItem `json:"list"`
	Total    int64                  `json:"total"`
	Page     int                    `json:"page"`
	PageSize int                    `json:"page_size"`
	HasMore  bool                   `json:"has_more"`
}

func catalogExclusionItem(row models.MediaCatalogExclusion, name string) CatalogExclusionItem {
	return CatalogExclusionItem{ID: row.ID, LibraryID: row.LibraryID, LibraryName: name,
		Title: row.Title, Kind: row.Kind, EntryCount: row.EntryCount, ExcludedAt: row.CreatedAt}
}

func (s *MediaLibraryService) ExcludeCatalogWork(ctx context.Context, actor Actor, libraryID uint, token string, request RequestContext) (CatalogExclusionItem, error) {
	var result CatalogExclusionItem
	if !actor.CanResource(authz.PermissionMediaLibrariesScan, models.AuthorizationResourceMediaLibrary, uintID(libraryID)) {
		return result, appError(CodePermissionDenied, "无权移出这个媒体库的作品", nil)
	}
	workKey, err := decodeCatalogToken(token)
	if err != nil {
		return result, err
	}
	var revision uint64
	var artifactGeneration uint64
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := s.ensureMediaLibraryReadableTx(tx, actor, libraryID); err != nil {
			return err
		}
		if err := requireMediaLibraryNotRetiringTx(tx, libraryID); err != nil {
			return err
		}
		var library models.MediaLibrary
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&library, libraryID).Error; err != nil {
			return err
		}
		var storage models.Storage
		if err := tx.First(&storage, library.StorageID).Error; err != nil {
			return err
		}
		fingerprint := catalogSourceFingerprint(library, storage)
		artifactGeneration = library.ArtifactGeneration
		var existing models.MediaCatalogExclusion
		err := tx.Where("library_id=? AND source_epoch=? AND work_key=?", libraryID, library.ExclusionEpoch, workKey).First(&existing).Error
		if err == nil {
			result = catalogExclusionItem(existing, library.Name)
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		reader, err := PinCatalogTx(tx, []uint{libraryID})
		if err != nil {
			return err
		}
		var first models.MediaLibraryEntry
		if err := reader.VisibleEntries().Where("library_id=? AND work_key=?", libraryID, workKey).
			Order("id").First(&first).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return appError(CodeNotFound, "媒体作品不存在", nil)
			}
			return err
		}
		kind := "movie"
		if first.MediaType == "tv" {
			kind = "series"
		}
		row := models.MediaCatalogExclusion{ID: uuid.NewString(), LibraryID: libraryID,
			SourceEpoch: library.ExclusionEpoch, SourceFingerprint: fingerprint,
			WorkKey: workKey, Title: first.Title, Kind: kind, CreatedAt: time.Now().UTC()}
		if err := tx.Create(&row).Error; err != nil {
			return err
		}
		// The new exclusion hides VisibleEntries in this transaction. Traverse raw
		// pinned facts by ID so large series are captured without a 2,000-file cap.
		const batchSize = 250
		var after uint
		for {
			var entries []models.MediaLibraryEntry
			if err := reader.Entries().Where("library_id=? AND work_key=? AND id>?", libraryID, workKey, after).
				Order("id").Limit(batchSize).Find(&entries).Error; err != nil {
				return err
			}
			if len(entries) == 0 {
				break
			}
			members := make([]models.MediaCatalogExclusionMember, 0, len(entries))
			for _, entry := range entries {
				members = append(members, models.MediaCatalogExclusionMember{
					ExclusionID: row.ID, RelativePath: entry.RelativePath, ProviderID: entry.ProviderID,
				})
			}
			if err := tx.CreateInBatches(members, batchSize).Error; err != nil {
				return err
			}
			row.EntryCount += len(entries)
			after = entries[len(entries)-1].ID
		}
		if err := tx.Model(&row).Update("entry_count", row.EntryCount).Error; err != nil {
			return err
		}
		if s.changes != nil {
			change, err := s.changes.RecordTx(tx, libraryID, library.DirtyGeneration, models.MediaLibraryChangeRemoval, true)
			if err != nil {
				return err
			}
			revision = change.Revision
		}
		if s.audit != nil {
			if err := s.audit.Record(tx, &actor.User.ID, "media_catalog.exclude", "media_library", strconv.FormatUint(uint64(libraryID), 10), "success", map[string]any{"work_hash": catalogWorkHash(workKey), "entries": row.EntryCount}, request); err != nil {
				return err
			}
		}
		result = catalogExclusionItem(row, library.Name)
		return nil
	})
	if err == nil && revision > 0 && s.changes != nil {
		s.changes.NotifyCommitted(libraryID, revision)
	}
	if err == nil && revision > 0 && s.artifacts != nil && artifactGeneration > 0 {
		if refreshErr := s.artifacts.RefreshGeneration(libraryID, artifactGeneration); refreshErr != nil {
			s.log.Warn().Err(refreshErr).Uint("library_id", libraryID).Msg("排除作品后暂无法刷新托管产物，等待下一次完整扫描")
		}
	}
	return result, err
}

func (s *MediaLibraryService) CatalogExclusions(ctx context.Context, actor Actor, libraryID uint, page, pageSize int) (CatalogExclusionPage, error) {
	result := CatalogExclusionPage{List: []CatalogExclusionItem{}, Page: page, PageSize: pageSize}
	if page < 1 || page > 100_000 || pageSize < 1 || pageSize > 100 {
		return result, appError(CodeInvalidRequest, "已移出作品分页参数无效", nil)
	}
	err := s.withCatalogReadTx(ctx, func(tx *gorm.DB) error {
		ids, err := s.authorizedMediaLibraryIDsTx(tx, actor, authz.PermissionMediaLibrariesRead, false)
		if err != nil {
			return err
		}
		if libraryID != 0 {
			allowed := false
			for _, id := range ids {
				allowed = allowed || id == libraryID
			}
			if !allowed {
				return appError(CodePermissionDenied, "无权查看这个媒体库", nil)
			}
			ids = []uint{libraryID}
		}
		if len(ids) == 0 {
			return nil
		}
		query := tx.Model(&models.MediaCatalogExclusion{}).
			Joins("JOIN media_libraries ON media_libraries.id=media_catalog_exclusions.library_id AND media_libraries.exclusion_epoch=media_catalog_exclusions.source_epoch").
			Where("media_catalog_exclusions.library_id IN ?", ids)
		if err := query.Count(&result.Total).Error; err != nil {
			return err
		}
		var rows []models.MediaCatalogExclusion
		if err := query.Order("media_catalog_exclusions.created_at DESC, media_catalog_exclusions.id DESC").
			Limit(pageSize).Offset((page - 1) * pageSize).Find(&rows).Error; err != nil {
			return err
		}
		names := make(map[uint]string)
		var libraries []models.MediaLibrary
		if err := tx.Select("id,name").Where("id IN ?", ids).Find(&libraries).Error; err != nil {
			return err
		}
		for _, library := range libraries {
			names[library.ID] = library.Name
		}
		for _, row := range rows {
			result.List = append(result.List, catalogExclusionItem(row, names[row.LibraryID]))
		}
		result.HasMore = int64((page-1)*pageSize+len(rows)) < result.Total
		return nil
	})
	return result, err
}

func (s *MediaLibraryService) RestoreCatalogExclusion(ctx context.Context, actor Actor, libraryID uint, exclusionID string, request RequestContext) error {
	if !actor.CanResource(authz.PermissionMediaLibrariesScan, models.AuthorizationResourceMediaLibrary, uintID(libraryID)) {
		return appError(CodePermissionDenied, "无权恢复这个媒体库的作品", nil)
	}
	if _, err := uuid.Parse(exclusionID); err != nil {
		return appError(CodeInvalidRequest, "已移出作品标识无效", nil)
	}
	var revision uint64
	var artifactGeneration uint64
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := s.ensureMediaLibraryReadableTx(tx, actor, libraryID); err != nil {
			return err
		}
		var library models.MediaLibrary
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&library, libraryID).Error; err != nil {
			return err
		}
		var storage models.Storage
		if err := tx.First(&storage, library.StorageID).Error; err != nil {
			return err
		}
		var row models.MediaCatalogExclusion
		if err := tx.Where("id=? AND library_id=? AND source_epoch=?", exclusionID, libraryID, library.ExclusionEpoch).First(&row).Error; err != nil {
			return appError(CodeNotFound, "已移出作品不存在", err)
		}
		if row.SourceFingerprint != catalogSourceFingerprint(library, storage) {
			return appError(CodeConflict, "媒体库来源已变化", nil)
		}
		artifactGeneration = library.ArtifactGeneration
		if err := tx.Where("exclusion_id=?", row.ID).Delete(&models.MediaCatalogExclusionMember{}).Error; err != nil {
			return err
		}
		if err := tx.Delete(&row).Error; err != nil {
			return err
		}
		if s.changes != nil {
			change, err := s.changes.RecordTx(tx, libraryID, library.DirtyGeneration, models.MediaLibraryChangeCatalog, true)
			if err != nil {
				return err
			}
			revision = change.Revision
		}
		if s.audit != nil {
			return s.audit.Record(tx, &actor.User.ID, "media_catalog.restore", "media_library", strconv.FormatUint(uint64(libraryID), 10), "success", map[string]any{"work_hash": catalogWorkHash(row.WorkKey)}, request)
		}
		return nil
	})
	if err == nil && revision > 0 && s.changes != nil {
		s.changes.NotifyCommitted(libraryID, revision)
	}
	if err == nil && revision > 0 && s.artifacts != nil && artifactGeneration > 0 {
		if refreshErr := s.artifacts.RefreshGeneration(libraryID, artifactGeneration); refreshErr != nil {
			s.log.Warn().Err(refreshErr).Uint("library_id", libraryID).Msg("恢复作品后暂无法刷新托管产物，等待下一次完整扫描")
		}
	}
	return err
}
