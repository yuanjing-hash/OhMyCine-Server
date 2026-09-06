package services

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// PrepareCollections runs after Seal freezes E/R/A writes. Every batch has a
// durable work-key cursor and shares the candidate's lease/head ownership fence.
// It changes only private projections and invisible stable identity anchors.
func (s *CatalogSnapshotStore) PrepareCollections(ctx context.Context, id, token string) error {
	if s == nil || s.readDB == nil || s.writeDB == nil {
		return ErrCatalogInvalid
	}
	if err := s.writeCatalogGrowth(ctx, 1024, func(tx *gorm.DB) error {
		candidate, err := catalogOwnedCandidate(tx, id, token, "validating")
		if err != nil {
			return err
		}
		return tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&models.CatalogCollectionPreparation{SnapshotID: id, State: "preparing", InputRowCount: candidate.RowCount, InputByteCount: candidate.ByteCount}).Error
	}); err != nil {
		return err
	}
	for {
		var preparation models.CatalogCollectionPreparation
		var candidate models.CatalogSnapshot
		var keys []string
		var facts []models.CatalogCollectionMemberFact
		err := s.readDB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			var err error
			candidate, err = catalogOwnedCandidate(tx, id, token, "validating")
			if err != nil {
				return err
			}
			if err := tx.First(&preparation, "snapshot_id=?", id).Error; err != nil {
				return err
			}
			if preparation.State == "prepared" {
				return nil
			}
			old, err := PinCatalogTx(tx, []uint{candidate.LibraryID})
			if err != nil {
				return err
			}
			next := *old
			next.legacy = nil
			next.privateLayers = true
			next.layers = append([]models.CatalogHeadLayer(nil), old.layers...)
			if candidate.Kind == "base" {
				next.layers = nil
			}
			next.layers = append(next.layers, models.CatalogHeadLayer{LibraryID: candidate.LibraryID, Rank: len(next.layers), SnapshotID: candidate.ID})
			works := next.Entries().Select("DISTINCT work_key").Where("work_key <> ''")
			if candidate.Kind == "delta" {
				changedEntries := tx.Table("catalog_entry_facts").Select("id").Where("snapshot_id=?", id)
				changedRecognitions := tx.Table("catalog_recognition_facts").Select("id").Where("snapshot_id=?", id)
				affected := func(reader *CatalogReader) *gorm.DB {
					return reader.Entries().Select("work_key").Where("work_key <> '' AND (id IN (?) OR recognition_id IN (?))", changedEntries, changedRecognitions)
				}
				works = tx.Table("(?) AS affected", tx.Raw("SELECT work_key FROM (?) AS old_work UNION SELECT work_key FROM (?) AS new_work", affected(old), affected(&next)))
			}
			if err := works.Where("work_key > ?", preparation.AfterWork).Order("work_key").Limit(CatalogBatchRows).Pluck("work_key", &keys).Error; err != nil {
				return err
			}
			if len(keys) == 0 {
				return nil
			}
			facts, err = prepareCollectionFactsTx(tx, &next, candidate, keys)
			return err
		})
		if err != nil {
			return err
		}
		if preparation.State == "prepared" {
			return nil
		}
		var bytes int64
		for _, fact := range facts {
			bytes += collectionFactSize(fact)
		}
		if bytes > CatalogBatchBytes {
			return ErrCatalogBudget
		}
		if err := s.writeCatalogGrowth(ctx, CatalogBatchBytes, func(tx *gorm.DB) error {
			current, err := catalogOwnedCandidate(tx, id, token, "validating")
			if err != nil {
				return err
			}
			var marker models.CatalogCollectionPreparation
			if err := tx.First(&marker, "snapshot_id=?", id).Error; err != nil {
				return err
			}
			if marker.State != "preparing" || marker.AfterWork != preparation.AfterWork || marker.RowCount != preparation.RowCount || current.RowCount != marker.InputRowCount+marker.RowCount || current.ByteCount != marker.InputByteCount+marker.ByteCount {
				return ErrCatalogFence
			}
			if current.Kind == "delta" && (current.RowCount+int64(len(facts)) > CatalogMaxDeltaRows || current.ByteCount+bytes > CatalogMaxDeltaBytes) {
				return ErrCatalogBudget
			}
			if err := prepareCollectionIdentitiesTx(tx, facts); err != nil {
				return err
			}
			if len(facts) > 0 {
				if err := tx.CreateInBatches(facts, 100).Error; err != nil {
					return err
				}
				if err := tx.Model(&models.CatalogSnapshot{}).Where("id=?", id).Updates(map[string]any{"row_count": current.RowCount + int64(len(facts)), "byte_count": current.ByteCount + bytes}).Error; err != nil {
					return err
				}
			}
			updates := map[string]any{"row_count": marker.RowCount + int64(len(facts)), "byte_count": marker.ByteCount + bytes}
			if len(keys) == 0 {
				updates["state"] = "prepared"
			} else {
				updates["after_work"] = keys[len(keys)-1]
			}
			return tx.Model(&models.CatalogCollectionPreparation{}).Where("snapshot_id=?", id).Updates(updates).Error
		}); err != nil {
			return err
		}
		if len(keys) == 0 {
			return nil
		}
	}
}

func prepareCollectionIdentitiesTx(tx *gorm.DB, facts []models.CatalogCollectionMemberFact) error {
	seen := make(map[int64]bool, len(facts))
	ids := make([]int64, 0, len(facts))
	for _, fact := range facts {
		if !fact.Tombstone && !seen[fact.TMDBCollectionID] {
			seen[fact.TMDBCollectionID] = true
			ids = append(ids, fact.TMDBCollectionID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	var existing []models.CatalogCollectionIdentity
	if err := tx.Where("tmdb_collection_id IN ?", ids).Find(&existing).Error; err != nil {
		return err
	}
	for _, identity := range existing {
		delete(seen, identity.TMDBCollectionID)
	}
	if len(seen) == 0 {
		return nil
	}
	missing := make([]int64, 0, len(seen))
	for _, id := range ids {
		if seen[id] {
			missing = append(missing, id)
		}
	}
	var legacy []models.PlayerMediaCollection
	if err := tx.Select("id", "tmdb_collection_id").Where("source=? AND tmdb_collection_id IN ?", models.PlayerMediaCollectionSourceTMDB, missing).Find(&legacy).Error; err != nil {
		return err
	}
	oldIDs := make(map[int64]string, len(legacy))
	for _, collection := range legacy {
		if collection.TMDBCollectionID != nil {
			oldIDs[*collection.TMDBCollectionID] = collection.ID
		}
	}
	rows := make([]models.CatalogCollectionIdentity, 0, len(missing))
	for _, tmdbID := range missing {
		id := oldIDs[tmdbID]
		if id == "" {
			id = uuid.NewSHA1(uuid.NameSpaceURL, []byte("ohmycine:tmdb-collection:v1:"+strconv.FormatInt(tmdbID, 10))).String()
		}
		rows = append(rows, models.CatalogCollectionIdentity{TMDBCollectionID: tmdbID, CollectionID: id})
	}
	return tx.CreateInBatches(rows, 100).Error
}

func collectionFactSize(fact models.CatalogCollectionMemberFact) int64 {
	return 160 + int64(len(fact.WorkKey)+len(fact.Name)+len(fact.PosterPath)+len(fact.BackdropPath))
}

func prepareCollectionFactsTx(tx *gorm.DB, reader *CatalogReader, candidate models.CatalogSnapshot, keys []string) ([]models.CatalogCollectionMemberFact, error) {
	type row struct {
		WorkKey      string
		TMDBMovieID  int64
		MetadataJSON string
		UpdatedAt    time.Time
	}
	// One deterministic recognition per logical work; physical versions are
	// never memberships. Domain decoding remains outside the immediate writer.
	unique := tx.Table("(?) AS e", reader.Entries()).Select("DISTINCT e.work_key,e.tmdb_id AS tmdb_movie_id,r.id,r.metadata_json,r.updated_at").Joins("JOIN (?) AS r ON r.id=e.recognition_id AND r.library_id=e.library_id", reader.Recognitions()).Where("e.work_key IN ? AND e.media_type='movie' AND e.match_status=? AND e.tmdb_id>0", keys, mediaRecognitionStatusMatched)
	ranked := tx.Table("(?) AS unique_recognition", unique).Select("*, ROW_NUMBER() OVER (PARTITION BY work_key ORDER BY updated_at DESC,id DESC) AS work_rank")
	var rows []row
	if err := tx.Table("(?) AS ranked", ranked).Where("work_rank=1").Scan(&rows).Error; err != nil {
		return nil, err
	}
	byWork := make(map[string]models.CatalogCollectionMemberFact, len(rows))
	for _, row := range rows {
		_, snapshot, err := decodeRecognitionMetadata(row.MetadataJSON)
		if err != nil || snapshot.TMDBID != row.TMDBMovieID || snapshot.Collection == nil || snapshot.Collection.TMDBID <= 0 || strings.TrimSpace(snapshot.Collection.Name) == "" {
			continue
		}
		byWork[row.WorkKey] = models.CatalogCollectionMemberFact{SnapshotID: candidate.ID, LibraryID: candidate.LibraryID, WorkKey: row.WorkKey, TMDBCollectionID: snapshot.Collection.TMDBID, TMDBMovieID: row.TMDBMovieID, Name: safeLabel(snapshot.Collection.Name, 512), PosterPath: safeTMDBImagePath(snapshot.Collection.PosterPath), BackdropPath: safeTMDBImagePath(snapshot.Collection.BackdropPath), MetadataUpdatedAt: row.UpdatedAt}
	}
	facts := make([]models.CatalogCollectionMemberFact, 0, len(keys))
	for _, key := range keys {
		if fact, exists := byWork[key]; exists {
			facts = append(facts, fact)
		} else if candidate.Kind == "delta" {
			facts = append(facts, models.CatalogCollectionMemberFact{SnapshotID: candidate.ID, LibraryID: candidate.LibraryID, WorkKey: key, Tombstone: true})
		}
	}
	return facts, nil
}

func catalogCollectionIdentityTx(tx *gorm.DB, tmdbID int64) (string, error) {
	var identity models.CatalogCollectionIdentity
	err := tx.First(&identity, "tmdb_collection_id=?", tmdbID).Error
	if err == nil {
		return identity.CollectionID, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return "", err
	}
	var legacy models.PlayerMediaCollection
	err = tx.Select("id").Where("source=? AND tmdb_collection_id=?", models.PlayerMediaCollectionSourceTMDB, tmdbID).First(&legacy).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return "", err
	}
	id := legacy.ID
	if id == "" {
		id = uuid.NewSHA1(uuid.NameSpaceURL, []byte("ohmycine:tmdb-collection:v1:"+strconv.FormatInt(tmdbID, 10))).String()
	}
	identity = models.CatalogCollectionIdentity{TMDBCollectionID: tmdbID, CollectionID: id}
	if err := tx.Create(&identity).Error; err != nil {
		return "", err
	}
	return id, nil
}

func validateCatalogCollectionsPrepared(tx *gorm.DB, candidate models.CatalogSnapshot) error {
	var marker models.CatalogCollectionPreparation
	if err := tx.First(&marker, "snapshot_id=?", candidate.ID).Error; err != nil {
		return err
	}
	if marker.State != "prepared" || marker.InputRowCount+marker.RowCount != candidate.RowCount || marker.InputByteCount+marker.ByteCount != candidate.ByteCount {
		return ErrCatalogFence
	}
	return nil
}
