package services

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/plugins/contract"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/metadata/tmdb"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func (s *PluginRepositoryService) OnlineHistoryAvailable() bool {
	return s.history != nil && s.history.plugins == s
}

type onlineHistoryIdentity struct{ library, work, segment, version, token string }
type onlineHistoryMetadata struct {
	Title        string `json:"title"`
	Subtitle     string `json:"subtitle,omitempty"`
	Kind         string `json:"kind"`
	SeriesTitle  string `json:"seriesTitle,omitempty"`
	EpisodeTitle string `json:"episodeTitle,omitempty"`
	Season       *int   `json:"season,omitempty"`
	Episode      *int   `json:"episode,omitempty"`
	Poster       string `json:"poster,omitempty"`
	Backdrop     string `json:"backdrop,omitempty"`
}

// Match encodeURIComponent used by Player, including the exact stable edition
// identity. Never synthesize an episode number or collapse multiple editions.
func encodeOnlineHistoryPart(value string) string {
	const hex = "0123456789ABCDEF"
	var result strings.Builder
	for _, b := range []byte(value) {
		if b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || strings.ContainsRune("-_.!~*'()", rune(b)) {
			result.WriteByte(b)
		} else {
			result.WriteByte('%')
			result.WriteByte(hex[b>>4])
			result.WriteByte(hex[b&15])
		}
	}
	return result.String()
}
func onlineHistoryToken(library, work, segment, version string) string {
	return "online-version|" + encodeOnlineHistoryPart(library) + "|" + encodeOnlineHistoryPart(work) + "|" + encodeOnlineHistoryPart(segment) + "|" + encodeOnlineHistoryPart(version)
}
func parseOnlineHistoryToken(token string) (onlineHistoryIdentity, error) {
	parts := strings.Split(token, "|")
	if len(parts) != 5 || parts[0] != "online-version" || len(token) > 512 {
		return onlineHistoryIdentity{}, appError(CodeInvalidRequest, "在线播放历史身份无效", nil)
	}
	values := make([]string, 4)
	for i, part := range parts[1:] {
		value, err := url.PathUnescape(part)
		if err != nil || !safeOnlineText(value, 512) || !utf8.ValidString(value) || encodeOnlineHistoryPart(value) != part {
			return onlineHistoryIdentity{}, appError(CodeInvalidRequest, "在线播放历史身份无效", nil)
		}
		values[i] = value
	}
	if _, err := uuid.Parse(values[0]); err != nil {
		return onlineHistoryIdentity{}, appError(CodeInvalidRequest, "在线播放历史媒体库无效", nil)
	}
	return onlineHistoryIdentity{library: values[0], work: values[1], segment: values[2], version: values[3], token: token}, nil
}
func isOnlineHistoryChange(change PlayerHistoryChange) bool {
	return change.SourceKind == "server" && (strings.HasPrefix(change.ItemToken, "online-version|") || strings.HasPrefix(change.ItemID, "online-version|") || strings.HasPrefix(change.HistoryIdentity, "online-version|") || strings.HasPrefix(change.LibraryID, "online-library|"))
}
func onlineChangeToken(change PlayerHistoryChange) string {
	if change.ItemToken != "" {
		return change.ItemToken
	}
	if change.ItemID != "" {
		return change.ItemID
	}
	return change.HistoryIdentity
}

// The established online boundary uses the functional read permission and the
// exact enabled virtual library/connection/package. Physical numeric library
// resource policies cannot represent these UUIDs; do not reinterpret them.
func (s *PluginRepositoryService) authorizedOnlineScope(actor Actor, libraryID string) (pluginCatalogueScope, error) {
	if !actor.Can(authz.PermissionMediaLibrariesRead) {
		return pluginCatalogueScope{}, appError(CodePermissionDenied, "无权使用在线媒体库", nil)
	}
	return s.catalogueScope(libraryID)
}

func (s *PluginRepositoryService) rememberOnlineDetail(ctx context.Context, scope pluginCatalogueScope, requested string, raw json.RawMessage) error {
	if len(raw) > 2<<20 {
		return appError(CodePluginResponseInvalid, "在线媒体详情过大", nil)
	}
	var work contract.MediaWork
	if json.Unmarshal(raw, &work) != nil || work.ID != requested || !safeOnlineText(work.Title, 512) || !safeOnlineText(work.Identity.Scheme, 128) || !safeOnlineText(work.Identity.Value, 512) || len(work.Segments) > 1000 || contract.ValidateDefaultSegment(work) != nil {
		return appError(CodePluginResponseInvalid, "在线媒体详情身份无效", nil)
	}
	switch work.Kind {
	case "movie", "series", "episode", "video", "live", "creator", "collection":
	default:
		return appError(CodePluginResponseInvalid, "在线媒体详情类型无效", nil)
	}
	now := time.Now().UTC()
	rows := make([]models.PluginOnlineMediaIdentity, 0)
	seen := map[string]bool{}
	for _, segment := range work.Segments {
		if !safeOnlineText(segment.ID, 512) || !safeOnlineText(segment.Title, 512) || len(segment.Versions) == 0 || len(segment.Versions) > 64 || !validHistoryEpisodeFacts(segment.SeasonNumber, segment.EpisodeNumber) {
			return appError(CodePluginResponseInvalid, "在线媒体分集身份无效", nil)
		}
		for _, version := range segment.Versions {
			if !safeOnlineText(version.ID, 512) || !safeOnlineText(version.Label, 256) {
				return appError(CodePluginResponseInvalid, "在线媒体版本身份无效", nil)
			}
			token := onlineHistoryToken(scope.library.ID, work.ID, segment.ID, version.ID)
			if len(token) > 512 || seen[token] || len(rows) >= 1000 {
				return appError(CodePluginResponseInvalid, "在线媒体详情身份超限或重复", nil)
			}
			seen[token] = true
			metadata := onlineHistoryMetadata{Title: work.Title, Subtitle: segment.Title + " · " + version.Label, Kind: work.Kind, Poster: durableCatalogueArtwork(work.PosterURL), Backdrop: durableCatalogueArtwork(work.BackdropURL)}
			for len(metadata.Subtitle) > 512 {
				_, size := utf8.DecodeLastRuneInString(metadata.Subtitle)
				metadata.Subtitle = metadata.Subtitle[:len(metadata.Subtitle)-size]
			}
			if work.Kind == "series" || work.Kind == "episode" {
				metadata.SeriesTitle = work.Title
				metadata.EpisodeTitle = segment.Title
				if validHistoryEpisodeFacts(segment.SeasonNumber, segment.EpisodeNumber) {
					metadata.Season = cloneInt(segment.SeasonNumber)
					metadata.Episode = cloneInt(segment.EpisodeNumber)
				}
			}
			encoded, _ := json.Marshal(metadata)
			rows = append(rows, models.PluginOnlineMediaIdentity{ID: catalogueHash(scope.key + "\x00" + token), LibraryID: scope.library.ID, ScopeKey: scope.key, ItemToken: token, MetadataJSON: string(encoded), ExpiresAt: now.Add(24 * time.Hour), UpdatedAt: now})
		}
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := checkOnlineScopeTx(tx, scope); err != nil {
			return err
		}
		if len(rows) > 0 {
			if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "id"}}, UpdateAll: true}).CreateInBatches(&rows, 100).Error; err != nil {
				return err
			}
		}
		if err := tx.Where("expires_at <= ?", now).Delete(&models.PluginOnlineMediaIdentity{}).Error; err != nil {
			return err
		}
		var discard []string
		if err := tx.Model(&models.PluginOnlineMediaIdentity{}).Where("library_id = ?", scope.library.ID).Order("updated_at DESC,id ASC").Offset(4000).Pluck("id", &discard).Error; err != nil {
			return err
		}
		if len(discard) > 0 {
			if err := tx.Where("id IN ?", discard).Delete(&models.PluginOnlineMediaIdentity{}).Error; err != nil {
				return err
			}
		}
		// Independent global count bound, including installations with many libraries.
		discard = nil
		if err := tx.Model(&models.PluginOnlineMediaIdentity{}).Order("updated_at DESC,id ASC").Offset(16000).Pluck("id", &discard).Error; err != nil {
			return err
		}
		if len(discard) > 0 {
			return tx.Where("id IN ?", discard).Delete(&models.PluginOnlineMediaIdentity{}).Error
		}
		return nil
	})
}
func checkOnlineScopeTx(tx *gorm.DB, scope pluginCatalogueScope) error {
	var library models.PluginOnlineLibrary
	var connection models.PluginConnection
	var installation models.PluginInstallation
	if err := tx.First(&library, "id = ? AND connection_id = ? AND plugin_id = ? AND enabled = ?", scope.library.ID, scope.connection.ID, scope.connection.PluginID, true).Error; err != nil {
		return appError(CodeNotFound, "在线媒体库不存在", nil)
	}
	if err := tx.First(&connection, "id = ? AND enabled = ?", scope.connection.ID, true).Error; err != nil {
		return appError(CodeNotFound, "在线媒体连接不存在", nil)
	}
	if err := tx.First(&installation, "plugin_id = ? AND status = ?", scope.connection.PluginID, models.PluginInstallationEnabled).Error; err != nil {
		return appError(CodeNotFound, "在线媒体插件未启用", nil)
	}
	if connection.PluginID != scope.connection.PluginID || connection.CredentialVersion != scope.connection.CredentialVersion || connection.ConfigJSON != scope.connection.ConfigJSON || installation.ActivePackageID != scope.packageID || installation.RuntimeGeneration != scope.generation {
		return appError(CodeConflict, "在线媒体来源已变更，请重试", nil)
	}
	return nil
}
func (s *PluginRepositoryService) rememberOnlinePlayback(ctx context.Context, actor Actor, scope pluginCatalogueScope, work, segment, version string) error {
	now := time.Now().UTC()
	receipt := models.PluginOnlinePlaybackReceipt{UserID: actor.User.ID, IdentityKey: catalogueHash(onlineHistoryToken(scope.library.ID, work, segment, version)), ScopeKey: scope.key, ExpiresAt: now.Add(24 * time.Hour)}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := checkOnlineScopeTx(tx, scope); err != nil {
			return err
		}
		if err := tx.Where("expires_at <= ?", now).Delete(&models.PluginOnlinePlaybackReceipt{}).Error; err != nil {
			return err
		}
		if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "user_id"}, {Name: "identity_key"}}, UpdateAll: true}).Create(&receipt).Error; err != nil {
			return err
		}
		if err := tx.Exec(`DELETE FROM plugin_online_playback_receipts WHERE rowid IN (SELECT rowid FROM plugin_online_playback_receipts WHERE user_id = ? ORDER BY expires_at DESC LIMIT -1 OFFSET 4096)`, actor.User.ID).Error; err != nil {
			return err
		}
		return tx.Exec(`DELETE FROM plugin_online_playback_receipts WHERE rowid IN (SELECT rowid FROM plugin_online_playback_receipts ORDER BY expires_at DESC LIMIT -1 OFFSET 16384)`).Error
	})
}

func (s *PlayerHistoryService) prepareOnlineHistoryChange(ctx context.Context, actor Actor, change PlayerHistoryChange) (PlayerHistoryChange, error) {
	if s.plugins == nil {
		return PlayerHistoryChange{}, appError(CodePluginRuntimeUnavailable, "在线播放历史服务不可用", nil)
	}
	identity, err := parseOnlineHistoryToken(onlineChangeToken(change))
	if err != nil {
		return PlayerHistoryChange{}, err
	}
	if change.LibraryID != "" && change.LibraryID != "online-library|"+encodeOnlineHistoryPart(identity.library) {
		return PlayerHistoryChange{}, appError(CodeInvalidRequest, "在线播放历史媒体库标识不一致", nil)
	}
	scope, err := s.plugins.authorizedOnlineScope(actor, identity.library)
	if err != nil {
		return PlayerHistoryChange{}, err
	}
	if change.Deleted {
		var owned models.PlayerPlaybackHistory
		if err := s.db.WithContext(ctx).First(&owned, "user_id = ? AND sync_key = ? AND canonical_identity = ? AND item_token = ?", actor.User.ID, playerHistoryCanonicalSyncKey(identity.token), identity.token, identity.token).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return PlayerHistoryChange{}, appError(CodeNotFound, "在线播放历史不存在", nil)
			}
			return PlayerHistoryChange{}, err
		}
		// Deleting one's existing canonical record does not need provider
		// availability, fresh entitlement, or a current ephemeral detail proof.
		originalKey, updatedAt := change.SyncKey, change.UpdatedAt
		change = playerHistoryChangeDTO(owned)
		change.Deleted, change.UpdatedAt, change.onlineOriginalKey, change.onlineScope = true, updatedAt, originalKey, &scope
		return change, nil
	}
	proofKey := catalogueHash(scope.key + "\x00" + identity.token)
	var proof models.PluginOnlineMediaIdentity
	query := s.db.WithContext(ctx)
	err = query.First(&proof, "id = ? AND expires_at > ?", proofKey, time.Now().UTC()).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		if _, detailErr := s.plugins.OnlineDetail(ctx, actor, identity.library, identity.work); detailErr != nil {
			return PlayerHistoryChange{}, detailErr
		}
		err = query.First(&proof, "id = ? AND expires_at > ?", proofKey, time.Now().UTC()).Error
	}
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return PlayerHistoryChange{}, appError(CodeInvalidRequest, "在线播放历史分集或版本不存在", nil)
		}
		return PlayerHistoryChange{}, err
	}
	// Positive progress backed by exact detail permits completed local-offline
	// playback to sync. A zero-position start requires a successful live plan;
	// a denied/failed playback cannot create a watched record.
	if !change.Deleted && change.Position <= 0 {
		var count int64
		if err := query.Model(&models.PluginOnlinePlaybackReceipt{}).Where("user_id = ? AND identity_key = ? AND scope_key = ? AND expires_at > ?", actor.User.ID, catalogueHash(identity.token), scope.key, time.Now().UTC()).Count(&count).Error; err != nil {
			return PlayerHistoryChange{}, err
		}
		if count != 1 {
			return PlayerHistoryChange{}, appError(CodeInvalidRequest, "尚未成功开始此在线播放", nil)
		}
	}
	var metadata onlineHistoryMetadata
	if json.Unmarshal([]byte(proof.MetadataJSON), &metadata) != nil || !safeOnlineText(metadata.Title, 512) {
		return PlayerHistoryChange{}, appError(CodePluginResponseInvalid, "在线播放历史元数据无效", nil)
	}
	// Private preparation fence: not serialized into any public DTO.
	change.onlineScope = &scope
	change.onlineOriginalKey = change.SyncKey
	change.SyncKey = playerHistoryCanonicalSyncKey(identity.token)
	change.HistoryIdentity = identity.token
	change.LibraryID = "online-library|" + encodeOnlineHistoryPart(identity.library)
	change.ItemID = identity.token
	change.ItemToken = identity.token
	change.MediaIdentity = identity.token
	change.StreamIdentity = identity.token
	change.SourceName = scope.manifest.Name
	change.SourceID, change.SourceLocator = scope.library.ID, ""
	change.Title = metadata.Title
	change.DisplayTitle = metadata.Title
	change.DisplaySubtitle = metadata.Subtitle
	change.SeriesTitle = metadata.SeriesTitle
	change.EpisodeTitle = metadata.EpisodeTitle
	change.SeasonNumber = cloneInt(metadata.Season)
	change.EpisodeNumber = cloneInt(metadata.Episode)
	change.MediaType = metadata.Kind
	if metadata.SeriesTitle != "" {
		change.MediaType = "episode"
	}
	change.PosterURL, change.BackdropURL, change.TitleLogoURL = durableCatalogueArtwork(metadata.Poster), durableCatalogueArtwork(metadata.Backdrop), ""
	change.PosterPath, change.BackdropPath, change.EpisodeStillPath = "", "", ""
	return change, nil
}

// Persist only official unsigned public artwork descriptors in the user row,
// then mint a current Host lease on each read. Old package/account revisions
// do not erase history; the current enabled scope/grants still gate projection.
func (s *PlayerHistoryService) projectOnlineHistoryArtwork(ctx context.Context, actor Actor, change PlayerHistoryChange, imageClient *tmdb.Client) PlayerHistoryChange {
	if !isOnlineHistoryChange(change) {
		return playerHistoryArtworkDTO(change, imageClient)
	}
	poster, backdrop := durableCatalogueArtwork(change.PosterURL), durableCatalogueArtwork(change.BackdropURL)
	change.PosterURL, change.BackdropURL, change.TitleLogoURL, change.EpisodeStillURL = "", "", "", ""
	if s.plugins == nil || change.Deleted {
		return change
	}
	identity, err := parseOnlineHistoryToken(change.ItemToken)
	if err != nil {
		return change
	}
	scope, err := s.plugins.authorizedOnlineScope(actor, identity.library)
	if err != nil {
		return change
	}
	count := 0
	change.PosterURL = s.plugins.registerOnlineArtwork(ctx, scope.connection.PluginID, scope.connection.ID, poster, &count)
	change.BackdropURL = s.plugins.registerOnlineArtwork(ctx, scope.connection.PluginID, scope.connection.ID, backdrop, &count)
	return change
}
func (s *PlayerHistoryService) commitOnlineHistoryChange(tx *gorm.DB, actor Actor, change PlayerHistoryChange) error {
	if change.onlineScope == nil || !actor.Can(authz.PermissionMediaLibrariesRead) {
		return appError(CodePermissionDenied, "无权同步在线播放历史", nil)
	}
	if err := checkOnlineScopeTx(tx, *change.onlineScope); err != nil {
		return err
	}
	if change.onlineOriginalKey != "" && change.onlineOriginalKey != change.SyncKey {
		legacy := change
		legacy.SyncKey = change.onlineOriginalKey
		legacy.Deleted = true
		if _, err := upsertPlayerHistoryChange(tx, actor.User.ID, legacy); err != nil {
			return err
		}
	}
	_, err := upsertPlayerHistoryChange(tx, actor.User.ID, change)
	return err
}

func onlineHistoryAvailableTx(tx *gorm.DB, actor Actor, row models.PlayerPlaybackHistory) (bool, error) {
	if !actor.Can(authz.PermissionMediaLibrariesRead) {
		return false, nil
	}
	identity, err := parseOnlineHistoryToken(row.ItemToken)
	if err != nil || row.HistoryIdentity != identity.token || row.SyncKey != playerHistoryCanonicalSyncKey(identity.token) || row.LibraryID != "online-library|"+encodeOnlineHistoryPart(identity.library) {
		return false, nil
	}
	var count int64
	err = tx.Table("plugin_online_libraries AS l").Joins("JOIN plugin_connections AS c ON c.id = l.connection_id AND c.plugin_id = l.plugin_id AND c.enabled = ?", true).Joins("JOIN plugin_installations AS i ON i.plugin_id = l.plugin_id AND i.status = ?", models.PluginInstallationEnabled).Joins("JOIN plugin_packages AS p ON p.id = i.active_package_id AND p.plugin_id = i.plugin_id").Where("l.id = ? AND l.enabled = ?", identity.library, true).Count(&count).Error
	return count == 1, err
}

func (s *PluginRepositoryService) saveOnlineProgress(ctx context.Context, actor Actor, libraryID, work, segment, version, event string, position float64, duration *float64, occurredAt string) error {
	if s.history == nil {
		return appError(CodePluginRuntimeUnavailable, "在线播放历史服务不可用", nil)
	}
	updatedAt := time.Now().UnixMilli()
	if occurredAt != "" {
		parsed, err := time.Parse(time.RFC3339Nano, occurredAt)
		if err != nil {
			return appError(CodeInvalidRequest, "在线播放进度时间无效", nil)
		}
		updatedAt = parsed.UnixMilli()
	}
	token := onlineHistoryToken(libraryID, work, segment, version)
	change := PlayerHistoryChange{SyncKey: playerHistoryCanonicalSyncKey(token), HistoryIdentity: token, SourceKind: "server", SourceID: libraryID, LibraryID: "online-library|" + encodeOnlineHistoryPart(libraryID), ItemToken: token, ItemID: token, MediaIdentity: token, Title: "在线媒体", Position: position, Duration: duration, Completed: event == "completed", UpdatedAt: updatedAt}
	normalized, err := normalizePlayerHistoryChange(change)
	if err != nil {
		return err
	}
	if updatedAt > time.Now().Add(playerHistoryFutureTolerance).UnixMilli() {
		return appError(CodeHistoryClockAhead, "设备时间明显超前，请校准设备时间后重试", nil)
	}
	normalized, err = s.history.prepareOnlineHistoryChange(ctx, actor, normalized)
	if err != nil {
		return err
	}
	// Progress acknowledgement does not enumerate or fetch artwork for the user's
	// entire sync delta. The separate sync endpoint supplies those user-owned rows.
	return withForegroundTransaction(ctx, s.db, s.history.writeAdmission, func(tx *gorm.DB) error { return s.history.commitOnlineHistoryChange(tx, actor, normalized) })
}
