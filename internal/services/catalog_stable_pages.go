package services

import (
	"errors"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

const catalogReadPageRows = 2000

type catalogTransactionRead func(func(*gorm.DB, *CatalogReader) error) error

// Short transactions protect each page against GC. The current head protects
// its layers between pages; if it changes, ALL buffered pages are discarded.
// No transient durable reference is created, so a crashed loader cannot pin
// retired snapshots forever. Durable jobs still use their own bound references.
func readStableCatalogPages(read catalogTransactionRead, libraryID uint, reset func(*gorm.DB, *CatalogReader) error, load func(func(func(*CatalogReader) error) error) error) error {
	for attempt := 0; attempt < 3; attempt++ {
		var expected models.CatalogHead
		if err := read(func(tx *gorm.DB, reader *CatalogReader) error {
			var ok bool
			expected, ok = reader.Head(libraryID)
			if !ok || expected.Mode != "versioned" {
				return ErrCatalogFence
			}
			return reset(tx, reader)
		}); err != nil {
			return err
		}
		err := load(func(page func(*CatalogReader) error) error {
			return read(func(_ *gorm.DB, reader *CatalogReader) error {
				current, ok := reader.Head(libraryID)
				if !ok || !catalogSameHead(expected, current) {
					return ErrCatalogFence
				}
				return page(reader)
			})
		})
		if !errors.Is(err, ErrCatalogFence) {
			return err
		}
	}
	return ErrCatalogFence
}

func catalogSameHead(expected, current models.CatalogHead) bool {
	return expected.LibraryID == current.LibraryID && expected.Mode == current.Mode && expected.Revision == current.Revision && expected.SourceEpoch == current.SourceEpoch && expected.SourceFingerprint == current.SourceFingerprint && expected.ConfigFingerprint == current.ConfigFingerprint
}

func appendCatalogReadPages[T any](read func(func(*CatalogReader) error) error, query func(*CatalogReader) *gorm.DB, id func(T) uint, target *[]T) error {
	for after := uint(0); ; {
		var page []T
		if err := read(func(reader *CatalogReader) error {
			return query(reader).Where("id>?", after).Order("id").Limit(catalogReadPageRows).Find(&page).Error
		}); err != nil {
			return err
		}
		*target = append(*target, page...)
		if len(page) < catalogReadPageRows {
			return nil
		}
		after = id(page[len(page)-1])
	}
}
