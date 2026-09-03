package services

import (
	"errors"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/metadata/tmdb"
	"gorm.io/gorm"
)

type tmdbCollectionCandidate struct {
	metadata tmdb.Collection
	members  map[string]tmdbCollectionMember
}

type tmdbCollectionMember struct {
	workKey string
	movieID int64
}

// reconcileTMDBCollectionsTx projects TMDB belongs_to_collection metadata from
// the committed catalog view. Partial scans may prove additions but never
// absence; only a complete scan is allowed to remove unseen TMDB-owned rows.
func reconcileTMDBCollectionsTx(tx *gorm.DB, libraryID uint, partial bool, now time.Time) error {
	var entries []models.MediaLibraryEntry
	if err := tx.Where("library_id = ? AND media_type = ? AND match_status = ? AND recognition_id IS NOT NULL AND tmdb_id IS NOT NULL", libraryID, "movie", mediaRecognitionStatusMatched).Find(&entries).Error; err != nil {
		return err
	}
	recognitionIDs := make([]uint, 0, len(entries))
	seenRecognitionIDs := make(map[uint]struct{}, len(entries))
	for _, entry := range entries {
		if entry.RecognitionID != nil {
			if _, exists := seenRecognitionIDs[*entry.RecognitionID]; !exists {
				seenRecognitionIDs[*entry.RecognitionID] = struct{}{}
				recognitionIDs = append(recognitionIDs, *entry.RecognitionID)
			}
		}
	}
	recognitions := make(map[uint]models.MediaLibraryRecognition, len(recognitionIDs))
	if len(recognitionIDs) > 0 {
		var rows []models.MediaLibraryRecognition
		if err := tx.Where("id IN ?", recognitionIDs).Find(&rows).Error; err != nil {
			return err
		}
		for _, row := range rows {
			recognitions[row.ID] = row
		}
	}

	candidates := make(map[int64]*tmdbCollectionCandidate)
	seenWorks := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		if entry.RecognitionID == nil || entry.TMDBID == nil || entry.WorkKey == "" {
			continue
		}
		// Multiple physical versions of one work contribute one collection
		// member, matching Emby's PrimaryVersionId exclusion.
		if _, duplicate := seenWorks[entry.WorkKey]; duplicate {
			continue
		}
		recognition, exists := recognitions[*entry.RecognitionID]
		if !exists {
			continue
		}
		_, snapshot, err := decodeRecognitionMetadata(recognition.MetadataJSON)
		if err != nil || snapshot.Collection == nil || snapshot.Collection.TMDBID <= 0 || snapshot.Collection.Name == "" || snapshot.TMDBID != *entry.TMDBID {
			continue
		}
		seenWorks[entry.WorkKey] = struct{}{}
		candidate := candidates[snapshot.Collection.TMDBID]
		if candidate == nil {
			candidate = &tmdbCollectionCandidate{metadata: *snapshot.Collection, members: make(map[string]tmdbCollectionMember)}
			candidates[snapshot.Collection.TMDBID] = candidate
		}
		candidate.members[entry.WorkKey] = tmdbCollectionMember{workKey: entry.WorkKey, movieID: *entry.TMDBID}
	}

	var existingItems []models.PlayerMediaCollectionItem
	if err := tx.Table("player_media_collection_items AS item").
		Select("item.*").
		Joins("JOIN player_media_collections AS collection ON collection.id = item.collection_id").
		Where("item.library_id = ? AND item.origin = ? AND collection.source = ?", libraryID, models.PlayerMediaCollectionItemOriginTMDB, models.PlayerMediaCollectionSourceTMDB).
		Scan(&existingItems).Error; err != nil {
		return err
	}
	affected := make(map[string]struct{})
	seenItems := make(map[string]struct{})
	for _, item := range existingItems {
		affected[item.CollectionID] = struct{}{}
	}

	for collectionTMDBID, candidate := range candidates {
		var collection models.PlayerMediaCollection
		err := tx.Where("source = ? AND tmdb_collection_id = ?", models.PlayerMediaCollectionSourceTMDB, collectionTMDBID).First(&collection).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			id := collectionTMDBID
			collection = models.PlayerMediaCollection{
				ID: uuid.NewString(), Source: models.PlayerMediaCollectionSourceTMDB,
				Kind: models.PlayerMediaCollectionKindCollection, Name: candidate.metadata.Name,
				TMDBCollectionID: &id, PosterPath: candidate.metadata.PosterPath,
				BackdropPath: candidate.metadata.BackdropPath, Revision: 1,
				CreatedAt: now, UpdatedAt: now,
			}
			if err := tx.Create(&collection).Error; err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else if !collection.Locked && (collection.Name != candidate.metadata.Name || collection.PosterPath != candidate.metadata.PosterPath || collection.BackdropPath != candidate.metadata.BackdropPath) {
			updates := map[string]any{
				"name": candidate.metadata.Name, "poster_path": candidate.metadata.PosterPath,
				"backdrop_path": candidate.metadata.BackdropPath, "revision": gorm.Expr("revision + 1"), "updated_at": now,
			}
			if err := tx.Model(&collection).Updates(updates).Error; err != nil {
				return err
			}
		}
		affected[collection.ID] = struct{}{}
		for _, member := range candidate.members {
			identity := collection.ID + "\x00" + member.workKey
			seenItems[identity] = struct{}{}
			var item models.PlayerMediaCollectionItem
			err := tx.Where("collection_id = ? AND library_id = ? AND work_key = ? AND origin = ?", collection.ID, libraryID, member.workKey, models.PlayerMediaCollectionItemOriginTMDB).First(&item).Error
			if errors.Is(err, gorm.ErrRecordNotFound) {
				movieID := member.movieID
				item = models.PlayerMediaCollectionItem{CollectionID: collection.ID, LibraryID: libraryID, WorkKey: member.workKey, TMDBMovieID: &movieID, Origin: models.PlayerMediaCollectionItemOriginTMDB, CreatedAt: now, UpdatedAt: now}
				if err := tx.Create(&item).Error; err != nil {
					return err
				}
			} else if err != nil {
				return err
			} else if item.TMDBMovieID == nil || *item.TMDBMovieID != member.movieID {
				if err := tx.Model(&item).Updates(map[string]any{"tmdb_movie_id": member.movieID, "updated_at": now}).Error; err != nil {
					return err
				}
			}
		}
	}

	if !partial {
		for _, item := range existingItems {
			if _, exists := seenItems[item.CollectionID+"\x00"+item.WorkKey]; exists {
				continue
			}
			if err := tx.Delete(&item).Error; err != nil {
				return err
			}
		}
	}

	for collectionID := range affected {
		var items []models.PlayerMediaCollectionItem
		if err := tx.Where("collection_id = ? AND origin = ?", collectionID, models.PlayerMediaCollectionItemOriginTMDB).Find(&items).Error; err != nil {
			return err
		}
		distinct := make(map[string]struct{}, len(items))
		for _, item := range items {
			key := strconv.FormatUint(uint64(item.LibraryID), 10) + ":" + item.WorkKey
			if item.TMDBMovieID != nil && *item.TMDBMovieID > 0 {
				key = "tmdb:" + strconv.FormatInt(*item.TMDBMovieID, 10)
			}
			distinct[key] = struct{}{}
		}
		visible := len(distinct) >= 2
		var collection models.PlayerMediaCollection
		if err := tx.First(&collection, "id = ?", collectionID).Error; err != nil {
			return err
		}
		if collection.Visible != visible {
			if err := tx.Model(&collection).Updates(map[string]any{"visible": visible, "revision": gorm.Expr("revision + 1"), "updated_at": now}).Error; err != nil {
				return err
			}
		}
	}
	return nil
}
