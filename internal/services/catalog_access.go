package services

import (
	"context"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

// withCatalogReadTx establishes a single read snapshot for authorization,
// catalogue facts, counts and decoration. Never do network or file IO here.
// The no-store path exists for legacy service fixtures only; it is NOT a
// deferred reader and refuses versioned catalogues instead of reading anchors.
func (s *MediaLibraryService) withCatalogReadTx(ctx context.Context, read func(*gorm.DB) error) error {
	if read == nil {
		return ErrCatalogInvalid
	}
	if s.catalogStore != nil {
		if s.catalogStore.readDB == nil {
			return ErrCatalogInvalid
		}
		return s.catalogStore.readDB.WithContext(ctx).Transaction(read)
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if tx.Migrator().HasTable(&models.CatalogHead{}) {
			var count int64
			if err := tx.Model(&models.CatalogHead{}).Where("mode NOT IN ?", []string{"legacy", "converting"}).Count(&count).Error; err != nil {
				return err
			}
			if count != 0 {
				return ErrCatalogInvalid
			}
		}
		return read(tx)
	})
}

func (s *MediaLibraryService) withCatalogRead(ctx context.Context, ids []uint, read func(*gorm.DB, *CatalogReader) error) error {
	return s.withCatalogReadTx(ctx, func(tx *gorm.DB) error {
		reader, err := PinCatalogTx(tx, ids)
		if err != nil {
			return err
		}
		return read(tx, reader)
	})
}

func (s *MediaLibraryService) ensureMediaLibraryReadableTx(tx *gorm.DB, actor Actor, libraryID uint) error {
	if !actor.CanResource(authz.PermissionMediaLibrariesRead, models.AuthorizationResourceMediaLibrary, uintID(libraryID)) {
		return appError(CodePermissionDenied, "无权查看媒体库", nil)
	}
	var count int64
	if err := tx.Model(&models.MediaLibrary{}).Where("id = ?", libraryID).Count(&count).Error; err != nil {
		return err
	}
	if count == 0 {
		return mediaLibraryNotFound(gorm.ErrRecordNotFound)
	}
	return nil
}

func (s *MediaLibraryService) authorizedMediaLibraryIDsTx(tx *gorm.DB, actor Actor, permission string, enabledOnly bool) ([]uint, error) {
	// Keyset enumeration bounds each DB read. Do not silently truncate an
	// account's authorized scope: a reader can pin at most 2000 libraries.
	allowed := make([]uint, 0)
	var after uint
	for {
		query := tx.Model(&models.MediaLibrary{}).Where("id > ?", after)
		if enabledOnly {
			query = query.Where("enabled = ?", true)
		}
		var ids []uint
		if err := query.Order("id").Limit(250).Pluck("id", &ids).Error; err != nil {
			return nil, err
		}
		for _, id := range ids {
			if actor.CanResource(permission, models.AuthorizationResourceMediaLibrary, uintID(id)) {
				allowed = append(allowed, id)
				if len(allowed) > 2000 {
					return nil, ErrCatalogBudget
				}
			}
		}
		if len(ids) < 250 {
			return allowed, nil
		}
		after = ids[len(ids)-1]
	}
}
