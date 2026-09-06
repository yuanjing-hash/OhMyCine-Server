package services

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

// A pinned reader carries facts, not permission. Callers still authorize the
// library and source in the same transaction before filtering/counting/paging.
// It must not escape its transaction or survive network/streaming/file work.
type CatalogReader struct {
	tx            *gorm.DB
	legacy        []uint
	layers        []models.CatalogHeadLayer
	heads         map[uint]models.CatalogHead
	privateLayers bool
}

func PinCatalogTx(tx *gorm.DB, libraryIDs []uint) (*CatalogReader, error) {
	if err := requireCatalogTransaction(tx); err != nil {
		return nil, err
	}
	if len(libraryIDs) > 2000 {
		return nil, ErrCatalogBudget
	}
	r := &CatalogReader{tx: tx, heads: make(map[uint]models.CatalogHead)}
	if len(libraryIDs) == 0 {
		return r, nil
	}
	var retiring int64
	if err := tx.Model(&models.MediaLibraryRetirement{}).Where("library_id IN ? AND phase<>?", libraryIDs, "completed").Count(&retiring).Error; err != nil {
		return nil, err
	}
	if retiring != 0 {
		return nil, appError(CodeConflict, "媒体库正在移除索引", nil)
	}
	var heads []models.CatalogHead
	if err := tx.Where("library_id IN ?", libraryIDs).Find(&heads).Error; err != nil {
		return nil, err
	}
	versioned := make([]uint, 0, len(heads))
	for _, h := range heads {
		r.heads[h.LibraryID] = h
		if h.Mode == "versioned" {
			versioned = append(versioned, h.LibraryID)
		} else if h.Mode != "legacy" && h.Mode != "converting" {
			return nil, ErrCatalogInvalid
		}
	}
	seen := make(map[uint]bool)
	for _, id := range libraryIDs {
		if !seen[id] && r.heads[id].Mode != "versioned" {
			r.legacy = append(r.legacy, id)
		}
		seen[id] = true
	}
	if len(versioned) > 0 {
		if err := tx.Where("library_id IN ?", versioned).Order("library_id,rank").Find(&r.layers).Error; err != nil {
			return nil, err
		}
		var snapshots []models.CatalogSnapshot
		if err := tx.Table("catalog_snapshots").Joins("JOIN catalog_head_layers l ON l.snapshot_id=catalog_snapshots.id").Where("l.library_id IN ?", versioned).Select("catalog_snapshots.*").Find(&snapshots).Error; err != nil {
			return nil, err
		}
		byID := make(map[string]models.CatalogSnapshot, len(snapshots))
		for _, s := range snapshots {
			byID[s.ID] = s
		}
		counts := make(map[uint]int)
		for _, layer := range r.layers {
			h := r.heads[layer.LibraryID]
			s, ok := byID[layer.SnapshotID]
			if !ok || s.State != "published" || s.LibraryID != h.LibraryID || s.SourceEpoch != h.SourceEpoch || s.SourceFingerprint != h.SourceFingerprint || layer.Rank != counts[h.LibraryID] || layer.Rank > CatalogMaxDeltas || (layer.Rank == 0 && s.Kind != "base") || (layer.Rank > 0 && s.Kind != "delta") {
				return nil, ErrCatalogInvalid
			}
			counts[h.LibraryID]++
		}
		for _, id := range versioned {
			if counts[id] == 0 {
				return nil, ErrCatalogInvalid
			}
		}
	}
	return r, nil
}

func (s *CatalogSnapshotStore) Read(ctx context.Context, libraryIDs []uint, read func(*CatalogReader) error) error {
	if s.readDB == nil || read == nil {
		return ErrCatalogInvalid
	}
	return s.readDB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		reader, err := PinCatalogTx(tx, libraryIDs)
		if err != nil {
			return err
		}
		return read(reader)
	})
}

func (r *CatalogReader) Head(libraryID uint) (models.CatalogHead, bool) {
	h, ok := r.heads[libraryID]
	return h, ok
}

const catalogEntryColumns = "id,library_id,relative_path,provider_id,recognition_id,size,modified_at,media_type,title,work_key,series_title,season,episode,match_status,tmdb_id,release_year,match_confidence,recognition_error_code,category_name,matched_rule_id,last_generation,created_at,updated_at"
const catalogRecognitionColumns = "id,library_id,source_key,input_fingerprint,profile_id,profile_revision,status,error_code,media_type,title,release_year,tmdb_id,confidence,category_name,matched_rule_id,metadata_json,manual_override,last_generation,created_at,updated_at"
const catalogAssetColumns = "id,library_id,generation,provider_id,parent_provider_id,relative_path,name,extension,size,modified_at,hash_hint,active,created_at,updated_at"

// Array order is a versioned storage mask; append new fields, never reorder.
var catalogSharedFields = []struct{ entry, recognition string }{
	{"media_type", "media_type"}, {"title", "title"}, {"work_key", "work_key"}, {"series_title", "series_title"},
	{"match_status", "status"}, {"tmdb_id", "tmdb_id"}, {"release_year", "release_year"}, {"match_confidence", "confidence"},
	{"recognition_error_code", "error_code"}, {"category_name", "category_name"}, {"matched_rule_id", "matched_rule_id"},
}

// CatalogEntryFromLegacy records exactly which file fields differ from the
// shared recognition projection. Unchanged shared values subsequently follow a
// recognition-only delta. Actual entry paths/season/episode never come from work
// metadata. Use this at conversion and every new physical-entry preparation.
func CatalogEntryFromLegacy(entry models.MediaLibraryEntry, recognition *models.MediaLibraryRecognition) models.CatalogEntryFact {
	fact := models.CatalogEntryFact{MediaLibraryEntry: entry}
	if recognition == nil {
		return fact
	}
	work := recognitionWorkKey(MediaRecognitionResult{Status: recognition.Status, MediaType: recognition.MediaType, Title: recognition.Title, TMDBID: recognition.TMDBID}, recognition.SourceKey)
	series := ""
	if recognition.MediaType == "tv" {
		series = recognition.Title
	}
	actual := []any{entry.MediaType, entry.Title, entry.WorkKey, entry.SeriesTitle, entry.MatchStatus, entry.TMDBID, entry.ReleaseYear, entry.MatchConfidence, entry.RecognitionErrorCode, entry.CategoryName, entry.MatchedRuleID}
	shared := []any{recognition.MediaType, recognition.Title, work, series, recognition.Status, recognition.TMDBID, recognition.ReleaseYear, recognition.Confidence, recognition.ErrorCode, recognition.CategoryName, recognition.MatchedRuleID}
	for i := range actual {
		if !reflect.DeepEqual(actual[i], shared[i]) {
			fact.SharedOverrideMask |= 1 << i
		}
	}
	return fact
}

func CatalogRecognitionFromLegacy(record models.MediaLibraryRecognition) models.CatalogRecognitionFact {
	return models.CatalogRecognitionFact{MediaLibraryRecognition: record, WorkKey: recognitionWorkKey(MediaRecognitionResult{Status: record.Status, MediaType: record.MediaType, Title: record.Title, TMDBID: record.TMDBID}, record.SourceKey)}
}

func (r *CatalogReader) layerSQL() (string, []any) {
	if len(r.layers) == 0 {
		return "SELECT '' AS snapshot_id,0 AS rank,0 AS library_id WHERE 0", nil
	}
	if !r.privateLayers {
		ids := make([]uint, 0, len(r.heads))
		for id, h := range r.heads {
			if h.Mode == "versioned" {
				ids = append(ids, id)
			}
		}
		return "SELECT snapshot_id,rank,library_id FROM catalog_head_layers WHERE library_id IN ?", []any{ids}
	}
	parts := make([]string, 0, len(r.layers))
	args := make([]any, 0, len(r.layers)*3)
	for _, l := range r.layers {
		parts = append(parts, "SELECT ? AS snapshot_id,? AS rank,? AS library_id")
		args = append(args, l.SnapshotID, l.Rank, l.LibraryID)
	}
	return strings.Join(parts, " UNION ALL "), args
}

func (r *CatalogReader) effectiveSQL(table string) (string, []any) {
	if len(r.layers) == 1 {
		// With only one pinned base there cannot be a newer winner. A direct
		// snapshot prefix also lets keyset ordering stream from (snapshot,id)
		// instead of sorting the whole remaining suffix on every page.
		layer := r.layers[0]
		return "SELECT f.* FROM " + table + " f WHERE f.snapshot_id=? AND f.library_id=?", []any{layer.SnapshotID, layer.LibraryID}
	}
	singleLibrary := len(r.layers) > 1
	for i := 1; i < len(r.layers); i++ {
		if r.layers[i].LibraryID != r.layers[0].LibraryID {
			singleLibrary = false
			break
		}
	}
	if singleLibrary {
		// Each branch has a fixed snapshot prefix, so SQLite can merge ID-
		// ordered indexes for keyset pages instead of sorting every remaining
		// file after a layer-driven join. Winner selection still precedes all
		// domain and tombstone filters, including for private pinned readers.
		parts := make([]string, 0, len(r.layers))
		args := make([]any, 0, len(r.layers)*3)
		for _, layer := range r.layers {
			part := "SELECT f.* FROM " + table + " f WHERE f.snapshot_id=? AND f.library_id=?"
			args = append(args, layer.SnapshotID, layer.LibraryID)
			newer := make([]string, 0, len(r.layers))
			for _, other := range r.layers {
				if other.LibraryID == layer.LibraryID && other.Rank > layer.Rank {
					newer = append(newer, other.SnapshotID)
				}
			}
			if len(newer) > 0 {
				part += " AND NOT EXISTS (SELECT 1 FROM " + table + " newer WHERE newer.snapshot_id IN ? AND newer.id=f.id AND newer.library_id=f.library_id)"
				args = append(args, newer)
			}
			parts = append(parts, part)
		}
		return strings.Join(parts, " UNION ALL "), args
	}
	layers, args := r.layerSQL()
	// NOT EXISTS chooses the latest physical row BEFORE tombstone and domain
	// filters. Pushing a title/status filter into this subquery resurrects facts.
	return "WITH layers AS (" + layers + ") SELECT f.* FROM " + table + " f JOIN layers l ON l.snapshot_id=f.snapshot_id AND l.library_id=f.library_id WHERE NOT EXISTS (SELECT 1 FROM " + table + " newer JOIN layers nl ON nl.snapshot_id=newer.snapshot_id AND nl.library_id=newer.library_id WHERE newer.id=f.id AND nl.rank>l.rank)", args
}

func prefixCatalogColumns(columns, alias string) string {
	parts := strings.Split(columns, ",")
	for i, p := range parts {
		parts[i] = alias + "." + p
	}
	return strings.Join(parts, ",")
}

func (r *CatalogReader) unionQuery(table, columns, versioned string, args []any) *gorm.DB {
	// An empty UNION arm prevents SQLite from flattening the effective query
	// and can sort the entire catalog again for every keyset page.
	if len(r.legacy) == 0 {
		return r.tx.Table("(?) AS "+table, r.tx.Raw(versioned, args...))
	}
	if len(r.layers) == 0 {
		return r.tx.Table(table).Where(table+".library_id IN ?", r.legacy)
	}
	legacy := "SELECT " + columns + " FROM " + table + " WHERE 0"
	if len(r.legacy) > 0 {
		legacy = "SELECT " + columns + " FROM " + table + " WHERE library_id IN ?"
		args = append([]any{r.legacy}, args...)
	}
	return r.tx.Table("(?) AS "+table, r.tx.Raw(legacy+" UNION ALL "+versioned, args...))
}

func (r *CatalogReader) Recognitions() *gorm.DB {
	effective, args := r.effectiveSQL("catalog_recognition_facts")
	return r.unionQuery("media_library_recognitions", catalogRecognitionColumns, "SELECT "+prefixCatalogColumns(catalogRecognitionColumns, "r")+" FROM ("+effective+") r WHERE r.tombstone=0", args)
}

func (r *CatalogReader) SourceAssets() *gorm.DB {
	effective, args := r.effectiveSQL("catalog_source_asset_facts")
	return r.unionQuery("media_library_source_assets", catalogAssetColumns, "SELECT "+prefixCatalogColumns(catalogAssetColumns, "a")+" FROM ("+effective+") a WHERE a.tombstone=0", args)
}

// EntryCount counts physical versions across the pinned scope. It is only for
// unfiltered library totals: a work/title/status predicate still needs Entries
// and its effective shared recognition values before filtering.
func (r *CatalogReader) EntryCount() (int64, error) {
	var count int64
	err := r.entryIdentities().Count(&count).Error
	return count, err
}

// entryIdentities exposes only non-shared physical identity columns. Structural
// validation and ID continuity do not need recognition decoration; excluding
// metadata columns prevents using this optimization for title/status reads.
func (r *CatalogReader) entryIdentities() *gorm.DB {
	const columns = "id,library_id,relative_path,provider_id,recognition_id"
	effective, args := r.effectiveSQL("catalog_entry_facts")
	return r.unionQuery("media_library_entries", columns, "SELECT "+prefixCatalogColumns(columns, "e")+" FROM ("+effective+") e WHERE e.tombstone=0", args)
}

func (r *CatalogReader) Entries() *gorm.DB {
	entries, args := r.effectiveSQL("catalog_entry_facts")
	layers, rargs := r.layerSQL()
	args = append(args, rargs...)
	columns := strings.Split(catalogEntryColumns, ",")
	for i, column := range columns {
		columns[i] = "e." + column
		for bit, field := range catalogSharedFields {
			if field.entry != column {
				continue
			}
			shared := "r." + field.recognition
			if column == "series_title" {
				shared = "CASE WHEN r.media_type='tv' THEN r.title ELSE '' END"
			}
			columns[i] = fmt.Sprintf("CASE WHEN r.id IS NOT NULL AND (e.shared_override_mask & %d)=0 THEN %s ELSE e.%s END AS %s", 1<<bit, shared, column, column)
		}
	}
	// Resolve the recognition winner by exact (snapshot,id) lookups across the
	// bounded layers. Joining a complete effective R subquery materializes every
	// recognition for each E page, even when that page contains only 250 files.
	// Select the winner BEFORE filtering tombstones: otherwise an older R could
	// be resurrected after its latest row was deleted.
	winner := "SELECT rl.snapshot_id FROM (" + layers + ") rl JOIN catalog_recognition_facts rf ON rf.snapshot_id=rl.snapshot_id AND rf.library_id=rl.library_id AND rf.id=e.recognition_id WHERE rl.library_id=e.library_id ORDER BY rl.rank DESC LIMIT 1"
	versioned := "SELECT " + strings.Join(columns, ",") + " FROM (" + entries + ") e LEFT JOIN catalog_recognition_facts r ON r.snapshot_id=(" + winner + ") AND r.id=e.recognition_id AND r.library_id=e.library_id AND r.tombstone=0 WHERE e.tombstone=0"
	return r.unionQuery("media_library_entries", catalogEntryColumns, versioned, args)
}
