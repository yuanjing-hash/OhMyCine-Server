package services

import (
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

// Snapshot selection precedes tombstone/domain filters. A work can move between
// collections; the newer work row masks the old membership irrespective of its
// collection ID. Only legacy libraries may use the old mutable member tables.
func catalogCollectionMembers(reader *CatalogReader) (*gorm.DB, error) {
	if len(reader.layers) > 0 {
		ids := make([]string, 0, len(reader.layers))
		for _, layer := range reader.layers {
			ids = append(ids, layer.SnapshotID)
		}
		var count int64
		if err := reader.tx.Model(&models.CatalogCollectionPreparation{}).Where("snapshot_id IN ? AND state=?", ids, "prepared").Count(&count).Error; err != nil {
			return nil, err
		}
		if count != int64(len(ids)) {
			return nil, ErrCatalogInvalid
		}
	}
	layers, args := reader.layerSQL()
	versioned := `WITH layers AS (` + layers + `) SELECT i.collection_id,f.library_id,f.work_key,f.tmdb_collection_id,f.tmdb_movie_id,f.name,f.poster_path,f.backdrop_path,f.metadata_updated_at AS updated_at FROM catalog_collection_member_facts f JOIN layers l ON l.snapshot_id=f.snapshot_id AND l.library_id=f.library_id JOIN catalog_collection_identities i ON i.tmdb_collection_id=f.tmdb_collection_id WHERE f.tombstone=0 AND NOT EXISTS (SELECT 1 FROM catalog_collection_member_facts newer JOIN layers nl ON nl.snapshot_id=newer.snapshot_id AND nl.library_id=newer.library_id WHERE newer.work_key=f.work_key AND newer.library_id=f.library_id AND nl.rank>l.rank)`
	legacy := reader.tx.Table("player_media_collection_items item").Select("c.id AS collection_id,item.library_id,item.work_key,c.tmdb_collection_id,COALESCE(item.tmdb_movie_id,0) AS tmdb_movie_id,c.name,c.poster_path,c.backdrop_path,c.updated_at").Joins("JOIN player_media_collections c ON c.id=item.collection_id").Where("item.library_id IN ? AND item.origin=? AND c.source=?", reader.legacy, models.PlayerMediaCollectionItemOriginTMDB, models.PlayerMediaCollectionSourceTMDB)
	union := reader.tx.Raw("SELECT * FROM (?) AS legacy_members UNION ALL SELECT * FROM (?) AS versioned_members", legacy, reader.tx.Raw(versioned, args...))
	// A stale manual/legacy row can never provide global visibility by itself.
	exists := reader.VisibleEntries().Select("1").Where("media_library_entries.library_id=members.library_id AND media_library_entries.work_key=members.work_key")
	return reader.tx.Table("(?) AS members", union).Where("EXISTS (?)", exists), nil
}

func (s *PlayerMediaStateService) automaticCollectionQueries(actor Actor) (*gorm.DB, *gorm.DB, error) {
	if s.reader == nil {
		return nil, nil, ErrCatalogInvalid
	}
	var ids []uint
	if err := s.db.Table("media_libraries l").Select("l.id").Joins("JOIN storages st ON st.id=l.storage_id").Where("l.enabled=? AND st.enabled=?", true, true).Order("l.id").Limit(2001).Pluck("l.id", &ids).Error; err != nil {
		return nil, nil, err
	}
	if len(ids) > 2000 {
		return nil, nil, ErrCatalogBudget
	}
	globalReader, err := PinCatalogTx(s.db, ids)
	if err != nil {
		return nil, nil, err
	}
	members, err := catalogCollectionMembers(globalReader)
	if err != nil {
		return nil, nil, err
	}
	global := s.db.Table("(?) AS global_members", members).Select("collection_id").Group("collection_id").Having("COUNT(DISTINCT CASE WHEN tmdb_movie_id>0 THEN 'tmdb:' || tmdb_movie_id ELSE 'work:' || library_id || ':' || work_key END)>=2")
	allowed, err := s.libraries.authorizedMediaLibraryIDsTx(s.db, actor, authz.PermissionMediaLibrariesRead, true)
	if err != nil {
		return nil, nil, err
	}
	eligible := s.db.Table("(?) AS eligible", members).Where("library_id IN ? AND collection_id IN (?)", allowed, global)
	return members, eligible, nil
}

const collectionProjectionColumns = "id,owner_id,source,kind,name,tmdb_collection_id,poster_path,backdrop_path,visible,locked,revision,created_at,updated_at"

// Metadata comes only from currently published member projections. The global
// distinct-film threshold is evaluated before actor filtering; one readable
// member is enough to expose an otherwise globally valid automatic collection.
func (s *PlayerMediaStateService) readableCollectionRows(actor Actor) (*gorm.DB, error) {
	members, eligible, err := s.automaticCollectionQueries(actor)
	if err != nil {
		return nil, err
	}
	counts := s.db.Table("(?) AS unique_members", s.db.Table("(?) AS eligible_members", eligible).Select("collection_id,library_id,work_key").Group("collection_id,library_id,work_key")).Select("collection_id,COUNT(*) AS item_count").Group("collection_id")
	// Stable source/work ordering avoids a legacy global row's reconciliation
	// timestamp overriding a converted library's committed metadata forever.
	ranked := s.db.Table("(?) AS committed_members", members).Select("*,ROW_NUMBER() OVER (PARTITION BY collection_id ORDER BY library_id,work_key,updated_at DESC) AS metadata_rank")
	automatic := s.db.Table("(?) AS p", ranked).Joins("JOIN (?) AS counts ON counts.collection_id=p.collection_id", counts).Joins("LEFT JOIN player_media_collections old ON old.id=p.collection_id AND old.source='tmdb'").Where("p.metadata_rank=1").Select("p.collection_id AS id,NULL AS owner_id,'tmdb' AS source,'collection' AS kind,CASE WHEN old.locked THEN old.name ELSE p.name END AS name,p.tmdb_collection_id,CASE WHEN old.locked THEN old.poster_path ELSE p.poster_path END AS poster_path,CASE WHEN old.locked THEN old.backdrop_path ELSE p.backdrop_path END AS backdrop_path,1 AS visible,COALESCE(old.locked,0) AS locked,COALESCE(old.revision,1) AS revision,COALESCE(old.created_at,p.updated_at) AS created_at,p.updated_at AS updated_at,counts.item_count")
	manualMembers, err := s.readableStateQuery(actor, "player_media_collection_items")
	if err != nil {
		return nil, err
	}
	manualCounts := s.db.Table("(?) AS unique_members", manualMembers.Select("collection_id,library_id,work_key").Group("collection_id,library_id,work_key")).Select("collection_id,COUNT(*) AS item_count").Group("collection_id")
	manual := s.db.Table("player_media_collections c").Joins("LEFT JOIN (?) AS counts ON counts.collection_id=c.id", manualCounts).Where("c.source='manual' AND c.owner_id=?", actor.User.ID).Select(prefixCatalogColumns(collectionProjectionColumns, "c") + ",COALESCE(counts.item_count,0) AS item_count")
	// SQLite expressions/UNION erase datetime affinity. These read DTOs need
	// no mutable row timestamps; do not scan expression strings into time.Time.
	return s.db.Table("(?) AS player_media_collections", s.db.Raw("SELECT * FROM (?) AS manual_collections UNION ALL SELECT * FROM (?) AS automatic_collections", manual, automatic)).Select("id,owner_id,source,kind,name,tmdb_collection_id,poster_path,backdrop_path,visible,locked,revision,item_count"), nil
}

func (s *PlayerMediaStateService) readableCollectionMemberRows(actor Actor, collection models.PlayerMediaCollection) (*gorm.DB, error) {
	if collection.Source != models.PlayerMediaCollectionSourceTMDB {
		query, err := s.readableStateQuery(actor, "player_media_collection_items")
		if err != nil {
			return nil, err
		}
		return query.Where("collection_id=?", collection.ID), nil
	}
	_, eligible, err := s.automaticCollectionQueries(actor)
	if err != nil {
		return nil, err
	}
	projected := s.db.Table("(?) AS automatic_member", eligible).Select("collection_id,library_id,work_key,tmdb_movie_id,'tmdb' AS origin,0 AS ordinal,0 AS id,updated_at AS created_at,updated_at")
	return s.db.Table("(?) AS player_media_collection_items", projected).Select("collection_id,library_id,work_key,tmdb_movie_id,origin,ordinal,id").Where("collection_id=?", collection.ID), nil
}
