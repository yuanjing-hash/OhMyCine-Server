package services

import (
	"context"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

type structureCatalogFacts struct {
	LogicalFence      *structureLogicalFence
	Library           models.MediaLibrary
	Head              models.CatalogHead
	SourceFingerprint string
	Entries           []models.MediaLibraryEntry
	Assets            []models.MediaLibrarySourceAsset
}

func captureStructureCatalogHeader(tx *gorm.DB, reader *CatalogReader, libraryID uint) (structureCatalogFacts, error) {
	var facts structureCatalogFacts
	if err := tx.First(&facts.Library, libraryID).Error; err != nil {
		return facts, mediaLibraryNotFound(err)
	}
	var storage models.Storage
	var profile models.MediaClassificationProfile
	if err := tx.First(&storage, facts.Library.StorageID).Error; err != nil {
		return facts, err
	}
	if err := tx.First(&profile, facts.Library.ProfileID).Error; err != nil {
		return facts, err
	}
	facts.Head, _ = reader.Head(libraryID)
	facts.SourceFingerprint = mediaLibraryScanSourceFingerprint(facts.Library, storage, profile)
	var err error
	facts.LogicalFence, err = captureStructureLogicalFenceTx(tx, reader, libraryID)
	if err != nil {
		return facts, err
	}
	return facts, nil
}

// The only catalog input loader for structure planning. Versioned pages use
// exact-head checks and separate short read transactions, not a multi-minute
// WAL snapshot. Parsing and all provider/file operations run after these reads.
func (s *MediaLibraryStructureService) loadStructureCatalog(ctx context.Context, libraryID uint) (structureCatalogFacts, error) {
	var facts structureCatalogFacts
	var versioned bool
	err := s.withCatalogRead(ctx, libraryID, func(tx *gorm.DB, reader *CatalogReader) error {
		var err error
		facts, err = captureStructureCatalogHeader(tx, reader, libraryID)
		if err != nil {
			return err
		}
		versioned = facts.Head.Mode == "versioned"
		if versioned {
			return nil
		}
		return loadStructureCatalogPages(reader, &facts)
	})
	if err != nil || !versioned {
		return facts, err
	}
	err = readStableCatalogPages(func(read func(*gorm.DB, *CatalogReader) error) error {
		return s.withCatalogRead(ctx, libraryID, read)
	}, libraryID, func(tx *gorm.DB, reader *CatalogReader) error {
		var err error
		facts, err = captureStructureCatalogHeader(tx, reader, libraryID)
		return err
	}, func(read func(func(*CatalogReader) error) error) error {
		if err := appendCatalogReadPages(read, func(reader *CatalogReader) *gorm.DB { return reader.Entries() }, func(row models.MediaLibraryEntry) uint { return row.ID }, &facts.Entries); err != nil {
			return err
		}
		return appendCatalogReadPages(read, func(reader *CatalogReader) *gorm.DB { return reader.SourceAssets().Where("active=?", true) }, func(row models.MediaLibrarySourceAsset) uint { return row.ID }, &facts.Assets)
	})
	if err != nil {
		return structureCatalogFacts{}, err
	}
	return facts, nil
}

func loadStructureCatalogPages(reader *CatalogReader, facts *structureCatalogFacts) error {
	var after uint
	for {
		var rows []models.MediaLibraryEntry
		if err := reader.Entries().Where("id>?", after).Order("id").Limit(2000).Find(&rows).Error; err != nil {
			return err
		}
		facts.Entries = append(facts.Entries, rows...)
		if len(rows) < 2000 {
			break
		}
		after = rows[len(rows)-1].ID
	}
	after = 0
	for {
		var rows []models.MediaLibrarySourceAsset
		if err := reader.SourceAssets().Where("id>? AND active=?", after, true).Order("id").Limit(2000).Find(&rows).Error; err != nil {
			return err
		}
		facts.Assets = append(facts.Assets, rows...)
		if len(rows) < 2000 {
			break
		}
		after = rows[len(rows)-1].ID
	}
	return nil
}

func validateStructureCatalogTx(tx *gorm.DB, facts structureCatalogFacts) error {
	reader, err := PinCatalogTx(tx, []uint{facts.Library.ID})
	if err != nil {
		return err
	}
	if facts.LogicalFence != nil {
		return validateStructureLogicalFenceTx(tx, reader, *facts.LogicalFence)
	}
	current, err := captureStructureCatalogHeader(tx, reader, facts.Library.ID)
	if err != nil {
		return err
	}
	if current.SourceFingerprint != facts.SourceFingerprint || current.Library.BaselineGeneration != facts.Library.BaselineGeneration || current.Head.Mode != facts.Head.Mode || current.Head.SourceEpoch != facts.Head.SourceEpoch || current.Head.SourceFingerprint != facts.Head.SourceFingerprint || current.Head.ConfigFingerprint != facts.Head.ConfigFingerprint || current.Head.Revision != facts.Head.Revision {
		return appError(CodeConflict, "媒体目录或识别结果已变化，请重新加载诊断", ErrCatalogFence)
	}
	return nil
}
