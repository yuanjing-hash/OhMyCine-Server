package services

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

const (
	maxPlayerMediaStateItems       = 1000
	maxPlayerMediaStateCollections = 500
)

type PlayerMediaStateService struct {
	db        *gorm.DB
	libraries *MediaLibraryService
	reader    *CatalogReader
}

// Only the lightweight state reader is request-scoped; never copy the library
// service, which owns mutexes and running supervisors. Nested reads keep the
// caller's transaction and exact catalog heads.
func readPlayerMediaState[T any](s *PlayerMediaStateService, actor Actor, read func(*PlayerMediaStateService) (T, error)) (T, error) {
	var result T
	if s == nil || s.libraries == nil {
		return result, appError(CodeInvalidRequest, "Server 暂不支持媒体状态", nil)
	}
	if !actor.HasPermission(authz.PermissionMediaLibrariesRead) {
		return result, appError(CodePermissionDenied, "无权查看媒体库", nil)
	}
	if s.reader != nil {
		return read(s)
	}
	err := s.libraries.withCatalogReadTx(context.Background(), func(tx *gorm.DB) error {
		ids, err := s.libraries.authorizedMediaLibraryIDsTx(tx, actor, authz.PermissionMediaLibrariesRead, true)
		if err != nil {
			return err
		}
		reader, err := PinCatalogTx(tx, ids)
		if err != nil {
			return err
		}
		result, err = read(&PlayerMediaStateService{db: tx, libraries: s.libraries, reader: reader})
		return err
	})
	return result, err
}

func validateMediaStateWorkTx(tx *gorm.DB, actor Actor, libraryID uint, workKey string) error {
	if err := ensurePlayerMediaLibraryReadableTx(tx, actor, libraryID); err != nil {
		return err
	}
	reader, err := PinCatalogTx(tx, []uint{libraryID})
	if err != nil {
		return err
	}
	var entryID uint
	if err := reader.VisibleEntries().Select("id").Where("library_id = ? AND work_key = ?", libraryID, workKey).Limit(1).Scan(&entryID).Error; err != nil {
		return err
	}
	if entryID == 0 {
		return appError(CodeNotFound, "媒体作品不存在", gorm.ErrRecordNotFound)
	}
	return nil
}

type PlayerCollectionSummary struct {
	// Revision is the mutable collection/owner CAS version, not a complete
	// automatic-membership cache key. Catalog media-change revisions invalidate
	// automatic projections; physical snapshot compaction must not invent one.
	Revision     uint64 `json:"revision"`
	ID           string `json:"id"`
	Name         string `json:"name"`
	Kind         string `json:"kind"`
	Source       string `json:"source"`
	ItemCount    int    `json:"item_count"`
	PosterPath   string `json:"poster_path,omitempty"`
	BackdropPath string `json:"backdrop_path,omitempty"`
	PosterURL    string `json:"poster_url,omitempty"`
	BackdropURL  string `json:"backdrop_url,omitempty"`
}

func NewPlayerMediaStateService(db *gorm.DB, libraries *MediaLibraryService) *PlayerMediaStateService {
	return &PlayerMediaStateService{db: db, libraries: libraries}
}

func playerMediaStateItemID(libraryID uint, workKey string) string {
	return strconv.FormatUint(uint64(libraryID), 10) + ":" + encodeCatalogToken(workKey)
}

func parsePlayerMediaStateItemID(value string) (uint, string, error) {
	if value == "" || len(value) > 320 {
		return 0, "", appError(CodeInvalidRequest, "媒体作品标识无效", nil)
	}
	parts := strings.SplitN(value, ":", 2)
	parsed, err := strconv.ParseUint(parts[0], 10, 32)
	if err != nil || parsed == 0 || len(parts) != 2 {
		return 0, "", appError(CodeInvalidRequest, "媒体作品标识无效", err)
	}
	workKey, err := decodeCatalogToken(parts[1])
	if err != nil {
		return 0, "", err
	}
	return uint(parsed), workKey, nil
}

func (s *PlayerMediaStateService) resolveItem(actor Actor, itemID string) (uint, string, PlayerMediaItem, error) {
	if s == nil || s.libraries == nil {
		return 0, "", PlayerMediaItem{}, appError(CodeInvalidRequest, "Server 暂不支持媒体状态", nil)
	}
	libraryID, workKey, err := parsePlayerMediaStateItemID(itemID)
	if err != nil {
		return 0, "", PlayerMediaItem{}, err
	}
	item, err := s.item(actor, libraryID, workKey)
	if err != nil {
		return 0, "", PlayerMediaItem{}, err
	}
	return libraryID, workKey, item, nil
}

func (s *PlayerMediaStateService) item(actor Actor, libraryID uint, workKey string) (PlayerMediaItem, error) {
	if s.reader == nil {
		return readPlayerMediaState(s, actor, func(view *PlayerMediaStateService) (PlayerMediaItem, error) {
			return view.item(actor, libraryID, workKey)
		})
	}
	if err := ensurePlayerMediaLibraryReadableTx(s.db, actor, libraryID); err != nil {
		return PlayerMediaItem{}, err
	}
	detail, err := s.libraries.catalogDetailTx(s.db, s.reader, actor, libraryID, encodeCatalogToken(workKey))
	if err != nil {
		return PlayerMediaItem{}, err
	}
	return s.libraries.playerMediaItemTx(s.db, s.reader, libraryID, detail.Work)
}

func (s *PlayerMediaStateService) SetFavorite(actor Actor, itemID string, favorite bool) (bool, error) {
	libraryID, workKey, err := parsePlayerMediaStateItemID(itemID)
	if err != nil {
		return false, err
	}
	err = s.db.Transaction(func(tx *gorm.DB) error {
		if err := validateMediaStateWorkTx(tx, actor, libraryID, workKey); err != nil {
			return err
		}
		if !favorite {
			return tx.Where("user_id = ? AND library_id = ? AND work_key = ?", actor.User.ID, libraryID, workKey).Delete(&models.PlayerMediaFavorite{}).Error
		}
		var existing models.PlayerMediaFavorite
		err := tx.Where("user_id = ? AND library_id = ? AND work_key = ?", actor.User.ID, libraryID, workKey).First(&existing).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			now := time.Now().UTC()
			return tx.Create(&models.PlayerMediaFavorite{UserID: actor.User.ID, LibraryID: libraryID, WorkKey: workKey, CreatedAt: now, UpdatedAt: now}).Error
		}
		return err
	})
	return favorite && err == nil, err
}

func (s *PlayerMediaStateService) FavoriteState(actor Actor, itemID string) (bool, error) {
	if s.reader == nil {
		return readPlayerMediaState(s, actor, func(view *PlayerMediaStateService) (bool, error) { return view.FavoriteState(actor, itemID) })
	}
	libraryID, workKey, _, err := s.resolveItem(actor, itemID)
	if err != nil {
		return false, err
	}
	var count int64
	if err := s.db.Model(&models.PlayerMediaFavorite{}).Where("user_id = ? AND library_id = ? AND work_key = ?", actor.User.ID, libraryID, workKey).Count(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}

func (s *PlayerMediaStateService) Favorites(actor Actor) ([]PlayerMediaItem, error) {
	if s.reader == nil {
		return readPlayerMediaState(s, actor, func(view *PlayerMediaStateService) ([]PlayerMediaItem, error) { return view.Favorites(actor) })
	}
	var rows []models.PlayerMediaFavorite
	if err := s.db.Where("user_id = ?", actor.User.ID).Order("updated_at DESC").Limit(maxPlayerMediaStateItems).Find(&rows).Error; err != nil {
		return nil, err
	}
	result := make([]PlayerMediaItem, 0, len(rows))
	for _, row := range rows {
		item, err := s.item(actor, row.LibraryID, row.WorkKey)
		if err == nil {
			result = append(result, item)
		} else if ErrorCode(err) != CodeNotFound && ErrorCode(err) != CodePermissionDenied {
			return nil, err
		}
	}
	return result, nil
}

func (s *PlayerMediaStateService) BrowserFavorites(actor Actor) ([]BrowserMediaItem, error) {
	items, err := s.Favorites(actor)
	if err != nil {
		return nil, err
	}
	return browserMediaItems(items), nil
}

func normalizePlayerCollectionInput(name, kind string) (string, string, error) {
	name = strings.TrimSpace(name)
	kind = strings.TrimSpace(kind)
	if name == "" || utf8.RuneCountInString(name) > 128 || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return "", "", appError(CodeInvalidRequest, "合集名称无效", nil)
	}
	if kind != models.PlayerMediaCollectionKindCollection && kind != models.PlayerMediaCollectionKindPlaylist {
		return "", "", appError(CodeInvalidRequest, "合集类型无效", nil)
	}
	return name, kind, nil
}

func (s *PlayerMediaStateService) CreateCollection(actor Actor, name, kind string) (models.PlayerMediaCollection, error) {
	name, kind, err := normalizePlayerCollectionInput(name, kind)
	if err != nil {
		return models.PlayerMediaCollection{}, err
	}
	now := time.Now().UTC()
	ownerID := actor.User.ID
	row := models.PlayerMediaCollection{ID: uuid.NewString(), OwnerID: &ownerID, Source: models.PlayerMediaCollectionSourceManual, Kind: kind, Name: name, Visible: true, Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := s.db.Create(&row).Error; err != nil {
		return models.PlayerMediaCollection{}, err
	}
	return row, nil
}

func (s *PlayerMediaStateService) readableCollection(actor Actor, id string, requireOwner bool) (models.PlayerMediaCollection, error) {
	if _, err := uuid.Parse(id); err != nil {
		return models.PlayerMediaCollection{}, appError(CodeInvalidRequest, "合集标识无效", err)
	}
	var row models.PlayerMediaCollection
	query := s.db.Model(&models.PlayerMediaCollection{})
	if !requireOwner && s.reader != nil {
		var err error
		query, err = s.readableCollectionRows(actor)
		if err != nil {
			return row, err
		}
	}
	if err := query.Where("id = ?", id).Take(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return models.PlayerMediaCollection{}, appError(CodeNotFound, "合集不存在", err)
		}
		return models.PlayerMediaCollection{}, err
	}
	owned := row.OwnerID != nil && *row.OwnerID == actor.User.ID && row.Source == models.PlayerMediaCollectionSourceManual
	if requireOwner && !owned {
		return models.PlayerMediaCollection{}, appError(CodePermissionDenied, "无权修改合集", nil)
	}
	if !owned && (row.Source != models.PlayerMediaCollectionSourceTMDB || !row.Visible) {
		return models.PlayerMediaCollection{}, appError(CodeNotFound, "合集不存在", nil)
	}
	return row, nil
}

func (s *PlayerMediaStateService) Collections(actor Actor, kind string) ([]PlayerCollectionSummary, error) {
	if s.reader == nil {
		return readPlayerMediaState(s, actor, func(view *PlayerMediaStateService) ([]PlayerCollectionSummary, error) {
			return view.Collections(actor, kind)
		})
	}
	if kind != "" && kind != models.PlayerMediaCollectionKindCollection && kind != models.PlayerMediaCollectionKindPlaylist {
		return nil, appError(CodeInvalidRequest, "合集类型无效", nil)
	}
	query, err := s.readableCollectionRows(actor)
	if err != nil {
		return nil, err
	}
	if kind != "" {
		query = query.Where("kind = ?", kind)
	}
	var rows []struct {
		models.PlayerMediaCollection
		ItemCount int
	}
	if err := query.Order("source, name, id").Limit(maxPlayerMediaStateCollections).Find(&rows).Error; err != nil {
		return nil, err
	}
	result := make([]PlayerCollectionSummary, 0, len(rows))
	imageClient := s.libraries.catalogImageClientTx(s.db)
	for _, row := range rows {
		result = append(result, PlayerCollectionSummary{ID: row.ID, Name: row.Name, Kind: row.Kind, Source: row.Source, ItemCount: row.ItemCount, Revision: row.Revision, PosterPath: row.PosterPath, BackdropPath: row.BackdropPath, PosterURL: playerCatalogImageURLWithClient(imageClient, row.PosterPath, "w500"), BackdropURL: playerCatalogImageURLWithClient(imageClient, row.BackdropPath, "w1280")})
	}
	return result, nil
}

func (s *PlayerMediaStateService) BrowserCollections(actor Actor, kind string) ([]BrowserCollectionSummary, error) {
	items, err := s.Collections(actor, kind)
	if err != nil {
		return nil, err
	}
	return browserCollections(items), nil
}

func (s *PlayerMediaStateService) collectionItems(actor Actor, collectionID string) ([]PlayerMediaItem, error) {
	collection, err := s.readableCollection(actor, collectionID, false)
	if err != nil {
		return nil, err
	}
	query, err := s.readableCollectionMemberRows(actor, collection)
	if err != nil {
		return nil, err
	}
	var rows []models.PlayerMediaCollectionItem
	if err := query.Order("ordinal, id, library_id, work_key").Limit(maxPlayerMediaStateItems).Scan(&rows).Error; err != nil {
		return nil, err
	}
	result := make([]PlayerMediaItem, 0, len(rows))
	seen := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		key := strconv.FormatUint(uint64(row.LibraryID), 10) + "\x00" + row.WorkKey
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		item, err := s.item(actor, row.LibraryID, row.WorkKey)
		if err == nil {
			seen[key] = struct{}{}
			result = append(result, item)
		} else if ErrorCode(err) != CodeNotFound && ErrorCode(err) != CodePermissionDenied {
			return nil, err
		}
	}
	return result, nil
}

func (s *PlayerMediaStateService) CollectionItems(actor Actor, collectionID string) ([]PlayerMediaItem, error) {
	if s.reader == nil {
		return readPlayerMediaState(s, actor, func(view *PlayerMediaStateService) ([]PlayerMediaItem, error) {
			return view.CollectionItems(actor, collectionID)
		})
	}
	if _, err := s.readableCollection(actor, collectionID, false); err != nil {
		return nil, err
	}
	return s.collectionItems(actor, collectionID)
}

func (s *PlayerMediaStateService) BrowserCollectionItems(actor Actor, collectionID string) ([]BrowserMediaItem, error) {
	items, err := s.CollectionItems(actor, collectionID)
	if err != nil {
		return nil, err
	}
	return browserMediaItems(items), nil
}

func (s *PlayerMediaStateService) AddCollectionItem(actor Actor, collectionID, itemID string) error {
	libraryID, workKey, err := parsePlayerMediaStateItemID(itemID)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	return s.db.Transaction(func(tx *gorm.DB) error {
		view := &PlayerMediaStateService{db: tx, libraries: s.libraries}
		collection, err := view.readableCollection(actor, collectionID, true)
		if err != nil {
			return err
		}
		if err := validateMediaStateWorkTx(tx, actor, libraryID, workKey); err != nil {
			return err
		}
		var existing models.PlayerMediaCollectionItem
		err = tx.Where("collection_id = ? AND library_id = ? AND work_key = ? AND origin = ?", collection.ID, libraryID, workKey, models.PlayerMediaCollectionItemOriginManual).First(&existing).Error
		if err == nil {
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		item := models.PlayerMediaCollectionItem{CollectionID: collection.ID, LibraryID: libraryID, WorkKey: workKey, Origin: models.PlayerMediaCollectionItemOriginManual, CreatedAt: now, UpdatedAt: now}
		if err := tx.Model(&models.PlayerMediaCollectionItem{}).Where("collection_id = ?", collection.ID).Select("COALESCE(MAX(ordinal),-1)+1").Scan(&item.Ordinal).Error; err != nil {
			return err
		}
		if err := tx.Create(&item).Error; err != nil {
			return err
		}
		return tx.Model(&models.PlayerMediaCollection{}).Where("id = ?", collection.ID).Updates(map[string]any{"revision": gorm.Expr("revision + 1"), "updated_at": now}).Error
	})
}

func (s *PlayerMediaStateService) RemoveCollectionItem(actor Actor, collectionID, itemID string) error {
	collection, err := s.readableCollection(actor, collectionID, true)
	if err != nil {
		return err
	}
	libraryID, workKey, err := parsePlayerMediaStateItemID(itemID)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	return s.db.Transaction(func(tx *gorm.DB) error {
		result := tx.Where("collection_id = ? AND library_id = ? AND work_key = ? AND origin = ?", collection.ID, libraryID, workKey, models.PlayerMediaCollectionItemOriginManual).Delete(&models.PlayerMediaCollectionItem{})
		if result.Error != nil || result.RowsAffected == 0 {
			return result.Error
		}
		return tx.Model(&models.PlayerMediaCollection{}).Where("id = ?", collection.ID).Updates(map[string]any{"revision": gorm.Expr("revision + 1"), "updated_at": now}).Error
	})
}

func (s *PlayerMediaStateService) DeleteCollection(actor Actor, collectionID string) error {
	collection, err := s.readableCollection(actor, collectionID, true)
	if err != nil {
		return err
	}
	return s.db.Delete(&collection).Error
}
