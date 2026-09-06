package services

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/database"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

type CatalogCompactionInput struct {
	LibraryID    uint
	JobID        *string
	JobLeaseHash string
	Force        bool
}

// CompactNextDue selects at most one library. The returned cursor advances even
// on compaction failure, preventing one space/fence-conflicted library from
// starving others. Zero means end of pass. Run in a dedicated maintenance loop,
// not the notification dispatcher: one large compaction has many short batches.
func (s *CatalogSnapshotStore) CompactNextDue(ctx context.Context, afterLibraryID uint) (nextAfter uint, compacted bool, err error) {
	var ids []uint
	err = s.readDB.WithContext(ctx).Table("catalog_heads h").Where(`h.mode='versioned' AND h.library_id>? AND EXISTS(SELECT 1 FROM catalog_head_layers l JOIN catalog_snapshots s ON s.id=l.snapshot_id WHERE l.library_id=h.library_id AND l.rank>0 GROUP BY l.library_id HAVING COUNT(*)>=? OR SUM(s.row_count)>=? OR SUM(s.byte_count)>=?)`, afterLibraryID, CatalogMaxDeltas/2, CatalogMaxDeltaRows/2, CatalogMaxDeltaBytes/2).Order("h.library_id").Limit(1).Pluck("h.library_id", &ids).Error
	if err != nil {
		return afterLibraryID, false, err
	}
	if len(ids) == 0 {
		return 0, false, nil
	}
	compacted, err = s.Compact(ctx, CatalogCompactionInput{LibraryID: ids[0]})
	return ids[0], compacted, err
}

func catalogCompactionOwnerIDs(candidateID string) []string {
	ids := make([]string, CatalogMaxDeltas+1)
	for rank := range ids {
		ids[rank] = candidateID + ":" + strconv.Itoa(rank)
	}
	return ids
}

// The persisted references encode the exact prefix, not a set of snapshots.
// Only this verified proof allows a compaction candidate to survive newer deltas.
func catalogCompactionPrefix(tx *gorm.DB, candidate models.CatalogSnapshot) ([]models.CatalogHeadLayer, error) {
	var refs []models.CatalogSnapshotReference
	if err := tx.Where("owner_kind=? AND owner_id IN ?", "compaction", catalogCompactionOwnerIDs(candidate.ID)).Find(&refs).Error; err != nil {
		return nil, err
	}
	if len(refs) == 0 {
		return nil, nil
	}
	if candidate.Kind != "base" || len(refs) > CatalogMaxDeltas+1 {
		return nil, ErrCatalogInvalid
	}
	prefix := make([]models.CatalogHeadLayer, len(refs))
	seen := make(map[int]bool)
	for _, ref := range refs {
		raw := strings.TrimPrefix(ref.OwnerID, candidate.ID+":")
		rank, err := strconv.Atoi(raw)
		if err != nil || rank < 0 || rank >= len(refs) || seen[rank] {
			return nil, ErrCatalogInvalid
		}
		seen[rank] = true
		prefix[rank] = models.CatalogHeadLayer{LibraryID: candidate.LibraryID, Rank: rank, SnapshotID: ref.SnapshotID}
	}
	var current []models.CatalogHeadLayer
	if err := tx.Where("library_id=?", candidate.LibraryID).Order("rank").Find(&current).Error; err != nil {
		return nil, err
	}
	if len(current) < len(prefix) {
		return nil, ErrCatalogFence
	}
	for index, layer := range prefix {
		if current[index] != layer {
			return nil, ErrCatalogFence
		}
	}
	return prefix, nil
}

func releaseCatalogCompactionRefsTx(tx *gorm.DB, candidateID string) error {
	return tx.Where("owner_kind=? AND owner_id IN ?", "compaction", catalogCompactionOwnerIDs(candidateID)).Delete(&models.CatalogSnapshotReference{}).Error
}

// Compact materializes the captured prefix and keeps newer verified deltas.
// It never invokes a scan/content/diagnosis hook or changes semantic generations.
func (s *CatalogSnapshotStore) Compact(ctx context.Context, input CatalogCompactionInput) (bool, error) {
	if input.LibraryID == 0 {
		return false, ErrCatalogInvalid
	}
	var head models.CatalogHead
	var layers []models.CatalogHeadLayer
	var sourceFingerprint string
	var totals struct{ Rows, Bytes int64 }
	err := s.Read(ctx, []uint{input.LibraryID}, func(reader *CatalogReader) error {
		var ok bool
		head, ok = reader.Head(input.LibraryID)
		if !ok || head.Mode != "versioned" {
			return ErrCatalogInvalid
		}
		layers = append([]models.CatalogHeadLayer(nil), reader.layers...)
		source, err := catalogRecognitionContextTx(ctx, reader.tx, reader, input.LibraryID)
		if err != nil {
			return err
		}
		sourceFingerprint = catalogRecognitionScanFingerprint(source.Library, source.Storage, source.Profile)
		return reader.tx.Table("catalog_snapshots s").Select("COALESCE(SUM(s.row_count),0) AS rows,COALESCE(SUM(s.byte_count),0) AS bytes").Joins("JOIN catalog_head_layers l ON l.snapshot_id=s.id").Where("l.library_id=? AND l.rank>0", input.LibraryID).Scan(&totals).Error
	})
	if err != nil {
		return false, err
	}
	if !input.Force && len(layers)-1 < CatalogMaxDeltas/2 && totals.Rows < CatalogMaxDeltaRows/2 && totals.Bytes < CatalogMaxDeltaBytes/2 {
		return false, nil
	}
	candidate, token, err := s.BeginCandidate(ctx, CatalogCandidateInput{LibraryID: head.LibraryID, Kind: "base", ExpectedRevision: head.Revision, SourceEpoch: head.SourceEpoch, SourceFingerprint: head.SourceFingerprint, ConfigFingerprint: head.ConfigFingerprint, JobID: input.JobID, JobLeaseHash: input.JobLeaseHash, LeaseDuration: 5 * time.Minute})
	if err != nil {
		return false, err
	}
	committed := false
	defer func() {
		if !committed {
			abandonCatalogScan(s, candidate.ID, token)
		}
	}()
	err = s.writeCatalogBatch(ctx, func(tx *gorm.DB) error {
		if _, err := catalogOwnedCandidate(tx, candidate.ID, token, "building"); err != nil {
			return err
		}
		for _, layer := range layers {
			if err := AcquireCatalogReferenceTx(tx, layer.SnapshotID, "compaction", candidate.ID+":"+strconv.Itoa(layer.Rank)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	if err := s.copyCatalogCompactionFacts(ctx, candidate, token, layers); err != nil {
		return false, err
	}
	if err := s.Seal(ctx, candidate.ID, token); err != nil {
		return false, err
	}
	err = s.writeCatalogBatch(ctx, func(tx *gorm.DB) error {
		_, err := s.PublishCompactionTx(tx, candidate.ID, token, func(tx *gorm.DB) error {
			reader, err := PinCatalogTx(tx, []uint{head.LibraryID})
			if err != nil {
				return err
			}
			current, err := catalogRecognitionContextTx(ctx, tx, reader, head.LibraryID)
			if err != nil {
				return err
			}
			if catalogRecognitionScanFingerprint(current.Library, current.Storage, current.Profile) != sourceFingerprint {
				return ErrCatalogFence
			}
			return nil
		})
		return err
	})
	committed = err == nil
	return committed, err
}

func (s *CatalogSnapshotStore) copyCatalogCompactionFacts(ctx context.Context, candidate models.CatalogSnapshot, token string, layers []models.CatalogHeadLayer) error {
	for _, table := range []string{"catalog_recognition_facts", "catalog_entry_facts", "catalog_source_asset_facts"} {
		for after := uint(0); ; {
			batch := CatalogFactBatch{}
			count := 0
			last := after
			err := s.readDB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
				if _, err := catalogOwnedCandidate(tx, candidate.ID, token, "building"); err != nil {
					return err
				}
				reader := &CatalogReader{tx: tx, layers: layers, privateLayers: true, heads: map[uint]models.CatalogHead{}}
				sql, args := reader.effectiveSQL(table)
				query := tx.Table("(?) AS effective", tx.Raw(sql, args...)).Where("tombstone=? AND id>?", false, after).Order("id").Limit(CatalogBatchRows)
				switch table {
				case "catalog_entry_facts":
					if err := query.Find(&batch.Entries).Error; err != nil {
						return err
					}
					count = len(batch.Entries)
					if count > 0 {
						last = batch.Entries[count-1].ID
					}
				case "catalog_recognition_facts":
					if err := query.Find(&batch.Recognitions).Error; err != nil {
						return err
					}
					count = len(batch.Recognitions)
					if count > 0 {
						last = batch.Recognitions[count-1].ID
					}
				case "catalog_source_asset_facts":
					if err := query.Find(&batch.SourceAssets).Error; err != nil {
						return err
					}
					count = len(batch.SourceAssets)
					if count > 0 {
						last = batch.SourceAssets[count-1].ID
					}
				}
				return nil
			})
			if err != nil {
				return err
			}
			if count == 0 {
				break
			}
			// Reuse the byte-aware scan batch writer, not effective projection
			// reconstruction: original SharedOverrideMask survives exactly.
			if err := s.appendCatalogFactBatches(ctx, candidate, token, batch); err != nil {
				return err
			}
			after = last
			if count < CatalogBatchRows {
				break
			}
		}
	}
	return nil
}

func (s *CatalogSnapshotStore) PublishCompactionTx(tx *gorm.DB, id, token string, guard func(*gorm.DB) error) (uint64, error) {
	if err := requireCatalogTransaction(tx); err != nil {
		return 0, err
	}
	if guard == nil {
		return 0, ErrCatalogInvalid
	}
	floor, err := database.ReadCatalogFormat(tx.Statement.Context, tx)
	if err != nil {
		return 0, err
	}
	if floor != CatalogFormat {
		return 0, ErrCatalogInvalid
	}
	var existing models.CatalogSnapshot
	if err := tx.First(&existing, "id=?", id).Error; err != nil {
		return 0, err
	}
	if existing.State == "published" && existing.Kind == "base" && token != "" && existing.OwnerTokenHash == catalogTokenHash(token) {
		return existing.PublishedRevision, nil
	}
	candidate, err := catalogOwnedCandidate(tx, id, token, "ready")
	if err != nil {
		return 0, err
	}
	prefix, err := catalogCompactionPrefix(tx, candidate)
	if err != nil {
		return 0, err
	}
	if len(prefix) == 0 {
		return 0, ErrCatalogInvalid
	}
	if err := validateCatalogCollectionsPrepared(tx, candidate); err != nil {
		return 0, err
	}
	reader, err := PinCatalogTx(tx, []uint{candidate.LibraryID})
	if err != nil {
		return 0, err
	}
	head, _ := reader.Head(candidate.LibraryID)
	if head.Mode != "versioned" {
		return 0, ErrCatalogInvalid
	}
	for _, layer := range reader.layers[len(prefix):] {
		var suffix models.CatalogSnapshot
		if err := tx.First(&suffix, "id=?", layer.SnapshotID).Error; err != nil {
			return 0, err
		}
		if suffix.Kind != "delta" || suffix.State != "published" || suffix.SourceEpoch != candidate.SourceEpoch || suffix.SourceFingerprint != candidate.SourceFingerprint || suffix.ConfigFingerprint != candidate.ConfigFingerprint || suffix.PublishedRevision <= candidate.ParentRevision {
			return 0, ErrCatalogFence
		}
	}
	if err := guard(tx); err != nil {
		return 0, err
	}
	revision := head.Revision + 1
	updated := tx.Model(&models.CatalogHead{}).Where("library_id=? AND revision=? AND mode=?", head.LibraryID, head.Revision, "versioned").Updates(map[string]any{"revision": revision, "updated_at": time.Now().UTC()})
	if updated.Error != nil {
		return 0, updated.Error
	}
	if updated.RowsAffected != 1 {
		return 0, ErrCatalogFence
	}
	// At most nine rows: delete/reinsert avoids rank uniqueness collisions.
	if err := tx.Where("library_id=?", head.LibraryID).Delete(&models.CatalogHeadLayer{}).Error; err != nil {
		return 0, err
	}
	layers := []models.CatalogHeadLayer{{LibraryID: head.LibraryID, Rank: 0, SnapshotID: id}}
	for _, suffix := range reader.layers[len(prefix):] {
		suffix.Rank = len(layers)
		layers = append(layers, suffix)
	}
	if err := tx.Create(&layers).Error; err != nil {
		return 0, err
	}
	if err := tx.Model(&models.CatalogSnapshot{}).Where("id=? AND state=?", id, "ready").Updates(map[string]any{"state": "published", "published_revision": revision, "updated_at": time.Now().UTC()}).Error; err != nil {
		return 0, err
	}
	if err := releaseCatalogCompactionRefsTx(tx, id); err != nil {
		return 0, err
	}
	return revision, nil
}
