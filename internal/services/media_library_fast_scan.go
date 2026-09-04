package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/rs/zerolog"
	serverlog "github.com/yuanjing-hash/OhMyCine-Server/internal/logging"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/medialibrary"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	JobTypeMediaLibraryRecognition = "media_library_recognition"
	mediaLibraryStagingBatchSize   = 5000
	mediaLibraryStagingRetention   = 7 * 24 * time.Hour
)

type mediaLibraryRecognitionJobPayload struct {
	LibraryID  uint   `json:"library_id"`
	ScanRunID  uint   `json:"scan_run_id"`
	Generation uint64 `json:"generation"`
}

type fastScanMediaAction struct {
	Action    string
	Title     string
	MediaType string
}

func mediaLibraryScanSourceFingerprint(library models.MediaLibrary, storage models.Storage, profile models.MediaClassificationProfile) string {
	hash := sha256.New()
	connectionID := uint64(0)
	if storage.ConnectionID != nil {
		connectionID = uint64(*storage.ConnectionID)
	}
	for _, value := range []string{
		strconv.FormatUint(uint64(library.ID), 10), strconv.FormatUint(uint64(storage.ID), 10), storage.Type,
		storage.RootPathNormalized, strconv.FormatUint(connectionID, 10), library.RelativeRoot, library.ProviderRootID,
		strconv.FormatBool(library.Recursive), library.VideoExtensionsJSON, library.STRMAssetExtraExtensionsJSON,
		library.IgnorePatternsJSON, library.MetadataLanguage, library.MetadataRegion, library.MatchStrategy,
		strconv.FormatUint(uint64(profile.ID), 10), strconv.FormatUint(profile.Revision, 10),
		strconv.FormatUint(library.DirtyGeneration, 10),
	} {
		hash.Write([]byte(value))
		hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func (s *MediaLibraryService) publishFastPan115Scan(ctx context.Context, library models.MediaLibrary, storage models.Storage, profile models.MediaClassificationProfile, run models.MediaLibraryScanRun, result medialibrary.Result, started time.Time, operation serverlog.Operation) (models.MediaLibraryScanRun, error) {
	run.Discovered = len(result.Files)
	run.Enumerated = max(result.Enumerated, len(result.Files)+len(result.Assets)+result.Deduplicated)
	run.Processed = len(result.Files) + len(result.Assets)
	run.Deduplicated = result.Deduplicated
	run.Partial = result.Partial
	run.Phase = "staging"
	if err := s.db.WithContext(ctx).Model(&run).Updates(map[string]any{
		"phase": run.Phase, "discovered": run.Discovered, "enumerated": run.Enumerated,
		"processed": run.Processed, "deduplicated": run.Deduplicated, "partial": run.Partial,
	}).Error; err != nil {
		return s.failFastScanPersistence(run, operation, started, mediaLibraryPersistenceStageScanRun, err)
	}
	operation.Event(s.log.Info()).Uint("library_id", library.ID).Uint("scan_run_id", run.ID).Uint64("generation", run.Generation).
		Str("scan_kind", run.Kind).Str("phase", "processing").Int("worker_count", medialibrary.ProviderProcessingWorkers).
		Int("discovered", run.Discovered).Int("deduplicated", run.Deduplicated).Msg(operation.Message("128 线程媒体处理完成"))
	for _, file := range result.Files {
		parsed := medialibrary.ParseMedia(filepath.Base(file.RelativePath), file.RelativePath)
		logFastScanMediaAction(s.log, operation, run, "processing", "discovered", parsed.Title, parsed.MediaType)
	}

	operation.Event(s.log.Info()).Uint("library_id", library.ID).Uint("scan_run_id", run.ID).Uint64("generation", run.Generation).
		Str("scan_kind", run.Kind).Str("phase", "staging").Str("action", "staging_started").Int("total", len(result.Files)+len(result.Assets)).
		Msg(operation.Message("开始分批保存扫描结果"))
	if err := s.stageFastMediaLibraryScan(ctx, &run, result, operation); err != nil {
		return s.failFastScanPersistence(run, operation, started, mediaLibraryPersistenceStageEntries, err)
	}
	operation.Event(s.log.Info()).Uint("library_id", library.ID).Uint("scan_run_id", run.ID).Uint64("generation", run.Generation).
		Str("scan_kind", run.Kind).Str("phase", "staging").Str("action", "staging_completed").Int("persisted", run.Persisted).
		Msg(operation.Message("扫描结果分批落库完成"))

	units := medialibrary.GroupRecognitionUnits(result.Files)
	var stabilizeErr error
	units, stabilizeErr = s.stabilizeExistingRecognitionUnits(ctx, library.ID, units)
	if stabilizeErr != nil {
		return s.failFastScanPersistence(run, operation, started, mediaLibraryPersistenceStageLoadEntries, stabilizeErr)
	}
	unitByPath := make(map[string]medialibrary.RecognitionUnit, len(result.Files))
	currentSourceKeys := make([]string, 0, len(units))
	for _, unit := range units {
		currentSourceKeys = append(currentSourceKeys, unit.SourceKey)
		for _, file := range unit.Files {
			unitByPath[file.RelativePath] = unit
		}
	}
	run.Phase = "publishing"
	_ = s.db.WithContext(ctx).Model(&run).Updates(map[string]any{"phase": run.Phase}).Error
	operation.Event(s.log.Info()).Uint("library_id", library.ID).Uint("scan_run_id", run.ID).Uint64("generation", run.Generation).
		Str("scan_kind", run.Kind).Str("phase", "publishing").Str("action", "catalog_publish_started").
		Msg(operation.Message("开始原子发布基础目录"))

	publishedAt := time.Now().UTC()
	var committedChange models.MediaLibraryChange
	var committedMediaActions []fastScanMediaAction
	var pendingSourceKeys map[string]struct{}
	var reusedSourceStatus map[string]string
	transactionErr := retryMediaLibraryBusy(ctx, func() error {
		run.Added, run.Updated, run.Removed = 0, 0, 0
		run.Matched, run.Unrecognized, run.CacheHits, run.RecognitionFailed = 0, 0, 0, 0
		committedChange = models.MediaLibraryChange{}
		committedMediaActions = committedMediaActions[:0]
		pendingSourceKeys = make(map[string]struct{}, len(units))
		reusedSourceStatus = make(map[string]string, len(units))
		return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			var currentLibrary models.MediaLibrary
			if err := tx.First(&currentLibrary, library.ID).Error; err != nil {
				return wrapMediaLibraryPersistence(mediaLibraryPersistenceStageConfiguration, err)
			}
			var currentStorage models.Storage
			if err := tx.First(&currentStorage, currentLibrary.StorageID).Error; err != nil {
				return wrapMediaLibraryPersistence(mediaLibraryPersistenceStageConfiguration, err)
			}
			var currentProfile models.MediaClassificationProfile
			if err := tx.First(&currentProfile, currentLibrary.ProfileID).Error; err != nil {
				return wrapMediaLibraryPersistence(mediaLibraryPersistenceStageConfiguration, err)
			}
			if mediaLibraryScanSourceFingerprint(currentLibrary, currentStorage, currentProfile) != run.SourceFingerprint {
				return wrapMediaLibraryPersistence(mediaLibraryPersistenceStageConfiguration, errMediaLibraryConfigurationChanged)
			}

			var existing []models.MediaLibraryEntry
			if err := tx.Where("library_id = ?", library.ID).Find(&existing).Error; err != nil {
				return wrapMediaLibraryPersistence(mediaLibraryPersistenceStageLoadEntries, err)
			}
			byPath := make(map[string]models.MediaLibraryEntry, len(existing))
			byProvider := make(map[string]models.MediaLibraryEntry, len(existing))
			for _, entry := range existing {
				byPath[entry.RelativePath] = entry
				if entry.ProviderID != "" {
					byProvider[entry.ProviderID] = entry
				}
			}
			deletedProviderIDs := make(map[string]struct{}, len(result.DeletedProviderIDs))
			for _, providerID := range result.DeletedProviderIDs {
				deletedProviderIDs[providerID] = struct{}{}
				if entry, exists := byProvider[providerID]; exists {
					run.Removed++
					committedMediaActions = append(committedMediaActions, fastScanMediaAction{Action: "removed", Title: entry.Title, MediaType: entry.MediaType})
					if err := tx.Delete(&entry).Error; err != nil {
						return wrapMediaLibraryPersistence(mediaLibraryPersistenceStagePrune, err)
					}
					delete(byProvider, providerID)
					delete(byPath, entry.RelativePath)
				}
			}
			var recognitionRecords []models.MediaLibraryRecognition
			if err := tx.Where("library_id = ?", library.ID).Find(&recognitionRecords).Error; err != nil {
				return wrapMediaLibraryPersistence(mediaLibraryPersistenceStageRecognition, err)
			}
			bySource := make(map[string]models.MediaLibraryRecognition, len(recognitionRecords))
			for _, record := range recognitionRecords {
				bySource[record.SourceKey] = record
			}

			now := time.Now().UTC()
			var existingAssets []models.MediaLibrarySourceAsset
			if err := tx.Where("library_id = ?", library.ID).Find(&existingAssets).Error; err != nil {
				return wrapMediaLibraryPersistence(mediaLibraryPersistenceStageSourceAssets, err)
			}
			assetsByPath := make(map[string]models.MediaLibrarySourceAsset, len(existingAssets))
			assetsByProvider := make(map[string]models.MediaLibrarySourceAsset, len(existingAssets))
			for _, asset := range existingAssets {
				assetsByPath[asset.RelativePath] = asset
				if asset.ProviderID != "" {
					assetsByProvider[asset.ProviderID] = asset
				}
			}
			for providerID := range deletedProviderIDs {
				if asset, exists := assetsByProvider[providerID]; exists {
					if err := tx.Delete(&asset).Error; err != nil {
						return wrapMediaLibraryPersistence(mediaLibraryPersistenceStagePrune, err)
					}
					delete(assetsByProvider, providerID)
					delete(assetsByPath, asset.RelativePath)
				}
			}
			assetRows := make([]models.MediaLibrarySourceAsset, 0, len(result.Assets))
			for _, source := range result.Assets {
				asset, exists := assetsByPath[source.RelativePath]
				oldPath := source.RelativePath
				if !exists && source.ProviderID != "" {
					if providerAsset, providerExists := assetsByProvider[source.ProviderID]; providerExists {
						asset, exists, oldPath = providerAsset, true, providerAsset.RelativePath
					}
				}
				if !exists {
					asset = models.MediaLibrarySourceAsset{LibraryID: library.ID, RelativePath: source.RelativePath, CreatedAt: now}
				}
				asset.RelativePath = source.RelativePath
				asset.Generation, asset.ProviderID, asset.ParentProviderID = run.Generation, source.ProviderID, source.ParentProviderID
				asset.Name, asset.Extension, asset.Size = source.Name, source.Extension, source.Size
				asset.ModifiedAt, asset.HashHint, asset.Active, asset.UpdatedAt = source.ModifiedAt, source.HashHint, true, now
				if exists && oldPath != source.RelativePath {
					if err := tx.Save(&asset).Error; err != nil {
						return wrapMediaLibraryPersistence(mediaLibraryPersistenceStageSourceAssets, err)
					}
				} else {
					asset.ID = 0
					assetRows = append(assetRows, asset)
				}
				delete(assetsByPath, oldPath)
				delete(assetsByPath, source.RelativePath)
				delete(assetsByProvider, source.ProviderID)
			}
			if len(assetRows) > 0 {
				columns := []string{"generation", "provider_id", "parent_provider_id", "name", "extension", "size", "modified_at", "hash_hint", "active", "updated_at"}
				if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "library_id"}, {Name: "relative_path"}}, DoUpdates: clause.AssignmentColumns(columns)}).CreateInBatches(assetRows, 500).Error; err != nil {
					return wrapMediaLibraryPersistence(mediaLibraryPersistenceStageSourceAssets, err)
				}
			}

			entryRows := make([]models.MediaLibraryEntry, 0, len(result.Files))
			for _, file := range result.Files {
				entry, exists := byPath[file.RelativePath]
				oldPath := file.RelativePath
				if !exists && file.ProviderID != "" {
					if providerEntry, providerExists := byProvider[file.ProviderID]; providerExists {
						entry, exists, oldPath = providerEntry, true, providerEntry.RelativePath
					}
				}
				before := entry
				physicalChanged := exists && (oldPath != file.RelativePath || entry.Size != file.Size || !entry.ModifiedAt.Equal(file.ModifiedAt))
				if !exists {
					run.Added++
					entry = models.MediaLibraryEntry{LibraryID: library.ID, RelativePath: file.RelativePath, CreatedAt: now}
				}
				entry.RelativePath, entry.ProviderID, entry.Size, entry.ModifiedAt = file.RelativePath, file.ProviderID, file.Size, file.ModifiedAt
				parsed := medialibrary.ParseMedia(filepath.Base(file.RelativePath), file.RelativePath)
				unit := unitByPath[file.RelativePath]
				record, reusable := bySource[unit.SourceKey]
				reusable = reusable && record.Status != mediaRecognitionStatusPending && record.ProfileID == profile.ID && record.ProfileRevision == profile.Revision && (record.ManualOverride || record.InputFingerprint == unit.InputFingerprint) && mediaLibraryRecognitionProjectionFresh(record)
				if reusable {
					reusedSourceStatus[unit.SourceKey] = record.Status
					entry.RecognitionID = &record.ID
					entry.MediaType, entry.Title = record.MediaType, record.Title
					if entry.MediaType == "" {
						entry.MediaType = parsed.MediaType
					}
					if entry.Title == "" {
						entry.Title = parsed.Title
					}
					entry.SeriesTitle, entry.Season, entry.Episode = parsed.SeriesTitle, parsed.Season, parsed.Episode
					if entry.MediaType == "tv" {
						entry.SeriesTitle = entry.Title
					}
					entry.MatchStatus, entry.RecognitionErrorCode = record.Status, record.ErrorCode
					entry.CategoryName, entry.MatchedRuleID = record.CategoryName, record.MatchedRuleID
					entry.TMDBID, entry.ReleaseYear, entry.MatchConfidence = cloneInt64(record.TMDBID), cloneInt(record.ReleaseYear), cloneFloat64(record.Confidence)
					entry.WorkKey = recognitionWorkKey(MediaRecognitionResult{Status: record.Status, MediaType: record.MediaType, Title: record.Title, TMDBID: record.TMDBID}, unit.SourceKey)
				} else {
					pendingSourceKeys[unit.SourceKey] = struct{}{}
					entry.RecognitionID = nil
					entry.MediaType, entry.Title, entry.SeriesTitle = parsed.MediaType, parsed.Title, parsed.SeriesTitle
					entry.Season, entry.Episode = parsed.Season, parsed.Episode
					entry.MatchStatus, entry.RecognitionErrorCode = mediaRecognitionStatusPending, ""
					entry.WorkKey = "file:" + unit.SourceKey
					entry.CategoryName, entry.MatchedRuleID = "", nil
					entry.TMDBID, entry.ReleaseYear, entry.MatchConfidence = nil, nil, nil
				}
				entry.LastGeneration, entry.UpdatedAt = run.Generation, now
				if exists && (physicalChanged || mediaLibraryEntryProjectionChanged(before, entry)) {
					run.Updated++
				}
				action := "unchanged"
				if !exists {
					action = "added"
				} else if physicalChanged || mediaLibraryEntryProjectionChanged(before, entry) {
					action = "updated"
				}
				committedMediaActions = append(committedMediaActions, fastScanMediaAction{Action: action, Title: entry.Title, MediaType: entry.MediaType})
				if exists && oldPath != file.RelativePath {
					if err := tx.Save(&entry).Error; err != nil {
						return wrapMediaLibraryPersistence(mediaLibraryPersistenceStageEntries, err)
					}
				} else {
					entry.ID = 0
					entryRows = append(entryRows, entry)
				}
				delete(byPath, oldPath)
				delete(byPath, file.RelativePath)
				delete(byProvider, file.ProviderID)
			}
			if len(entryRows) > 0 {
				columns := []string{"provider_id", "recognition_id", "size", "modified_at", "media_type", "title", "work_key", "series_title", "season", "episode", "match_status", "tmdb_id", "release_year", "match_confidence", "recognition_error_code", "category_name", "matched_rule_id", "last_generation", "updated_at"}
				if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "library_id"}, {Name: "relative_path"}}, DoUpdates: clause.AssignmentColumns(columns)}).CreateInBatches(entryRows, 500).Error; err != nil {
					return wrapMediaLibraryPersistence(mediaLibraryPersistenceStageEntries, err)
				}
			}
			if !result.Partial {
				for _, entry := range byPath {
					run.Removed++
					committedMediaActions = append(committedMediaActions, fastScanMediaAction{Action: "removed", Title: entry.Title, MediaType: entry.MediaType})
					if err := tx.Delete(&entry).Error; err != nil {
						return wrapMediaLibraryPersistence(mediaLibraryPersistenceStagePrune, err)
					}
				}
				for _, asset := range assetsByPath {
					if err := tx.Delete(&asset).Error; err != nil {
						return wrapMediaLibraryPersistence(mediaLibraryPersistenceStagePrune, err)
					}
				}
			}
			run.RecognitionTotal = len(pendingSourceKeys)
			run.CacheHits = len(reusedSourceStatus)
			for _, status := range reusedSourceStatus {
				if status == mediaRecognitionStatusMatched {
					run.Matched++
				} else {
					run.Unrecognized++
				}
			}
			// Reused projections belong to this published generation even when
			// the same scan also queued other units for recognition. Artifact
			// generation filters by LastGeneration, so advancing only the fully
			// cached case would silently omit valid NFO/artwork in mixed scans.
			if len(reusedSourceStatus) > 0 {
				reusedKeys := make([]string, 0, len(reusedSourceStatus))
				for sourceKey := range reusedSourceStatus {
					reusedKeys = append(reusedKeys, sourceKey)
				}
				if err := tx.Model(&models.MediaLibraryRecognition{}).Where("library_id = ? AND source_key IN ?", library.ID, reusedKeys).
					Updates(map[string]any{"last_generation": run.Generation, "updated_at": now}).Error; err != nil {
					return wrapMediaLibraryPersistence(mediaLibraryPersistenceStageRecognition, err)
				}
			}
			if result.Scoped {
				if err := tx.Where("library_id = ? AND NOT EXISTS (SELECT 1 FROM media_library_entries WHERE media_library_entries.recognition_id = media_library_recognitions.id)", library.ID).
					Delete(&models.MediaLibraryRecognition{}).Error; err != nil {
					return wrapMediaLibraryPersistence(mediaLibraryPersistenceStageRecognition, err)
				}
				if run.RecognitionTotal > 0 {
					if err := reconcileTMDBCollectionsTx(tx, library.ID, false, now); err != nil {
						return wrapMediaLibraryPersistence(mediaLibraryPersistenceStageCollections, err)
					}
				}
			}
			if run.RecognitionTotal == 0 {
				if !result.Partial || result.Scoped {
					deleteQuery := tx.Where("library_id = ?", library.ID)
					if len(currentSourceKeys) > 0 {
						deleteQuery = deleteQuery.Where("source_key NOT IN ?", currentSourceKeys)
					}
					if err := deleteQuery.Delete(&models.MediaLibraryRecognition{}).Error; err != nil {
						return wrapMediaLibraryPersistence(mediaLibraryPersistenceStageRecognition, err)
					}
				}
				if err := reconcileTMDBCollectionsTx(tx, library.ID, result.Partial && !result.Scoped, now); err != nil {
					return wrapMediaLibraryPersistence(mediaLibraryPersistenceStageCollections, err)
				}
			}

			updates := map[string]any{"dirty_generation": run.Generation, "baseline_generation": run.Generation, "last_scan_at": publishedAt, "last_successful_scan_at": publishedAt, "profile_revision": profile.Revision, "reclassification_due": false, "status_error_code": "", "next_retry_at": nil}
			if err := tx.Model(&models.MediaLibrary{}).Where("id = ?", library.ID).Updates(updates).Error; err != nil {
				return wrapMediaLibraryPersistence(mediaLibraryPersistenceStageGeneration, err)
			}
			run.Status, run.Phase, run.CatalogPublishedAt = "catalog_ready", "recognition_queued", &publishedAt
			if run.RecognitionTotal == 0 {
				run.Status, run.Phase, run.RecognitionCompleted, run.FinishedAt = "success", "completed", 0, &publishedAt
			}
			run.Persisted = len(result.Files) + len(result.Assets)
			if err := tx.Save(&run).Error; err != nil {
				return wrapMediaLibraryPersistence(mediaLibraryPersistenceStageScanRun, err)
			}
			if err := tx.Where("run_id = ?", run.ID).Delete(&models.MediaLibraryScanStaging{}).Error; err != nil {
				return wrapMediaLibraryPersistence(mediaLibraryPersistenceStageEntries, err)
			}
			if s.changes != nil && (!run.Partial || result.Scoped) && (run.Added > 0 || run.Updated > 0 || run.Removed > 0) {
				kind := models.MediaLibraryChangeCatalog
				if run.Removed > 0 && run.Added == 0 && run.Updated == 0 {
					kind = models.MediaLibraryChangeRemoval
				}
				requiresArtifacts := mediaLibraryRequiresArtifacts(storage.Type, currentLibrary, s.artifacts != nil)
				change, err := s.changes.RecordTx(tx, library.ID, run.Generation, kind, !requiresArtifacts)
				if err != nil {
					return wrapMediaLibraryPersistence(mediaLibraryPersistenceStageChange, err)
				}
				committedChange = change
			}
			return nil
		})
	})
	if transactionErr != nil {
		stage, _ := mediaLibraryPersistenceDiagnostics(transactionErr)
		return s.failFastScanPersistence(run, operation, started, stage, transactionErr)
	}
	changedSamples := 0
	changedTotal := 0
	for _, item := range committedMediaActions {
		logFastScanMediaAction(s.log, operation, run, "publishing", item.Action, item.Title, item.MediaType)
		if item.Action == "unchanged" {
			continue
		}
		changedTotal++
		if changedSamples < 20 {
			operation.Event(s.log.Info()).Uint("library_id", run.LibraryID).Uint("scan_run_id", run.ID).Uint64("generation", run.Generation).
				Str("scan_kind", run.Kind).Str("phase", "publishing").Str("action", item.Action).
				Str("media_display_name", safeMediaDisplayName(item.Title)).Str("media_type", item.MediaType).
				Msg(operation.Message("媒体目录变更"))
			changedSamples++
		}
	}
	if changedTotal > changedSamples {
		operation.Event(s.log.Info()).Uint("library_id", run.LibraryID).Uint("scan_run_id", run.ID).Uint64("generation", run.Generation).
			Str("scan_kind", run.Kind).Str("phase", "publishing").Int("sampled", changedSamples).Int("remaining", changedTotal-changedSamples).
			Msg(operation.Message("其余媒体目录变更已汇总，debug 级别可查看逐条明细"))
	}
	if committedChange.State == models.MediaLibraryChangeReady && s.changes != nil {
		s.changes.NotifyCommitted(committedChange.LibraryID, committedChange.Revision)
	}
	operation.Event(s.log.Info()).Uint("library_id", library.ID).Uint("scan_run_id", run.ID).Uint64("generation", run.Generation).
		Str("scan_kind", run.Kind).Str("phase", run.Phase).Int("added", run.Added).Int("updated", run.Updated).
		Int("removed", run.Removed).Int("recognition_total", run.RecognitionTotal).Int64("duration_ms", time.Since(started).Milliseconds()).
		Msg(operation.Message(map[bool]string{true: "基础目录已发布，本轮元数据全部复用", false: "基础目录已发布，元数据转入后台识别"}[run.RecognitionTotal == 0]))
	if s.structure != nil && !run.Partial {
		if err := s.structure.EnqueueAutomaticDiagnosis(ctx, library.ID, run.ID, run.Generation, run.Kind); err != nil {
			serverlog.OperationMediaLibraryStructureDiagnosis.Event(s.log.Warn()).Uint("library_id", library.ID).Uint("scan_run_id", run.ID).Uint64("generation", run.Generation).
				Str("scan_kind", run.Kind).Str("phase", "enqueue_failed").Str("error_code", CodeMediaLibraryStructureDiagnosisFailed).
				Msg(serverlog.OperationMediaLibraryStructureDiagnosis.Message("目录结构诊断入队失败，基础目录仍然可用"))
		}
	}
	// STRM and base artwork are allowed to consume the published pending
	// catalog immediately. The recognition worker schedules the same generation
	// again only when metadata changes; the artifact service coalesces it.
	if s.artifacts != nil && mediaLibraryArtifactGenerationRequired(run.Kind, run, false) {
		if err := s.artifacts.ScheduleGeneration(library.ID, run.Generation); err != nil {
			serverlog.OperationMediaArtifact.Event(s.log.Warn()).Uint("library_id", library.ID).Uint("scan_run_id", run.ID).Uint64("generation", run.Generation).
				Str("scan_kind", run.Kind).Str("phase", "artifact_scheduling").Str("error_code", "artifact_schedule_failed").Msg(serverlog.OperationMediaArtifact.Message("基础目录产物入队失败"))
		} else {
			serverlog.OperationMediaArtifact.Event(s.log.Info()).Uint("library_id", library.ID).Uint("scan_run_id", run.ID).Uint64("generation", run.Generation).
				Str("scan_kind", run.Kind).Str("phase", "artifact_scheduling").Str("action", "artifact_queued").Msg(serverlog.OperationMediaArtifact.Message("基础目录 STRM/产物已进入后台队列"))
		}
	}
	if s.libraryArtwork != nil {
		if err := s.libraryArtwork.ScheduleGeneration(library.ID, !run.Partial); err != nil {
			serverlog.OperationMediaArtifact.Event(s.log.Warn()).Uint("library_id", library.ID).Uint("scan_run_id", run.ID).Uint64("generation", run.Generation).
				Str("scan_kind", run.Kind).Str("phase", "artifact_scheduling").Str("error_code", "library_artwork_schedule_failed").Msg(serverlog.OperationMediaArtifact.Message("媒体库分类封面入队失败"))
		} else {
			serverlog.OperationMediaArtifact.Event(s.log.Info()).Uint("library_id", library.ID).Uint("scan_run_id", run.ID).Uint64("generation", run.Generation).
				Str("scan_kind", run.Kind).Str("phase", "artifact_scheduling").Str("action", "library_artwork_queued").Msg(serverlog.OperationMediaArtifact.Message("媒体库分类封面已进入后台队列"))
		}
	}
	if run.RecognitionTotal == 0 {
		return run, nil
	}
	if err := s.enqueueMediaLibraryRecognition(library, run); err != nil {
		run.Phase, run.ErrorCode = "recognition_enqueue_failed", "media_library_recognition_enqueue_failed"
		_ = s.db.Model(&run).Updates(map[string]any{"phase": run.Phase, "error_code": run.ErrorCode}).Error
		serverlog.OperationMediaRecognition.Event(s.log.Error()).Uint("library_id", library.ID).Uint("scan_run_id", run.ID).Uint64("generation", run.Generation).
			Str("scan_kind", run.Kind).Str("phase", run.Phase).Str("error_code", "media_library_recognition_enqueue_failed").Msg(serverlog.OperationMediaRecognition.Message("后台识别入队失败，基础目录仍然可用"))
		return run, nil
	}
	serverlog.OperationMediaRecognition.Event(s.log.Info()).Uint("library_id", library.ID).Uint("scan_run_id", run.ID).Uint64("generation", run.Generation).
		Str("scan_kind", run.Kind).Str("phase", "recognition_queued").Str("action", "recognition_queued").Int("recognition_total", run.RecognitionTotal).Msg(serverlog.OperationMediaRecognition.Message("后台识别已进入持久任务队列"))
	return run, nil
}

func logFastScanMediaAction(logger zerolog.Logger, operation serverlog.Operation, run models.MediaLibraryScanRun, phase, action, title, mediaType string) {
	operation.Event(logger.Debug()).Uint("library_id", run.LibraryID).Uint("scan_run_id", run.ID).Uint64("generation", run.Generation).
		Str("scan_kind", run.Kind).Str("phase", phase).Str("action", action).
		Str("media_display_name", safeMediaDisplayName(title)).Str("media_type", mediaType).
		Msg(operation.Message("媒体目录动作"))
}

func (s *MediaLibraryService) stageFastMediaLibraryScan(ctx context.Context, run *models.MediaLibraryScanRun, result medialibrary.Result, operation serverlog.Operation) error {
	now := time.Now().UTC()
	rows := make([]models.MediaLibraryScanStaging, 0, len(result.Files)+len(result.Assets))
	for index, file := range result.Files {
		rows = append(rows, models.MediaLibraryScanStaging{RunID: run.ID, LibraryID: run.LibraryID, ItemKind: "video", RelativePath: file.RelativePath, ProviderID: file.ProviderID, Size: file.Size, ModifiedAt: file.ModifiedAt, RowOffset: index, CreatedAt: now, UpdatedAt: now})
	}
	for index, asset := range result.Assets {
		rows = append(rows, models.MediaLibraryScanStaging{RunID: run.ID, LibraryID: run.LibraryID, ItemKind: "asset", RelativePath: asset.RelativePath, ProviderID: asset.ProviderID, ParentProviderID: asset.ParentProviderID, Name: asset.Name, Extension: asset.Extension, Size: asset.Size, ModifiedAt: asset.ModifiedAt, HashHint: asset.HashHint, RowOffset: len(result.Files) + index, CreatedAt: now, UpdatedAt: now})
	}
	columns := []string{"provider_id", "parent_provider_id", "name", "extension", "size", "modified_at", "hash_hint", "row_offset", "updated_at"}
	type stagingCheckpoint struct {
		NextRow int `json:"next_row"`
		Total   int `json:"total"`
	}
	checkpointState := stagingCheckpoint{}
	_ = json.Unmarshal([]byte(run.CheckpointJSON), &checkpointState)
	startRow := 0
	if checkpointState.Total == len(rows) && checkpointState.NextRow > 0 && checkpointState.NextRow <= len(rows) {
		var durablePrefix int64
		if err := s.db.WithContext(ctx).Model(&models.MediaLibraryScanStaging{}).
			Select("COUNT(DISTINCT row_offset)").
			Where("run_id = ? AND row_offset >= 0 AND row_offset < ?", run.ID, checkpointState.NextRow).
			Scan(&durablePrefix).Error; err != nil {
			return err
		}
		// There are exactly NextRow possible offsets in [0, NextRow). Requiring
		// that many distinct values proves the whole durable prefix and rejects
		// duplicate offsets that would make a plain row count lie.
		if durablePrefix == int64(checkpointState.NextRow) {
			startRow = checkpointState.NextRow
		}
	}
	if startRow > 0 {
		operation.Event(s.log.Info()).Uint("library_id", run.LibraryID).Uint("scan_run_id", run.ID).Uint64("generation", run.Generation).
			Str("scan_kind", run.Kind).Str("phase", "staging").Int("persisted", startRow).Int("total", len(rows)).
			Msg(operation.Message("从已提交的扫描批次继续落库"))
	}
	for start := startRow; start < len(rows); start += mediaLibraryStagingBatchSize {
		end := min(start+mediaLibraryStagingBatchSize, len(rows))
		checkpoint, _ := json.Marshal(stagingCheckpoint{NextRow: end, Total: len(rows)})
		if err := retryMediaLibraryBusy(ctx, func() error {
			return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
				if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "run_id"}, {Name: "item_kind"}, {Name: "relative_path"}}, DoUpdates: clause.AssignmentColumns(columns)}).CreateInBatches(rows[start:end], 500).Error; err != nil {
					return err
				}
				return tx.Model(&models.MediaLibraryScanRun{}).Where("id = ?", run.ID).Updates(map[string]any{"persisted": end, "checkpoint_json": string(checkpoint)}).Error
			})
		}); err != nil {
			return err
		}
		run.Persisted = end
		run.CheckpointJSON = string(checkpoint)
		operation.Event(s.log.Info()).Uint("library_id", run.LibraryID).Uint("scan_run_id", run.ID).Uint64("generation", run.Generation).
			Str("scan_kind", run.Kind).Str("phase", "staging").Int("batch", start/mediaLibraryStagingBatchSize+1).
			Int("batch_items", end-start).Int("persisted", end).Int("total", len(rows)).Msg(operation.Message("扫描批次已安全落库"))
	}
	return nil
}

func (s *MediaLibraryService) failFastScanPersistence(run models.MediaLibraryScanRun, operation serverlog.Operation, started time.Time, stage string, err error) (models.MediaLibraryScanRun, error) {
	finished := time.Now().UTC()
	wrapped := wrapMediaLibraryPersistence(stage, err)
	persistenceStage, databaseErrorClass := mediaLibraryPersistenceDiagnostics(wrapped)
	run.Status, run.Phase, run.ErrorCode, run.PersistenceStage, run.DatabaseErrorClass, run.FinishedAt = "failed", "failed", CodeMediaLibraryScanFailed, persistenceStage, databaseErrorClass, &finished
	_ = s.db.Model(&run).Updates(map[string]any{"status": run.Status, "phase": run.Phase, "error_code": run.ErrorCode, "persistence_stage": persistenceStage, "database_error_class": databaseErrorClass, "finished_at": finished}).Error
	operation.Event(s.log.Error()).Uint("library_id", run.LibraryID).Uint("scan_run_id", run.ID).Uint64("generation", run.Generation).
		Str("scan_kind", run.Kind).Str("phase", run.Phase).Str("error_code", run.ErrorCode).Str("persistence_stage", persistenceStage).
		Str("database_error_class", databaseErrorClass).Int64("duration_ms", time.Since(started).Milliseconds()).Msg(operation.Message("扫描结果提交失败"))
	return run, appError(CodeMediaLibraryScanFailed, "扫描结果提交失败", wrapped)
}

func (s *MediaLibraryService) enqueueMediaLibraryRecognition(library models.MediaLibrary, run models.MediaLibraryScanRun) error {
	if s.queue == nil {
		return errors.New("media library recognition queue is unavailable")
	}
	_, err := s.queue.Enqueue(EnqueueJobInput{System: true, JobType: JobTypeMediaLibraryRecognition, Priority: 20,
		DisplayName: "媒体库后台识别 · " + safeMediaDisplayName(library.Name), Provider: "media_library",
		ResourceKey: mediaArtifactResourceKey(library.ID), CoalescingKey: "generation:" + strconv.FormatUint(run.Generation, 10),
		Payload: mediaLibraryRecognitionJobPayload{LibraryID: library.ID, ScanRunID: run.ID, Generation: run.Generation}})
	return err
}

func (s *MediaLibraryService) recoverMediaLibraryRecognitionJobs() error {
	if s.queue == nil {
		return nil
	}
	var runs []models.MediaLibraryScanRun
	if err := s.db.Where("status = ? AND phase IN ?", "catalog_ready", []string{"recognition_queued", "recognition_running", "recognition_failed", "recognition_enqueue_failed", "recognition_artifact_enqueue_failed"}).Order("id").Find(&runs).Error; err != nil {
		return err
	}
	for _, run := range runs {
		var library models.MediaLibrary
		if err := s.db.First(&library, run.LibraryID).Error; err != nil {
			continue
		}
		if err := s.enqueueMediaLibraryRecognition(library, run); err != nil {
			return err
		}
	}
	return nil
}

func (s *MediaLibraryService) cleanupTerminalMediaLibraryScanStaging(now time.Time) error {
	terminalRuns := s.db.Model(&models.MediaLibraryScanRun{}).Select("id").
		Where("status IN ? AND started_at < ?", []string{"failed", "superseded", "success"}, now.UTC().Add(-mediaLibraryStagingRetention))
	return s.db.Where("run_id IN (?)", terminalRuns).Delete(&models.MediaLibraryScanStaging{}).Error
}

type MediaLibraryRecognitionWorker struct{ service *MediaLibraryService }

func NewMediaLibraryRecognitionWorker(service *MediaLibraryService) *MediaLibraryRecognitionWorker {
	return &MediaLibraryRecognitionWorker{service: service}
}

func (w *MediaLibraryRecognitionWorker) Run(ctx context.Context, runtime JobRuntime, job ClaimedJob) WorkerResult {
	if w == nil || w.service == nil {
		return WorkerResult{ErrorCode: "media_library_recognition_unavailable", ErrorMessage: "媒体库识别服务不可用"}
	}
	var payload mediaLibraryRecognitionJobPayload
	if err := json.Unmarshal([]byte(job.Job.PayloadJSON), &payload); err != nil || payload.LibraryID == 0 || payload.ScanRunID == 0 || payload.Generation == 0 {
		return WorkerResult{ErrorCode: "media_library_recognition_payload_invalid", ErrorMessage: "媒体库识别任务参数无效"}
	}
	if err := w.service.completeFastMediaLibraryRecognition(ctx, runtime, payload); err != nil {
		return WorkerResult{ErrorCode: "media_library_recognition_failed", ErrorMessage: "媒体库后台识别失败"}
	}
	return WorkerResult{}
}

func (s *MediaLibraryService) completeFastMediaLibraryRecognition(ctx context.Context, runtime JobRuntime, payload mediaLibraryRecognitionJobPayload) error {
	started := time.Now()
	var library models.MediaLibrary
	if err := s.db.WithContext(ctx).First(&library, payload.LibraryID).Error; err != nil {
		return err
	}
	var run models.MediaLibraryScanRun
	if err := s.db.WithContext(ctx).First(&run, payload.ScanRunID).Error; err != nil {
		return err
	}
	if library.DirtyGeneration != payload.Generation || run.Generation != payload.Generation {
		finished := time.Now().UTC()
		return s.db.Model(&run).Updates(map[string]any{"status": "superseded", "phase": "superseded", "finished_at": finished}).Error
	}
	var profile models.MediaClassificationProfile
	if err := s.db.WithContext(ctx).First(&profile, library.ProfileID).Error; err != nil {
		return err
	}
	var entries []models.MediaLibraryEntry
	if err := s.db.WithContext(ctx).Where("library_id = ? AND last_generation = ?", library.ID, payload.Generation).Order("relative_path").Find(&entries).Error; err != nil {
		return err
	}
	files := make([]medialibrary.File, 0, len(entries))
	pendingPaths := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		files = append(files, medialibrary.File{RelativePath: entry.RelativePath, ProviderID: entry.ProviderID, ProviderIDStable: true, Size: entry.Size, ModifiedAt: entry.ModifiedAt})
		if entry.MatchStatus == mediaRecognitionStatusPending || entry.RecognitionID == nil {
			pendingPaths[entry.RelativePath] = struct{}{}
		}
	}
	allUnits := medialibrary.GroupRecognitionUnits(files)
	units := make([]medialibrary.RecognitionUnit, 0, len(allUnits))
	currentSourceKeys := make([]string, 0, len(allUnits))
	for _, unit := range allUnits {
		currentSourceKeys = append(currentSourceKeys, unit.SourceKey)
		for _, file := range unit.Files {
			if _, pending := pendingPaths[file.RelativePath]; pending {
				units = append(units, unit)
				break
			}
		}
	}
	_ = s.db.Model(&run).Updates(map[string]any{"phase": "recognition_running", "recognition_total": len(units)}).Error
	total := int64(len(units))
	processed := int64(0)
	progress := float64(0)
	_ = runtime.Heartbeat(&progress, &processed, &total, nil, nil)
	serverlog.OperationMediaRecognition.Event(s.log.Info()).Uint("library_id", library.ID).Uint("scan_run_id", run.ID).Uint64("generation", run.Generation).
		Str("scan_kind", run.Kind).Str("phase", "recognition_running").Int("unit_count", len(units)).Msg(serverlog.OperationMediaRecognition.Message("后台识别开始"))
	recognizedUnits, err := s.recognizeLibraryUnits(ctx, library, profile, units)
	if err != nil {
		_ = s.db.Model(&run).Updates(map[string]any{"phase": "recognition_failed", "error_code": "media_library_recognition_failed"}).Error
		return err
	}

	matched, unrecognized, cacheHits, recognitionFailed := run.Matched, run.Unrecognized, run.CacheHits, run.RecognitionFailed
	for _, recognized := range recognizedUnits {
		if recognized.Result.Status == mediaRecognitionStatusMatched {
			matched++
		} else {
			unrecognized++
			if recognized.Result.ErrorCode != "" {
				recognitionFailed++
			}
		}
		if recognized.CacheHit {
			cacheHits++
		}
		s.log.Debug().Str("operation", serverlog.OperationMediaRecognition.Code).Str("operation_label", serverlog.OperationMediaRecognition.Label).
			Uint("library_id", library.ID).Uint("scan_run_id", run.ID).Uint64("generation", run.Generation).
			Str("scan_kind", run.Kind).Str("phase", "recognition_running").
			Str("media_display_name", safeMediaDisplayName(recognized.Unit.PackageName)).Str("media_type", recognized.Result.MediaType).
			Str("action", "recognition_"+recognized.Result.Status).Msg(serverlog.OperationMediaRecognition.Message("媒体识别结果"))
	}

	finished := time.Now().UTC()
	var committedChange models.MediaLibraryChange
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current models.MediaLibrary
		if err := tx.First(&current, library.ID).Error; err != nil {
			return err
		}
		if current.DirtyGeneration != payload.Generation || current.ProfileID != profile.ID || current.ProfileRevision != profile.Revision {
			return errMediaLibraryConfigurationChanged
		}
		type projection struct {
			ID         uint
			SourceKey  string
			Result     MediaRecognitionResult
			SingleFile bool
		}
		byFile := make(map[string]projection, len(entries))
		now := time.Now().UTC()
		metadataChanged := false
		for _, recognized := range recognizedUnits {
			var record models.MediaLibraryRecognition
			findErr := tx.Where("library_id = ? AND source_key = ?", library.ID, recognized.Unit.SourceKey).First(&record).Error
			existed := findErr == nil
			if errors.Is(findErr, gorm.ErrRecordNotFound) {
				record = models.MediaLibraryRecognition{LibraryID: library.ID, SourceKey: recognized.Unit.SourceKey, CreatedAt: now}
			} else if findErr != nil {
				return findErr
			} else {
				recognized.Result = preservePlayerEpisodeMetadata(recognized.Result, record.MetadataJSON, library.MetadataLanguage)
			}
			metadataJSON, marshalErr := marshalRecognitionMetadata(recognized.Result)
			if marshalErr != nil {
				return marshalErr
			}
			if existed && mediaRecognitionProjectionChanged(record, recognized.Result, metadataJSON, recognized.Manual) {
				metadataChanged = true
			}
			record.InputFingerprint, record.ProfileID, record.ProfileRevision = recognized.Unit.InputFingerprint, profile.ID, profile.Revision
			record.Status, record.ErrorCode, record.MediaType, record.Title = recognized.Result.Status, recognized.Result.ErrorCode, recognized.Result.MediaType, recognized.Result.Title
			record.ReleaseYear, record.TMDBID, record.Confidence = cloneInt(recognized.Result.ReleaseYear), cloneInt64(recognized.Result.TMDBID), cloneFloat64(recognized.Result.Confidence)
			record.CategoryName, record.MatchedRuleID, record.MetadataJSON, record.ManualOverride = recognized.Result.CategoryName, recognized.Result.MatchedRuleID, metadataJSON, recognized.Manual
			record.LastGeneration, record.UpdatedAt = payload.Generation, now
			if err := tx.Save(&record).Error; err != nil {
				return err
			}
			item := projection{ID: record.ID, SourceKey: record.SourceKey, Result: recognized.Result, SingleFile: len(recognized.Unit.Files) == 1}
			for _, file := range recognized.Unit.Files {
				byFile[file.RelativePath] = item
			}
		}
		for index := range entries {
			entry := entries[index]
			item, ok := byFile[entry.RelativePath]
			if !ok {
				continue
			}
			before := entry
			entry.RecognitionID = &item.ID
			recognized := item.Result
			if recognized.MediaType != "" {
				entry.MediaType = recognized.MediaType
			}
			if recognized.Title != "" {
				entry.Title = recognized.Title
			}
			if entry.MediaType == "tv" {
				entry.SeriesTitle = entry.Title
			}
			applyRecognitionEpisodeHints(&entry, recognized, item.SingleFile)
			entry.MatchStatus, entry.RecognitionErrorCode = recognized.Status, recognized.ErrorCode
			entry.WorkKey = recognitionWorkKey(recognized, item.SourceKey)
			entry.CategoryName, entry.MatchedRuleID = recognized.CategoryName, recognized.MatchedRuleID
			entry.TMDBID, entry.ReleaseYear, entry.MatchConfidence = cloneInt64(recognized.TMDBID), cloneInt(recognized.ReleaseYear), cloneFloat64(recognized.Confidence)
			entry.UpdatedAt = now
			if mediaLibraryEntryProjectionChanged(before, entry) {
				metadataChanged = true
			}
			if err := tx.Save(&entry).Error; err != nil {
				return err
			}
		}
		if !run.Partial {
			deleteQuery := tx.Where("library_id = ?", library.ID)
			if len(currentSourceKeys) > 0 {
				deleteQuery = deleteQuery.Where("source_key NOT IN ?", currentSourceKeys)
			}
			if err := deleteQuery.Delete(&models.MediaLibraryRecognition{}).Error; err != nil {
				return err
			}
		}
		if err := reconcileTMDBCollectionsTx(tx, library.ID, run.Partial, now); err != nil {
			return err
		}
		run.Status, run.Phase, run.Matched, run.Unrecognized, run.CacheHits, run.RecognitionFailed = "success", "completed", matched, unrecognized, cacheHits, recognitionFailed
		run.RecognitionCompleted, run.ErrorCode, run.FinishedAt = len(recognizedUnits), "", &finished
		if err := tx.Save(&run).Error; err != nil {
			return err
		}
		if s.changes != nil && metadataChanged {
			change, changeErr := s.changes.RecordTx(tx, library.ID, payload.Generation, models.MediaLibraryChangeMetadata, !mediaLibraryRequiresArtifacts(models.StorageTypePan115, current, s.artifacts != nil))
			if changeErr != nil {
				return changeErr
			}
			committedChange = change
		}
		return nil
	})
	if err != nil {
		return err
	}
	serverlog.OperationMediaRecognition.Event(s.log.Info()).Uint("library_id", library.ID).Uint("scan_run_id", run.ID).Uint64("generation", run.Generation).
		Str("scan_kind", run.Kind).Str("phase", "recognition_running").Str("action", "collections_reconciled").Bool("complete_snapshot", !run.Partial).
		Msg(serverlog.OperationMediaRecognition.Message("识别结果与自动合集已完成入库"))
	if committedChange.State == models.MediaLibraryChangeReady && s.changes != nil {
		s.changes.NotifyCommitted(committedChange.LibraryID, committedChange.Revision)
	}
	processed = int64(len(recognizedUnits))
	progress = 1
	_ = runtime.Heartbeat(&progress, &processed, &total, nil, nil)
	serverlog.OperationMediaRecognition.Event(s.log.Info()).Uint("library_id", library.ID).Uint("scan_run_id", run.ID).Uint64("generation", run.Generation).
		Str("scan_kind", run.Kind).Str("phase", "completed").
		Int("matched", matched).Int("unrecognized", unrecognized).Int("cache_hits", cacheHits).Int("recognition_failed", recognitionFailed).
		Int64("duration_ms", time.Since(started).Milliseconds()).Msg(serverlog.OperationMediaRecognition.Message("后台识别完成"))
	if s.structure != nil && !run.Partial {
		if err := s.structure.EnqueueAutomaticDiagnosis(ctx, library.ID, run.ID, run.Generation, run.Kind); err != nil {
			serverlog.OperationMediaLibraryStructureDiagnosis.Event(s.log.Warn()).Uint("library_id", library.ID).Uint("scan_run_id", run.ID).Uint64("generation", run.Generation).
				Str("scan_kind", run.Kind).Str("phase", "recognition_completed_enqueue_failed").Str("error_code", CodeMediaLibraryStructureDiagnosisFailed).
				Msg(serverlog.OperationMediaLibraryStructureDiagnosis.Message("识别完成后的自动诊断收敛检查失败，媒体目录仍然可用"))
		} else {
			serverlog.OperationMediaLibraryStructureDiagnosis.Event(s.log.Info()).Uint("library_id", library.ID).Uint("scan_run_id", run.ID).Uint64("generation", run.Generation).
				Str("scan_kind", run.Kind).Str("phase", "recognition_completed").Str("action", "automatic_diagnosis_convergence_checked").
				Msg(serverlog.OperationMediaLibraryStructureDiagnosis.Message("识别完成，已检查本次来源版本是否需要自动诊断"))
		}
	}
	if s.artifacts != nil && mediaLibraryArtifactGenerationRequired(run.Kind, run, true) {
		if err := s.artifacts.RefreshGeneration(library.ID, payload.Generation); err != nil {
			run.Status, run.Phase, run.ErrorCode = "catalog_ready", "recognition_artifact_enqueue_failed", "artifact_refresh_schedule_failed"
			_ = s.db.Model(&run).Updates(map[string]any{"status": run.Status, "phase": run.Phase, "error_code": run.ErrorCode, "finished_at": nil}).Error
			serverlog.OperationMediaArtifact.Event(s.log.Error()).Uint("library_id", library.ID).Uint("scan_run_id", run.ID).Uint64("generation", run.Generation).
				Str("scan_kind", run.Kind).Str("phase", run.Phase).Str("error_code", "artifact_refresh_schedule_failed").Msg(serverlog.OperationMediaArtifact.Message("识别后的元数据产物入队失败"))
			return err
		}
	}
	if s.libraryArtwork != nil {
		_ = s.libraryArtwork.ScheduleGeneration(library.ID, !run.Partial)
	}
	return nil
}

func safeMediaDisplayName(value string) string {
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, value)
	value = strings.Join(strings.Fields(value), " ")
	normalized := strings.ReplaceAll(value, "\\", "/")
	if parsed, err := url.Parse(value); err == nil && parsed.Scheme != "" {
		return "未命名媒体"
	}
	if strings.HasPrefix(normalized, "/") || (len(normalized) >= 3 && normalized[1] == ':' && normalized[2] == '/') {
		return "未命名媒体"
	}
	runes := []rune(value)
	if len(runes) > 160 {
		value = string(runes[:160]) + "…"
	}
	if value == "" {
		return "未命名媒体"
	}
	return value
}
