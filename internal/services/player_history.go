package services

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"net/url"
	"strings"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const playerHistorySyncLimit = 500

type PlayerHistoryChange struct {
	SyncKey        string   `json:"sync_key"`
	SourceKind     string   `json:"source_kind"`
	SourceLocator  string   `json:"source_locator,omitempty"`
	SourceID       string   `json:"source_id"`
	LibraryID      string   `json:"library_id,omitempty"`
	ItemID         string   `json:"item_id,omitempty"`
	MediaIdentity  string   `json:"media_identity"`
	Title          string   `json:"title"`
	StreamIdentity string   `json:"stream_identity,omitempty"`
	MediaType      string   `json:"media_type,omitempty"`
	PosterURL      string   `json:"poster_url,omitempty"`
	BackdropURL    string   `json:"backdrop_url,omitempty"`
	TitleLogoURL   string   `json:"title_logo_url,omitempty"`
	Position       float64  `json:"position"`
	Duration       *float64 `json:"duration,omitempty"`
	Completed      bool     `json:"completed"`
	Deleted        bool     `json:"deleted,omitempty"`
	UpdatedAt      int64    `json:"updated_at"`
	Revision       uint64   `json:"revision,omitempty"`
}

type PlayerHistorySyncResult struct {
	Cursor  uint64                `json:"cursor"`
	Changes []PlayerHistoryChange `json:"changes"`
}

type PlayerHistoryService struct{ db *gorm.DB }

func NewPlayerHistoryService(db *gorm.DB) *PlayerHistoryService { return &PlayerHistoryService{db: db} }

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
		for _, change := range normalized {
			var current models.PlayerPlaybackHistory
			err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("user_id = ? AND sync_key = ?", actor.User.ID, change.SyncKey).First(&current).Error
			if err != nil && err != gorm.ErrRecordNotFound {
				return err
			}
			if err == nil && (current.ClientUpdatedAt > change.UpdatedAt || current.ClientUpdatedAt == change.UpdatedAt && (current.Completed || current.Deleted) && !change.Completed && !change.Deleted) {
				continue
			}
			revision := models.PlayerPlaybackHistoryRevision{UserID: actor.User.ID, SyncKey: change.SyncKey, ChangedAt: time.Now().UTC()}
			if err := tx.Create(&revision).Error; err != nil {
				return err
			}
			now := time.Now().UTC()
			record := models.PlayerPlaybackHistory{UserID: actor.User.ID, SyncKey: change.SyncKey, SourceKind: change.SourceKind, SourceLocator: change.SourceLocator, SourceID: change.SourceID, LibraryID: change.LibraryID, ItemID: change.ItemID, MediaIdentity: change.MediaIdentity, Title: change.Title, StreamIdentity: change.StreamIdentity, MediaType: change.MediaType, PosterURL: change.PosterURL, BackdropURL: change.BackdropURL, TitleLogoURL: change.TitleLogoURL, Position: change.Position, Duration: change.Duration, Completed: change.Completed, Deleted: change.Deleted, ClientUpdatedAt: change.UpdatedAt, Revision: revision.ID, CreatedAt: now, UpdatedAt: now}
			if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "user_id"}, {Name: "sync_key"}}, DoUpdates: clause.AssignmentColumns([]string{"source_kind", "source_locator", "source_id", "library_id", "item_id", "media_identity", "title", "stream_identity", "media_type", "poster_url", "backdrop_url", "title_logo_url", "position", "duration", "completed", "deleted", "client_updated_at", "revision", "updated_at"})}).Create(&record).Error; err != nil {
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

func normalizePlayerHistoryChange(change PlayerHistoryChange) (PlayerHistoryChange, error) {
	change.SyncKey = strings.ToLower(strings.TrimSpace(change.SyncKey))
	change.SourceKind = strings.ToLower(strings.TrimSpace(change.SourceKind))
	change.SourceLocator = strings.TrimSpace(change.SourceLocator)
	change.SourceID = strings.TrimSpace(change.SourceID)
	change.LibraryID = strings.TrimSpace(change.LibraryID)
	change.ItemID = strings.TrimSpace(change.ItemID)
	change.MediaIdentity = strings.TrimSpace(change.MediaIdentity)
	change.Title = strings.TrimSpace(change.Title)
	change.StreamIdentity = strings.TrimSpace(change.StreamIdentity)
	change.MediaType = strings.ToLower(strings.TrimSpace(change.MediaType))
	if len(change.SyncKey) != 64 || !isHex(change.SyncKey) || change.SourceKind == "" || len(change.SourceKind) > 32 || change.SourceID == "" || len(change.SourceID) > 128 || change.MediaIdentity == "" || len(change.MediaIdentity) > 2048 || change.Title == "" || len(change.Title) > 512 || len(change.SourceLocator) > 512 || len(change.LibraryID) > 256 || len(change.ItemID) > 512 || len(change.StreamIdentity) > 2048 || change.UpdatedAt <= 0 || !finiteNonNegative(change.Position) || change.Duration != nil && !finiteNonNegative(*change.Duration) {
		return PlayerHistoryChange{}, appError(CodeInvalidRequest, "播放历史同步数据无效", nil)
	}
	for _, artwork := range []*string{&change.PosterURL, &change.BackdropURL, &change.TitleLogoURL} {
		*artwork = safeHistoryArtwork(*artwork)
	}
	if change.SourceLocator != "" {
		parsed, err := url.Parse(change.SourceLocator)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.Path != "" && parsed.Path != "/" || parsed.RawQuery != "" || parsed.Fragment != "" {
			return PlayerHistoryChange{}, appError(CodeInvalidRequest, "播放历史来源地址无效", nil)
		}
		change.SourceLocator = parsed.Scheme + "://" + parsed.Host
	}
	return change, nil
}

func playerHistoryChangeDTO(row models.PlayerPlaybackHistory) PlayerHistoryChange {
	return PlayerHistoryChange{SyncKey: row.SyncKey, SourceKind: row.SourceKind, SourceLocator: row.SourceLocator, SourceID: row.SourceID, LibraryID: row.LibraryID, ItemID: row.ItemID, MediaIdentity: row.MediaIdentity, Title: row.Title, StreamIdentity: row.StreamIdentity, MediaType: row.MediaType, PosterURL: row.PosterURL, BackdropURL: row.BackdropURL, TitleLogoURL: row.TitleLogoURL, Position: row.Position, Duration: row.Duration, Completed: row.Completed, Deleted: row.Deleted, UpdatedAt: row.ClientUpdatedAt, Revision: row.Revision}
}

func safeHistoryArtwork(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 2048 {
		return ""
	}
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil {
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
