package services

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

// The durable preview binds logical content, not the physical snapshot revision:
// compaction may change the latter without changing any deletion authority.
type catalogDeletionFence struct {
	Mode              string `json:"mode"`
	SourceEpoch       uint64 `json:"source_epoch"`
	SourceFingerprint string `json:"source_fingerprint"`
	ConfigFingerprint string `json:"config_fingerprint"`
	ContentRevision   uint64 `json:"content_revision"`
	BoundaryDigest    string `json:"boundary_digest"`
	ProfileRevision   uint64 `json:"profile_revision"`
	LibraryEnabled    bool   `json:"library_enabled"`
	StorageEnabled    bool   `json:"storage_enabled"`
}

type catalogDeletionWrite struct {
	Library   models.MediaLibrary
	Storage   models.Storage
	Head      models.CatalogHead
	Fence     catalogDeletionFence
	Entries   []models.MediaLibraryEntry
	Assets    []models.MediaLibrarySourceAsset
	WorkKey   string
	Candidate models.CatalogSnapshot
	Token     string
}

func catalogDeletionFenceTx(tx *gorm.DB, reader *CatalogReader, libraryID uint) (models.MediaLibrary, models.Storage, models.CatalogHead, catalogDeletionFence, error) {
	var library models.MediaLibrary
	var storage models.Storage
	var head models.CatalogHead
	var fence catalogDeletionFence
	if err := tx.First(&library, libraryID).Error; err != nil {
		return library, storage, head, fence, err
	}
	if err := tx.First(&storage, library.StorageID).Error; err != nil {
		return library, storage, head, fence, err
	}
	head, _ = reader.Head(libraryID)
	if head.Mode != "" && head.Mode != "legacy" && head.Mode != "versioned" {
		return library, storage, head, fence, ErrCatalogFence
	}
	fence = catalogDeletionFence{Mode: head.Mode, SourceEpoch: head.SourceEpoch, SourceFingerprint: head.SourceFingerprint, ConfigFingerprint: head.ConfigFingerprint, ContentRevision: library.ContentRevision, BoundaryDigest: catalogDeletionBoundaryDigest(library, storage), ProfileRevision: library.ProfileRevision, LibraryEnabled: library.Enabled, StorageEnabled: storage.Enabled}
	return library, storage, head, fence, nil
}

func (s *MediaLibraryService) captureCatalogDeletion(ctx context.Context, libraryID uint, workKey string, paths []string) (catalogDeletionWrite, error) {
	var out catalogDeletionWrite
	err := s.withCatalogRead(ctx, []uint{libraryID}, func(tx *gorm.DB, reader *CatalogReader) error {
		var err error
		out.Library, out.Storage, out.Head, out.Fence, err = catalogDeletionFenceTx(tx, reader, libraryID)
		if err != nil {
			return err
		}
		out.WorkKey = workKey
		query := reader.Entries().Where("library_id = ?", libraryID)
		if workKey != "" {
			query = query.Where("work_key = ?", workKey)
		} else if len(paths) > 0 {
			query = query.Where("relative_path IN ?", paths)
		} else {
			return nil
		}
		if err := query.Order("id").Limit(maxReorganizationItems + 1).Find(&out.Entries).Error; err != nil {
			return err
		}
		if len(out.Entries) > maxReorganizationItems {
			return ErrCatalogBudget
		}
		if workKey == "" && len(paths) > 0 {
			if err := reader.SourceAssets().Where("library_id=? AND relative_path IN ? AND active=?", libraryID, paths, true).Order("id").Limit(maxReorganizationItems + 1).Find(&out.Assets).Error; err != nil {
				return err
			}
			if len(out.Assets) > maxReorganizationItems {
				return ErrCatalogBudget
			}
		}
		return nil
	})
	return out, err
}

func validateCatalogDeletionTx(tx *gorm.DB, reader *CatalogReader, frozen catalogDeletionWrite, entries bool) error {
	_, _, _, fence, err := catalogDeletionFenceTx(tx, reader, frozen.Library.ID)
	if err != nil {
		return err
	}
	if fence != frozen.Fence {
		return ErrCatalogFence
	}
	if !entries {
		return nil
	}
	if len(frozen.Assets) > 0 {
		ids := make([]uint, 0, len(frozen.Assets))
		for _, asset := range frozen.Assets {
			if asset.ID == 0 || asset.LibraryID != frozen.Library.ID {
				return ErrCatalogInvalid
			}
			ids = append(ids, asset.ID)
		}
		var current []models.MediaLibrarySourceAsset
		if err := reader.SourceAssets().Where("library_id=? AND id IN ? AND active=?", frozen.Library.ID, ids, true).Order("id").Find(&current).Error; err != nil {
			return err
		}
		if catalogDeletionAssetDigest(current) != catalogDeletionAssetDigest(frozen.Assets) {
			return ErrCatalogFence
		}
	}
	ids := make([]uint, 0, len(frozen.Entries))
	for _, entry := range frozen.Entries {
		if entry.ID == 0 || entry.LibraryID != frozen.Library.ID {
			return ErrCatalogInvalid
		}
		ids = append(ids, entry.ID)
	}
	var current []models.MediaLibraryEntry
	query := reader.Entries().Where("library_id = ?", frozen.Library.ID)
	if frozen.WorkKey != "" {
		query = query.Where("work_key = ?", frozen.WorkKey)
	} else if len(ids) > 0 {
		query = query.Where("id IN ?", ids)
	} else {
		return nil
	}
	if err := query.Order("id").Limit(len(ids) + 1).Find(&current).Error; err != nil {
		return err
	}
	if catalogDeletionDigest(current) != catalogDeletionDigest(frozen.Entries) {
		return ErrCatalogFence
	}
	return nil
}

func (s *MediaLibraryService) validateCatalogDeletion(ctx context.Context, frozen catalogDeletionWrite, entries bool) error {
	return s.withCatalogRead(ctx, []uint{frozen.Library.ID}, func(tx *gorm.DB, reader *CatalogReader) error {
		return validateCatalogDeletionTx(tx, reader, frozen, entries)
	})
}

func (s *MediaLibraryService) abandonCatalogDeletion(write *catalogDeletionWrite) {
	if write.Token == "" || s.catalogStore == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.catalogStore.Abandon(ctx, write.Candidate.ID, write.Token)
	write.Token = ""
}

// Prepare and validate the complete derived budget BEFORE any physical deletion.
// A busy/full delta chain is a retryable refusal, never a partial file operation.
func (s *MediaLibraryService) prepareCatalogDeletion(ctx context.Context, write *catalogDeletionWrite) error {
	if write.Head.Mode != "versioned" {
		return s.validateCatalogDeletion(ctx, *write, true)
	}
	if s.catalogStore == nil {
		return ErrCatalogInvalid
	}
	var records []models.MediaLibraryRecognition
	err := s.withCatalogRead(ctx, []uint{write.Library.ID}, func(tx *gorm.DB, reader *CatalogReader) error {
		if err := validateCatalogDeletionTx(tx, reader, *write, true); err != nil {
			return err
		}
		write.Head, _ = reader.Head(write.Library.ID)
		ids, recs := make([]uint, 0, len(write.Entries)), make(map[uint]bool)
		for _, entry := range write.Entries {
			ids = append(ids, entry.ID)
			if entry.RecognitionID != nil {
				recs[*entry.RecognitionID] = true
			}
		}
		if len(recs) > 0 {
			recIDs := make([]uint, 0, len(recs))
			for id := range recs {
				recIDs = append(recIDs, id)
			}
			remaining := reader.Entries().Where("library_id=? AND id NOT IN ? AND recognition_id IS NOT NULL", write.Library.ID, ids).Select("recognition_id")
			if err := reader.Recognitions().Where("library_id=? AND id IN ? AND id NOT IN (?)", write.Library.ID, recIDs, remaining).Find(&records).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(write.Entries)+len(write.Assets) == 0 {
		return nil
	}
	candidate, token, err := s.catalogStore.BeginCandidate(ctx, CatalogCandidateInput{LibraryID: write.Library.ID, Kind: "delta", ExpectedRevision: write.Head.Revision, SourceEpoch: write.Head.SourceEpoch, SourceFingerprint: write.Head.SourceFingerprint, ConfigFingerprint: write.Head.ConfigFingerprint, LeaseDuration: 2 * time.Minute})
	if err != nil {
		return err
	}
	write.Candidate, write.Token = candidate, token
	batch := CatalogFactBatch{}
	var batchBytes int64
	flush := func() error {
		if len(batch.Entries)+len(batch.Recognitions)+len(batch.SourceAssets) == 0 {
			return nil
		}
		err := s.catalogStore.AppendBatch(ctx, candidate.ID, token, batch)
		batch = CatalogFactBatch{}
		batchBytes = 0
		return err
	}
	for _, entry := range write.Entries {
		fact := CatalogEntryFromLegacy(entry, nil)
		fact.Tombstone = true
		rowBytes := catalogBatchSize(CatalogFactBatch{Entries: []models.CatalogEntryFact{fact}})
		if len(batch.Entries)+len(batch.Recognitions) >= CatalogBatchRows || batchBytes+rowBytes > CatalogBatchBytes {
			if err := flush(); err != nil {
				return err
			}
		}
		batch.Entries = append(batch.Entries, fact)
		batchBytes += rowBytes
	}
	for _, record := range records {
		fact := CatalogRecognitionFromLegacy(record)
		fact.Tombstone = true
		rowBytes := catalogBatchSize(CatalogFactBatch{Recognitions: []models.CatalogRecognitionFact{fact}})
		if len(batch.Entries)+len(batch.Recognitions) >= CatalogBatchRows || batchBytes+rowBytes > CatalogBatchBytes {
			if err := flush(); err != nil {
				return err
			}
		}
		batch.Recognitions = append(batch.Recognitions, fact)
		batchBytes += rowBytes
	}
	for _, asset := range write.Assets {
		fact := models.CatalogSourceAssetFact{MediaLibrarySourceAsset: asset, Tombstone: true}
		rowBytes := catalogBatchSize(CatalogFactBatch{SourceAssets: []models.CatalogSourceAssetFact{fact}})
		if len(batch.Entries)+len(batch.Recognitions)+len(batch.SourceAssets) >= CatalogBatchRows || batchBytes+rowBytes > CatalogBatchBytes {
			if err := flush(); err != nil {
				return err
			}
		}
		batch.SourceAssets = append(batch.SourceAssets, fact)
		batchBytes += rowBytes
	}
	if err := flush(); err != nil {
		return err
	}
	if err := s.catalogStore.Seal(ctx, candidate.ID, token); err != nil {
		return err
	}
	return s.withCatalogRead(ctx, []uint{write.Library.ID}, func(tx *gorm.DB, reader *CatalogReader) error {
		if err := validateCatalogDeletionTx(tx, reader, *write, true); err != nil {
			return err
		}
		var prepared models.CatalogSnapshot
		if err := tx.First(&prepared, "id = ?", candidate.ID).Error; err != nil {
			return err
		}
		var totals struct {
			Rows  int64
			Bytes int64
			Count int64
		}
		if err := tx.Table("catalog_head_layers AS l").Joins("JOIN catalog_snapshots AS s ON s.id=l.snapshot_id").Where("l.library_id=? AND l.rank>0", write.Library.ID).Select("COALESCE(SUM(s.row_count),0) AS rows, COALESCE(SUM(s.byte_count),0) AS bytes, COUNT(*) AS count").Scan(&totals).Error; err != nil {
			return err
		}
		if totals.Count >= CatalogMaxDeltas || totals.Rows+prepared.RowCount > CatalogMaxDeltaRows || totals.Bytes+prepared.ByteCount > CatalogMaxDeltaBytes {
			return ErrCatalogBudget
		}
		return nil
	})
}

// Physical completion is checked by the caller from its exact durable manifest.
// The commit callback runs AFTER the head switch in the same rollback boundary.
func (s *MediaLibraryService) commitCatalogDeletion(ctx context.Context, write *catalogDeletionWrite, commit func(*gorm.DB) error) error {
	if write.Head.Mode == "versioned" && len(write.Entries)+len(write.Assets) > 0 {
		var current models.CatalogHead
		if err := s.withCatalogRead(ctx, []uint{write.Library.ID}, func(tx *gorm.DB, r *CatalogReader) error {
			if err := validateCatalogDeletionTx(tx, r, *write, true); err != nil {
				return err
			}
			current, _ = r.Head(write.Library.ID)
			return nil
		}); err != nil {
			return err
		}
		if current.Revision != write.Head.Revision {
			s.abandonCatalogDeletion(write)
			if err := s.prepareCatalogDeletion(ctx, write); err != nil {
				return err
			}
		}
	}
	run := func() error {
		return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			validate := func(tx *gorm.DB) error {
				reader, err := PinCatalogTx(tx, []uint{write.Library.ID})
				if err != nil {
					return err
				}
				return validateCatalogDeletionTx(tx, reader, *write, true)
			}
			if write.Head.Mode == "versioned" && len(write.Entries)+len(write.Assets) > 0 {
				if write.Token == "" {
					return ErrCatalogInvalid
				}
				if _, err := s.catalogStore.PublishTx(tx, write.Candidate.ID, write.Token, write.Head.Revision, validate); err != nil {
					return err
				}
			} else if err := validate(tx); err != nil {
				return err
			}
			return commit(tx)
		})
	}
	var err error
	if s.catalogStore != nil {
		err = s.catalogStore.Admission().WithForeground(ctx, run)
	} else {
		err = run()
	}
	if err == nil {
		write.Token = ""
	}
	return err
}

func catalogDeletionAssetDigest(assets []models.MediaLibrarySourceAsset) string {
	ordered := append([]models.MediaLibrarySourceAsset(nil), assets...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
	h := sha256.New()
	for _, a := range ordered {
		_, _ = fmt.Fprintf(h, "%d\x00%d\x00%s\x00%s\x00%s\x00%d\x00%d\x00%d\x00%s\n", a.ID, a.LibraryID, a.RelativePath, a.ProviderID, a.ParentProviderID, a.Size, a.ModifiedAt.UnixNano(), a.UpdatedAt.UnixNano(), a.HashHint)
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

func catalogDeletionWriteError(err error) error {
	if errors.Is(err, ErrCatalogBudget) {
		return appError(CodeConflict, "单次删除超过目录预算，或目录正在等待合并；请稍后重试或分批处理", err)
	}
	if errors.Is(err, ErrCatalogFence) {
		return appError(CodeConflict, "媒体目录已变化，请重新预览后删除", err)
	}
	return err
}
