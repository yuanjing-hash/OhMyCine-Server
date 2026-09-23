package services

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/classification"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/medialibrary"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	storagefs "github.com/yuanjing-hash/OhMyCine-Server/internal/storage"
	cloudpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/metadata/releaseversion"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/metadata/tmdb"
	"gorm.io/gorm"
)

type PlayerMediaLibrary struct {
	ID              uint       `json:"id"`
	Name            string     `json:"name"`
	StorageType     string     `json:"storage_type"`
	SortOrder       int        `json:"sort_order"`
	Status          string     `json:"status"`
	EntryCount      int64      `json:"entry_count"`
	WorkCount       int64      `json:"work_count"`
	DirectStream    bool       `json:"direct_stream"`
	STRMEnabled     bool       `json:"strm_enabled"`
	ArtworkURL      string     `json:"artwork_url,omitempty"`
	ArtworkRevision string     `json:"artwork_revision,omitempty"`
	ArtworkSource   string     `json:"artwork_source,omitempty"`
	LastSuccessful  *time.Time `json:"last_successful_scan_at,omitempty"`
}

type PlayerMediaCategory struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	MediaType       string `json:"media_type"`
	ItemCount       int64  `json:"item_count"`
	SortOrder       int    `json:"sort_order"`
	ArtworkURL      string `json:"artwork_url,omitempty"`
	ArtworkRevision string `json:"artwork_revision,omitempty"`
	ArtworkSource   string `json:"artwork_source,omitempty"`
}

type PlayerMediaIdentity struct {
	Scheme    string `json:"scheme"`
	MediaType string `json:"media_type"`
	Value     string `json:"value"`
}

type PlayerMediaPerson struct {
	TMDBID      int64  `json:"tmdb_id,omitempty"`
	Name        string `json:"name"`
	Role        string `json:"role,omitempty"`
	Character   string `json:"character,omitempty"`
	ProfilePath string `json:"profile_path,omitempty"`
	ProfileURL  string `json:"profile_url,omitempty"`
}

type PlayerMediaItem struct {
	ID              string              `json:"id"`
	ItemToken       string              `json:"item_token"`
	HistoryIdentity string              `json:"history_identity,omitempty"`
	LibraryID       uint                `json:"library_id"`
	Title           string              `json:"title"`
	OriginalTitle   string              `json:"original_title,omitempty"`
	Kind            string              `json:"kind"`
	ReleaseYear     *int                `json:"release_year,omitempty"`
	Overview        string              `json:"overview,omitempty"`
	Tagline         string              `json:"tagline,omitempty"`
	Rating          float64             `json:"rating,omitempty"`
	RuntimeMinutes  int                 `json:"runtime_minutes,omitempty"`
	Genres          []string            `json:"genres,omitempty"`
	Directors       []string            `json:"directors,omitempty"`
	Writers         []string            `json:"writers,omitempty"`
	Cast            []string            `json:"cast,omitempty"`
	People          []PlayerMediaPerson `json:"people,omitempty"`
	TMDBID          int64               `json:"tmdb_id,omitempty"`
	IMDbID          string              `json:"imdb_id,omitempty"`
	PosterPath      string              `json:"poster_path,omitempty"`
	BackdropPath    string              `json:"backdrop_path,omitempty"`
	PosterURL       string              `json:"poster_url,omitempty"`
	BackdropURL     string              `json:"backdrop_url,omitempty"`
	StillPaths      []string            `json:"still_paths,omitempty"`
	StillURLs       []string            `json:"still_urls,omitempty"`
	WorkIdentity    PlayerMediaIdentity `json:"work_identity"`
	FileCount       int64               `json:"file_count"`
	SeasonCount     int64               `json:"season_count"`
	EpisodeCount    int64               `json:"episode_count"`
	ModifiedAt      time.Time           `json:"modified_at"`
	CategoryName    string              `json:"category_name"`
	MatchStatus     string              `json:"match_status"`
}

type PlayerMediaItemPage struct {
	List     []PlayerMediaItem `json:"list"`
	Total    int               `json:"total"`
	Page     int               `json:"page"`
	PageSize int               `json:"page_size"`
}

type PlayerMediaVersion struct {
	ID               uint      `json:"id"`
	ItemToken        string    `json:"item_token"`
	HistoryIdentity  string    `json:"history_identity"`
	Title            string    `json:"title"`
	VersionName      string    `json:"version_name,omitempty"`
	DisplayTitle     string    `json:"display_title"`
	DisplaySubtitle  string    `json:"display_subtitle,omitempty"`
	SeriesTitle      string    `json:"series_title,omitempty"`
	EpisodeTitle     string    `json:"episode_title,omitempty"`
	Season           *int      `json:"season,omitempty"`
	Episode          *int      `json:"episode,omitempty"`
	Overview         string    `json:"overview,omitempty"`
	StillPath        string    `json:"still_path,omitempty"`
	StillURL         string    `json:"still_url,omitempty"`
	PosterPath       string    `json:"poster_path,omitempty"`
	PosterURL        string    `json:"poster_url,omitempty"`
	BackdropPath     string    `json:"backdrop_path,omitempty"`
	BackdropURL      string    `json:"backdrop_url,omitempty"`
	EpisodeStillPath string    `json:"episode_still_path,omitempty"`
	EpisodeStillURL  string    `json:"episode_still_url,omitempty"`
	AirDate          string    `json:"air_date,omitempty"`
	RuntimeMinutes   int       `json:"runtime_minutes,omitempty"`
	Rating           float64   `json:"rating,omitempty"`
	Size             int64     `json:"size"`
	ModifiedAt       time.Time `json:"modified_at"`
	Playable         bool      `json:"playable"`
	StreamPath       string    `json:"stream_path,omitempty"`
	DeliveryKind     string    `json:"delivery_kind,omitempty"`
	ExactIdentity    string    `json:"exact_identity"`
}

type PlayerMediaDetail struct {
	Item     PlayerMediaItem      `json:"item"`
	Versions []PlayerMediaVersion `json:"versions"`
}

type PlayerEmbyInstance struct {
	ConnectionID        uint   `json:"connection_id"`
	Name                string `json:"name"`
	HealthStatus        string `json:"health_status"`
	InstanceFingerprint string `json:"instance_fingerprint"`
}

const (
	playerDeliveryServerStream   = "server_stream"
	playerDeliveryServerRedirect = "server_redirect"
	playerStreamKindLocal        = "local_file"
	playerStreamKindRedirect     = "redirect"
)

// PlayerStreamResolution is an internal request-scoped result. LocalPath is
// deliberately absent: an absolute filesystem path must never cross the API
// boundary. The handler owns and closes File after ServeContent returns.
type PlayerStreamResolution struct {
	Kind        string
	File        *os.File
	Name        string
	ModifiedAt  time.Time
	RedirectURL string
}

func PlayerStreamUnavailableError() error {
	return appError(CodeProxyTargetUnavailable, "播放目标不可用", nil)
}

func (s *MediaLibraryService) PlayerLibraries(actor Actor) ([]PlayerMediaLibrary, error) {
	var result []PlayerMediaLibrary
	err := s.withCatalogReadTx(context.Background(), func(tx *gorm.DB) error {
		var err error
		result, err = s.playerLibrariesTx(tx, actor)
		return err
	})
	return result, err
}

func (s *MediaLibraryService) playerLibrariesTx(tx *gorm.DB, actor Actor) ([]PlayerMediaLibrary, error) {
	if !actor.HasPermission(authz.PermissionMediaLibrariesRead) {
		return nil, appError(CodePermissionDenied, "无权查看媒体库", nil)
	}
	libraryIDs, err := s.authorizedMediaLibraryIDsTx(tx, actor, authz.PermissionMediaLibrariesRead, true)
	if err != nil {
		return nil, err
	}
	if len(libraryIDs) == 0 {
		return []PlayerMediaLibrary{}, nil
	}
	reader, err := PinCatalogTx(tx, libraryIDs)
	if err != nil {
		return nil, err
	}
	type row struct {
		ID                   uint
		Name                 string
		StorageType          string
		SortOrder            int
		Status               string
		STRMEnabled          bool
		SignedProxyEnabled   bool
		LastSuccessfulScanAt *time.Time
		EntryCount           int64
		WorkCount            int64
	}
	var rows []row
	err = tx.Table("media_libraries").
		Select("media_libraries.id, media_libraries.name, storages.type AS storage_type, media_libraries.sort_order, media_libraries.status, media_libraries.strm_enabled, media_libraries.signed_proxy_enabled, media_libraries.last_successful_scan_at, COUNT(media_library_entries.id) AS entry_count, COUNT(DISTINCT CASE WHEN media_library_entries.work_key <> '' THEN media_library_entries.work_key END) AS work_count").
		Joins("JOIN storages ON storages.id = media_libraries.storage_id").
		Joins("LEFT JOIN (?) AS media_library_entries ON media_library_entries.library_id = media_libraries.id", reader.Entries()).
		Where("media_libraries.enabled = ? AND storages.enabled = ? AND media_libraries.id IN ?", true, true, libraryIDs).
		Group("media_libraries.id").Order("media_libraries.sort_order, media_libraries.id").Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	result := make([]PlayerMediaLibrary, 0, len(rows))
	for _, row := range rows {
		directStream := row.StorageType == models.StorageTypeLocal || row.StorageType == models.StorageTypePan115 && row.STRMEnabled && row.SignedProxyEnabled
		artworkURL := playerLibraryArtworkURL(row.StorageType)
		result = append(result, PlayerMediaLibrary{ID: row.ID, Name: row.Name, StorageType: row.StorageType, SortOrder: row.SortOrder, Status: row.Status, EntryCount: row.EntryCount, WorkCount: row.WorkCount, DirectStream: directStream, STRMEnabled: row.STRMEnabled, ArtworkURL: artworkURL, ArtworkRevision: fallbackArtworkRevision(artworkURL), ArtworkSource: "fallback", LastSuccessful: row.LastSuccessfulScanAt})
	}
	return result, nil
}

func (s *MediaLibraryService) PlayerCategories(actor Actor, libraryID uint) ([]PlayerMediaCategory, error) {
	var result []PlayerMediaCategory
	err := s.withCatalogRead(context.Background(), []uint{libraryID}, func(tx *gorm.DB, reader *CatalogReader) error {
		var err error
		result, err = s.playerCategoriesTx(tx, reader, actor, libraryID)
		return err
	})
	return result, err
}

func (s *MediaLibraryService) playerCategoriesTx(tx *gorm.DB, reader *CatalogReader, actor Actor, libraryID uint) ([]PlayerMediaCategory, error) {
	if err := ensurePlayerMediaLibraryReadableTx(tx, actor, libraryID); err != nil {
		return nil, err
	}
	var library models.MediaLibrary
	if err := tx.Select("id", "profile_id").First(&library, libraryID).Error; err != nil {
		return nil, err
	}
	var profile models.MediaClassificationProfile
	if err := tx.Select("id", "rules_json").First(&profile, library.ProfileID).Error; err != nil {
		return nil, appError(CodeMediaLibraryProfileUnavailable, "媒体库分类规则不可用", err)
	}
	rules, err := classification.DecodeStrict([]byte(profile.RulesJSON))
	if err != nil {
		return nil, appError(CodeProfileValidation, "媒体库分类规则无效", err)
	}
	type countRow struct {
		CategoryName string
		MediaType    string
		ItemCount    int64
	}
	var rows []countRow
	if err := reader.Entries().
		Select("category_name, CASE WHEN media_type = 'tv' THEN 'series' ELSE 'movie' END AS media_type, COUNT(DISTINCT work_key) AS item_count").
		Where("library_id = ? AND work_key <> '' AND category_name <> ''", libraryID).
		Group("category_name, CASE WHEN media_type = 'tv' THEN 'series' ELSE 'movie' END").
		Order("media_type ASC, category_name ASC").
		Scan(&rows).Error; err != nil {
		return nil, err
	}
	counts := make(map[string]int64, len(rows))
	for _, row := range rows {
		counts[row.MediaType+"\x00"+row.CategoryName] += row.ItemCount
	}
	result := make([]PlayerMediaCategory, 0, len(rows)+4)
	seen := make(map[string]struct{}, len(rows)+4)
	appendCategory := func(mediaType, name string) {
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		key := mediaType + "\x00" + name
		if counts[key] <= 0 {
			return
		}
		if _, exists := seen[key]; exists {
			return
		}
		seen[key] = struct{}{}
		result = append(result, PlayerMediaCategory{
			ID: base64.RawURLEncoding.EncodeToString([]byte(key)), Name: name, MediaType: mediaType,
			ItemCount: counts[key], SortOrder: len(result), ArtworkURL: "/api/v1/assets/library-covers/category-cinema.png", ArtworkSource: "fallback",
		})
	}
	for _, group := range rules.Groups {
		mediaType := "movie"
		if group.MediaType == classification.MediaTypeTV {
			mediaType = "series"
		}
		for _, category := range group.Categories {
			appendCategory(mediaType, category.Name)
		}
		appendCategory(mediaType, group.FallbackCategoryName)
	}
	for _, row := range rows {
		appendCategory(row.MediaType, row.CategoryName)
	}
	return result, nil
}

func playerLibraryArtworkURL(storageType string) string {
	if storageType == models.StorageTypeLocal {
		return "/api/v1/assets/library-covers/library-local.png"
	}
	return "/api/v1/assets/library-covers/library-cloud.png"
}

func (s *MediaLibraryService) PlayerCatalog(actor Actor, libraryID uint, query MediaPageQuery) (PlayerMediaItemPage, error) {
	var result PlayerMediaItemPage
	err := s.withCatalogRead(context.Background(), []uint{libraryID}, func(tx *gorm.DB, reader *CatalogReader) error {
		var err error
		result, err = s.playerCatalogTx(tx, reader, actor, libraryID, query)
		return err
	})
	return result, err
}

func (s *MediaLibraryService) playerCatalogTx(tx *gorm.DB, reader *CatalogReader, actor Actor, libraryID uint, query MediaPageQuery) (PlayerMediaItemPage, error) {
	if err := ensurePlayerMediaLibraryReadableTx(tx, actor, libraryID); err != nil {
		return PlayerMediaItemPage{}, err
	}
	page, err := s.catalogTx(tx, reader, actor, libraryID, query)
	if err != nil {
		return PlayerMediaItemPage{}, err
	}
	items := make([]PlayerMediaItem, 0, len(page.List))
	for _, item := range page.List {
		projected, err := s.playerMediaItemTx(tx, reader, libraryID, item)
		if err != nil {
			return PlayerMediaItemPage{}, err
		}
		items = append(items, projected)
	}
	return PlayerMediaItemPage{List: items, Total: int(page.Total), Page: page.Page, PageSize: page.PageSize}, nil
}

func (s *MediaLibraryService) PlayerCatalogDetail(ctx context.Context, actor Actor, libraryID uint, token string) (PlayerMediaDetail, error) {
	workKey, err := decodeCatalogToken(token)
	if err != nil {
		return PlayerMediaDetail{}, err
	}
	var item PlayerMediaItem
	var streamMode string
	var entries []models.MediaLibraryEntry
	var source playerEpisodeMetadataSource
	err = s.withCatalogRead(ctx, []uint{libraryID}, func(tx *gorm.DB, reader *CatalogReader) error {
		if err := ensurePlayerMediaLibraryReadableTx(tx, actor, libraryID); err != nil {
			return err
		}
		detail, err := s.catalogDetailTx(tx, reader, actor, libraryID, token)
		if err != nil {
			return err
		}
		item, err = s.playerMediaItemTx(tx, reader, libraryID, detail.Work)
		if err != nil {
			return err
		}
		streamMode, err = playerDirectStreamModeTx(tx, libraryID)
		if err != nil {
			return err
		}
		if err := reader.Entries().Where("library_id = ? AND work_key = ?", libraryID, workKey).Order("COALESCE(season,0),COALESCE(episode,0),relative_path").Find(&entries).Error; err != nil {
			return err
		}
		source, err = playerEpisodeMetadataSourceTx(tx, reader, libraryID, workKey)
		return err
	})
	if err != nil {
		return PlayerMediaDetail{}, err
	}
	episodeMetadata := map[playerEpisodeKey]tmdb.EpisodeSnapshot{}
	if item.Kind == "series" {
		episodeMetadata = s.playerEpisodeMetadata(ctx, source, entries)
	}
	imageClient := s.catalogImageClientTx(s.db)
	versions := make([]PlayerMediaVersion, 0, len(entries))
	versionLabels := playerVersionLabels(entries)
	for _, entry := range entries {
		season, episode := resolvedCatalogEpisodeFacts(entry)
		episodeSnapshot := tmdb.EpisodeSnapshot{}
		if episode != nil {
			key := playerEpisodeKey{episode: *episode}
			if season != nil {
				key.season = *season
			}
			episodeSnapshot = episodeMetadata[key]
		}
		playable := false
		deliveryKind := ""
		exactIdentity := "server:entry:" + strconv.FormatUint(uint64(entry.ID), 10)
		switch streamMode {
		case models.StorageTypeLocal:
			file, _, openErr := openLocalPlayerEntrySource(entry, source.Library, source.Storage)
			if openErr == nil {
				playable = true
				deliveryKind = playerDeliveryServerStream
				_ = file.Close()
			}
		case models.StorageTypePan115:
			if strings.TrimSpace(entry.ProviderID) != "" {
				playable = true
				deliveryKind = playerDeliveryServerRedirect
			}
		}
		streamPath := ""
		if playable {
			streamPath = "/api/v1/player/media-entries/" + strconv.FormatUint(uint64(entry.ID), 10) + "/stream"
		}
		title := entry.Title
		versionName := versionLabels[entry.ID]
		if item.Kind != "series" && versionName != "" {
			title += " · " + versionName
		}
		if item.Kind == "series" {
			title = strings.TrimSpace(episodeSnapshot.Name)
			if title == "" {
				title = playerEpisodeFallbackTitle(entry, episode)
			}
		}
		itemToken := playerHistoryEntryToken(libraryID, token, entry.ID)
		historyIdentity := playerHistoryCanonicalIdentity(libraryID, token, item.Kind, season, episode, entry.ID)
		displayTitle := item.Title
		displaySubtitle := ""
		seriesTitle := ""
		episodeTitle := ""
		episodeStillPath := ""
		if item.Kind == "series" {
			seriesTitle = item.Title
			episodeTitle = strings.TrimSpace(episodeSnapshot.Name)
			episodeStillPath = safeTMDBImagePath(episodeSnapshot.StillPath)
			displaySubtitle = playerHistoryEpisodeSubtitle(season, episode, episodeTitle)
		}
		versions = append(versions, PlayerMediaVersion{ID: entry.ID, ItemToken: itemToken, HistoryIdentity: historyIdentity, Title: title, DisplayTitle: displayTitle, DisplaySubtitle: displaySubtitle, SeriesTitle: seriesTitle, EpisodeTitle: episodeTitle, Season: season, Episode: episode, Overview: episodeSnapshot.Overview, StillPath: episodeStillPath, StillURL: playerCatalogImageURLWithClient(imageClient, episodeStillPath, "w780"), PosterPath: item.PosterPath, PosterURL: item.PosterURL, BackdropPath: item.BackdropPath, BackdropURL: item.BackdropURL, EpisodeStillPath: episodeStillPath, EpisodeStillURL: playerCatalogImageURLWithClient(imageClient, episodeStillPath, "w780"), AirDate: episodeSnapshot.AirDate, RuntimeMinutes: episodeSnapshot.RuntimeMinutes, Rating: episodeSnapshot.VoteAverage, Size: entry.Size, ModifiedAt: entry.ModifiedAt, Playable: playable, StreamPath: streamPath, DeliveryKind: deliveryKind, ExactIdentity: exactIdentity})
		versions[len(versions)-1].VersionName = versionName
	}
	sort.SliceStable(versions, func(i, j int) bool {
		leftSeason, rightSeason := pointerIntValue(versions[i].Season), pointerIntValue(versions[j].Season)
		if leftSeason != rightSeason {
			return leftSeason < rightSeason
		}
		leftEpisode, rightEpisode := pointerIntValue(versions[i].Episode), pointerIntValue(versions[j].Episode)
		if leftEpisode != rightEpisode {
			return leftEpisode < rightEpisode
		}
		return versions[i].ID < versions[j].ID
	})
	return PlayerMediaDetail{Item: item, Versions: versions}, nil
}

// Only allowlisted technical labels leave the Server, never the source path
// or an arbitrary release suffix. Equal labels remain distinct file choices.
func playerVersionLabels(entries []models.MediaLibraryEntry) map[uint]string {
	labels := make(map[uint]string, len(entries))
	groups := make(map[string][]uint)
	for _, entry := range entries {
		label := releaseversion.Parse(entry.RelativePath)
		labels[entry.ID] = label
		season, episode := resolvedCatalogEpisodeFacts(entry)
		key := fmt.Sprintf("%d/%d/%s", pointerIntValue(season), pointerIntValue(episode), label)
		groups[key] = append(groups[key], entry.ID)
	}
	for _, ids := range groups {
		if len(ids) < 2 {
			continue
		}
		for _, id := range ids {
			label := labels[id]
			if label != "" {
				label += " · "
			}
			labels[id] = label + fmt.Sprintf("版本 %d", id)
		}
	}
	return labels
}

func playerDirectStreamModeTx(tx *gorm.DB, libraryID uint) (string, error) {
	type row struct {
		StorageType    string
		LibraryEnabled bool
		StorageEnabled bool
		ConnectionID   uint
	}
	var item row
	err := tx.Table("media_libraries").
		Select("storages.type AS storage_type, media_libraries.enabled AS library_enabled, storages.enabled AS storage_enabled, COALESCE(storages.connection_id, 0) AS connection_id").
		Joins("JOIN storages ON storages.id = media_libraries.storage_id").
		Where("media_libraries.id = ?", libraryID).Take(&item).Error
	if err != nil {
		return "", err
	}
	if !item.LibraryEnabled || !item.StorageEnabled {
		return "", nil
	}
	if item.StorageType == models.StorageTypeLocal {
		return models.StorageTypeLocal, nil
	}
	if item.StorageType == models.StorageTypePan115 && item.ConnectionID > 0 {
		return models.StorageTypePan115, nil
	}
	return "", nil
}

func (s *MediaLibraryService) PlayerSearch(actor Actor, query MediaPageQuery) (PlayerMediaItemPage, error) {
	var result PlayerMediaItemPage
	err := s.withCatalogReadTx(context.Background(), func(tx *gorm.DB) error { var err error; result, err = s.playerSearchTx(tx, actor, query); return err })
	return result, err
}

func (s *MediaLibraryService) playerSearchTx(tx *gorm.DB, actor Actor, query MediaPageQuery) (PlayerMediaItemPage, error) {
	query, err := normalizeMediaPageQuery(query)
	if err != nil {
		return PlayerMediaItemPage{}, err
	}
	libraries, err := s.playerLibrariesTx(tx, actor)
	if err != nil {
		return PlayerMediaItemPage{}, err
	}
	all := make([]PlayerMediaItem, 0)
	ids := make([]uint, 0, len(libraries))
	for _, library := range libraries {
		ids = append(ids, library.ID)
	}
	reader, err := PinCatalogTx(tx, ids)
	if err != nil {
		return PlayerMediaItemPage{}, err
	}
	for _, library := range libraries {
		for pageNumber := 1; ; pageNumber++ {
			page, err := s.playerCatalogTx(tx, reader, actor, library.ID, MediaPageQuery{Page: pageNumber, PageSize: 100, Query: query.Query, MediaType: query.MediaType, MatchStatus: query.MatchStatus})
			if err != nil {
				return PlayerMediaItemPage{}, err
			}
			all = append(all, page.List...)
			if len(page.List) == 0 || pageNumber*page.PageSize >= page.Total {
				break
			}
		}
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].Title == all[j].Title {
			return all[i].ID < all[j].ID
		}
		return strings.ToLower(all[i].Title) < strings.ToLower(all[j].Title)
	})
	total := len(all)
	start := total
	pageIndex := query.Page - 1
	if pageIndex <= total/query.PageSize {
		candidate := pageIndex * query.PageSize
		if candidate < total {
			start = candidate
		}
	}
	end := start + query.PageSize
	if end > total {
		end = total
	}
	return PlayerMediaItemPage{List: all[start:end], Total: total, Page: query.Page, PageSize: query.PageSize}, nil
}

func ensurePlayerMediaLibraryReadableTx(tx *gorm.DB, actor Actor, libraryID uint) error {
	if !actor.CanResource(authz.PermissionMediaLibrariesRead, models.AuthorizationResourceMediaLibrary, uintID(libraryID)) {
		return appError(CodePermissionDenied, "无权查看媒体库", nil)
	}
	var count int64
	err := tx.Table("media_libraries").
		Joins("JOIN storages ON storages.id = media_libraries.storage_id").
		Where("media_libraries.id = ? AND media_libraries.enabled = ? AND storages.enabled = ?", libraryID, true, true).
		Count(&count).Error
	if err != nil {
		return err
	}
	if count == 0 {
		return mediaLibraryNotFound(gorm.ErrRecordNotFound)
	}
	return nil
}

func (s *MediaLibraryService) playerMediaItemTx(tx *gorm.DB, reader *CatalogReader, libraryID uint, item MediaCatalogItem) (PlayerMediaItem, error) {
	workKey, err := decodeCatalogToken(item.ID)
	if err != nil {
		return PlayerMediaItem{}, err
	}
	var recognition models.MediaLibraryRecognition
	err = reader.Recognitions().
		Joins("JOIN (?) AS media_library_entries ON media_library_entries.recognition_id = media_library_recognitions.id", reader.Entries()).
		Where("media_library_entries.library_id = ? AND media_library_entries.work_key = ?", libraryID, workKey).
		Order("media_library_recognitions.updated_at DESC").First(&recognition).Error
	var snapshot tmdb.Snapshot
	if err == nil && recognition.MetadataJSON != "" && recognition.MetadataJSON != "{}" {
		_, snapshot, err = decodeRecognitionMetadata(recognition.MetadataJSON)
		if err != nil {
			return PlayerMediaItem{}, err
		}
	} else if err != nil && err != gorm.ErrRecordNotFound {
		return PlayerMediaItem{}, err
	}
	identity := PlayerMediaIdentity{Scheme: "server", MediaType: item.Kind, Value: fmt.Sprintf("library:%d:work:%s", libraryID, item.ID)}
	if item.TMDBID != nil {
		identity = PlayerMediaIdentity{Scheme: "tmdb", MediaType: item.Kind, Value: strconv.FormatInt(*item.TMDBID, 10)}
	}
	historyIdentity := ""
	if item.Kind == "movie" {
		historyIdentity = playerHistoryCanonicalIdentity(libraryID, item.ID, item.Kind, nil, nil, 0)
	}
	imageClient := s.catalogImageClientTx(tx)
	return PlayerMediaItem{
		ID: item.ID, ItemToken: playerHistoryWorkToken(libraryID, item.ID), HistoryIdentity: historyIdentity, LibraryID: libraryID, Title: item.Title, OriginalTitle: snapshot.OriginalTitle,
		Kind: item.Kind, ReleaseYear: item.ReleaseYear, Overview: snapshot.Overview, Tagline: snapshot.Tagline,
		Rating: snapshot.VoteAverage, RuntimeMinutes: snapshot.RuntimeMinutes, Genres: genreNames(snapshot.Genres),
		Directors: personNames(snapshot.Directors), Writers: personNames(snapshot.Writers), Cast: personNames(snapshot.Cast),
		People: playerMediaPeople(snapshot, imageClient),
		TMDBID: snapshot.TMDBID, IMDbID: snapshot.IMDbID, PosterPath: safeTMDBImagePath(snapshot.PosterPath), BackdropPath: safeTMDBImagePath(snapshot.BackdropPath),
		PosterURL: playerCatalogImageURLWithClient(imageClient, snapshot.PosterPath, "w500"), BackdropURL: playerCatalogImageURLWithClient(imageClient, snapshot.BackdropPath, "w1280"),
		StillPaths: snapshotStillPaths(snapshot), StillURLs: playerStillURLs(imageClient, snapshotStillPaths(snapshot)), WorkIdentity: identity, FileCount: item.FileCount,
		SeasonCount: item.SeasonCount, EpisodeCount: item.EpisodeCount, ModifiedAt: item.ModifiedAt,
		CategoryName: item.CategoryName, MatchStatus: item.MatchStatus,
	}, nil
}

func playerMediaPeople(snapshot tmdb.Snapshot, imageClients ...*tmdb.Client) []PlayerMediaPerson {
	var imageClient *tmdb.Client
	if len(imageClients) > 0 {
		imageClient = imageClients[0]
	}
	result := make([]PlayerMediaPerson, 0, len(snapshot.Directors)+len(snapshot.Writers)+len(snapshot.Cast))
	seen := make(map[string]struct{}, cap(result))
	appendPeople := func(people []tmdb.Person, defaultRole string) {
		for _, person := range people {
			if len(result) >= 100 {
				return
			}
			name := strings.TrimSpace(person.Name)
			role := strings.TrimSpace(person.Job)
			if role == "" {
				role = defaultRole
			}
			key := strconv.FormatInt(person.TMDBID, 10) + "\x00" + strings.ToLower(name) + "\x00" + strings.ToLower(role)
			if name == "" {
				continue
			}
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			result = append(result, PlayerMediaPerson{TMDBID: person.TMDBID, Name: name, Role: role, Character: strings.TrimSpace(person.Character), ProfilePath: safeTMDBImagePath(person.ProfilePath), ProfileURL: playerCatalogImageURLWithClient(imageClient, person.ProfilePath, "w300")})
		}
	}
	appendPeople(snapshot.Directors, "Director")
	appendPeople(snapshot.Writers, "Writer")
	appendPeople(snapshot.Cast, "Actor")
	return result
}

func genreNames(genres []tmdb.Genre) []string {
	result := make([]string, 0, len(genres))
	seen := make(map[string]struct{}, len(genres))
	for _, genre := range genres {
		name := strings.TrimSpace(genre.Name)
		key := strings.ToLower(name)
		if name == "" {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, name)
		if len(result) == 100 {
			break
		}
	}
	return result
}

func personNames(people []tmdb.Person) []string {
	result := make([]string, 0, len(people))
	seen := make(map[string]struct{}, len(people))
	for _, person := range people {
		name := strings.TrimSpace(person.Name)
		key := strings.ToLower(name)
		if name == "" {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, name)
		if len(result) == 100 {
			break
		}
	}
	return result
}

func playerStillURLs(imageClient *tmdb.Client, paths []string) []string {
	result := make([]string, 0, len(paths))
	for _, path := range paths {
		if imageURL := playerCatalogImageURLWithClient(imageClient, path, "w1280"); imageURL != "" {
			result = append(result, imageURL)
		}
	}
	return result
}

func snapshotStillPaths(snapshot tmdb.Snapshot) []string {
	paths := append([]string{snapshot.BackdropPath}, snapshot.BackdropPaths...)
	result := make([]string, 0, 8)
	seen := make(map[string]struct{}, 8)
	for _, path := range paths {
		path = safeTMDBImagePath(path)
		if path == "" {
			continue
		}
		if _, exists := seen[path]; exists {
			continue
		}
		seen[path] = struct{}{}
		result = append(result, path)
		if len(result) == 8 {
			break
		}
	}
	return result
}

func safeTMDBImagePath(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 512 || !strings.HasPrefix(value, "/") || strings.ContainsAny(value, "?#\\\r\n") || strings.Contains(value, "..") {
		return ""
	}
	return value
}

func (s *ConnectionService) PlayerEmbyInstances(actor Actor) ([]PlayerEmbyInstance, error) {
	if !actor.Can(authz.PermissionMediaLibrariesRead) {
		return []PlayerEmbyInstance{}, nil
	}
	var records []models.Connection
	if err := s.db.Where("provider = ? AND enabled = ? AND account_id <> ''", models.ConnectionProviderEmby, true).Order("name_normalized, id").Find(&records).Error; err != nil {
		return nil, err
	}
	result := make([]PlayerEmbyInstance, 0, len(records))
	for _, record := range records {
		if strings.TrimSpace(record.AccountID) == "" {
			continue
		}
		result = append(result, PlayerEmbyInstance{ConnectionID: record.ID, Name: record.Name, HealthStatus: record.LastHealthStatus, InstanceFingerprint: EmbyInstanceFingerprint(record.AccountID)})
	}
	return result, nil
}

func EmbyInstanceFingerprint(systemID string) string {
	value := "ohmycine:emby-instance:v1\x00" + strings.ToLower(strings.TrimSpace(systemID))
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// SetCatalogSnapshotStore is startup-only wiring for the same deferred reader
// used by catalog browsing. A missing store never permits versioned fallback.
func (s *SignedProxyService) SetCatalogSnapshotStore(store *CatalogSnapshotStore) {
	s.catalogStore = store
}

type playerStreamSource struct {
	Entry   models.MediaLibraryEntry
	Library models.MediaLibrary
	Storage models.Storage
	Head    models.CatalogHead
}

func (s *SignedProxyService) playerStreamSource(ctx context.Context, actor Actor, entryID uint) (playerStreamSource, error) {
	var source playerStreamSource
	// Fresh facade, not a copy of a service containing mutexes. Only the
	// explicit read-pool boundary is reused; no service metadata is delegated.
	facade := &MediaLibraryService{db: s.db, catalogStore: s.catalogStore}
	err := facade.withCatalogReadTx(ctx, func(tx *gorm.DB) error {
		var anchor struct{ LibraryID uint }
		// Anchors may locate the owner only. No anchor locator is played.
		if err := tx.Table("media_library_entries").Select("library_id").Where("id=?", entryID).Take(&anchor).Error; err != nil {
			return appError(CodeNotFound, "媒体文件不存在", err)
		}
		if err := ensurePlayerMediaLibraryReadableTx(tx, actor, anchor.LibraryID); err != nil {
			return err
		}
		reader, err := PinCatalogTx(tx, []uint{anchor.LibraryID})
		if err != nil {
			return err
		}
		source.Head, _ = reader.Head(anchor.LibraryID)
		if err := reader.Entries().Where("id=? AND library_id=?", entryID, anchor.LibraryID).First(&source.Entry).Error; err != nil {
			return appError(CodeNotFound, "媒体文件不存在", err)
		}
		if err := tx.First(&source.Library, anchor.LibraryID).Error; err != nil {
			return err
		}
		return tx.First(&source.Storage, source.Library.StorageID).Error
	})
	return source, err
}

func samePlayerStreamSource(left, right playerStreamSource) bool {
	return left.Entry.ID == right.Entry.ID && left.Entry.LibraryID == right.Entry.LibraryID && left.Entry.RelativePath == right.Entry.RelativePath && left.Entry.ProviderID == right.Entry.ProviderID && left.Entry.Size == right.Entry.Size && left.Entry.ModifiedAt.Equal(right.Entry.ModifiedAt) && left.Library.StorageID == right.Library.StorageID && left.Library.RelativeRoot == right.Library.RelativeRoot && left.Library.ProviderRootID == right.Library.ProviderRootID && left.Storage.Type == right.Storage.Type && left.Storage.RootPath == right.Storage.RootPath && uintPointerValue(left.Storage.ConnectionID) == uintPointerValue(right.Storage.ConnectionID) && (left.Head.Mode != "versioned" || (right.Head.Mode == "versioned" && left.Head.SourceEpoch == right.Head.SourceEpoch && left.Head.SourceFingerprint == right.Head.SourceFingerprint))
}

func uintPointerValue(value *uint) uint {
	if value == nil {
		return 0
	}
	return *value
}

func (s *SignedProxyService) revalidatePlayerStreamSource(ctx context.Context, actor Actor, source playerStreamSource) error {
	current, err := s.playerStreamSource(ctx, actor, source.Entry.ID)
	if err != nil {
		return err
	}
	if !samePlayerStreamSource(source, current) {
		return PlayerStreamUnavailableError()
	}
	return nil
}

func (s *SignedProxyService) ResolvePlayerEntry(ctx context.Context, actor Actor, entryID uint, userAgent, remoteAddr string) (PlayerStreamResolution, error) {
	if !actor.HasPermission(authz.PermissionMediaLibrariesRead) {
		return PlayerStreamResolution{}, appError(CodePermissionDenied, "无权播放该媒体", nil)
	}
	source, err := s.playerStreamSource(ctx, actor, entryID)
	if err != nil {
		return PlayerStreamResolution{}, err
	}
	entry, library, storage := source.Entry, source.Library, source.Storage
	if storage.Type == models.StorageTypeLocal {
		file, info, err := openLocalPlayerEntrySource(entry, library, storage)
		if err != nil {
			return PlayerStreamResolution{}, appError(CodeProxyTargetUnavailable, "播放目标不可用", err)
		}
		if err := s.revalidatePlayerStreamSource(ctx, actor, source); err != nil {
			_ = file.Close()
			return PlayerStreamResolution{}, err
		}
		return PlayerStreamResolution{Kind: playerStreamKindLocal, File: file, Name: filepath.Base(filepath.FromSlash(entry.RelativePath)), ModifiedAt: info.ModTime()}, nil
	}
	if storage.Type != models.StorageTypePan115 || storage.ConnectionID == nil || *storage.ConnectionID == 0 || strings.TrimSpace(entry.ProviderID) == "" || s.connections == nil {
		return PlayerStreamResolution{}, appError(CodeProxyTargetUnavailable, "播放目标不可用", nil)
	}
	_, driver, err := s.connections.driver(*storage.ConnectionID)
	if err != nil || !driver.Capabilities().TemporaryDirectURL {
		return PlayerStreamResolution{}, appError(CodeProxyTargetUnavailable, "播放目标不可用", err)
	}
	readCtx := cloudpkg.WithReadClass(ctx, cloudpkg.ReadClassInteractive)
	item, err := driver.Stat(readCtx, entry.ProviderID)
	if err != nil || item.IsDir || item.ID != entry.ProviderID || strings.TrimSpace(item.PickCode) == "" {
		return PlayerStreamResolution{}, appError(CodeProxyTargetUnavailable, "播放目标不可用", err)
	}
	rootID := strings.TrimSpace(library.ProviderRootID)
	if rootID == "" {
		rootID = strings.TrimSpace(storage.RootPath)
	}
	within, err := providerParentWithinRoot(readCtx, driver, item.ParentID, rootID)
	if err != nil || !within {
		return PlayerStreamResolution{}, appError(CodeProxyTargetUnavailable, "播放目标不可用", err)
	}
	if err := s.revalidatePlayerStreamSource(ctx, actor, source); err != nil {
		return PlayerStreamResolution{}, err
	}
	target := signedProxyTarget{LibraryID: library.ID, ConnectionID: *storage.ConnectionID, ProviderItemID: item.ID, StorageType: storage.Type, LibraryEnabled: true, StorageEnabled: true}
	redirect, err := s.resolveTargetWithItem(ctx, playerEntryProxyIdentity(library.ID, entry.ID, item.ID), target, userAgent, playbackClientFingerprint(remoteAddr, userAgent), &item)
	if err != nil {
		return PlayerStreamResolution{}, err
	}
	if err := s.revalidatePlayerStreamSource(ctx, actor, source); err != nil {
		return PlayerStreamResolution{}, err
	}
	return PlayerStreamResolution{Kind: playerStreamKindRedirect, RedirectURL: redirect.URL}, nil
}

func playerEntryProxyIdentity(libraryID, entryID uint, providerItemID string) string {
	provider := sha256.Sum256([]byte("ohmycine:player-entry-provider:v1\x00" + providerItemID))
	return "player-entry-" + strconv.FormatUint(uint64(libraryID), 10) + "-" + strconv.FormatUint(uint64(entryID), 10) + "-" + hex.EncodeToString(provider[:8])
}

func openLocalPlayerEntry(db *gorm.DB, entry models.MediaLibraryEntry) (*os.File, os.FileInfo, error) {
	var library models.MediaLibrary
	if err := db.First(&library, entry.LibraryID).Error; err != nil || !library.Enabled {
		return nil, nil, appError(CodeProxyTargetUnavailable, "播放目标不可用", err)
	}
	var storage models.Storage
	if err := db.First(&storage, library.StorageID).Error; err != nil || !storage.Enabled || storage.Type != models.StorageTypeLocal {
		return nil, nil, appError(CodeProxyTargetUnavailable, "播放目标不可用", err)
	}
	return openLocalPlayerEntrySource(entry, library, storage)
}

func openLocalPlayerEntrySource(entry models.MediaLibraryEntry, library models.MediaLibrary, storage models.Storage) (*os.File, os.FileInfo, error) {
	if entry.LibraryID != library.ID || library.StorageID != storage.ID || !library.Enabled || !storage.Enabled || storage.Type != models.StorageTypeLocal {
		return nil, nil, PlayerStreamUnavailableError()
	}
	libraryRoot, err := medialibrary.ResolveRoot(storage.RootPath, library.RelativeRoot)
	if err != nil {
		return nil, nil, appError(CodeProxyTargetUnavailable, "播放目标不可用", err)
	}
	relative := strings.TrimSpace(strings.ReplaceAll(entry.RelativePath, "\\", "/"))
	if relative == "" || strings.Contains(relative, "\x00") || strings.HasPrefix(relative, "//") {
		return nil, nil, appError(CodeProxyTargetUnavailable, "播放目标不可用", nil)
	}
	relative = strings.TrimLeft(relative, "/")
	cleanRelative := filepath.Clean(filepath.FromSlash(relative))
	if cleanRelative == "." || cleanRelative == ".." || filepath.IsAbs(cleanRelative) || filepath.VolumeName(cleanRelative) != "" || strings.HasPrefix(cleanRelative, ".."+string(filepath.Separator)) {
		return nil, nil, appError(CodeProxyTargetUnavailable, "播放目标不可用", nil)
	}
	target, err := storagefs.Constrain(libraryRoot, filepath.Join(libraryRoot, cleanRelative))
	if err != nil {
		return nil, nil, appError(CodeProxyTargetUnavailable, "播放目标不可用", err)
	}
	current := libraryRoot
	rootInfo, err := os.Lstat(current)
	if err != nil || !rootInfo.IsDir() || storagefs.IsReparsePoint(current, rootInfo) {
		return nil, nil, appError(CodeProxyTargetUnavailable, "播放目标不可用", err)
	}
	for _, part := range strings.Split(cleanRelative, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, statErr := os.Lstat(current)
		if statErr != nil || storagefs.IsReparsePoint(current, info) {
			return nil, nil, appError(CodeProxyTargetUnavailable, "播放目标不可用", statErr)
		}
		if current != target && !info.IsDir() {
			return nil, nil, appError(CodeProxyTargetUnavailable, "播放目标不可用", nil)
		}
	}
	resolvedRoot, err := filepath.EvalSymlinks(libraryRoot)
	if err != nil {
		return nil, nil, appError(CodeProxyTargetUnavailable, "播放目标不可用", err)
	}
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		return nil, nil, appError(CodeProxyTargetUnavailable, "播放目标不可用", err)
	}
	if _, err := storagefs.Constrain(resolvedRoot, resolved); err != nil || storagefs.NormalizeForComparison(resolved) != storagefs.NormalizeForComparison(filepath.Join(resolvedRoot, cleanRelative)) {
		return nil, nil, appError(CodeProxyTargetUnavailable, "播放目标不可用", err)
	}
	file, err := os.Open(target)
	if err != nil {
		return nil, nil, appError(CodeProxyTargetUnavailable, "播放目标不可用", err)
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, nil, appError(CodeProxyTargetUnavailable, "播放目标不可用", err)
	}
	return file, info, nil
}
