package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"hash"
	"reflect"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

var catalogConversionKinds = []string{"recognition", "entry", "asset"}

func catalogConversionTable(kind string) (string, string) {
	switch kind {
	case "recognition":
		return "media_library_recognitions", "catalog_recognition_facts"
	case "entry":
		return "media_library_entries", "catalog_entry_facts"
	case "asset":
		return "media_library_source_assets", "catalog_source_asset_facts"
	default:
		return "", ""
	}
}

func catalogConversionPage(tx *gorm.DB, candidate models.CatalogSnapshot, kind string, after uint, rawCandidate bool) (CatalogFactBatch, int, uint, error) {
	legacyTable, factTable := catalogConversionTable(kind)
	if legacyTable == "" {
		return CatalogFactBatch{}, 0, after, ErrCatalogInvalid
	}
	batch := CatalogFactBatch{}
	query := tx.Where("library_id=? AND id>?", candidate.LibraryID, after).Order("id").Limit(CatalogBatchRows)
	if rawCandidate {
		query = query.Table(factTable).Where("snapshot_id=?", candidate.ID)
	} else {
		query = query.Table(legacyTable)
	}
	switch kind {
	case "recognition":
		if rawCandidate {
			if err := query.Find(&batch.Recognitions).Error; err != nil {
				return batch, 0, after, err
			}
		} else {
			var rows []models.MediaLibraryRecognition
			if err := query.Find(&rows).Error; err != nil {
				return batch, 0, after, err
			}
			for _, row := range rows {
				batch.Recognitions = append(batch.Recognitions, CatalogRecognitionFromLegacy(row))
			}
		}
		if len(batch.Recognitions) > 0 {
			after = batch.Recognitions[len(batch.Recognitions)-1].ID
		}
		return batch, len(batch.Recognitions), after, nil
	case "entry":
		if rawCandidate {
			if err := query.Find(&batch.Entries).Error; err != nil {
				return batch, 0, after, err
			}
		} else {
			var rows []models.MediaLibraryEntry
			if err := query.Find(&rows).Error; err != nil {
				return batch, 0, after, err
			}
			ids := make([]uint, 0, len(rows))
			for _, row := range rows {
				if row.RecognitionID != nil {
					ids = append(ids, *row.RecognitionID)
				}
			}
			byID := map[uint]models.MediaLibraryRecognition{}
			if len(ids) > 0 {
				var records []models.MediaLibraryRecognition
				if err := tx.Where("library_id=? AND id IN ?", candidate.LibraryID, ids).Find(&records).Error; err != nil {
					return batch, 0, after, err
				}
				for _, record := range records {
					byID[record.ID] = record
				}
			}
			for _, row := range rows {
				var record *models.MediaLibraryRecognition
				if row.RecognitionID != nil {
					value, ok := byID[*row.RecognitionID]
					if !ok {
						return batch, 0, after, ErrCatalogInvalid
					}
					record = &value
				}
				batch.Entries = append(batch.Entries, CatalogEntryFromLegacy(row, record))
			}
		}
		if len(batch.Entries) > 0 {
			after = batch.Entries[len(batch.Entries)-1].ID
		}
		return batch, len(batch.Entries), after, nil
	case "asset":
		if rawCandidate {
			if err := query.Find(&batch.SourceAssets).Error; err != nil {
				return batch, 0, after, err
			}
		} else {
			var rows []models.MediaLibrarySourceAsset
			if err := query.Find(&rows).Error; err != nil {
				return batch, 0, after, err
			}
			for _, row := range rows {
				batch.SourceAssets = append(batch.SourceAssets, models.CatalogSourceAssetFact{MediaLibrarySourceAsset: row})
			}
		}
		if len(batch.SourceAssets) > 0 {
			after = batch.SourceAssets[len(batch.SourceAssets)-1].ID
		}
		return batch, len(batch.SourceAssets), after, nil
	}
	return batch, 0, after, ErrCatalogInvalid
}

// Hash the actual GORM columns, including private provider/path/metadata fields
// and SharedOverrideMask. json.Marshal(fact) would intentionally yield {} because
// these models are forbidden as DTOs. Neither raw values nor digest input is logged.
func hashCatalogConversionFact(ctx context.Context, db *gorm.DB, digest hash.Hash, kind string, value any) error {
	statement := gorm.Statement{DB: db}
	if err := statement.Parse(value); err != nil {
		return err
	}
	fields := make([]any, 0, len(statement.Schema.Fields))
	for _, field := range statement.Schema.Fields {
		if field.DBName == "" || field.DBName == "snapshot_id" {
			continue
		}
		item, _ := field.ValueOf(ctx, reflect.ValueOf(value))
		fields = append(fields, []any{field.DBName, item})
	}
	encoded, err := json.Marshal([]any{kind, fields})
	if err != nil {
		return err
	}
	_, _ = digest.Write(encoded)
	_, _ = digest.Write([]byte{'\n'})
	return nil
}

func (s *CatalogSnapshotStore) catalogConversionDigest(ctx context.Context, candidate models.CatalogSnapshot, token string, input catalogConversionInput, rawCandidate bool) (string, int64, error) {
	digest := sha256.New()
	var rows int64
	for _, kind := range catalogConversionKinds {
		for after := uint(0); ; {
			var batch CatalogFactBatch
			count := 0
			err := s.readDB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
				if _, err := validateCatalogConversionTx(tx, candidate, token, input); err != nil {
					return err
				}
				var err error
				batch, count, after, err = catalogConversionPage(tx, candidate, kind, after, rawCandidate)
				return err
			})
			if err != nil {
				return "", 0, err
			}
			// Hash/JSON work is outside the read transaction, too.
			for _, row := range batch.Recognitions {
				if err := hashCatalogConversionFact(ctx, s.readDB, digest, kind, row); err != nil {
					return "", 0, err
				}
			}
			for _, row := range batch.Entries {
				if err := hashCatalogConversionFact(ctx, s.readDB, digest, kind, row); err != nil {
					return "", 0, err
				}
			}
			for _, row := range batch.SourceAssets {
				if err := hashCatalogConversionFact(ctx, s.readDB, digest, kind, row); err != nil {
					return "", 0, err
				}
			}
			rows += int64(count)
			if count < CatalogBatchRows {
				break
			}
			if err := s.RenewCandidate(ctx, candidate.ID, token, time.Minute); err != nil {
				return "", 0, err
			}
		}
	}
	return hex.EncodeToString(digest.Sum(nil)), rows, nil
}

func (s *CatalogSnapshotStore) captureCatalogConversionInput(ctx context.Context, candidate models.CatalogSnapshot, token string, input catalogConversionInput) error {
	var manifest models.CatalogConversionManifest
	if err := s.readDB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var err error
		manifest, err = validateCatalogConversionTx(tx, candidate, token, input)
		return err
	}); err != nil {
		return err
	}
	if manifest.State == "captured" || manifest.State == "verified" {
		return nil
	}
	if manifest.State != "capturing" || candidate.RowCount != 0 {
		return ErrCatalogInvalid
	}
	digest, rows, err := s.catalogConversionDigest(ctx, candidate, token, input, false)
	if err != nil {
		return err
	}
	return s.writeCatalogBatch(ctx, func(tx *gorm.DB) error {
		if _, err := validateCatalogConversionTx(tx, candidate, token, input); err != nil {
			return err
		}
		var actual models.CatalogSnapshot
		if err := tx.First(&actual, "id=?", candidate.ID).Error; err != nil {
			return err
		}
		if actual.RowCount != 0 {
			return ErrCatalogInvalid
		}
		updated := tx.Model(&models.CatalogConversionManifest{}).Where("snapshot_id=? AND state='capturing'", candidate.ID).Updates(map[string]any{"state": "captured", "input_digest": digest, "input_rows": rows, "updated_at": time.Now().UTC()})
		if updated.Error != nil {
			return updated.Error
		}
		if updated.RowsAffected != 1 {
			return ErrCatalogFence
		}
		return nil
	})
}

func (s *CatalogSnapshotStore) copyCatalogConversionFacts(ctx context.Context, candidate models.CatalogSnapshot, token string, input catalogConversionInput) error {
	for _, kind := range catalogConversionKinds {
		_, table := catalogConversionTable(kind)
		var after uint
		if err := s.readDB.WithContext(ctx).Table(table).Where("snapshot_id=?", candidate.ID).Select("COALESCE(MAX(id),0)").Scan(&after).Error; err != nil {
			return err
		}
		for {
			var batch CatalogFactBatch
			count := 0
			if err := s.readDB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
				if _, err := validateCatalogConversionTx(tx, candidate, token, input); err != nil {
					return err
				}
				var err error
				batch, count, after, err = catalogConversionPage(tx, candidate, kind, after, false)
				return err
			}); err != nil {
				return err
			}
			requests := make([]CatalogIdentityRequest, 0, count)
			for _, row := range batch.Recognitions {
				requests = append(requests, CatalogIdentityRequest{Kind: kind, SourceKey: row.SourceKey, ExistingID: row.ID})
			}
			for _, row := range batch.Entries {
				requests = append(requests, CatalogIdentityRequest{Kind: kind, SourceKey: row.RelativePath, ExistingID: row.ID})
			}
			for _, row := range batch.SourceAssets {
				requests = append(requests, CatalogIdentityRequest{Kind: kind, SourceKey: row.RelativePath, ExistingID: row.ID})
			}
			if count > 0 {
				if _, err := s.ResolveIdentities(ctx, candidate.ID, token, requests); err != nil {
					return err
				}
				if err := s.appendCatalogFactBatches(ctx, candidate, token, batch); err != nil {
					return err
				}
			}
			if count < CatalogBatchRows {
				break
			}
		}
	}
	return nil
}

func (s *CatalogSnapshotStore) verifyCatalogConversion(ctx context.Context, candidate models.CatalogSnapshot, token string, input catalogConversionInput) error {
	var manifest models.CatalogConversionManifest
	if err := s.readDB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var err error
		manifest, err = validateCatalogConversionTx(tx, candidate, token, input)
		return err
	}); err != nil {
		return err
	}
	if manifest.State != "captured" && manifest.State != "verified" {
		return ErrCatalogInvalid
	}
	for _, candidateFacts := range []bool{false, true} {
		digest, rows, err := s.catalogConversionDigest(ctx, candidate, token, input, candidateFacts)
		if err != nil {
			return err
		}
		if digest != manifest.InputDigest || rows != manifest.InputRows {
			return ErrCatalogInvalid
		}
	}
	return s.writeCatalogBatch(ctx, func(tx *gorm.DB) error {
		if _, err := validateCatalogConversionTx(tx, candidate, token, input); err != nil {
			return err
		}
		return tx.Model(&models.CatalogConversionManifest{}).Where("snapshot_id=?", candidate.ID).Updates(map[string]any{"state": "verified", "updated_at": time.Now().UTC()}).Error
	})
}
