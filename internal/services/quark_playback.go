package services

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
	"gorm.io/gorm"
)

// quarkPlaybackError translates only known provider failures. Never copy an
// upstream message, URL, header or response body into the public error envelope.
func quarkPlaybackError(provider string, err error) error {
	if provider != models.StorageTypeQuark || err == nil {
		return nil
	}
	code, _ := cloud.ErrorInfo(err)
	message := ""
	switch code {
	case quark.CodeAuth:
		message = "夸克播放授权已失效，请管理员在连接设置中重新授权后重试。"
	case quark.CodeLimited:
		message = "夸克请求受到限流，请稍后重试。"
		var failure *quark.Failure
		if errors.As(err, &failure) && failure.RetryAfter > 0 && failure.RetryAfter <= 24*time.Hour {
			seconds := (failure.RetryAfter + time.Second - 1) / time.Second
			message = fmt.Sprintf("夸克请求受到限流，请在 %d 秒后重试。", seconds)
		}
	case quark.CodeInvalid:
		message = "夸克返回的播放信息无效，暂时无法获取原文件链接，请稍后重试。"
	case quark.CodeUnavailable:
		message = "暂时无法连接夸克播放服务，请稍后重试。"
	case quark.CodeUnsupported:
		message = "当前夸克连接尚不具备原文件播放能力，请管理员检查播放授权。"
	case quark.CodeIdentity:
		message = "夸克授权身份与当前媒体库不一致，请管理员检查连接。"
	case quark.CodeProof:
		message = "夸克播放授权尚未完成此媒体库的共同访问验证，请管理员检查连接。"
	case quark.CodeIncomplete:
		message = "夸克目录信息尚未读取完整，暂时无法确认播放文件，请稍后重试。"
	default:
		return nil
	}
	return appError(code, message, err)
}

// scopedPlaybackResolver supplies fresh root membership without relying on a
// process-local listing cache. Its final identity must equal the catalog item.
type scopedPlaybackResolver interface {
	ResolveScopedItem(context.Context, string, string, string) (cloud.Item, error)
}

func resolvePlaybackItem(ctx context.Context, driver cloud.Driver, provider, rootID, relativePath, itemID string) (cloud.Item, error) {
	if provider == models.StorageTypeQuark {
		if scoped, ok := driver.(scopedPlaybackResolver); ok {
			item, err := scoped.ResolveScopedItem(ctx, rootID, relativePath, itemID)
			if mapped := quarkPlaybackError(provider, err); mapped != nil {
				return cloud.Item{}, mapped
			}
			if err != nil || item.ID != itemID || item.IsDir {
				return cloud.Item{}, appError(CodeProxyTargetUnavailable, "播放文件已不在当前媒体库范围内", err)
			}
			return item, nil
		}
	}
	item, err := driver.Stat(ctx, itemID)
	if mapped := quarkPlaybackError(provider, err); mapped != nil {
		return cloud.Item{}, mapped
	}
	if err != nil || item.ID != itemID || item.IsDir {
		return cloud.Item{}, appError(CodeProxyTargetUnavailable, "播放目标不可用", err)
	}
	within, err := providerParentWithinRoot(ctx, driver, item.ParentID, rootID)
	if mapped := quarkPlaybackError(provider, err); mapped != nil {
		return cloud.Item{}, mapped
	}
	if err != nil || !within {
		return cloud.Item{}, appError(CodeProxyTargetUnavailable, "播放文件已不在当前媒体库范围内", err)
	}
	return item, nil
}

func (s *SignedProxyService) quarkArtifactEntry(ctx context.Context, target signedProxyTarget) (models.MediaLibraryEntry, error) {
	var entries []models.MediaLibraryEntry
	facade := &MediaLibraryService{db: s.db, catalogStore: s.catalogStore}
	err := facade.withCatalogReadTx(ctx, func(tx *gorm.DB) error {
		reader, err := PinCatalogTx(tx, []uint{target.LibraryID})
		if err != nil {
			return err
		}
		return reader.VisibleEntries().Where("library_id = ? AND provider_id = ?", target.LibraryID, target.ProviderItemID).Limit(2).Find(&entries).Error
	})
	if err != nil || len(entries) != 1 {
		return models.MediaLibraryEntry{}, appError(CodeProxyTargetUnavailable, "播放文件已不在当前媒体库范围内", err)
	}
	return entries[0], nil
}

func (s *SignedProxyService) resolveQuarkArtifactItem(ctx context.Context, driver cloud.Driver, target signedProxyTarget) (cloud.Item, error) {
	entry, err := s.quarkArtifactEntry(ctx, target)
	if err != nil {
		return cloud.Item{}, err
	}
	item, err := resolvePlaybackItem(ctx, driver, target.StorageType, target.ProviderRootID, entry.RelativePath, target.ProviderItemID)
	if err != nil {
		return cloud.Item{}, err
	}
	current, err := s.quarkArtifactEntry(ctx, target)
	if err != nil || current.ID != entry.ID || current.RelativePath != entry.RelativePath || current.ProviderID != entry.ProviderID || current.Size != entry.Size || !current.ModifiedAt.Equal(entry.ModifiedAt) {
		return cloud.Item{}, appError(CodeProxyTargetUnavailable, "播放文件已变化，请重试", err)
	}
	return item, nil
}
