package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"gorm.io/gorm"
)

type QuarkIntakeSettings struct {
	Enabled      bool                       `json:"enabled"`
	DownloaderID string                     `json:"downloader_id"`
	Directory    string                     `json:"directory"`
	Status       *ProviderObservationStatus `json:"synchronization,omitempty"`
}

func (s *MediaLibraryService) QuarkIntake(ctx context.Context, actor Actor, libraryID uint) (QuarkIntakeSettings, error) {
	if !actor.CanResource(authz.PermissionMediaLibrariesRead, models.AuthorizationResourceMediaLibrary, uintID(libraryID)) {
		return QuarkIntakeSettings{}, appError(CodePermissionDenied, "无权查看媒体库", nil)
	}
	var scope models.ProviderObservationScope
	err := s.db.WithContext(ctx).Where("library_id = ? AND kind = 'intake' AND enabled = ?", libraryID, true).First(&scope).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return QuarkIntakeSettings{}, nil
	}
	if err != nil {
		return QuarkIntakeSettings{}, err
	}
	out := QuarkIntakeSettings{Enabled: scope.AutoIngest, DownloaderID: scope.DownloaderID, Directory: scope.RootPath}
	if s.observations != nil {
		status, err := s.observations.Status(ctx, scope.ID)
		if err != nil {
			return out, err
		}
		out.Status = &status
	}
	return out, nil
}

// A configured downloader already owns a verified directory below its Storage.
// This setting adds explicit permission to adopt stable external direct children.
func (s *MediaLibraryService) SetQuarkIntake(ctx context.Context, actor Actor, libraryID uint, enabled bool, downloaderID string, request RequestContext) (QuarkIntakeSettings, error) {
	if !actor.CanResource(authz.PermissionMediaLibrariesUpdate, models.AuthorizationResourceMediaLibrary, uintID(libraryID)) || !actor.CanIngestLibrary(uintID(libraryID)) {
		return QuarkIntakeSettings{}, appError(CodePermissionDenied, "无权修改此媒体库的自动接管", nil)
	}
	if s.observations == nil {
		return QuarkIntakeSettings{}, ErrObservationUnavailable
	}
	if !enabled {
		err := s.db.WithContext(ctx).Model(&models.ProviderObservationScope{}).Where("library_id = ? AND kind = 'intake'", libraryID).Updates(map[string]any{"enabled": false, "auto_ingest": false}).Error
		return QuarkIntakeSettings{}, err
	}
	downloaderID = strings.TrimSpace(downloaderID)
	if !actor.CanResource(authz.PermissionDownloadsCreate, models.AuthorizationResourceDownloader, downloaderID) {
		return QuarkIntakeSettings{}, appError(CodePermissionDenied, "无权使用此下载器", nil)
	}
	in, err := quarkIntakeScopeInput(s.db.WithContext(ctx), libraryID, downloaderID, actor.User.ID)
	if err != nil {
		return QuarkIntakeSettings{}, err
	}
	if s.connections == nil {
		return QuarkIntakeSettings{}, ErrObservationUnavailable
	}
	_, driver, err := s.connections.driver(in.ConnectionID)
	if err != nil {
		return QuarkIntakeSettings{}, err
	}
	var dl models.Downloader
	var source models.Storage
	if err := s.db.First(&dl, "id = ?", downloaderID).Error; err != nil {
		return QuarkIntakeSettings{}, err
	}
	if dl.StorageID == nil {
		return QuarkIntakeSettings{}, ErrObservationFence
	}
	if err := s.db.First(&source, *dl.StorageID).Error; err != nil {
		return QuarkIntakeSettings{}, err
	}
	root, err := providerItemWithinRoot(ctx, driver, in.RootID, source.RootPath)
	if err != nil || !root.IsDir {
		return QuarkIntakeSettings{}, appError(CodeMediaLibraryPathInvalid, "接收目录已变化，请重新选择下载器目录", nil)
	}
	// Validate physical provider ancestry, not only path strings, across all
	// enabled final roots of this account before any background adoption.
	var roots []string
	if err := s.db.Table("media_libraries AS l").Select("l.provider_root_id").Joins("JOIN storages AS s ON s.id=l.storage_id").Where("l.enabled = ? AND s.enabled = ? AND s.type = ? AND s.connection_id = ?", true, true, models.StorageTypeQuark, in.ConnectionID).Scan(&roots).Error; err != nil {
		return QuarkIntakeSettings{}, err
	}
	for _, final := range roots {
		overlap, err := providerDirectoriesOverlap(ctx, driver, root.ID, final)
		if err != nil {
			return QuarkIntakeSettings{}, err
		}
		if overlap {
			return QuarkIntakeSettings{}, appError(CodeMediaLibraryOverlap, "接收目录不能与最终媒体库重叠", nil)
		}
	}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		fresh, err := quarkIntakeScopeInput(tx, libraryID, downloaderID, actor.User.ID)
		if err != nil {
			return err
		}
		if fresh != in {
			return ErrObservationFence
		}
		bound := *s.observations
		bound.db = tx
		scope, err := bound.EnsureScope(ctx, in)
		if err != nil {
			return err
		}
		if err := tx.Model(&scope).Updates(map[string]any{"owner_id": actor.User.ID, "downloader_id": downloaderID}).Error; err != nil {
			return err
		}
		return s.audit.Record(tx, &actor.User.ID, "media_library.quark_intake.update", "media_library", uintID(libraryID), "success", map[string]any{"enabled": true, "downloader_id": downloaderID}, request)
	})
	if err != nil {
		return QuarkIntakeSettings{}, err
	}
	return s.QuarkIntake(ctx, actor, libraryID)
}

func quarkIntakeScopeInput(tx *gorm.DB, libraryID uint, downloaderID string, ownerID uint) (ObservationScopeInput, error) {
	base, err := quarkObservationScopeTx(tx, libraryID)
	if err != nil {
		return base, err
	}
	var dl models.Downloader
	var storage models.Storage
	if err := tx.First(&dl, "id = ?", downloaderID).Error; err != nil {
		return base, err
	}
	if !dl.Enabled || dl.Type != models.DownloaderTypeQuarkShare || dl.StorageID == nil || dl.ProviderDirectoryID == "" || ownerID == 0 {
		return base, appError(CodeDownloaderUnavailable, "请选择已启用的夸克分享接收器", nil)
	}
	if err := tx.First(&storage, *dl.StorageID).Error; err != nil {
		return base, err
	}
	if !storage.Enabled || storage.Type != models.StorageTypeQuark || storage.ConnectionID == nil || *storage.ConnectionID != base.ConnectionID {
		return base, appError(CodeDownloaderUnavailable, "接收目录与最终媒体库必须属于同一夸克连接", nil)
	}
	// The saved path is relative to this downloader's Storage root.
	base.RootID, base.RootPath, base.Kind, base.AutoIngest = dl.ProviderDirectoryID, path.Join("/", storage.RootDisplayPath, dl.ProviderDirectoryPath), "intake", true
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%d\x00%s\x00%s", base.Fence, dl.ID, base.RootID, base.RootPath, ownerID, dl.ExecutionLocation, optionalNodeID(dl.NodeID))))
	base.Fence = hex.EncodeToString(sum[:])
	return base, nil
}

func optionalNodeID(id *string) string {
	if id == nil {
		return ""
	}
	return *id
}

type quarkIntakeEnqueuer interface {
	AdoptQuarkObservedItem(context.Context, *ProviderObservationService, models.ProviderObservationScope, models.ProviderObservationItem) error
}

var errQuarkIntakeReady = errors.New("quark intake metadata validated")

func (s *MediaLibraryService) processQuarkIntake(ctx context.Context, libraryID uint) error {
	enqueuer, ok := s.ingest.(quarkIntakeEnqueuer)
	if !ok || s.observations == nil {
		return nil
	}
	var scope models.ProviderObservationScope
	err := s.db.WithContext(ctx).Where("library_id = ? AND kind = 'intake' AND enabled = ? AND auto_ingest = ?", libraryID, true, true).First(&scope).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	var candidates []models.ProviderObservationItem
	err = observationCurrent(s.db.WithContext(ctx), scope.ID).
		Where("parent_id = ? AND excluded = ? AND missing_since IS NULL AND downlisted_at IS NULL AND stable_observations >= 2 AND stable_since <= ?", scope.RootID, false, time.Now().UTC().Add(-time.Minute)).
		Where("NOT EXISTS (SELECT 1 FROM provider_observation_intake_claims c WHERE c.scope_id = provider_observation_items.scope_id AND c.resource_id = provider_observation_items.resource_id AND c.fence = ?)", scope.Fence).
		Order("intake_attempted_at, seen_at, resource_id").Limit(50).Find(&candidates).Error
	if err != nil {
		return err
	}
	var firstErr error
	for _, item := range candidates {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Persist fair rotation, including deferred trees and rejected candidates.
		// A busy first page must never starve a later stable file after restart.
		if err = observationCurrent(s.db.WithContext(ctx), scope.ID).Model(&models.ProviderObservationItem{}).Where("resource_id = ?", item.ResourceID).Update("intake_attempted_at", time.Now().UTC()).Error; err != nil {
			return err
		}
		if err = enqueuer.AdoptQuarkObservedItem(ctx, s.observations, scope, item); err != nil && !errors.Is(err, ErrObservationDeferred) {
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func (s *DownloadService) AdoptQuarkObservedItem(ctx context.Context, observations *ProviderObservationService, scope models.ProviderObservationScope, item models.ProviderObservationItem) error {
	key := sha256.Sum256([]byte(scope.ID + "\x00" + scope.Fence + "\x00" + item.ResourceID))
	claimID := hex.EncodeToString(key[:])
	var count int64
	if err := s.db.WithContext(ctx).Model(&models.ProviderObservationIntakeClaim{}).Where("id = ?", claimID).Count(&count).Error; err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	// Preflight is metadata-only; the final claim repeats every stability/fence
	// condition in the task-creation transaction after all network reads finish.
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		_, err := observations.claimIntakeTx(tx, scope.ID, item.ResourceID, func(*gorm.DB, models.ProviderObservationItem) (string, error) { return "", errQuarkIntakeReady })
		return err
	})
	if !errors.Is(err, errQuarkIntakeReady) {
		return err
	}
	targetID := scope.LibraryID
	input := SubmitDownloadInput{DownloaderID: scope.DownloaderID, MediaLibraryID: &targetID, DisplayName: item.Name, Source: DownloadSourceInput{Kind: downloadpkg.SourceProviderItem}}
	input.AfterPersist = func(tx *gorm.DB, task models.DownloadTask) error {
		id, err := observations.claimIntakeTx(tx, scope.ID, item.ResourceID, func(_ *gorm.DB, current models.ProviderObservationItem) (string, error) {
			if current.Fingerprint != item.Fingerprint {
				return "", ErrObservationDeferred
			}
			return task.ID, nil
		})
		if err != nil {
			return err
		}
		if id != task.ID {
			return ErrObservationDeferred
		}
		return nil
	}
	_, err = s.submit(ctx, scope.OwnerID, input, RequestContext{}, models.DownloadSourceOriginProviderIngest, claimID, item.ResourceID)
	if err == nil {
		s.log.Info().Uint("library_id", scope.LibraryID).Str("file_name", item.Name).Msg("夸克接收目录内容已稳定，已创建整理入库任务")
	}
	return err
}
