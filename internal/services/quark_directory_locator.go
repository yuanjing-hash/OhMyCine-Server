package services

import (
	"context"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

// Directory path snapshots are hints, never authority. Quark verifies both the
// saved Storage root and the requested directory on a fresh, bounded path walk.
// Its TV API has no ancestry endpoint and cold Stat cannot prove directories.
func providerDirectoryWithinStorage(ctx context.Context, driver cloud.Driver, storage models.Storage, relative, id string) (cloud.Item, error) {
	if storage.Type == models.StorageTypeQuark {
		if scoped, ok := driver.(interface {
			ResolveScopedDirectory(context.Context, string, string, string, string) (cloud.Item, error)
		}); ok {
			item, err := scoped.ResolveScopedDirectory(ctx, storage.RootPath, storage.RootDisplayPath, relative, id)
			if err != nil || item.ID != id || !item.IsDir {
				return cloud.Item{}, appError(CodeMediaLibraryPathInvalid, "夸克目录身份或所属 Storage 已变化", err)
			}
			return item, nil
		}
	}
	return providerItemWithinRoot(ctx, driver, id, storage.RootPath)
}
