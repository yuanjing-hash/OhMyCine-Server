package services

import (
	"context"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

// Large recognition backlogs create one hidden base instead of repeatedly
// compacting the whole catalog after every few file-link deltas. Copy unchanged
// RAW facts so file-specific masks, companions and already recognized works
// survive byte-for-byte. Every read is short and every write remains bounded.
func (s *MediaLibraryService) copyCatalogRecognitionBase(ctx context.Context, candidate models.CatalogSnapshot, token string, expected models.CatalogHead, changes CatalogFactBatch) error {
	entries := make(map[uint]models.CatalogEntryFact, len(changes.Entries))
	records := make(map[uint]models.CatalogRecognitionFact, len(changes.Recognitions))
	assets := make(map[uint]models.CatalogSourceAssetFact, len(changes.SourceAssets))
	for _, row := range changes.Entries {
		entries[row.ID] = row
	}
	for _, row := range changes.Recognitions {
		records[row.ID] = row
	}
	for _, row := range changes.SourceAssets {
		assets[row.ID] = row
	}
	for _, table := range []string{"catalog_recognition_facts", "catalog_entry_facts", "catalog_source_asset_facts"} {
		for after := uint(0); ; {
			batch := CatalogFactBatch{}
			count, last := 0, after
			err := s.withCatalogRead(ctx, []uint{expected.LibraryID}, func(tx *gorm.DB, reader *CatalogReader) error {
				head, ok := reader.Head(expected.LibraryID)
				if !ok || !catalogSameHead(expected, head) {
					return ErrCatalogFence
				}
				sql, args := reader.effectiveSQL(table)
				query := tx.Table("(?) AS f", tx.Raw(sql, args...)).Where("id>? AND tombstone=0", after).Order("id").Limit(CatalogBatchRows)
				switch table {
				case "catalog_entry_facts":
					var rows []models.CatalogEntryFact
					if err := query.Find(&rows).Error; err != nil {
						return err
					}
					count = len(rows)
					for _, row := range rows {
						last = row.ID
						if changed, ok := entries[row.ID]; ok {
							row = changed
							delete(entries, row.ID)
						}
						if !row.Tombstone {
							batch.Entries = append(batch.Entries, row)
						}
					}
				case "catalog_recognition_facts":
					var rows []models.CatalogRecognitionFact
					if err := query.Find(&rows).Error; err != nil {
						return err
					}
					count = len(rows)
					for _, row := range rows {
						last = row.ID
						if changed, ok := records[row.ID]; ok {
							row = changed
							delete(records, row.ID)
						}
						if !row.Tombstone {
							batch.Recognitions = append(batch.Recognitions, row)
						}
					}
				case "catalog_source_asset_facts":
					var rows []models.CatalogSourceAssetFact
					if err := query.Find(&rows).Error; err != nil {
						return err
					}
					count = len(rows)
					for _, row := range rows {
						last = row.ID
						if changed, ok := assets[row.ID]; ok {
							row = changed
							delete(assets, row.ID)
						}
						if !row.Tombstone {
							batch.SourceAssets = append(batch.SourceAssets, row)
						}
					}
				}
				return nil
			})
			if err != nil {
				return err
			}
			if err := s.appendCatalogScanFacts(ctx, candidate, token, batch); err != nil {
				return err
			}
			if count < CatalogBatchRows {
				break
			}
			after = last
		}
	}
	newFacts := CatalogFactBatch{}
	for _, row := range entries {
		if !row.Tombstone {
			newFacts.Entries = append(newFacts.Entries, row)
		}
	}
	for _, row := range records {
		if !row.Tombstone {
			newFacts.Recognitions = append(newFacts.Recognitions, row)
		}
	}
	for _, row := range assets {
		if !row.Tombstone {
			newFacts.SourceAssets = append(newFacts.SourceAssets, row)
		}
	}
	return s.appendCatalogScanFacts(ctx, candidate, token, newFacts)
}
