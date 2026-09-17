package services

import (
	"context"
	"errors"
	"path"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/medialibrary"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	cloudpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const providerEventNeedsReview = "needs_review"
const providerDeletionNoResiduals = "no_local_residuals"

// Absence completes only this notification. It never authorizes filesystem or
// catalog deletion, and same-name rows with another stable ID are irrelevant.
func (s *MediaLibraryService) completeAbsentProviderDeletion(ctx context.Context, libraryID uint, rowID uint, itemID string) (bool, error) {
	completed := false
	newlyCompleted := false
	completedName := ""
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row models.MediaLibraryProviderEvent
		if err := tx.Where("id = ? AND library_id = ?", rowID, libraryID).First(&row).Error; err != nil {
			return err
		}
		if row.ProcessedAt != nil {
			completed = true
			return nil
		}
		var library models.MediaLibrary
		var storage models.Storage
		var inbox models.ProviderEvent
		if err := tx.First(&library, libraryID).Error; err != nil {
			return err
		}
		if err := tx.First(&storage, library.StorageID).Error; err != nil {
			return err
		}
		if err := tx.First(&inbox, row.InboxEventID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}
		if itemID == "" || inbox.Kind != cloudpkg.ChangeDeleted || inbox.ItemID != itemID || storage.ConnectionID == nil || inbox.ConnectionID != *storage.ConnectionID {
			return nil
		}
		reader, err := PinCatalogTx(tx, []uint{libraryID})
		if err != nil {
			return err
		}
		var entries []models.MediaLibraryEntry
		if err := reader.Entries().Where("provider_id = ?", itemID).Limit(1).Find(&entries).Error; err != nil {
			return err
		}
		if len(entries) > 0 {
			return nil
		}
		var assets []models.MediaLibrarySourceAsset
		if err := reader.SourceAssets().Where("provider_id = ?", itemID).Limit(1).Find(&assets).Error; err != nil {
			return err
		}
		if len(assets) > 0 {
			return nil
		}
		var artifacts []models.MediaArtifact
		if err := tx.Where("library_id = ? AND provider_item_id = ? AND managed = ? AND status != ?", libraryID, itemID, true, "removed").Limit(1).Find(&artifacts).Error; err != nil {
			return err
		}
		if len(artifacts) > 0 {
			return nil
		}
		var paths []models.MediaLibraryProviderPath
		if err := publishedProviderPaths(tx, libraryID, catalogSourceFingerprint(library, storage)).Where("provider_id = ?", itemID).Limit(1).Find(&paths).Error; err != nil {
			return err
		}
		if len(paths) > 0 && paths[0].IsDir {
			prefix := paths[0].RelativePath + "/"
			if err := reader.Entries().Where("substr(relative_path, 1, ?) = ?", len([]rune(prefix)), prefix).Limit(1).Find(&entries).Error; err != nil {
				return err
			}
			if len(entries) > 0 {
				return nil
			}
			if err := reader.SourceAssets().Where("substr(relative_path, 1, ?) = ?", len([]rune(prefix)), prefix).Limit(1).Find(&assets).Error; err != nil {
				return err
			}
			if len(assets) > 0 {
				return nil
			}
			descendants := publishedProviderPaths(tx, libraryID, catalogSourceFingerprint(library, storage)).Select("paths.provider_id").Where("substr(relative_path, 1, ?) = ?", len([]rune(prefix)), prefix)
			if err := tx.Where("library_id = ? AND managed = ? AND status != ? AND provider_item_id IN (?)", libraryID, true, "removed", descendants).Limit(1).Find(&artifacts).Error; err != nil {
				return err
			}
			if len(artifacts) > 0 {
				return nil
			}
		}
		now := time.Now().UTC()
		if err := tx.Model(&row).Updates(map[string]any{"processed_at": now, "updated_at": now, "resolution_code": providerDeletionNoResiduals, "resolution_reason": providerDeletionNoResiduals, "retry_after": nil}).Error; err != nil {
			return err
		}
		completed = true
		newlyCompleted = true
		if payload, ok := decodeProviderEventPayload(row.PayloadJSON); ok {
			completedName = safeProviderEventName(payload.Name)
		}
		return nil
	})
	if err == nil && newlyCompleted {
		s.log.Info().Uint("library_id", libraryID).Uint("delivery_id", rowID).Str("file_name", completedName).Str("reason_code", providerDeletionNoResiduals).Msg("删除通知已完成：本地已无对应媒体或待清理产物")
	}
	return completed, err
}

var errProviderEventOrderUnproven = errors.New("provider event order is ambiguous")

type providerDeletionGuard struct {
	DeliveryID  uint
	ProviderIDs []string
}

// SQLite stores time.Time with zone suffixes; compare instants in Go instead
// of lexical SQL or julianday(), which returns NULL for that representation.
func conflictingProviderEvents(tx *gorm.DB, inbox models.ProviderEvent, ids []string) (map[string]bool, error) {
	conflicts := map[string]bool{}
	for after := uint(0); ; {
		var rows []models.ProviderEvent
		if err := tx.Where("connection_id = ? AND stream = ? AND item_id IN ? AND id > ?", inbox.ConnectionID, inbox.Stream, ids, after).Order("id").Limit(250).Find(&rows).Error; err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			break
		}
		for _, row := range rows {
			if row.ID != inbox.ID && row.EventTime.Equal(inbox.EventTime) {
				digits := func(v string) bool {
					return v != "" && strings.IndexFunc(v, func(r rune) bool { return r < '0' || r > '9' }) < 0
				}
				if !digits(row.ProviderEventID) || !digits(inbox.ProviderEventID) {
					return nil, errProviderEventOrderUnproven
				}
			}
			if row.ID != inbox.ID && compareCursor(cloudpkg.ChangeCursor{Time: row.EventTime, ID: row.ProviderEventID}, cloudpkg.ChangeCursor{Time: inbox.EventTime, ID: inbox.ProviderEventID}) > 0 {
				conflicts[row.ItemID] = true
			}
		}
		after = rows[len(rows)-1].ID
	}
	return conflicts, nil
}

func validateProviderDeletionScopeTx(ctx context.Context, tx *gorm.DB, library models.MediaLibrary, storage models.Storage) error {
	scope, _ := providerChangeScopeFromContext(ctx)
	if len(scope.DeletionGuards) == 0 {
		return nil
	}
	if err := tx.First(&library, library.ID).Error; err != nil {
		return err
	}
	if err := tx.First(&storage, library.StorageID).Error; err != nil {
		return err
	}
	if scope.DeletionGeneration == nil || library.DirtyGeneration != *scope.DeletionGeneration {
		return errProviderChangeScopeUnproven
	}
	for _, guard := range scope.DeletionGuards {
		var row models.MediaLibraryProviderEvent
		if err := tx.Where("id = ? AND library_id = ?", guard.DeliveryID, library.ID).First(&row).Error; err != nil {
			return err
		}
		if row.SourceFingerprint != catalogSourceFingerprint(library, storage) {
			return errMediaLibraryConfigurationChanged
		}
		var inbox models.ProviderEvent
		if err := tx.First(&inbox, row.InboxEventID).Error; err != nil {
			return err
		}
		if storage.ConnectionID == nil || inbox.ConnectionID != *storage.ConnectionID {
			return errMediaLibraryConfigurationChanged
		}
		for start := 0; start < len(guard.ProviderIDs); start += 100 {
			end := min(start+100, len(guard.ProviderIDs))
			newer, err := conflictingProviderEvents(tx, inbox, guard.ProviderIDs[start:end])
			if err != nil {
				return err
			}
			if len(newer) > 0 {
				return errProviderChangeScopeUnproven
			}
			var paths []models.MediaLibraryProviderPath
			if err := publishedProviderPaths(tx, library.ID, row.SourceFingerprint).Where("provider_id IN ?", guard.ProviderIDs[start:end]).Find(&paths).Error; err != nil {
				return err
			}
			for _, p := range paths {
				if p.ObservedAt.After(inbox.EventTime) {
					return errProviderChangeScopeUnproven
				}
			}
		}
	}
	return nil
}

// A deletion notification carries final absence, not a path authorizing I/O.
// Resolve only against source-bound local identities; filenames never prove type.
func (s *MediaLibraryService) prepareLocalProviderDeletion(ctx context.Context, library models.MediaLibrary, storage models.Storage, row models.MediaLibraryProviderEvent, p providerEventPayload, known map[string]struct{}) (medialibrary.Result, string, error) {
	result := medialibrary.Result{Partial: true, Scoped: true}
	fingerprint := catalogSourceFingerprint(library, storage)
	if complete, err := s.completeAbsentProviderDeletion(ctx, library.ID, row.ID, p.ItemID); err != nil {
		return result, "", err
	} else if complete {
		return result, providerDeletionNoResiduals, nil
	}
	if row.SourceFingerprint != "" && row.SourceFingerprint != fingerprint {
		return result, "deletion_source_unproven", nil
	}
	var inbox models.ProviderEvent
	if err := s.db.WithContext(ctx).First(&inbox, row.InboxEventID).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return result, "deletion_event_unavailable", nil
	} else if err != nil {
		return result, "", err
	}
	if storage.ConnectionID == nil || inbox.ConnectionID != *storage.ConnectionID || inbox.Kind != cloudpkg.ChangeDeleted || inbox.ItemID != p.ItemID || inbox.EventTime.IsZero() {
		return result, "deletion_source_unproven", nil
	}
	// A newer notification for the same stable identity owns its current state.
	// Never replay an old deletion over a restored or moved version.
	newer, err := conflictingProviderEvents(s.db.WithContext(ctx), inbox, []string{p.ItemID})
	if errors.Is(err, errProviderEventOrderUnproven) {
		return result, "deletion_event_order_unproven", nil
	}
	if err != nil {
		return result, "", err
	}
	if len(newer) > 0 {
		return result, "", nil
	}
	var localPath models.MediaLibraryProviderPath
	pathErr := publishedProviderPaths(s.db.WithContext(ctx), library.ID, fingerprint).Where("provider_id = ?", p.ItemID).First(&localPath).Error
	if pathErr != nil && !errors.Is(pathErr, gorm.ErrRecordNotFound) {
		return result, "", pathErr
	}
	if pathErr == nil && localPath.ObservedAt.After(inbox.EventTime) {
		return result, "deletion_newer_observation", nil
	}
	// A current source-bound path or catalog identity may recover an older
	// delivery's missing fingerprint, but a bare filename never may.
	if row.SourceFingerprint == "" {
		_, currentIdentity := known[p.ItemID]
		if !currentIdentity && pathErr != nil {
			artifactIDs, err := deletedProviderArtifactIDs(s.db.WithContext(ctx), library.ID, []string{p.ItemID})
			if err != nil && !errors.Is(err, ErrCatalogFence) && !errors.Is(err, gorm.ErrRecordNotFound) {
				return result, "", err
			}
			if err != nil || len(artifactIDs) == 0 {
				return result, "deletion_source_unproven", nil
			}
		}
		if err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			var current models.MediaLibrary
			var currentStorage models.Storage
			if err := tx.First(&current, library.ID).Error; err != nil {
				return err
			}
			if err := tx.First(&currentStorage, current.StorageID).Error; err != nil {
				return err
			}
			if catalogSourceFingerprint(current, currentStorage) != fingerprint || current.DirtyGeneration != library.DirtyGeneration {
				return errMediaLibraryConfigurationChanged
			}
			return tx.Model(&models.MediaLibraryProviderEvent{}).Where("id = ? AND library_id = ? AND source_fingerprint = '' AND processed_at IS NULL", row.ID, library.ID).Update("source_fingerprint", fingerprint).Error
		}); err != nil {
			return result, "", err
		}
		row.SourceFingerprint = fingerprint
	}
	if pathErr == nil && localPath.IsDir {
		return s.prepareLocalDirectoryDeletion(ctx, library, storage, inbox, localPath)
	}
	_, proven := known[p.ItemID]
	proven = proven || pathErr == nil
	var newerObservation bool
	if err := s.withCatalogRead(ctx, []uint{library.ID}, func(tx *gorm.DB, reader *CatalogReader) error {
		var entries []models.MediaLibraryEntry
		if err := reader.Entries().Where("provider_id = ?", p.ItemID).Find(&entries).Error; err != nil {
			return err
		}
		for _, e := range entries {
			newerObservation = newerObservation || e.UpdatedAt.After(inbox.EventTime)
		}
		var assets []models.MediaLibrarySourceAsset
		if err := reader.SourceAssets().Where("provider_id = ?", p.ItemID).Find(&assets).Error; err != nil {
			return err
		}
		for _, a := range assets {
			newerObservation = newerObservation || a.UpdatedAt.After(inbox.EventTime)
		}
		return nil
	}); err != nil {
		return result, "", err
	}
	if newerObservation {
		return result, "deletion_newer_observation", nil
	}
	if !proven {
		var count int64
		if err := s.db.WithContext(ctx).Model(&models.MediaArtifact{}).Where("library_id = ? AND provider_item_id = ? AND managed = ? AND status != ?", library.ID, p.ItemID, true, "removed").Count(&count).Error; err != nil {
			return result, "", err
		}
		proven = count > 0
	}
	if !proven {
		var count int64
		if err := s.db.WithContext(ctx).Model(&models.MediaLibraryDeletionEvidence{}).Where("library_id = ? AND source_fingerprint = ? AND provider_id = ?", library.ID, fingerprint, p.ItemID).Count(&count).Error; err != nil {
			return result, "", err
		}
		proven = count > 0
	}
	if !proven {
		return result, "deletion_identity_unproven", nil
	}
	if _, err := deletedProviderArtifactIDs(s.db.WithContext(ctx), library.ID, []string{p.ItemID}); err != nil {
		if errors.Is(err, ErrCatalogFence) || errors.Is(err, gorm.ErrRecordNotFound) {
			return result, "deletion_artifact_source_unproven", nil
		}
		return result, "", err
	}
	// Preserve this file-type proof before publication so a lost acknowledgement
	// still resolves after the entry and its owned STRM have disappeared.
	proof := models.MediaLibraryDeletionEvidence{LibraryID: library.ID, SourceFingerprint: fingerprint, ProviderID: p.ItemID, EventTime: inbox.EventTime, CreatedAt: time.Now().UTC()}
	if err := s.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&proof).Error; err != nil {
		return result, "", err
	}
	result.DeletedProviderIDs = []string{p.ItemID}
	return result, "", nil
}

// Revisit at most 32 paused notices per hydration, rotating via retry_after.
// This is local SQL only, at most once per notice per five minutes; no provider
// request and no automatic full scan. NULL deadlines make unseen rows first.
func (s *MediaLibraryService) recheckPausedProviderDeletions(ctx context.Context, libraryID uint) error {
	var rows []models.MediaLibraryProviderEvent
	if err := s.db.WithContext(ctx).Where("library_id = ? AND processed_at IS NULL AND resolution_code = ?", libraryID, providerEventNeedsReview).Order("retry_after, id").Limit(32).Find(&rows).Error; err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, row := range rows {
		if row.RetryAfter != nil && row.RetryAfter.After(now) {
			continue
		}
		payload, ok := decodeProviderEventPayload(row.PayloadJSON)
		if ok && payload.Kind == cloudpkg.ChangeDeleted {
			complete, err := s.completeAbsentProviderDeletion(ctx, libraryID, row.ID, payload.ItemID)
			if err != nil {
				return err
			}
			if !complete && row.ResolutionReason != "deletion_scope_too_large" {
				var library models.MediaLibrary
				var storage models.Storage
				if err := s.db.WithContext(ctx).First(&library, libraryID).Error; err != nil {
					return err
				}
				if err := s.db.WithContext(ctx).First(&storage, library.StorageID).Error; err != nil {
					return err
				}
				known, err := s.knownPan115CatalogProviderIDs(ctx, libraryID, providerChangeScope{Events: []providerChangeEvent{providerChangeEvent(payload)}})
				if err != nil {
					return err
				}
				_, reason, err := s.prepareLocalProviderDeletion(ctx, library, storage, row, payload, known)
				if err != nil {
					return err
				}
				if reason == "" {
					if err := s.db.WithContext(ctx).Model(&models.MediaLibraryProviderEvent{}).Where("id = ? AND processed_at IS NULL AND resolution_code = ?", row.ID, providerEventNeedsReview).Updates(map[string]any{"resolution_code": "", "resolution_reason": "", "retry_after": nil}).Error; err != nil {
						return err
					}
					continue
				}
			}
		}
		if err := s.db.WithContext(ctx).Model(&models.MediaLibraryProviderEvent{}).Where("id = ? AND processed_at IS NULL AND resolution_code = ?", row.ID, providerEventNeedsReview).Update("retry_after", now.Add(5*time.Minute)).Error; err != nil {
			return err
		}
	}
	return nil
}

func (s *MediaLibraryService) deferProviderDelivery(ctx context.Context, row models.MediaLibraryProviderEvent, p providerEventPayload, reason string, review bool) error {
	now := time.Now().UTC()
	code := reason
	var next *time.Time
	message := "事件暂时无法处理，延后重试；继续处理其他文件"
	if review {
		code = providerEventNeedsReview
		message = "删除通知仍有关联但暂不能安全处理，已保留；系统将继续本地检查，不重复请求网盘"
	}
	// Existing supervisor/provider adaptive delay owns transient failures.
	if err := s.db.WithContext(ctx).Model(&models.MediaLibraryProviderEvent{}).Where("id = ? AND library_id = ? AND processed_at IS NULL", row.ID, row.LibraryID).Updates(map[string]any{"resolution_code": code, "resolution_reason": reason, "retry_after": next, "updated_at": now}).Error; err != nil {
		return err
	}
	name := safeProviderEventName(p.Name)
	s.log.Warn().Uint("library_id", row.LibraryID).Uint("delivery_id", row.ID).Str("file_name", name).Str("operation", p.Kind).Str("reason_code", reason).Msg(message)
	return nil
}

func safeProviderEventName(raw string) string {
	name := path.Base(strings.ReplaceAll(raw, "\\", "/"))
	// Event names are untrusted; URLs and opaque provider IDs are not diagnostics.
	if name == "." || strings.ContainsAny(name, "?:#") || strings.Contains(raw, "://") {
		name = ""
	}
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, name)
	return name
}

func (s *MediaLibraryService) prepareLocalDirectoryDeletion(ctx context.Context, library models.MediaLibrary, storage models.Storage, inbox models.ProviderEvent, directory models.MediaLibraryProviderPath) (medialibrary.Result, string, error) {
	result := medialibrary.Result{Partial: true, Scoped: true}
	if directory.RelativePath == "/" || path.Clean(directory.RelativePath) != directory.RelativePath {
		return result, "deletion_directory_unproven", nil
	}
	prefix := directory.RelativePath + "/"
	ids := map[string]bool{}
	var paths []models.MediaLibraryProviderPath
	if err := publishedProviderPaths(s.db.WithContext(ctx), library.ID, directory.SourceFingerprint).Where("substr(relative_path, 1, ?) = ? AND is_dir = ?", len([]rune(prefix)), prefix, false).Find(&paths).Error; err != nil {
		return result, "", err
	}
	for _, p := range paths {
		if !p.ObservedAt.After(inbox.EventTime) {
			ids[p.ProviderID] = true
		}
	}
	if err := s.withCatalogRead(ctx, []uint{library.ID}, func(tx *gorm.DB, reader *CatalogReader) error {
		var entries []models.MediaLibraryEntry
		if err := reader.Entries().Where("substr(relative_path, 1, ?) = ?", len([]rune(prefix)), prefix).Find(&entries).Error; err != nil {
			return err
		}
		for _, e := range entries {
			if e.UpdatedAt.After(inbox.EventTime) {
				delete(ids, e.ProviderID)
			} else {
				ids[e.ProviderID] = true
			}
		}
		var assets []models.MediaLibrarySourceAsset
		if err := reader.SourceAssets().Where("substr(relative_path, 1, ?) = ?", len([]rune(prefix)), prefix).Find(&assets).Error; err != nil {
			return err
		}
		for _, a := range assets {
			if a.UpdatedAt.After(inbox.EventTime) {
				delete(ids, a.ProviderID)
			} else {
				ids[a.ProviderID] = true
			}
		}
		// Saved identity history may refer to a former path. Recheck ALL IDs,
		// including files now outside this subtree, before issuing tombstones.
		candidates := make([]string, 0, len(ids))
		for id := range ids {
			candidates = append(candidates, id)
		}
		for start := 0; start < len(candidates); start += 100 {
			batch := candidates[start:min(start+100, len(candidates))]
			var currentEntries []models.MediaLibraryEntry
			if err := reader.Entries().Where("provider_id IN ?", batch).Find(&currentEntries).Error; err != nil {
				return err
			}
			for _, e := range currentEntries {
				if !strings.HasPrefix(e.RelativePath, prefix) || e.UpdatedAt.After(inbox.EventTime) {
					delete(ids, e.ProviderID)
				}
			}
			var currentAssets []models.MediaLibrarySourceAsset
			if err := reader.SourceAssets().Where("provider_id IN ?", batch).Find(&currentAssets).Error; err != nil {
				return err
			}
			for _, a := range currentAssets {
				if !strings.HasPrefix(a.RelativePath, prefix) || a.UpdatedAt.After(inbox.EventTime) {
					delete(ids, a.ProviderID)
				}
			}
		}
		return nil
	}); err != nil {
		return result, "", err
	}
	candidates := make([]string, 0, len(ids))
	for id := range ids {
		candidates = append(candidates, id)
	}
	for start := 0; start < len(candidates); start += 100 {
		newer, err := conflictingProviderEvents(s.db.WithContext(ctx), inbox, candidates[start:min(start+100, len(candidates))])
		if errors.Is(err, errProviderEventOrderUnproven) {
			return result, "deletion_event_order_unproven", nil
		}
		if err != nil {
			return result, "", err
		}
		for id := range newer {
			delete(ids, id)
		}
	}
	for id := range ids {
		result.DeletedProviderIDs = append(result.DeletedProviderIDs, id)
	}
	sort.Strings(result.DeletedProviderIDs)
	return result, "", nil
}
