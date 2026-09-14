package services

import (
	"context"
	"errors"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/medialibrary"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

// Exact batch publication reads only matching identities/paths and linked
// recognition rows. It never materializes the whole catalog baseline.
func (s *MediaLibraryService) loadCatalogBatchBaseline(ctx context.Context, libraryID uint, input medialibrary.Result) (catalogScanBaseline, error) {
	var baseline catalogScanBaseline
	if len(input.AuthoritativeParentPaths) != 0 {
		return baseline, errors.New("directory-wide batch publication is unsupported")
	}
	ids, paths := []string{}, []string{}
	for _, file := range input.Files {
		ids = append(ids, file.ProviderID)
		paths = append(paths, file.RelativePath)
	}
	for _, asset := range input.Assets {
		ids = append(ids, asset.ProviderID)
		paths = append(paths, asset.RelativePath)
	}
	ids = append(ids, input.DeletedProviderIDs...)
	sources := []string{}
	for _, unit := range medialibrary.GroupRecognitionUnits(input.Files) {
		sources = append(sources, unit.SourceKey)
	}
	err := s.withCatalogRead(ctx, []uint{libraryID}, func(_ *gorm.DB, reader *CatalogReader) error {
		baseline.head, _ = reader.Head(libraryID)
		entrySeen, assetSeen := map[uint]bool{}, map[uint]bool{}
		for start := 0; start < max(len(ids), len(paths)); start += 250 {
			idChunk := ids[min(start, len(ids)):min(start+250, len(ids))]
			pathChunk := paths[min(start, len(paths)):min(start+250, len(paths))]
			var entries []models.MediaLibraryEntry
			if err := reader.Entries().Where("provider_id IN ? OR relative_path IN ?", idChunk, pathChunk).Find(&entries).Error; err != nil {
				return err
			}
			for _, entry := range entries {
				if !entrySeen[entry.ID] {
					baseline.entries = append(baseline.entries, entry)
					entrySeen[entry.ID] = true
				}
			}
			var assets []models.MediaLibrarySourceAsset
			if err := reader.SourceAssets().Where("provider_id IN ? OR relative_path IN ?", idChunk, pathChunk).Find(&assets).Error; err != nil {
				return err
			}
			for _, asset := range assets {
				if !assetSeen[asset.ID] {
					baseline.assets = append(baseline.assets, asset)
					assetSeen[asset.ID] = true
				}
			}
		}
		recIDs := []uint{}
		entryIDs := []uint{}
		for _, entry := range baseline.entries {
			entryIDs = append(entryIDs, entry.ID)
			if entry.RecognitionID != nil {
				recIDs = append(recIDs, *entry.RecognitionID)
			}
		}
		recSeen := map[uint]bool{}
		for start := 0; start < max(len(recIDs), len(sources)); start += 250 {
			idChunk := recIDs[min(start, len(recIDs)):min(start+250, len(recIDs))]
			sourceChunk := sources[min(start, len(sources)):min(start+250, len(sources))]
			var records []models.MediaLibraryRecognition
			if err := reader.Recognitions().Where("id IN ? OR source_key IN ?", idChunk, sourceChunk).Find(&records).Error; err != nil {
				return err
			}
			for _, record := range records {
				if !recSeen[record.ID] {
					baseline.recognitions = append(baseline.recognitions, record)
					recSeen[record.ID] = true
				}
			}
		}
		baseline.protectedRecognitions = map[uint]bool{}
		// Existing episodes outside this batch keep their shared recognition.
		for _, record := range baseline.recognitions {
			var count int64
			query := reader.Entries().Where("recognition_id = ?", record.ID)
			if len(entryIDs) > 0 {
				query = query.Where("id NOT IN ?", entryIDs)
			}
			if err := query.Limit(1).Count(&count).Error; err != nil {
				return err
			}
			if count > 0 {
				baseline.protectedRecognitions[record.ID] = true
			}
		}
		return nil
	})
	return baseline, err
}

func catalogBatchPathReplacements(input medialibrary.Result, baseline catalogScanBaseline) medialibrary.Result {
	if !input.Partial {
		return input
	}
	current := map[string]bool{}
	paths := map[string]string{}
	for _, file := range input.Files {
		current[file.ProviderID] = true
		paths[file.RelativePath] = file.ProviderID
	}
	for _, asset := range input.Assets {
		current[asset.ProviderID] = true
		paths[asset.RelativePath] = asset.ProviderID
	}
	for _, old := range baseline.entries {
		if replacement, ok := paths[old.RelativePath]; ok && replacement != old.ProviderID && !current[old.ProviderID] {
			input.DeletedProviderIDs = append(input.DeletedProviderIDs, old.ProviderID)
		}
	}
	for _, old := range baseline.assets {
		if replacement, ok := paths[old.RelativePath]; ok && replacement != old.ProviderID && !current[old.ProviderID] {
			input.DeletedProviderIDs = append(input.DeletedProviderIDs, old.ProviderID)
		}
	}
	return input
}

func loadLegacyBatchFactsTx(tx *gorm.DB, libraryID uint, input medialibrary.Result) ([]models.MediaLibraryEntry, []models.MediaLibrarySourceAsset, error) {
	ids, paths := append([]string(nil), input.DeletedProviderIDs...), []string{}
	for _, file := range input.Files {
		ids = append(ids, file.ProviderID)
		paths = append(paths, file.RelativePath)
	}
	for _, asset := range input.Assets {
		ids = append(ids, asset.ProviderID)
		paths = append(paths, asset.RelativePath)
	}
	entries, assets := []models.MediaLibraryEntry{}, []models.MediaLibrarySourceAsset{}
	es, as := map[uint]bool{}, map[uint]bool{}
	for start := 0; start < max(len(ids), len(paths)); start += 250 {
		i := ids[min(start, len(ids)):min(start+250, len(ids))]
		p := paths[min(start, len(paths)):min(start+250, len(paths))]
		var er []models.MediaLibraryEntry
		if err := tx.Where("library_id = ? AND (provider_id IN ? OR relative_path IN ?)", libraryID, i, p).Find(&er).Error; err != nil {
			return nil, nil, err
		}
		for _, row := range er {
			if !es[row.ID] {
				es[row.ID] = true
				entries = append(entries, row)
			}
		}
		var ar []models.MediaLibrarySourceAsset
		if err := tx.Where("library_id = ? AND (provider_id IN ? OR relative_path IN ?)", libraryID, i, p).Find(&ar).Error; err != nil {
			return nil, nil, err
		}
		for _, row := range ar {
			if !as[row.ID] {
				as[row.ID] = true
				assets = append(assets, row)
			}
		}
	}
	return entries, assets, nil
}
