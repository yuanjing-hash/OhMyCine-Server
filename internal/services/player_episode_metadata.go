package services

import (
	"context"
	"errors"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/metadata/tmdb"
	"gorm.io/gorm"
)

const (
	maxPlayerEpisodeMetadata        = 1000
	maxPlayerEpisodeSeasonsPerFetch = 8
)

type playerEpisodeKey struct {
	season  int
	episode int
}

type playerEpisodeMetadataSource struct {
	Recognition       models.MediaLibraryRecognition
	Library           models.MediaLibrary
	Storage           models.Storage
	Profile           models.MediaClassificationProfile
	Head              models.CatalogHead
	SourceFingerprint string
}

func playerEpisodeMetadataSourceTx(tx *gorm.DB, reader *CatalogReader, libraryID uint, workKey string) (playerEpisodeMetadataSource, error) {
	var source playerEpisodeMetadataSource
	if err := tx.First(&source.Library, libraryID).Error; err != nil {
		return source, err
	}
	if err := tx.First(&source.Storage, source.Library.StorageID).Error; err != nil {
		return source, err
	}
	if err := tx.First(&source.Profile, source.Library.ProfileID).Error; err != nil {
		return source, err
	}
	source.Head, _ = reader.Head(libraryID)
	source.SourceFingerprint = mediaLibraryScanSourceFingerprint(source.Library, source.Storage, source.Profile)
	err := reader.Recognitions().Joins("JOIN (?) AS media_library_entries ON media_library_entries.recognition_id=media_library_recognitions.id", reader.Entries()).Where("media_library_entries.library_id=? AND media_library_entries.work_key=?", libraryID, workKey).Order("media_library_recognitions.updated_at DESC,media_library_recognitions.id").First(&source.Recognition).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return source, nil
	}
	return source, err
}

func (s *MediaLibraryService) playerEpisodeMetadata(ctx context.Context, source playerEpisodeMetadataSource, entries []models.MediaLibraryEntry) map[playerEpisodeKey]tmdb.EpisodeSnapshot {
	libraryID := source.Library.ID
	wanted := make(map[playerEpisodeKey]struct{}, len(entries))
	for _, entry := range entries {
		season, episode := resolvedCatalogEpisodeFacts(entry)
		if episode == nil || *episode <= 0 {
			continue
		}
		key := playerEpisodeKey{episode: *episode}
		if season != nil {
			key.season = *season
		}
		wanted[key] = struct{}{}
	}
	if len(wanted) == 0 {
		return map[playerEpisodeKey]tmdb.EpisodeSnapshot{}
	}

	recognition := source.Recognition
	if recognition.ID == 0 {
		return map[playerEpisodeKey]tmdb.EpisodeSnapshot{}
	}
	classificationMetadata, snapshot, err := decodeRecognitionMetadata(recognition.MetadataJSON)
	if err != nil {
		s.log.Warn().Uint("library_id", libraryID).Str("error_code", "player_episode_snapshot_invalid").Msg("分集元数据快照无效")
		return map[playerEpisodeKey]tmdb.EpisodeSnapshot{}
	}
	if snapshot.TMDBID <= 0 && recognition.TMDBID != nil && recognition.MediaType == "tv" {
		snapshot.TMDBID = *recognition.TMDBID
		snapshot.MediaType = "tv"
		snapshot.Title = recognition.Title
	}
	library := source.Library
	metadataLanguage := strings.TrimSpace(library.MetadataLanguage)
	if snapshot.EpisodeLanguage != metadataLanguage {
		snapshot.EpisodeSnapshots = nil
		snapshot.EpisodeSeasons = nil
		snapshot.EpisodeLanguage = metadataLanguage
	}

	cache := make(map[playerEpisodeKey]tmdb.EpisodeSnapshot, min(len(snapshot.EpisodeSnapshots), maxPlayerEpisodeMetadata))
	for _, episode := range snapshot.EpisodeSnapshots {
		if len(cache) == maxPlayerEpisodeMetadata {
			break
		}
		if episode.SeasonNumber < 0 || episode.SeasonNumber > 10000 || episode.EpisodeNumber <= 0 || episode.EpisodeNumber > 100000 {
			continue
		}
		cache[playerEpisodeKey{season: episode.SeasonNumber, episode: episode.EpisodeNumber}] = episode
	}
	result := episodeMetadataForWanted(snapshot.EpisodeSnapshots, wanted)
	completed := make(map[int]struct{}, len(snapshot.EpisodeSeasons))
	for _, season := range snapshot.EpisodeSeasons {
		if season >= 0 && season <= 10000 {
			completed[season] = struct{}{}
		}
	}
	missingSet := make(map[int]struct{})
	for key := range wanted {
		if _, ok := completed[key.season]; !ok {
			missingSet[key.season] = struct{}{}
		}
	}
	if len(missingSet) == 0 || snapshot.TMDBID <= 0 || snapshot.MediaType != "tv" || s.metadata == nil || ctx.Err() != nil {
		return result
	}

	client, err := s.metadata.Client()
	if err != nil {
		return result
	}
	metadataCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	missing := make([]int, 0, len(missingSet))
	for season := range missingSet {
		missing = append(missing, season)
	}
	sort.Ints(missing)
	if len(missing) > maxPlayerEpisodeSeasonsPerFetch {
		missing = missing[:maxPlayerEpisodeSeasonsPerFetch]
	}

	changed := false
	for _, season := range missing {
		if metadataCtx.Err() != nil {
			break
		}
		episodes, fetchErr := client.GetTVSeasonEpisodes(metadataCtx, snapshot.TMDBID, season, metadataLanguage)
		if fetchErr != nil {
			if metadataCtx.Err() == nil {
				s.log.Debug().Uint("library_id", libraryID).Int("season", season).Str("error_code", "player_episode_tmdb_unavailable").Msg("TMDB 分集元数据暂时不可用")
			}
			continue
		}
		seasonCacheFits := len(cache) <= maxPlayerEpisodeMetadata
		if seasonCacheFits {
			additional := 0
			for _, episode := range episodes {
				key := playerEpisodeKey{season: episode.SeasonNumber, episode: episode.EpisodeNumber}
				if _, exists := cache[key]; !exists {
					additional++
				}
			}
			seasonCacheFits = len(cache)+additional <= maxPlayerEpisodeMetadata
		}
		if seasonCacheFits {
			completed[season] = struct{}{}
			changed = true
		}
		for _, episode := range episodes {
			key := playerEpisodeKey{season: episode.SeasonNumber, episode: episode.EpisodeNumber}
			if _, ok := wanted[key]; ok {
				result[key] = episode
			}
			if seasonCacheFits {
				cache[key] = episode
			}
		}
	}
	if !changed {
		return result
	}

	keys := make([]playerEpisodeKey, 0, len(cache))
	for key := range cache {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].season != keys[j].season {
			return keys[i].season < keys[j].season
		}
		return keys[i].episode < keys[j].episode
	})
	snapshot.EpisodeSnapshots = snapshot.EpisodeSnapshots[:0]
	for _, key := range keys {
		snapshot.EpisodeSnapshots = append(snapshot.EpisodeSnapshots, cache[key])
		if len(snapshot.EpisodeSnapshots) == maxPlayerEpisodeMetadata {
			break
		}
	}
	snapshot.EpisodeSeasons = snapshot.EpisodeSeasons[:0]
	for season := range completed {
		snapshot.EpisodeSeasons = append(snapshot.EpisodeSeasons, season)
	}
	sort.Ints(snapshot.EpisodeSeasons)
	if len(snapshot.EpisodeSeasons) > 64 {
		snapshot.EpisodeSeasons = snapshot.EpisodeSeasons[:64]
	}
	metadataJSON, marshalErr := marshalRecognitionMetadata(MediaRecognitionResult{Metadata: classificationMetadata, Snapshot: snapshot})
	if marshalErr == nil {
		if err := s.persistPlayerEpisodeMetadata(ctx, source, string(metadataJSON)); err != nil {
			s.log.Warn().Uint("library_id", libraryID).Str("error_code", "player_episode_snapshot_write_failed").Msg("保存分集元数据快照失败")
		}
	}
	return result
}

// Optional enrichment is fetched outside a read transaction. Its persistence
// is still a catalog write: a versioned library gets one normalized recognition
// delta, never an update to a legacy identity anchor or a sealed snapshot.
func (s *MediaLibraryService) persistPlayerEpisodeMetadata(ctx context.Context, source playerEpisodeMetadataSource, metadataJSON string) error {
	validate := func(tx *gorm.DB, reader *CatalogReader) error {
		head, _ := reader.Head(source.Library.ID)
		if source.Head.Mode == "versioned" {
			if head.Mode != "versioned" || head.Revision != source.Head.Revision || head.SourceEpoch != source.Head.SourceEpoch || head.SourceFingerprint != source.Head.SourceFingerprint || head.ConfigFingerprint != source.Head.ConfigFingerprint {
				return ErrCatalogFence
			}
		} else if head.Mode == "versioned" || head.Mode == "converting" {
			return ErrCatalogFence
		}
		var library models.MediaLibrary
		var storage models.Storage
		var profile models.MediaClassificationProfile
		if err := tx.First(&library, source.Library.ID).Error; err != nil {
			return err
		}
		if err := tx.First(&storage, library.StorageID).Error; err != nil {
			return err
		}
		if err := tx.First(&profile, library.ProfileID).Error; err != nil {
			return err
		}
		if !library.Enabled || !storage.Enabled || library.ProfileID != source.Library.ProfileID || library.ProfileRevision != source.Library.ProfileRevision || profile.Revision != source.Profile.Revision || profile.RulesJSON != source.Profile.RulesJSON || mediaLibraryScanSourceFingerprint(library, storage, profile) != source.SourceFingerprint {
			return ErrCatalogFence
		}
		var current models.MediaLibraryRecognition
		if err := reader.Recognitions().Where("library_id=? AND id=?", library.ID, source.Recognition.ID).First(&current).Error; err != nil {
			return err
		}
		if current.SourceKey != source.Recognition.SourceKey || current.LastGeneration != source.Recognition.LastGeneration || current.MetadataJSON != source.Recognition.MetadataJSON || current.ManualOverride != source.Recognition.ManualOverride || current.InputFingerprint != source.Recognition.InputFingerprint || current.ProfileID != source.Recognition.ProfileID || current.ProfileRevision != source.Recognition.ProfileRevision || !current.UpdatedAt.Equal(source.Recognition.UpdatedAt) {
			return ErrCatalogFence
		}
		return nil
	}
	if source.Head.Mode != "versioned" {
		return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			reader, err := PinCatalogTx(tx, []uint{source.Library.ID})
			if err != nil {
				return err
			}
			if err := validate(tx, reader); err != nil {
				return err
			}
			update := tx.Model(&models.MediaLibraryRecognition{}).Where("id=? AND library_id=? AND metadata_json=?", source.Recognition.ID, source.Library.ID, source.Recognition.MetadataJSON).UpdateColumn("metadata_json", metadataJSON)
			if update.Error != nil {
				return update.Error
			}
			if update.RowsAffected != 1 {
				return ErrCatalogFence
			}
			return nil
		})
	}
	next := source.Recognition
	next.MetadataJSON = metadataJSON
	var change models.MediaLibraryChange
	err := s.publishCatalogRecognitionDelta(ctx, source.Head, []models.MediaLibraryRecognition{next}, validate, func(tx *gorm.DB) error {
		if s.changes != nil {
			var err error
			change, err = s.changes.RecordTx(tx, source.Library.ID, source.Library.BaselineGeneration, models.MediaLibraryChangeMetadata, true)
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	if s.changes != nil && change.Revision > 0 {
		s.changes.NotifyCommitted(change.LibraryID, change.Revision)
	}
	return nil
}

func episodeMetadataForWanted(values []tmdb.EpisodeSnapshot, wanted map[playerEpisodeKey]struct{}) map[playerEpisodeKey]tmdb.EpisodeSnapshot {
	result := make(map[playerEpisodeKey]tmdb.EpisodeSnapshot, min(len(values), len(wanted)))
	for _, value := range values {
		key := playerEpisodeKey{season: value.SeasonNumber, episode: value.EpisodeNumber}
		if _, ok := wanted[key]; ok {
			result[key] = value
		}
	}
	return result
}

func preservePlayerEpisodeMetadata(next MediaRecognitionResult, storedJSON, metadataLanguage string) MediaRecognitionResult {
	if next.Snapshot.TMDBID <= 0 || next.Snapshot.MediaType != "tv" || strings.TrimSpace(storedJSON) == "" {
		return next
	}
	_, stored, err := decodeRecognitionMetadata(storedJSON)
	metadataLanguage = strings.TrimSpace(metadataLanguage)
	if err != nil || stored.TMDBID != next.Snapshot.TMDBID || stored.MediaType != next.Snapshot.MediaType || stored.EpisodeLanguage != metadataLanguage {
		return next
	}
	if len(stored.EpisodeSnapshots) > maxPlayerEpisodeMetadata {
		stored.EpisodeSnapshots = stored.EpisodeSnapshots[:maxPlayerEpisodeMetadata]
	}
	if len(stored.EpisodeSeasons) > 64 {
		stored.EpisodeSeasons = stored.EpisodeSeasons[:64]
	}
	next.Snapshot.EpisodeSnapshots = append([]tmdb.EpisodeSnapshot(nil), stored.EpisodeSnapshots...)
	next.Snapshot.EpisodeSeasons = append([]int(nil), stored.EpisodeSeasons...)
	next.Snapshot.EpisodeLanguage = metadataLanguage
	return next
}

func playerEpisodeFallbackTitle(entry models.MediaLibraryEntry, episode *int) string {
	normalized := strings.ReplaceAll(entry.RelativePath, "\\", "/")
	base := path.Base(normalized)
	stem := strings.TrimSpace(strings.TrimSuffix(base, path.Ext(base)))
	if stem != "" {
		return stem
	}
	if episode != nil {
		return "第 " + strconv.Itoa(*episode) + " 集"
	}
	return "未命名分集"
}
