package services

import (
	"context"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/metadata/tmdb"
	"gorm.io/gorm"
)

type BrowserMediaPage[T any] struct {
	List     []T    `json:"list"`
	Total    int64  `json:"total"`
	Page     int    `json:"page"`
	PageSize int    `json:"page_size"`
	HasMore  bool   `json:"has_more"`
	Revision uint64 `json:"revision,omitempty"`
}

func validateMediaStatePage(page, size int) error {
	if page < 1 || page > 100_000 || size < 1 || size > 100 {
		return appError(CodeInvalidRequest, "媒体分页参数无效", nil)
	}
	return nil
}

// The table identifier is a constant supplied only by the service. Existence,
// enabled storage/library and actor scope are applied before count/pagination.
func (s *PlayerMediaStateService) readableStateQuery(actor Actor, table string) (*gorm.DB, error) {
	if !actor.HasPermission(authz.PermissionMediaLibrariesRead) {
		return nil, appError(CodePermissionDenied, "无权查看媒体库", nil)
	}
	if s.reader == nil {
		return nil, ErrCatalogInvalid
	}
	ids, err := s.libraries.authorizedMediaLibraryIDsTx(s.db, actor, authz.PermissionMediaLibrariesRead, true)
	if err != nil {
		return nil, err
	}
	active := s.db.Table("media_libraries l").Select("l.id").Joins("JOIN storages st ON st.id = l.storage_id").Where("l.id IN ? AND l.enabled = ? AND st.enabled = ?", ids, true, true)
	exists := s.reader.VisibleEntries().Select("1").Where("media_library_entries.library_id = " + table + ".library_id AND media_library_entries.work_key = " + table + ".work_key")
	return s.db.Table(table).Where(table+".library_id IN (?)", active).Where("EXISTS (?)", exists), nil
}

func (s *PlayerMediaStateService) FavoritePage(actor Actor, page, size int) (BrowserMediaPage[BrowserMediaItem], error) {
	if s.reader == nil {
		return readPlayerMediaState(s, actor, func(view *PlayerMediaStateService) (BrowserMediaPage[BrowserMediaItem], error) {
			return view.FavoritePage(actor, page, size)
		})
	}
	result := BrowserMediaPage[BrowserMediaItem]{List: []BrowserMediaItem{}, Page: page, PageSize: size}
	if err := validateMediaStatePage(page, size); err != nil {
		return result, err
	}
	query, err := s.readableStateQuery(actor, "player_media_favorites")
	if err != nil {
		return result, err
	}
	query = query.Where("user_id = ?", actor.User.ID)
	if err := query.Count(&result.Total).Error; err != nil {
		return result, err
	}
	var rows []models.PlayerMediaFavorite
	if err := query.Order("updated_at DESC, library_id, work_key").Limit(size).Offset((page - 1) * size).Find(&rows).Error; err != nil {
		return result, err
	}
	keys := make([]mediaStateWork, 0, len(rows))
	for _, row := range rows {
		keys = append(keys, mediaStateWork{row.LibraryID, row.WorkKey})
	}
	result.List, err = s.browserStateItems(keys)
	if err != nil {
		return result, err
	}
	result.HasMore = int64(page*size) < result.Total
	return result, nil
}

func (s *PlayerMediaStateService) CollectionItemPage(actor Actor, id string, page, size int) (BrowserMediaPage[BrowserMediaItem], error) {
	if s.reader == nil {
		return readPlayerMediaState(s, actor, func(view *PlayerMediaStateService) (BrowserMediaPage[BrowserMediaItem], error) {
			return view.CollectionItemPage(actor, id, page, size)
		})
	}
	result := BrowserMediaPage[BrowserMediaItem]{List: []BrowserMediaItem{}, Page: page, PageSize: size}
	if err := validateMediaStatePage(page, size); err != nil {
		return result, err
	}
	collection, err := s.readableCollection(actor, id, false)
	if err != nil {
		return result, err
	}
	result.Revision = collection.Revision
	query, err := s.readableCollectionMemberRows(actor, collection)
	if err != nil {
		return result, err
	}
	query = query.Where("collection_id = ?", id)
	// Origin is not media identity: collapse any legacy mixed-origin rows.
	grouped := query.Select("library_id, work_key, MIN(ordinal) AS ordinal, MIN(id) AS id").Group("library_id, work_key")
	if err := s.db.Table("(?) AS visible", grouped).Count(&result.Total).Error; err != nil {
		return result, err
	}
	var rows []models.PlayerMediaCollectionItem
	if err := grouped.Order("ordinal, id, library_id, work_key").Limit(size).Offset((page - 1) * size).Scan(&rows).Error; err != nil {
		return result, err
	}
	keys := make([]mediaStateWork, 0, len(rows))
	for _, row := range rows {
		keys = append(keys, mediaStateWork{row.LibraryID, row.WorkKey})
	}
	result.List, err = s.browserStateItems(keys)
	if err != nil {
		return result, err
	}
	result.HasMore = int64(page*size) < result.Total
	return result, nil
}

type mediaStateWork struct {
	libraryID uint
	workKey   string
}

// Keys come only from the already-authorized, bounded page above. Read card
// aggregates and the latest recognition in two queries, not full file/season/
// transfer details per card. Do not merge identical work keys across libraries.
func (s *PlayerMediaStateService) browserStateItems(keys []mediaStateWork) ([]BrowserMediaItem, error) {
	result := make([]BrowserMediaItem, 0, len(keys))
	if len(keys) == 0 {
		return result, nil
	}
	if len(keys) > 100 {
		return nil, appError(CodeInvalidRequest, "媒体分页参数无效", nil)
	}
	if s.reader == nil {
		ids := make([]uint, 0, len(keys))
		for _, key := range keys {
			ids = append(ids, key.libraryID)
		}
		err := s.libraries.withCatalogRead(context.Background(), ids, func(tx *gorm.DB, reader *CatalogReader) error {
			var err error
			result, err = (&PlayerMediaStateService{db: tx, libraries: s.libraries, reader: reader}).browserStateItems(keys)
			return err
		})
		return result, err
	}
	pairs := make([][]any, 0, len(keys))
	for _, key := range keys {
		pairs = append(pairs, []any{key.libraryID, key.workKey})
	}
	var rows []mediaCatalogRow
	if err := selectCatalogRows(s.reader.VisibleEntries().Where("(library_id, work_key) IN ?", pairs)).Scan(&rows).Error; err != nil {
		return nil, err
	}
	byKey := make(map[mediaStateWork]mediaCatalogRow, len(rows))
	for _, row := range rows {
		byKey[mediaStateWork{row.LibraryID, row.WorkKey}] = row
	}
	// DISTINCT avoids ranking the same recognition once per episode/version.
	unique := s.db.Table("(?) AS e", s.reader.VisibleEntries()).Select("DISTINCT e.library_id,e.work_key,r.id,r.updated_at,r.metadata_json").Joins("JOIN (?) AS r ON r.id = e.recognition_id AND r.library_id = e.library_id", s.reader.Recognitions()).Where("(e.library_id,e.work_key) IN ?", pairs)
	ranked := s.db.Table("(?) AS recognition", unique).Select("*, ROW_NUMBER() OVER (PARTITION BY library_id,work_key ORDER BY updated_at DESC,id DESC) AS rank")
	var metadata []struct {
		LibraryID    uint
		WorkKey      string
		MetadataJSON string
	}
	if err := s.db.Table("(?) AS latest", ranked).Where("rank = 1").Scan(&metadata).Error; err != nil {
		return nil, err
	}
	snapshots := make(map[mediaStateWork]tmdb.Snapshot, len(metadata))
	for _, record := range metadata {
		if record.MetadataJSON == "" || record.MetadataJSON == "{}" {
			continue
		}
		_, snapshot, err := decodeRecognitionMetadata(record.MetadataJSON)
		if err != nil {
			return nil, err
		}
		snapshots[mediaStateWork{record.LibraryID, record.WorkKey}] = snapshot
	}
	imageClient := s.libraries.catalogImageClientTx(s.db)
	for _, key := range keys {
		row, exists := byKey[key]
		if !exists {
			return nil, appError(CodeConflict, "媒体库已变化，请刷新后重试", nil)
		}
		item, snapshot := catalogItem(row), snapshots[key]
		result = append(result, BrowserMediaItem{LibraryID: key.libraryID, WorkID: item.ID, Title: item.Title, Kind: item.Kind, ReleaseYear: item.ReleaseYear, Rating: snapshot.VoteAverage,
			PosterURL: catalogImageURLWithClient(imageClient, snapshot.PosterPath, "w500"), BackdropURL: catalogImageURLWithClient(imageClient, snapshot.BackdropPath, "w1280"), SeasonCount: item.SeasonCount, EpisodeCount: item.EpisodeCount, CategoryName: item.CategoryName, ModifiedAt: item.ModifiedAt})
	}
	return result, nil
}

func (s *PlayerMediaStateService) CollectionPage(actor Actor, kind, source string, page, size int) (BrowserMediaPage[BrowserCollectionSummary], error) {
	if s.reader == nil {
		return readPlayerMediaState(s, actor, func(view *PlayerMediaStateService) (BrowserMediaPage[BrowserCollectionSummary], error) {
			return view.CollectionPage(actor, kind, source, page, size)
		})
	}
	result := BrowserMediaPage[BrowserCollectionSummary]{List: []BrowserCollectionSummary{}, Page: page, PageSize: size}
	if err := validateMediaStatePage(page, size); err != nil {
		return result, err
	}
	if kind != "" && kind != "collection" && kind != "playlist" || source != "" && source != "manual" && source != "tmdb" {
		return result, appError(CodeInvalidRequest, "合集筛选无效", nil)
	}
	query, err := s.readableCollectionRows(actor)
	if err != nil {
		return result, err
	}
	if kind != "" {
		query = query.Where("kind = ?", kind)
	}
	if source != "" {
		query = query.Where("source = ?", source)
	}
	if err := query.Count(&result.Total).Error; err != nil {
		return result, err
	}
	var rows []struct {
		models.PlayerMediaCollection
		ItemCount int
	}
	if err := query.Order("source, name, id").Limit(size).Offset((page - 1) * size).Scan(&rows).Error; err != nil {
		return result, err
	}
	imageClient := s.libraries.catalogImageClientTx(s.db)
	for _, row := range rows {
		result.List = append(result.List, BrowserCollectionSummary{ID: row.ID, Name: row.Name, Kind: row.Kind, Source: row.Source, ItemCount: row.ItemCount, Revision: row.Revision, PosterURL: catalogImageURLWithClient(imageClient, row.PosterPath, "w500"), BackdropURL: catalogImageURLWithClient(imageClient, row.BackdropPath, "w1280")})
	}
	result.HasMore = int64(page*size) < result.Total
	return result, nil
}

// Rename and one-item relocation use revision CAS. Moving relative to a known
// neighbor works for arbitrarily long collections without uploading every ID.
func (s *PlayerMediaStateService) RenameCollection(actor Actor, id, name string, revision uint64) error {
	collection, err := s.readableCollection(actor, id, true)
	if err != nil {
		return err
	}
	name, _, err = normalizePlayerCollectionInput(name, collection.Kind)
	if err != nil {
		return err
	}
	result := s.db.Model(&models.PlayerMediaCollection{}).Where("id = ? AND owner_id = ? AND source = ? AND revision = ?", id, actor.User.ID, "manual", revision).
		Updates(map[string]any{"name": name, "revision": gorm.Expr("revision + 1"), "updated_at": time.Now().UTC()})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return appError(CodeConflict, "合集已变化，请刷新后重试", nil)
	}
	return nil
}

func (s *PlayerMediaStateService) MoveCollectionItem(actor Actor, id, itemID, beforeID string, revision uint64) error {
	if _, err := s.readableCollection(actor, id, true); err != nil {
		return err
	}
	libraryID, work, err := parsePlayerMediaStateItemID(itemID)
	if err != nil {
		return err
	}
	var beforeLibrary uint
	var beforeWork string
	if beforeID != "" {
		beforeLibrary, beforeWork, err = parsePlayerMediaStateItemID(beforeID)
		if err != nil {
			return err
		}
	}
	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := validateMediaStateWorkTx(tx, actor, libraryID, work); err != nil {
			return err
		}
		if beforeID != "" {
			if err := validateMediaStateWorkTx(tx, actor, beforeLibrary, beforeWork); err != nil {
				return err
			}
		}
		result := tx.Model(&models.PlayerMediaCollection{}).Where("id = ? AND owner_id = ? AND source = ? AND revision = ?", id, actor.User.ID, "manual", revision).Updates(map[string]any{"revision": gorm.Expr("revision + 1"), "updated_at": time.Now().UTC()})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return appError(CodeConflict, "合集已变化，请刷新后重试", nil)
		}
		// Normalize legacy tied ordinals in one statement, not one UPDATE per
		// member or an unbounded array of metadata in the request/Go heap.
		if err := tx.Exec(`WITH ranked AS (SELECT id, ROW_NUMBER() OVER (ORDER BY ordinal,id)-1 AS n FROM player_media_collection_items WHERE collection_id = ? AND origin = 'manual') UPDATE player_media_collection_items SET ordinal = ranked.n FROM ranked WHERE player_media_collection_items.id = ranked.id AND player_media_collection_items.ordinal <> ranked.n`, id).Error; err != nil {
			return err
		}
		var moved models.PlayerMediaCollectionItem
		members := tx.Model(&models.PlayerMediaCollectionItem{}).Where("collection_id = ? AND origin = ?", id, "manual")
		if err := members.Session(&gorm.Session{}).Where("library_id = ? AND work_key = ?", libraryID, work).First(&moved).Error; err != nil {
			return appError(CodeNotFound, "合集成员不存在", err)
		}
		to := 0
		if beforeID == "" {
			if err := members.Select("COALESCE(MAX(ordinal),-1)+1").Scan(&to).Error; err != nil {
				return err
			}
		} else {
			var before models.PlayerMediaCollectionItem
			if err := members.Session(&gorm.Session{}).Where("library_id = ? AND work_key = ?", beforeLibrary, beforeWork).First(&before).Error; err != nil {
				return appError(CodeNotFound, "合集成员不存在", err)
			}
			to = before.Ordinal
		}
		if to > moved.Ordinal {
			to--
		}
		if to == moved.Ordinal {
			return nil
		}
		if to < moved.Ordinal {
			if err := members.Session(&gorm.Session{}).Where("ordinal >= ? AND ordinal < ?", to, moved.Ordinal).Update("ordinal", gorm.Expr("ordinal + 1")).Error; err != nil {
				return err
			}
		} else {
			if err := members.Session(&gorm.Session{}).Where("ordinal > ? AND ordinal <= ?", moved.Ordinal, to).Update("ordinal", gorm.Expr("ordinal - 1")).Error; err != nil {
				return err
			}
		}
		return tx.Model(&models.PlayerMediaCollectionItem{}).Where("id = ?", moved.ID).Update("ordinal", to).Error
	})
}
