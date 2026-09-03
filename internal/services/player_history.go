package services

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/metadata/tmdb"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const playerHistorySyncLimit = 500

type PlayerHistoryChange struct {
	SyncKey          string   `json:"sync_key"`
	HistoryIdentity  string   `json:"history_identity,omitempty"`
	SourceKind       string   `json:"source_kind"`
	SourceLocator    string   `json:"source_locator,omitempty"`
	SourceID         string   `json:"source_id"`
	LibraryID        string   `json:"library_id,omitempty"`
	ItemID           string   `json:"item_id,omitempty"`
	ItemToken        string   `json:"item_token,omitempty"`
	MediaIdentity    string   `json:"media_identity"`
	Title            string   `json:"title"`
	DisplayTitle     string   `json:"display_title,omitempty"`
	DisplaySubtitle  string   `json:"display_subtitle,omitempty"`
	SeriesTitle      string   `json:"series_title,omitempty"`
	EpisodeTitle     string   `json:"episode_title,omitempty"`
	SeasonNumber     *int     `json:"season_number,omitempty"`
	EpisodeNumber    *int     `json:"episode_number,omitempty"`
	StreamIdentity   string   `json:"stream_identity,omitempty"`
	MediaType        string   `json:"media_type,omitempty"`
	PosterURL        string   `json:"poster_url,omitempty"`
	BackdropURL      string   `json:"backdrop_url,omitempty"`
	TitleLogoURL     string   `json:"title_logo_url,omitempty"`
	PosterPath       string   `json:"poster_path,omitempty"`
	BackdropPath     string   `json:"backdrop_path,omitempty"`
	EpisodeStillPath string   `json:"episode_still_path,omitempty"`
	Position         float64  `json:"position"`
	Duration         *float64 `json:"duration,omitempty"`
	Completed        bool     `json:"completed"`
	Deleted          bool     `json:"deleted,omitempty"`
	UpdatedAt        int64    `json:"updated_at"`
	Revision         uint64   `json:"revision,omitempty"`
}

type PlayerHistorySyncResult struct {
	Cursor  uint64                `json:"cursor"`
	Changes []PlayerHistoryChange `json:"changes"`
}

type PlayerHistoryPage struct {
	List     []PlayerHistoryChange `json:"list"`
	Total    int64                 `json:"total"`
	Page     int                   `json:"page"`
	PageSize int                   `json:"page_size"`
	HasMore  bool                  `json:"has_more"`
}

type PlayerHistoryService struct {
	db        *gorm.DB
	libraries *MediaLibraryService
}

func NewPlayerHistoryService(db *gorm.DB, libraries ...*MediaLibraryService) *PlayerHistoryService {
	service := &PlayerHistoryService{db: db}
	if len(libraries) > 0 {
		service.libraries = libraries[0]
	}
	return service
}

func (s *PlayerHistoryService) Sync(actor Actor, cursor uint64, changes []PlayerHistoryChange) (PlayerHistorySyncResult, error) {
	if len(changes) > playerHistorySyncLimit {
		return PlayerHistorySyncResult{}, appError(CodeInvalidRequest, "一次最多同步 500 条播放记录", nil)
	}
	normalized := make([]PlayerHistoryChange, 0, len(changes))
	for _, change := range changes {
		item, err := normalizePlayerHistoryChange(change)
		if err != nil {
			return PlayerHistorySyncResult{}, err
		}
		normalized = append(normalized, item)
	}
	err := s.db.Transaction(func(tx *gorm.DB) error {
		for _, original := range normalized {
			if original.SourceKind == "server" && s.libraries != nil {
				authority, err := s.resolveServerHistoryAuthority(tx, actor, original)
				if err != nil {
					return err
				}
				canonical := applyServerHistoryAuthority(original, authority)
				if err := s.mergeCanonicalServerHistory(tx, actor, original, canonical, authority); err != nil {
					return err
				}
				continue
			}
			if _, err := upsertPlayerHistoryChange(tx, actor.User.ID, original); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return PlayerHistorySyncResult{}, err
	}
	var rows []models.PlayerPlaybackHistory
	if err := s.db.Where("user_id = ? AND revision > ?", actor.User.ID, cursor).Order("revision ASC").Limit(playerHistorySyncLimit).Find(&rows).Error; err != nil {
		return PlayerHistorySyncResult{}, err
	}
	result := PlayerHistorySyncResult{Cursor: cursor, Changes: make([]PlayerHistoryChange, 0, len(rows))}
	for _, row := range rows {
		result.Changes = append(result.Changes, playerHistoryChangeDTO(row))
		if row.Revision > result.Cursor {
			result.Cursor = row.Revision
		}
	}
	return result, nil
}

func (s *PlayerHistoryService) List(actor Actor, page, pageSize int, sourceKind string) (PlayerHistoryPage, error) {
	if page < 1 || page > 100_000 || pageSize < 1 || pageSize > 100 {
		return PlayerHistoryPage{}, appError(CodeInvalidRequest, "播放历史分页参数无效", nil)
	}
	sourceKind = strings.ToLower(strings.TrimSpace(sourceKind))
	if sourceKind != "" && (len(sourceKind) > 32 || strings.ContainsAny(sourceKind, "\r\n\x00")) {
		return PlayerHistoryPage{}, appError(CodeInvalidRequest, "播放历史来源类型无效", nil)
	}
	query := s.db.Model(&models.PlayerPlaybackHistory{}).Where("user_id = ? AND deleted = ?", actor.User.ID, false)
	if sourceKind != "" {
		query = query.Where("source_kind = ?", sourceKind)
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return PlayerHistoryPage{}, err
	}
	var rows []models.PlayerPlaybackHistory
	offset := (page - 1) * pageSize
	if err := query.Order("client_updated_at DESC, sync_key ASC").Limit(pageSize).Offset(offset).Find(&rows).Error; err != nil {
		return PlayerHistoryPage{}, err
	}
	result := PlayerHistoryPage{List: make([]PlayerHistoryChange, 0, len(rows)), Total: total, Page: page, PageSize: pageSize, HasMore: int64(offset+len(rows)) < total}
	for _, row := range rows {
		result.List = append(result.List, playerHistoryChangeDTO(row))
	}
	return result, nil
}

type serverHistoryAuthority struct {
	HistoryIdentity  string
	SyncKey          string
	LibraryID        uint
	WorkToken        string
	ItemToken        string
	MediaType        string
	DisplayTitle     string
	DisplaySubtitle  string
	SeriesTitle      string
	EpisodeTitle     string
	SeasonNumber     *int
	EpisodeNumber    *int
	PosterPath       string
	BackdropPath     string
	EpisodeStillPath string
}

type parsedServerHistoryToken struct {
	kind      string
	libraryID uint
	workToken string
	workKey   string
	entryID   uint
}

func parseServerHistoryToken(value string) (parsedServerHistoryToken, error) {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 512 || strings.ContainsAny(value, "\r\n\x00") {
		return parsedServerHistoryToken{}, appError(CodeInvalidRequest, "播放历史媒体标识无效", nil)
	}
	parts := strings.Split(value, "|")
	if len(parts) != 3 && len(parts) != 4 {
		return parsedServerHistoryToken{}, appError(CodeInvalidRequest, "播放历史媒体标识无效", nil)
	}
	kind := parts[0]
	if kind != "work" && kind != "entry" || kind == "work" && len(parts) != 3 || kind == "entry" && len(parts) != 4 {
		return parsedServerHistoryToken{}, appError(CodeInvalidRequest, "播放历史媒体标识无效", nil)
	}
	parsedLibraryID, err := strconv.ParseUint(parts[1], 10, 32)
	if err != nil || parsedLibraryID == 0 {
		return parsedServerHistoryToken{}, appError(CodeInvalidRequest, "播放历史媒体标识无效", err)
	}
	workKey, err := decodeCatalogToken(parts[2])
	if err != nil {
		return parsedServerHistoryToken{}, appError(CodeInvalidRequest, "播放历史媒体标识无效", err)
	}
	result := parsedServerHistoryToken{kind: kind, libraryID: uint(parsedLibraryID), workToken: parts[2], workKey: workKey}
	if kind == "entry" {
		parsedEntryID, parseErr := strconv.ParseUint(parts[3], 10, 32)
		if parseErr != nil || parsedEntryID == 0 {
			return parsedServerHistoryToken{}, appError(CodeInvalidRequest, "播放历史媒体标识无效", parseErr)
		}
		result.entryID = uint(parsedEntryID)
	}
	return result, nil
}

func (s *PlayerHistoryService) resolveServerHistoryAuthority(tx *gorm.DB, actor Actor, change PlayerHistoryChange) (serverHistoryAuthority, error) {
	token := strings.TrimSpace(change.ItemToken)
	if token == "" {
		token = strings.TrimSpace(change.ItemID)
	}
	if token == "" {
		token = strings.TrimSpace(change.MediaIdentity)
	}
	parsed, err := parseServerHistoryToken(token)
	if err != nil {
		return serverHistoryAuthority{}, err
	}
	if change.LibraryID != "" && change.LibraryID != strconv.FormatUint(uint64(parsed.libraryID), 10) {
		return serverHistoryAuthority{}, appError(CodeInvalidRequest, "播放历史媒体库标识不一致", nil)
	}
	if !actor.CanResource(authz.PermissionMediaLibrariesRead, models.AuthorizationResourceMediaLibrary, uintID(parsed.libraryID)) {
		return serverHistoryAuthority{}, appError(CodePermissionDenied, "无权同步该媒体库的播放历史", nil)
	}
	var available int64
	if err := tx.Table("media_libraries").Joins("JOIN storages ON storages.id = media_libraries.storage_id").Where("media_libraries.id = ? AND media_libraries.enabled = ? AND storages.enabled = ?", parsed.libraryID, true, true).Count(&available).Error; err != nil {
		return serverHistoryAuthority{}, err
	}
	if available == 0 {
		return serverHistoryAuthority{}, appError(CodeNotFound, "播放历史媒体不存在", gorm.ErrRecordNotFound)
	}
	entryQuery := tx.Where("library_id = ? AND work_key = ?", parsed.libraryID, parsed.workKey)
	if parsed.entryID != 0 {
		entryQuery = entryQuery.Where("id = ?", parsed.entryID)
	}
	var entry models.MediaLibraryEntry
	if err := entryQuery.Order("id").First(&entry).Error; err != nil {
		return serverHistoryAuthority{}, appError(CodeNotFound, "播放历史媒体不存在", err)
	}
	isSeries := entry.MediaType == "tv" || strings.HasPrefix(entry.WorkKey, "series:")
	if parsed.kind == "work" && isSeries {
		return serverHistoryAuthority{}, appError(CodeInvalidRequest, "剧集播放历史必须指向具体分集", nil)
	}
	season, episode := resolvedCatalogEpisodeFacts(entry)
	var recognition models.MediaLibraryRecognition
	var recognitionErr error
	if entry.RecognitionID != nil {
		recognitionErr = tx.First(&recognition, *entry.RecognitionID).Error
	} else {
		recognitionErr = tx.Table("media_library_recognitions").Joins("JOIN media_library_entries ON media_library_entries.recognition_id = media_library_recognitions.id").Where("media_library_entries.library_id = ? AND media_library_entries.work_key = ?", parsed.libraryID, parsed.workKey).Order("media_library_recognitions.updated_at DESC").First(&recognition).Error
	}
	var snapshot tmdb.Snapshot
	if recognitionErr == nil && recognition.MetadataJSON != "" && recognition.MetadataJSON != "{}" {
		_, snapshot, _ = decodeRecognitionMetadata(recognition.MetadataJSON)
	} else if recognitionErr != nil && recognitionErr != gorm.ErrRecordNotFound {
		return serverHistoryAuthority{}, recognitionErr
	}
	displayTitle := strings.TrimSpace(snapshot.Title)
	if displayTitle == "" {
		displayTitle = strings.TrimSpace(recognition.Title)
	}
	if isSeries && displayTitle == "" {
		displayTitle = strings.TrimSpace(entry.SeriesTitle)
	}
	if displayTitle == "" {
		displayTitle = strings.TrimSpace(entry.Title)
	}
	if displayTitle == "" {
		return serverHistoryAuthority{}, appError(CodeInvalidRequest, "播放历史媒体标题无效", nil)
	}
	authority := serverHistoryAuthority{
		LibraryID:     parsed.libraryID,
		WorkToken:     parsed.workToken,
		ItemToken:     token,
		MediaType:     "movie",
		DisplayTitle:  displayTitle,
		PosterPath:    safeTMDBImagePath(snapshot.PosterPath),
		BackdropPath:  safeTMDBImagePath(snapshot.BackdropPath),
		SeasonNumber:  cloneInt(season),
		EpisodeNumber: cloneInt(episode),
	}
	if parsed.kind == "entry" {
		authority.ItemToken = playerHistoryEntryToken(parsed.libraryID, parsed.workToken, entry.ID)
	} else {
		authority.ItemToken = playerHistoryWorkToken(parsed.libraryID, parsed.workToken)
	}
	if isSeries {
		authority.MediaType = "episode"
		authority.SeriesTitle = displayTitle
		for _, candidate := range snapshot.EpisodeSnapshots {
			if season != nil && episode != nil && candidate.SeasonNumber == *season && candidate.EpisodeNumber == *episode {
				authority.EpisodeTitle = strings.TrimSpace(candidate.Name)
				authority.EpisodeStillPath = safeTMDBImagePath(candidate.StillPath)
				break
			}
		}
		if authority.EpisodeTitle == "" {
			entryTitle := strings.TrimSpace(entry.Title)
			if entryTitle != "" && !strings.EqualFold(entryTitle, displayTitle) {
				authority.EpisodeTitle = entryTitle
			}
		}
		authority.DisplaySubtitle = playerHistoryEpisodeSubtitle(season, episode, authority.EpisodeTitle)
	}
	kind := "movie"
	if isSeries {
		kind = "series"
	}
	authority.HistoryIdentity = playerHistoryCanonicalIdentity(parsed.libraryID, parsed.workToken, kind, season, episode, entry.ID)
	authority.SyncKey = playerHistoryCanonicalSyncKey(authority.HistoryIdentity)
	return authority, nil
}

func applyServerHistoryAuthority(change PlayerHistoryChange, authority serverHistoryAuthority) PlayerHistoryChange {
	change.SyncKey = authority.SyncKey
	change.HistoryIdentity = authority.HistoryIdentity
	change.LibraryID = strconv.FormatUint(uint64(authority.LibraryID), 10)
	change.ItemID = authority.ItemToken
	change.ItemToken = authority.ItemToken
	change.MediaIdentity = authority.HistoryIdentity
	change.Title = authority.DisplayTitle
	change.DisplayTitle = authority.DisplayTitle
	change.DisplaySubtitle = authority.DisplaySubtitle
	change.SeriesTitle = authority.SeriesTitle
	change.EpisodeTitle = authority.EpisodeTitle
	change.SeasonNumber = cloneInt(authority.SeasonNumber)
	change.EpisodeNumber = cloneInt(authority.EpisodeNumber)
	change.StreamIdentity = authority.ItemToken
	change.MediaType = authority.MediaType
	change.PosterURL = ""
	change.BackdropURL = ""
	change.TitleLogoURL = ""
	change.PosterPath = authority.PosterPath
	change.BackdropPath = authority.BackdropPath
	change.EpisodeStillPath = authority.EpisodeStillPath
	return change
}

func (s *PlayerHistoryService) mergeCanonicalServerHistory(tx *gorm.DB, actor Actor, original, canonical PlayerHistoryChange, authority serverHistoryAuthority) error {
	workToken := playerHistoryWorkToken(authority.LibraryID, authority.WorkToken)
	entryPrefix := "entry|" + strconv.FormatUint(uint64(authority.LibraryID), 10) + "|" + authority.WorkToken + "|"
	seasonPrefix := "season|" + strconv.FormatUint(uint64(authority.LibraryID), 10) + "|" + authority.WorkToken + "|"
	var rows []models.PlayerPlaybackHistory
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
		`user_id = ? AND source_kind = 'server' AND (
			sync_key = ? OR canonical_identity = ? OR item_id = ? OR item_token = ? OR media_identity = ? OR
			instr(item_id, ?) = 1 OR instr(item_token, ?) = 1 OR instr(media_identity, ?) = 1 OR
			instr(item_id, ?) = 1 OR instr(item_token, ?) = 1 OR instr(media_identity, ?) = 1
		)`,
		actor.User.ID, authority.SyncKey, authority.HistoryIdentity, workToken, workToken, workToken,
		entryPrefix, entryPrefix, entryPrefix, seasonPrefix, seasonPrefix, seasonPrefix,
	).Find(&rows).Error
	if err != nil {
		return err
	}

	matched := make([]models.PlayerPlaybackHistory, 0, len(rows))
	var currentCanonical *models.PlayerPlaybackHistory
	for index := range rows {
		row := rows[index]
		if row.SyncKey == authority.SyncKey {
			copy := row
			currentCanonical = &copy
			continue
		}
		if row.HistoryIdentity == authority.HistoryIdentity {
			matched = append(matched, row)
			continue
		}
		candidate := playerHistoryChangeDTO(row)
		resolved, resolveErr := s.resolveServerHistoryAuthority(tx, actor, candidate)
		if resolveErr == nil && resolved.HistoryIdentity == authority.HistoryIdentity {
			matched = append(matched, row)
		}
	}

	winner := canonical
	winnerFromIncoming := true
	if currentCanonical != nil {
		winner = playerHistoryChangeDTO(*currentCanonical)
		winnerFromIncoming = false
		if playerHistoryIncomingWins(winner, canonical) {
			winner = canonical
			winnerFromIncoming = true
		}
	}
	for _, row := range matched {
		// Non-canonical tombstones only retire a legacy client key. The semantic
		// deletion winner is always stored on the canonical row itself.
		if row.Deleted {
			continue
		}
		candidate := playerHistoryChangeDTO(row)
		if playerHistoryIncomingWins(winner, candidate) {
			winner = candidate
			winnerFromIncoming = false
		}
	}
	target := canonical
	copyPlayerHistoryState(&target, winner)
	if !winnerFromIncoming {
		if resolved, resolveErr := s.resolveServerHistoryAuthority(tx, actor, winner); resolveErr == nil && resolved.HistoryIdentity == authority.HistoryIdentity {
			target.SourceLocator = winner.SourceLocator
			target.SourceID = winner.SourceID
			target.ItemID = resolved.ItemToken
			target.ItemToken = resolved.ItemToken
			target.StreamIdentity = resolved.ItemToken
		}
	}
	target.SyncKey = authority.SyncKey
	target.HistoryIdentity = authority.HistoryIdentity
	target.MediaIdentity = authority.HistoryIdentity

	legacyByKey := make(map[string]models.PlayerPlaybackHistory, len(matched))
	for _, row := range matched {
		legacyByKey[row.SyncKey] = row
	}
	if original.SyncKey != authority.SyncKey {
		row, exists := legacyByKey[original.SyncKey]
		if !exists || row.ClientUpdatedAt < original.UpdatedAt {
			legacyByKey[original.SyncKey] = playerHistoryRecord(actor.User.ID, original)
		}
	}
	for _, row := range legacyByKey {
		if row.Deleted {
			continue
		}
		tombstone := playerHistoryChangeDTO(row)
		tombstone.HistoryIdentity = authority.HistoryIdentity
		tombstone.Deleted = true
		if _, err := writePlayerHistoryChange(tx, actor.User.ID, tombstone); err != nil {
			return err
		}
	}
	if currentCanonical != nil && playerHistoryRecordMatchesChange(*currentCanonical, target) {
		return nil
	}
	_, err = writePlayerHistoryChange(tx, actor.User.ID, target)
	return err
}

func copyPlayerHistoryState(target *PlayerHistoryChange, source PlayerHistoryChange) {
	target.SourceLocator = source.SourceLocator
	target.SourceID = source.SourceID
	target.Position = source.Position
	target.Duration = cloneHistoryFloat64(source.Duration)
	target.Completed = source.Completed
	target.Deleted = source.Deleted
	target.UpdatedAt = source.UpdatedAt
}

func cloneHistoryFloat64(value *float64) *float64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func playerHistoryIncomingWins(current, incoming PlayerHistoryChange) bool {
	if current.UpdatedAt > incoming.UpdatedAt {
		return false
	}
	if current.UpdatedAt < incoming.UpdatedAt {
		return true
	}
	currentTerminal := current.Completed || current.Deleted
	incomingTerminal := incoming.Completed || incoming.Deleted
	return !currentTerminal && incomingTerminal
}

func upsertPlayerHistoryChange(tx *gorm.DB, userID uint, change PlayerHistoryChange) (bool, error) {
	var current models.PlayerPlaybackHistory
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("user_id = ? AND sync_key = ?", userID, change.SyncKey).First(&current).Error
	if err != nil && err != gorm.ErrRecordNotFound {
		return false, err
	}
	if err == nil && !playerHistoryIncomingWins(playerHistoryChangeDTO(current), change) {
		return false, nil
	}
	_, err = writePlayerHistoryChange(tx, userID, change)
	return err == nil, err
}

func writePlayerHistoryChange(tx *gorm.DB, userID uint, change PlayerHistoryChange) (uint64, error) {
	revision := models.PlayerPlaybackHistoryRevision{UserID: userID, SyncKey: change.SyncKey, ChangedAt: time.Now().UTC()}
	if err := tx.Create(&revision).Error; err != nil {
		return 0, err
	}
	now := time.Now().UTC()
	record := playerHistoryRecord(userID, change)
	record.Revision = revision.ID
	record.CreatedAt = now
	record.UpdatedAt = now
	updates := []string{"canonical_identity", "source_kind", "source_locator", "source_id", "library_id", "item_id", "item_token", "media_identity", "title", "display_title", "display_subtitle", "series_title", "episode_title", "season_number", "episode_number", "stream_identity", "media_type", "poster_url", "backdrop_url", "title_logo_url", "poster_path", "backdrop_path", "episode_still_path", "position", "duration", "completed", "deleted", "client_updated_at", "revision", "updated_at"}
	if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "user_id"}, {Name: "sync_key"}}, DoUpdates: clause.AssignmentColumns(updates)}).Create(&record).Error; err != nil {
		return 0, err
	}
	return revision.ID, nil
}

func playerHistoryRecord(userID uint, change PlayerHistoryChange) models.PlayerPlaybackHistory {
	return models.PlayerPlaybackHistory{
		UserID: userID, SyncKey: change.SyncKey, HistoryIdentity: change.HistoryIdentity,
		SourceKind: change.SourceKind, SourceLocator: change.SourceLocator, SourceID: change.SourceID,
		LibraryID: change.LibraryID, ItemID: change.ItemID, ItemToken: change.ItemToken,
		MediaIdentity: change.MediaIdentity, Title: change.Title, DisplayTitle: change.DisplayTitle,
		DisplaySubtitle: change.DisplaySubtitle, SeriesTitle: change.SeriesTitle, EpisodeTitle: change.EpisodeTitle,
		SeasonNumber: cloneInt(change.SeasonNumber), EpisodeNumber: cloneInt(change.EpisodeNumber),
		StreamIdentity: change.StreamIdentity, MediaType: change.MediaType,
		PosterURL: change.PosterURL, BackdropURL: change.BackdropURL, TitleLogoURL: change.TitleLogoURL,
		PosterPath: change.PosterPath, BackdropPath: change.BackdropPath, EpisodeStillPath: change.EpisodeStillPath,
		Position: change.Position, Duration: cloneHistoryFloat64(change.Duration), Completed: change.Completed,
		Deleted: change.Deleted, ClientUpdatedAt: change.UpdatedAt,
	}
}

func playerHistoryRecordMatchesChange(row models.PlayerPlaybackHistory, change PlayerHistoryChange) bool {
	return row.HistoryIdentity == change.HistoryIdentity && row.SourceKind == change.SourceKind && row.SourceLocator == change.SourceLocator && row.SourceID == change.SourceID && row.LibraryID == change.LibraryID && row.ItemID == change.ItemID && row.ItemToken == change.ItemToken && row.MediaIdentity == change.MediaIdentity && row.Title == change.Title && row.DisplayTitle == change.DisplayTitle && row.DisplaySubtitle == change.DisplaySubtitle && row.SeriesTitle == change.SeriesTitle && row.EpisodeTitle == change.EpisodeTitle && equalIntPointers(row.SeasonNumber, change.SeasonNumber) && equalIntPointers(row.EpisodeNumber, change.EpisodeNumber) && row.StreamIdentity == change.StreamIdentity && row.MediaType == change.MediaType && row.PosterURL == change.PosterURL && row.BackdropURL == change.BackdropURL && row.TitleLogoURL == change.TitleLogoURL && row.PosterPath == change.PosterPath && row.BackdropPath == change.BackdropPath && row.EpisodeStillPath == change.EpisodeStillPath && row.Position == change.Position && equalFloatPointers(row.Duration, change.Duration) && row.Completed == change.Completed && row.Deleted == change.Deleted && row.ClientUpdatedAt == change.UpdatedAt
}

func equalIntPointers(left, right *int) bool {
	return left == nil && right == nil || left != nil && right != nil && *left == *right
}

func equalFloatPointers(left, right *float64) bool {
	return left == nil && right == nil || left != nil && right != nil && *left == *right
}

func normalizePlayerHistoryChange(change PlayerHistoryChange) (PlayerHistoryChange, error) {
	change.SyncKey = strings.ToLower(strings.TrimSpace(change.SyncKey))
	change.HistoryIdentity = strings.TrimSpace(change.HistoryIdentity)
	change.SourceKind = strings.ToLower(strings.TrimSpace(change.SourceKind))
	change.SourceLocator = strings.TrimSpace(change.SourceLocator)
	change.SourceID = strings.TrimSpace(change.SourceID)
	change.LibraryID = strings.TrimSpace(change.LibraryID)
	change.ItemID = strings.TrimSpace(change.ItemID)
	change.ItemToken = strings.TrimSpace(change.ItemToken)
	change.MediaIdentity = strings.TrimSpace(change.MediaIdentity)
	change.Title = strings.TrimSpace(change.Title)
	change.DisplayTitle = strings.TrimSpace(change.DisplayTitle)
	change.DisplaySubtitle = strings.TrimSpace(change.DisplaySubtitle)
	change.SeriesTitle = strings.TrimSpace(change.SeriesTitle)
	change.EpisodeTitle = strings.TrimSpace(change.EpisodeTitle)
	change.StreamIdentity = strings.TrimSpace(change.StreamIdentity)
	change.MediaType = strings.ToLower(strings.TrimSpace(change.MediaType))
	if change.DisplayTitle == "" {
		change.DisplayTitle = change.Title
	}
	if len(change.SyncKey) != 64 || !isHex(change.SyncKey) || change.SourceKind == "" || len(change.SourceKind) > 32 || change.SourceID == "" || len(change.SourceID) > 128 || change.MediaIdentity == "" || len(change.MediaIdentity) > 2048 || change.Title == "" || len(change.Title) > 512 || len(change.HistoryIdentity) > 512 || len(change.DisplayTitle) > 512 || len(change.DisplaySubtitle) > 512 || len(change.SeriesTitle) > 512 || len(change.EpisodeTitle) > 512 || len(change.SourceLocator) > 512 || len(change.LibraryID) > 256 || len(change.ItemID) > 512 || len(change.ItemToken) > 512 || len(change.StreamIdentity) > 2048 || change.UpdatedAt <= 0 || !finiteNonNegative(change.Position) || change.Duration != nil && !finiteNonNegative(*change.Duration) || !validHistoryEpisodeFacts(change.SeasonNumber, change.EpisodeNumber) {
		return PlayerHistoryChange{}, appError(CodeInvalidRequest, "播放历史同步数据无效", nil)
	}
	for _, artwork := range []*string{&change.PosterURL, &change.BackdropURL, &change.TitleLogoURL} {
		*artwork = safeHistoryArtwork(*artwork)
	}
	change.PosterPath = safeTMDBImagePath(change.PosterPath)
	change.BackdropPath = safeTMDBImagePath(change.BackdropPath)
	change.EpisodeStillPath = safeTMDBImagePath(change.EpisodeStillPath)
	if change.SourceLocator != "" {
		parsed, err := url.Parse(change.SourceLocator)
		if err != nil || parsed.Scheme != "http" && parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Path != "" && parsed.Path != "/" || parsed.RawQuery != "" || parsed.Fragment != "" {
			return PlayerHistoryChange{}, appError(CodeInvalidRequest, "播放历史来源地址无效", nil)
		}
		change.SourceLocator = parsed.Scheme + "://" + parsed.Host
	}
	return change, nil
}

func validHistoryEpisodeFacts(season, episode *int) bool {
	return (season == nil || *season >= 0 && *season <= 10000) && (episode == nil || *episode > 0 && *episode <= 100000)
}

func playerHistoryChangeDTO(row models.PlayerPlaybackHistory) PlayerHistoryChange {
	return PlayerHistoryChange{
		SyncKey: row.SyncKey, HistoryIdentity: row.HistoryIdentity, SourceKind: row.SourceKind,
		SourceLocator: row.SourceLocator, SourceID: row.SourceID, LibraryID: row.LibraryID,
		ItemID: row.ItemID, ItemToken: row.ItemToken, MediaIdentity: row.MediaIdentity,
		Title: row.Title, DisplayTitle: row.DisplayTitle, DisplaySubtitle: row.DisplaySubtitle,
		SeriesTitle: row.SeriesTitle, EpisodeTitle: row.EpisodeTitle, SeasonNumber: cloneInt(row.SeasonNumber),
		EpisodeNumber: cloneInt(row.EpisodeNumber), StreamIdentity: row.StreamIdentity, MediaType: row.MediaType,
		PosterURL: row.PosterURL, BackdropURL: row.BackdropURL, TitleLogoURL: row.TitleLogoURL,
		PosterPath: row.PosterPath, BackdropPath: row.BackdropPath, EpisodeStillPath: row.EpisodeStillPath,
		Position: row.Position, Duration: cloneHistoryFloat64(row.Duration), Completed: row.Completed,
		Deleted: row.Deleted, UpdatedAt: row.ClientUpdatedAt, Revision: row.Revision,
	}
}

func playerHistoryWorkToken(libraryID uint, workToken string) string {
	return "work|" + strconv.FormatUint(uint64(libraryID), 10) + "|" + workToken
}

func playerHistoryEntryToken(libraryID uint, workToken string, entryID uint) string {
	return "entry|" + strconv.FormatUint(uint64(libraryID), 10) + "|" + workToken + "|" + strconv.FormatUint(uint64(entryID), 10)
}

func playerHistoryCanonicalIdentity(libraryID uint, workToken, kind string, season, episode *int, entryID uint) string {
	prefix := "server:v1:"
	library := strconv.FormatUint(uint64(libraryID), 10)
	if kind == "series" {
		if season != nil && episode != nil && *season >= 0 && *episode > 0 {
			return fmt.Sprintf("%sepisode:%s:%s:%d:%d", prefix, library, workToken, *season, *episode)
		}
		return fmt.Sprintf("%sepisode-entry:%s:%s:%d", prefix, library, workToken, entryID)
	}
	return prefix + "movie:" + library + ":" + workToken
}

func playerHistoryCanonicalSyncKey(identity string) string {
	sum := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(sum[:])
}

func playerHistoryEpisodeSubtitle(season, episode *int, episodeTitle string) string {
	episodeTitle = strings.TrimSpace(episodeTitle)
	if season == nil || episode == nil {
		return episodeTitle
	}
	label := fmt.Sprintf("S%02dE%02d", *season, *episode)
	if episodeTitle == "" {
		return label
	}
	return label + " · " + episodeTitle
}

func safeHistoryArtwork(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 2048 {
		return ""
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "http" && parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
		return ""
	}
	for key := range parsed.Query() {
		lower := strings.ToLower(key)
		if strings.Contains(lower, "token") || strings.Contains(lower, "key") || strings.Contains(lower, "auth") || strings.Contains(lower, "signature") || lower == "sig" || lower == "expires" {
			return ""
		}
	}
	return parsed.String()
}

func finiteNonNegative(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0
}

func isHex(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}
