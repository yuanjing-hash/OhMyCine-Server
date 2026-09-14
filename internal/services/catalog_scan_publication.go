package services

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/medialibrary"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

// CatalogScanCommit is the compact publication boundary for logical revisions
// and durable follow-up work. Implementations MUST NOT enumerate media, rebuild
// collections or fan out targets in this transaction. A nil hook is a hard
// rollout gate, never a successful scan with silently missing follow-up work.
type CatalogScanPublication struct {
	Candidate          *models.CatalogSnapshot
	Head               models.CatalogHead
	Run                models.MediaLibraryScanRun
	MetadataChanged    bool
	NoContentChange    bool
	PreserveGeneration bool
	RecognitionOnly    bool
	ArtifactChanges    CatalogArtifactChangeSet
}

type CatalogScanCommit func(*gorm.DB, CatalogScanPublication) error

type catalogScanBaseline struct {
	head                  models.CatalogHead
	entries               []models.MediaLibraryEntry
	recognitions          []models.MediaLibraryRecognition
	assets                []models.MediaLibrarySourceAsset
	protectedRecognitions map[uint]bool
}

func (s *MediaLibraryService) catalogScanVersioned(ctx context.Context, libraryID uint) (bool, error) {
	var head models.CatalogHead
	err := s.db.WithContext(ctx).First(&head, "library_id = ?", libraryID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	switch head.Mode {
	case "legacy":
		return false, nil
	case "versioned":
		if s.catalogStore == nil || s.catalogScanCommit == nil {
			return true, ErrCatalogInvalid
		}
		return true, nil
	case "converting":
		return false, ErrCatalogFence
	default:
		return false, ErrCatalogInvalid
	}
}

func (s *MediaLibraryService) loadCatalogScanBaseline(ctx context.Context, libraryID uint) (catalogScanBaseline, error) {
	var baseline catalogScanBaseline
	err := readStableCatalogPages(func(read func(*gorm.DB, *CatalogReader) error) error {
		return s.withCatalogRead(ctx, []uint{libraryID}, read)
	}, libraryID, func(_ *gorm.DB, reader *CatalogReader) error {
		head, _ := reader.Head(libraryID)
		baseline = catalogScanBaseline{head: head}
		return nil
	}, func(read func(func(*CatalogReader) error) error) error {
		if err := appendCatalogReadPages(read, func(reader *CatalogReader) *gorm.DB { return reader.Entries() }, func(row models.MediaLibraryEntry) uint { return row.ID }, &baseline.entries); err != nil {
			return err
		}
		if err := appendCatalogReadPages(read, func(reader *CatalogReader) *gorm.DB { return reader.Recognitions() }, func(row models.MediaLibraryRecognition) uint { return row.ID }, &baseline.recognitions); err != nil {
			return err
		}
		return appendCatalogReadPages(read, func(reader *CatalogReader) *gorm.DB { return reader.SourceAssets() }, func(row models.MediaLibrarySourceAsset) uint { return row.ID }, &baseline.assets)
	})
	if err != nil {
		return catalogScanBaseline{}, err
	}
	return baseline, err
}

func (s *MediaLibraryService) resolveCatalogScanIDs(ctx context.Context, candidate models.CatalogSnapshot, token string, requests []CatalogIdentityRequest) ([]uint, error) {
	ids := make([]uint, 0, len(requests))
	for start := 0; start < len(requests); start += CatalogBatchRows {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		part, err := s.catalogStore.ResolveIdentities(ctx, candidate.ID, token, requests[start:min(start+CatalogBatchRows, len(requests))])
		if err != nil {
			return nil, err
		}
		ids = append(ids, part...)
	}
	return ids, nil
}

// appendCatalogScanFacts is row- AND byte-bounded. Large recognition snapshots
// cannot accidentally turn one nominal 250-row write into a huge transaction.
func (s *MediaLibraryService) appendCatalogScanFacts(ctx context.Context, candidate models.CatalogSnapshot, token string, facts CatalogFactBatch) error {
	return s.catalogStore.appendCatalogFactBatches(ctx, candidate, token, facts)
}

func (s *CatalogSnapshotStore) appendCatalogFactBatches(ctx context.Context, candidate models.CatalogSnapshot, token string, facts CatalogFactBatch) error {
	batch := CatalogFactBatch{}
	flush := func() error {
		if len(batch.Entries)+len(batch.Recognitions)+len(batch.SourceAssets) == 0 {
			return nil
		}
		if err := s.RenewCandidate(ctx, candidate.ID, token, 5*time.Minute); err != nil {
			return err
		}
		if err := s.AppendBatch(ctx, candidate.ID, token, batch); err != nil {
			return err
		}
		batch = CatalogFactBatch{}
		return nil
	}
	appendOne := func(one CatalogFactBatch) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		bytes := catalogBatchSize(one)
		if bytes > CatalogBatchBytes {
			return ErrCatalogBudget
		}
		if len(batch.Entries)+len(batch.Recognitions)+len(batch.SourceAssets) >= CatalogBatchRows || catalogBatchSize(batch)+bytes > CatalogBatchBytes {
			if err := flush(); err != nil {
				return err
			}
		}
		batch.Entries = append(batch.Entries, one.Entries...)
		batch.Recognitions = append(batch.Recognitions, one.Recognitions...)
		batch.SourceAssets = append(batch.SourceAssets, one.SourceAssets...)
		return nil
	}
	for _, fact := range facts.Recognitions {
		if err := appendOne(CatalogFactBatch{Recognitions: []models.CatalogRecognitionFact{fact}}); err != nil {
			return err
		}
	}
	for _, fact := range facts.Entries {
		if err := appendOne(CatalogFactBatch{Entries: []models.CatalogEntryFact{fact}}); err != nil {
			return err
		}
	}
	for _, fact := range facts.SourceAssets {
		if err := appendOne(CatalogFactBatch{SourceAssets: []models.CatalogSourceAssetFact{fact}}); err != nil {
			return err
		}
	}
	return flush()
}

func catalogScanEntry(file medialibrary.File, record *models.MediaLibraryRecognition, sourceKey string, now time.Time, generation uint64, pending bool) models.MediaLibraryEntry {
	parsed := medialibrary.ParseMedia(filepath.Base(file.RelativePath), file.RelativePath)
	entry := models.MediaLibraryEntry{RelativePath: file.RelativePath, ProviderID: file.ProviderID, Size: file.Size, ModifiedAt: file.ModifiedAt,
		MediaType: parsed.MediaType, Title: parsed.Title, SeriesTitle: parsed.SeriesTitle, Season: parsed.Season, Episode: parsed.Episode,
		MatchStatus: mediaRecognitionStatusPending, WorkKey: "file:" + sourceKey, LastGeneration: generation, CreatedAt: now, UpdatedAt: now}
	if !pending {
		entry.MatchStatus = mediaRecognitionStatusUnrecognized
	}
	if record == nil {
		return entry
	}
	entry.RecognitionID = &record.ID
	if record.MediaType != "" {
		entry.MediaType = record.MediaType
	}
	if record.Title != "" {
		entry.Title = record.Title
	}
	if entry.MediaType == "tv" {
		entry.SeriesTitle = entry.Title
	}
	entry.MatchStatus, entry.RecognitionErrorCode = record.Status, record.ErrorCode
	entry.CategoryName, entry.MatchedRuleID = record.CategoryName, record.MatchedRuleID
	entry.TMDBID, entry.ReleaseYear, entry.MatchConfidence = cloneInt64(record.TMDBID), cloneInt(record.ReleaseYear), cloneFloat64(record.Confidence)
	entry.WorkKey = recognitionWorkKey(MediaRecognitionResult{Status: record.Status, MediaType: record.MediaType, Title: record.Title, TMDBID: record.TMDBID}, sourceKey)
	return entry
}

func (s *MediaLibraryService) commitCatalogScan(ctx context.Context, candidate models.CatalogSnapshot, token string, library models.MediaLibrary, profile models.MediaClassificationProfile, run *models.MediaLibraryScanRun, metadataChanged bool, artifactChanges CatalogArtifactChangeSet, hook CatalogScanCommit) error {
	if hook == nil {
		return ErrCatalogInvalid
	}
	return s.catalogStore.writeCatalogBatch(ctx, func(tx *gorm.DB) error {
		revision, err := s.catalogStore.PublishTx(tx, candidate.ID, token, candidate.ParentRevision, func(tx *gorm.DB) error {
			return s.validateCatalogScanRunTx(tx, library.ID, *run)
		})
		if err != nil {
			return err
		}
		// Bind durable downstream work to the NEW effective head, still inside
		// the same transaction so a failed hook rolls publication back.
		var head models.CatalogHead
		if err := tx.First(&head, "library_id=?", library.ID).Error; err != nil {
			return err
		}
		candidate.State, candidate.PublishedRevision = "published", revision
		return s.finishCatalogScanTx(tx, profile, run, CatalogScanPublication{Candidate: &candidate, Head: head, MetadataChanged: metadataChanged, ArtifactChanges: artifactChanges}, hook)
	})
}

func (s *MediaLibraryService) validateCatalogScanRunTx(tx *gorm.DB, libraryID uint, run models.MediaLibraryScanRun) error {
	current, err := catalogRebaseScanLibraryTx(tx, libraryID, run)
	if err != nil {
		return err
	}
	if current.DirtyGeneration+1 != run.Generation {
		return ErrCatalogFence
	}
	return nil
}

// The persisted run retains its original source fingerprint and generation
// until publication. A metadata-only dirty bump may choose a newer publication
// generation; it must not launder changed source/configuration into that run.
func catalogRebaseScanLibraryTx(tx *gorm.DB, libraryID uint, run models.MediaLibraryScanRun) (models.MediaLibrary, error) {
	var current models.MediaLibrary
	if err := tx.First(&current, libraryID).Error; err != nil {
		return current, err
	}
	var storage models.Storage
	if err := tx.First(&storage, current.StorageID).Error; err != nil {
		return current, err
	}
	var profile models.MediaClassificationProfile
	if err := tx.First(&profile, current.ProfileID).Error; err != nil {
		return current, err
	}
	var active models.MediaLibraryScanRun
	if err := tx.First(&active, run.ID).Error; err != nil {
		return current, err
	}
	if active.LibraryID != libraryID || active.Generation == 0 || active.SourceFingerprint != run.SourceFingerprint || active.Status != "running" {
		return current, ErrCatalogFence
	}
	// Original scan allocation is original dirty + 1. This substitutes only
	// that metadata counter; every original physical/profile/config field is
	// compared unchanged against persisted enumeration evidence.
	source := current
	source.DirtyGeneration = active.Generation - 1
	if mediaLibraryScanSourceFingerprint(source, storage, profile) != active.SourceFingerprint {
		return current, errMediaLibraryConfigurationChanged
	}
	return current, nil
}

func (s *MediaLibraryService) finishCatalogScanTx(tx *gorm.DB, profile models.MediaClassificationProfile, run *models.MediaLibraryScanRun, publication CatalogScanPublication, hook CatalogScanCommit) error {
	finished := time.Now().UTC()
	run.Status, run.Phase, run.CatalogPublishedAt, run.FinishedAt = "success", "completed", &finished, &finished
	if run.RecognitionTotal > 0 {
		run.Status, run.Phase, run.FinishedAt = "catalog_ready", "recognition_queued", nil
	}
	publication.Run = *run
	libraryUpdates := map[string]any{"last_scan_at": finished, "last_successful_scan_at": finished, "profile_revision": profile.Revision, "reclassification_due": false, "status_error_code": "", "next_retry_at": nil}
	if !publication.PreserveGeneration {
		libraryUpdates["dirty_generation"] = run.Generation
		libraryUpdates["baseline_generation"] = run.Generation
	}
	if err := tx.Model(&models.MediaLibrary{}).Where("id=?", run.LibraryID).Updates(libraryUpdates).Error; err != nil {
		return err
	}
	if err := tx.Save(run).Error; err != nil {
		return err
	}
	// Binding fingerprints include the final logical generation. The hook must
	// see it together with the new head; all of these writes still roll back if
	// binding/outbox persistence fails.
	return hook(tx, publication)
}

func (s *MediaLibraryService) commitNoopCatalogScan(ctx context.Context, head models.CatalogHead, profile models.MediaClassificationProfile, run *models.MediaLibraryScanRun, hook CatalogScanCommit) error {
	if hook == nil {
		return ErrCatalogInvalid
	}
	return s.catalogStore.writeCatalogBatch(ctx, func(tx *gorm.DB) error {
		var current models.CatalogHead
		if err := tx.First(&current, "library_id=?", head.LibraryID).Error; err != nil {
			return err
		}
		if current.Mode != "versioned" || current.Revision != head.Revision || current.SourceEpoch != head.SourceEpoch || current.SourceFingerprint != head.SourceFingerprint || current.ConfigFingerprint != head.ConfigFingerprint {
			return ErrCatalogFence
		}
		if err := s.validateCatalogScanRunTx(tx, head.LibraryID, *run); err != nil {
			return err
		}
		var library models.MediaLibrary
		if err := tx.First(&library, head.LibraryID).Error; err != nil {
			return err
		}
		// The allocated generation is provisional enumeration evidence. A true
		// no-op keeps the published logical generation, so unchanged pending
		// recognition can only replay the exact existing generation key and an
		// artifact run cannot be fabricated for an empty delta.
		run.Generation = library.BaselineGeneration
		var storage models.Storage
		if err := tx.First(&storage, library.StorageID).Error; err != nil {
			return err
		}
		library.DirtyGeneration = library.BaselineGeneration
		run.SourceFingerprint = mediaLibraryScanSourceFingerprint(library, storage, profile)
		return s.finishCatalogScanTx(tx, profile, run, CatalogScanPublication{Head: current, NoContentChange: true, PreserveGeneration: true}, hook)
	})
}

func abandonCatalogScan(store *CatalogSnapshotStore, id, token string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = store.Abandon(ctx, id, token)
}

// publishCatalogScan performs no provider enumeration. A stale automatic build
// may retry against freshly pinned manual results without re-scanning the source.
func (s *MediaLibraryService) publishCatalogScan(ctx context.Context, library models.MediaLibrary, storage models.Storage, profile models.MediaClassificationProfile, run models.MediaLibraryScanRun, input medialibrary.Result, fast bool, hook CatalogScanCommit) (models.MediaLibraryScanRun, error) {
	if s.catalogStore == nil || hook == nil {
		return run, ErrCatalogInvalid
	}
	operation := mediaLibraryScanOperation(run.Kind)
	for attempt := 0; attempt < 3; attempt++ {
		if err := ctx.Err(); err != nil {
			return run, err
		}
		// A full replacement already folds the old chain. Partial/event scans
		// proactively compact at half budget before preparing their next delta.
		if input.Partial {
			if _, err := s.catalogStore.Compact(ctx, CatalogCompactionInput{LibraryID: library.ID}); err != nil {
				if errors.Is(err, ErrCatalogFence) {
					continue
				}
				return run, err
			}
		}
		if err := s.withCatalogReadTx(ctx, func(tx *gorm.DB) error {
			current, err := catalogRebaseScanLibraryTx(tx, library.ID, run)
			if err != nil {
				return err
			}
			library = current
			run.Generation = current.DirtyGeneration + 1
			return nil
		}); err != nil {
			return run, err
		}
		var baseline catalogScanBaseline
		var err error
		if input.Partial {
			baseline, err = s.loadCatalogBatchBaseline(ctx, library.ID, input)
		} else {
			baseline, err = s.loadCatalogScanBaseline(ctx, library.ID)
		}
		if err != nil {
			return run, err
		}
		if err := s.catalogScanProgress(ctx, run.ID, "running", map[string]any{"phase": "preparing", "discovered": len(input.Files), "partial": input.Partial}); err != nil {
			return run, err
		}
		result := input
		result = catalogBatchPathReplacements(result, baseline)
		units := stabilizeRecognitionUnits(medialibrary.GroupRecognitionUnits(result.Files), baseline.entries, baseline.recognitions)
		var recognized []mediaLibraryRecognizedUnit
		if !fast {
			recognized, err = s.recognizeLibraryUnitsWithExisting(ctx, library, profile, units, baseline.recognitions)
			if err != nil {
				return run, err
			}
		}
		kind := "base"
		if result.Partial {
			kind = "delta"
		}
		candidate, token, err := s.catalogStore.BeginCandidate(ctx, CatalogCandidateInput{LibraryID: library.ID, Kind: kind, ExpectedRevision: baseline.head.Revision,
			SourceEpoch: baseline.head.SourceEpoch, SourceFingerprint: baseline.head.SourceFingerprint, ConfigFingerprint: baseline.head.ConfigFingerprint, LeaseDuration: 5 * time.Minute})
		if err != nil {
			if errors.Is(err, ErrCatalogFence) {
				continue
			}
			return run, err
		}
		operation.Event(s.log.Info()).Uint("library_id", library.ID).Uint("scan_run_id", run.ID).Str("phase", "preparing").Str("catalog_kind", kind).Int("attempt", attempt+1).Msg(operation.Message("开始分批构建隐藏目录候选"))
		preparedRun := run
		preparedRun.Added, preparedRun.Updated, preparedRun.Removed = 0, 0, 0
		preparedRun.Matched, preparedRun.Unrecognized, preparedRun.CacheHits, preparedRun.RecognitionFailed = 0, 0, 0, 0
		preparedRun.RecognitionTotal = 0
		preparedRun.Discovered, preparedRun.Enumerated, preparedRun.Processed = len(result.Files), max(input.Enumerated, len(input.Files)+len(input.Assets)+input.Deduplicated), len(result.Files)+len(result.Assets)
		preparedRun.Deduplicated, preparedRun.Partial = input.Deduplicated, result.Partial
		facts, metadataChanged, artifactChanges, err := s.prepareCatalogScanFacts(ctx, candidate, token, library, storage, profile, &preparedRun, result, baseline, units, recognized, fast)
		if err == nil && kind == "delta" && preparedRun.Persisted == 0 {
			abandonCatalogScan(s.catalogStore, candidate.ID, token)
			err = s.commitNoopCatalogScan(ctx, baseline.head, profile, &preparedRun, hook)
			if errors.Is(err, ErrCatalogFence) {
				continue
			}
			return preparedRun, err
		}
		if err == nil {
			err = s.appendCatalogScanFacts(ctx, candidate, token, facts)
		}
		if err == nil {
			err = s.catalogScanProgress(ctx, run.ID, "running", map[string]any{"phase": "validating", "persisted": preparedRun.Persisted})
		}
		if err == nil {
			err = s.catalogStore.Seal(ctx, candidate.ID, token)
		}
		if err == nil {
			operation.Event(s.log.Info()).Uint("library_id", library.ID).Uint("scan_run_id", run.ID).Str("phase", "publishing").Int("persisted", preparedRun.Persisted).Msg(operation.Message("目录候选验证完成，准备短事务切换"))
			err = s.commitCatalogScan(ctx, candidate, token, library, profile, &preparedRun, metadataChanged, artifactChanges, hook)
		}
		if err == nil {
			operation.Event(s.log.Info()).Uint("library_id", library.ID).Uint("scan_run_id", run.ID).Str("phase", preparedRun.Phase).Int("added", preparedRun.Added).Int("updated", preparedRun.Updated).Int("removed", preparedRun.Removed).Int("recognition_total", preparedRun.RecognitionTotal).Msg(operation.Message("新目录已原子发布，后续工作由持久记录接续"))
			return preparedRun, nil
		}
		abandonCatalogScan(s.catalogStore, candidate.ID, token)
		if !errors.Is(err, ErrCatalogFence) {
			return run, err
		}
		operation.Event(s.log.Info()).Uint("library_id", library.ID).Uint("scan_run_id", run.ID).Str("phase", "rebasing").Int("attempt", attempt+1).Msg(operation.Message("目录修订已变化，保留枚举结果并重新合并人工识别"))
	}
	return run, ErrCatalogFence
}

func (s *MediaLibraryService) catalogScanProgress(ctx context.Context, runID uint, status string, updates map[string]any) error {
	return s.catalogStore.writeCatalogBatch(ctx, func(tx *gorm.DB) error {
		return tx.Model(&models.MediaLibraryScanRun{}).Where("id=? AND status=?", runID, status).Updates(updates).Error
	})
}

func (s *MediaLibraryService) prepareCatalogScanFacts(ctx context.Context, candidate models.CatalogSnapshot, token string, library models.MediaLibrary, storage models.Storage, profile models.MediaClassificationProfile, run *models.MediaLibraryScanRun, result medialibrary.Result, baseline catalogScanBaseline, units []medialibrary.RecognitionUnit, recognized []mediaLibraryRecognizedUnit, fast bool) (CatalogFactBatch, bool, CatalogArtifactChangeSet, error) {
	facts := CatalogFactBatch{}
	artifactChanges := CatalogArtifactChangeSet{}
	now := time.Now().UTC()
	metadataChanged := false
	bySource := make(map[string]models.MediaLibraryRecognition, len(baseline.recognitions))
	oldRecognitions := make(map[uint]models.MediaLibraryRecognition, len(baseline.recognitions))
	for _, record := range baseline.recognitions {
		bySource[record.SourceKey], oldRecognitions[record.ID] = record, record
	}
	selected := make(map[string]models.MediaLibraryRecognition, len(units))
	unitByPath := make(map[string]medialibrary.RecognitionUnit, len(result.Files))
	recognizedBySource := make(map[string]mediaLibraryRecognizedUnit, len(recognized))
	for _, item := range recognized {
		recognizedBySource[item.Unit.SourceKey] = item
	}
	for _, unit := range units {
		for _, file := range unit.Files {
			unitByPath[file.RelativePath] = unit
		}
	}
	if fast {
		for _, unit := range units {
			record, exists := bySource[unit.SourceKey]
			reusable := exists && (record.ManualOverride || (record.Status != mediaRecognitionStatusPending && record.ProfileID == profile.ID && record.ProfileRevision == profile.Revision && record.InputFingerprint == unit.InputFingerprint && mediaLibraryRecognitionProjectionFresh(record)))
			if !reusable {
				run.RecognitionTotal++
				continue
			}
			selected[unit.SourceKey] = record
			run.CacheHits++
			if record.Status == mediaRecognitionStatusMatched {
				run.Matched++
			} else {
				run.Unrecognized++
			}
		}
	} else {
		requests := make([]CatalogIdentityRequest, 0, len(recognized))
		for _, item := range recognized {
			requests = append(requests, CatalogIdentityRequest{Kind: "recognition", SourceKey: item.Unit.SourceKey, ExistingID: bySource[item.Unit.SourceKey].ID})
		}
		ids, err := s.resolveCatalogScanIDs(ctx, candidate, token, requests)
		if err != nil {
			return facts, false, artifactChanges, err
		}
		for index, item := range recognized {
			record := bySource[item.Unit.SourceKey]
			item.Result = preservePlayerEpisodeMetadata(item.Result, record.MetadataJSON, library.MetadataLanguage)
			metadata, err := marshalRecognitionMetadata(item.Result)
			if err != nil {
				return facts, false, artifactChanges, err
			}
			if record.ID != 0 && mediaRecognitionProjectionChanged(record, item.Result, metadata, item.Manual) {
				metadataChanged = true
			}
			if record.CreatedAt.IsZero() {
				record.CreatedAt = now
			}
			record.ID, record.LibraryID, record.SourceKey, record.InputFingerprint = ids[index], library.ID, item.Unit.SourceKey, item.Unit.InputFingerprint
			record.ProfileID, record.ProfileRevision = profile.ID, profile.Revision
			record.Status, record.ErrorCode, record.MediaType, record.Title = item.Result.Status, item.Result.ErrorCode, item.Result.MediaType, item.Result.Title
			record.ReleaseYear, record.TMDBID, record.Confidence = cloneInt(item.Result.ReleaseYear), cloneInt64(item.Result.TMDBID), cloneFloat64(item.Result.Confidence)
			record.CategoryName, record.MatchedRuleID = item.Result.CategoryName, item.Result.MatchedRuleID
			record.MetadataJSON, record.ManualOverride, record.LastGeneration, record.UpdatedAt = metadata, item.Manual, run.Generation, now
			selected[item.Unit.SourceKey] = record
			if item.CacheHit {
				run.CacheHits++
			}
			if item.Result.Status == mediaRecognitionStatusMatched {
				run.Matched++
			} else {
				run.Unrecognized++
				if item.Result.ErrorCode != "" {
					run.RecognitionFailed++
				}
			}
		}
	}
	byPath, byProvider := make(map[string]models.MediaLibraryEntry), make(map[string]models.MediaLibraryEntry)
	for _, entry := range baseline.entries {
		byPath[entry.RelativePath] = entry
		if storage.Type != models.StorageTypeLocal && entry.ProviderID != "" {
			byProvider[entry.ProviderID] = entry
		}
	}
	deleted := make(map[string]bool, len(result.DeletedProviderIDs))
	for _, providerID := range result.DeletedProviderIDs {
		deleted[providerID] = true
	}
	entryRows := make([]models.MediaLibraryEntry, 0, len(result.Files))
	entryRecords := make([]*models.MediaLibraryRecognition, 0, len(result.Files))
	entryChanged := make([]bool, 0, len(result.Files))
	entryBeforeRecognition := make([]*uint, 0, len(result.Files))
	requests := make([]CatalogIdentityRequest, 0, len(result.Files))
	seenEntries := make(map[uint]bool)
	survivingRecognitions := make(map[uint]bool)
	for id := range baseline.protectedRecognitions {
		survivingRecognitions[id] = true
	}
	for _, file := range result.Files {
		if err := ctx.Err(); err != nil {
			return facts, false, artifactChanges, err
		}
		old, exists := byPath[file.RelativePath]
		// A stable remote provider identity outranks a recycled path. Otherwise
		// moving B onto A's old pathname would silently steal A's history ID.
		if exists && storage.Type != models.StorageTypeLocal && file.ProviderIDStable && old.ProviderID != file.ProviderID {
			old, exists = models.MediaLibraryEntry{}, false
		}
		if exists && deleted[old.ProviderID] {
			old, exists = models.MediaLibraryEntry{}, false
		}
		if !exists && storage.Type != models.StorageTypeLocal && file.ProviderIDStable {
			old, exists = byProvider[file.ProviderID]
			if deleted[old.ProviderID] {
				old, exists = models.MediaLibraryEntry{}, false
			}
		}
		unit := unitByPath[file.RelativePath]
		var record *models.MediaLibraryRecognition
		if chosen, ok := selected[unit.SourceKey]; ok {
			copy := chosen
			record = &copy
		}
		entry := catalogScanEntry(file, record, unit.SourceKey, now, run.Generation, fast)
		entry.ID, entry.LibraryID = old.ID, library.ID
		if exists {
			entry.CreatedAt = old.CreatedAt
			seenEntries[old.ID] = true
		}
		if record != nil {
			survivingRecognitions[record.ID] = true
		}
		if !fast && record != nil {
			if item, ok := recognizedBySource[unit.SourceKey]; ok {
				applyRecognitionEpisodeHints(&entry, item.Result, len(unit.Files) == 1)
			}
		}
		changed := !exists || old.RelativePath != entry.RelativePath || old.ProviderID != entry.ProviderID || old.Size != entry.Size || !old.ModifiedAt.Equal(entry.ModifiedAt) || !sameOptional(old.RecognitionID, entry.RecognitionID) || mediaLibraryEntryProjectionChanged(old, entry)
		if !exists {
			run.Added++
		} else if changed {
			run.Updated++
		}
		action := "unchanged"
		if !exists {
			action = "added"
		} else if changed {
			action = "updated"
		}
		logFastScanMediaAction(s.log, mediaLibraryScanOperation(run.Kind), *run, "preparing", action, entry.Title, entry.MediaType)
		if candidate.Kind == "delta" && !changed {
			continue
		}
		entryRows = append(entryRows, entry)
		entryRecords = append(entryRecords, record)
		// STRM bytes follow the stable Entry identity, but an opaque provider
		// rebind still has to update the managed manifest. Recognition identity
		// changes also retire/rebind the exact old/new metadata families. Pure
		// size/mtime changes intentionally remain outside the artifact workset.
		entryChanged = append(entryChanged, !exists || old.RelativePath != entry.RelativePath || old.ProviderID != entry.ProviderID || !sameOptional(old.RecognitionID, entry.RecognitionID))
		entryBeforeRecognition = append(entryBeforeRecognition, cloneUint(old.RecognitionID))
		requests = append(requests, CatalogIdentityRequest{Kind: "entry", SourceKey: file.RelativePath, ExistingID: old.ID, ProviderID: file.ProviderID})
	}
	ids, err := s.resolveCatalogScanIDs(ctx, candidate, token, requests)
	if err != nil {
		return facts, false, artifactChanges, err
	}
	for i, entry := range entryRows {
		entry.ID = ids[i]
		facts.Entries = append(facts.Entries, CatalogEntryFromLegacy(entry, entryRecords[i]))
		if entryChanged[i] {
			artifactChanges.Entries = append(artifactChanges.Entries, entry.ID)
			if entryBeforeRecognition[i] != nil {
				artifactChanges.Recognitions = append(artifactChanges.Recognitions, *entryBeforeRecognition[i])
			}
			if entry.RecognitionID != nil {
				artifactChanges.Recognitions = append(artifactChanges.Recognitions, *entry.RecognitionID)
			}
		}
	}
	for _, old := range baseline.entries {
		if seenEntries[old.ID] {
			continue
		}
		if !result.Partial || deleted[old.ProviderID] {
			run.Removed++
			logFastScanMediaAction(s.log, mediaLibraryScanOperation(run.Kind), *run, "preparing", "removed", old.Title, old.MediaType)
			if candidate.Kind == "delta" {
				fact := models.CatalogEntryFact{MediaLibraryEntry: old, Tombstone: true}
				facts.Entries = append(facts.Entries, fact)
			}
			artifactChanges.Entries = append(artifactChanges.Entries, old.ID)
			if old.RecognitionID != nil {
				artifactChanges.Recognitions = append(artifactChanges.Recognitions, *old.RecognitionID)
			}
		} else if old.RecognitionID != nil {
			survivingRecognitions[*old.RecognitionID] = true
		}
	}
	for _, record := range selected {
		if !survivingRecognitions[record.ID] {
			continue
		}
		before := oldRecognitions[record.ID]
		changed := before.ID == 0 || mediaLibraryRecognitionArtifactChanged(before, record)
		before.LastGeneration, before.UpdatedAt = record.LastGeneration, record.UpdatedAt
		if candidate.Kind == "base" || !reflect.DeepEqual(before, record) {
			facts.Recognitions = append(facts.Recognitions, CatalogRecognitionFromLegacy(record))
		}
		if changed {
			artifactChanges.Recognitions = append(artifactChanges.Recognitions, record.ID)
		}
	}
	if (candidate.Kind == "base" && !result.Partial) || result.Scoped {
		for _, record := range baseline.recognitions {
			if !survivingRecognitions[record.ID] {
				if candidate.Kind == "delta" {
					fact := CatalogRecognitionFromLegacy(record)
					fact.Tombstone = true
					facts.Recognitions = append(facts.Recognitions, fact)
				}
				artifactChanges.Recognitions = append(artifactChanges.Recognitions, record.ID)
			}
		}
	}
	assetFacts, assetChanges, err := s.prepareCatalogScanAssets(ctx, candidate, token, library, storage, run, result, baseline.assets, deleted, now)
	if err != nil {
		return facts, false, artifactChanges, err
	}
	facts.SourceAssets = assetFacts
	artifactChanges.SourceAssets = append(artifactChanges.SourceAssets, assetChanges...)
	run.Persisted = len(facts.Entries) + len(facts.Recognitions) + len(facts.SourceAssets)
	if candidate.Kind == "delta" && (run.Persisted > CatalogMaxDeltaRows || catalogBatchSize(facts) > CatalogMaxDeltaBytes) {
		return facts, false, artifactChanges, ErrCatalogBudget
	}
	return facts, metadataChanged, normalizeCatalogArtifactChanges(artifactChanges), nil
}

func (s *MediaLibraryService) prepareCatalogScanAssets(ctx context.Context, candidate models.CatalogSnapshot, token string, library models.MediaLibrary, storage models.Storage, run *models.MediaLibraryScanRun, result medialibrary.Result, existing []models.MediaLibrarySourceAsset, deleted map[string]bool, now time.Time) ([]models.CatalogSourceAssetFact, []uint, error) {
	byPath, byProvider := make(map[string]models.MediaLibrarySourceAsset), make(map[string]models.MediaLibrarySourceAsset)
	for _, asset := range existing {
		byPath[asset.RelativePath] = asset
		if storage.Type != models.StorageTypeLocal && asset.ProviderID != "" {
			byProvider[asset.ProviderID] = asset
		}
	}
	rows := make([]models.MediaLibrarySourceAsset, 0, len(result.Assets))
	changedRows := make([]bool, 0, len(result.Assets))
	requests := make([]CatalogIdentityRequest, 0, len(result.Assets))
	seen := make(map[uint]bool)
	for _, source := range result.Assets {
		old, exists := byPath[source.RelativePath]
		if exists && storage.Type != models.StorageTypeLocal && source.ProviderID != "" && old.ProviderID != source.ProviderID {
			old, exists = models.MediaLibrarySourceAsset{}, false
		}
		if exists && deleted[old.ProviderID] {
			old, exists = models.MediaLibrarySourceAsset{}, false
		}
		if !exists && storage.Type != models.StorageTypeLocal && source.ProviderID != "" {
			old, exists = byProvider[source.ProviderID]
			if deleted[old.ProviderID] {
				old, exists = models.MediaLibrarySourceAsset{}, false
			}
		}
		asset := models.MediaLibrarySourceAsset{ID: old.ID, LibraryID: library.ID, RelativePath: source.RelativePath, Generation: run.Generation, ProviderID: source.ProviderID, ParentProviderID: source.ParentProviderID, Name: source.Name, Extension: source.Extension, Size: source.Size, ModifiedAt: source.ModifiedAt, HashHint: source.HashHint, Active: true, CreatedAt: now, UpdatedAt: now}
		if exists {
			asset.CreatedAt = old.CreatedAt
			seen[old.ID] = true
		}
		before := old
		before.Generation, before.UpdatedAt = asset.Generation, asset.UpdatedAt
		if candidate.Kind == "delta" && exists && reflect.DeepEqual(before, asset) {
			continue
		}
		rows = append(rows, asset)
		changedRows = append(changedRows, !exists || !reflect.DeepEqual(before, asset))
		requests = append(requests, CatalogIdentityRequest{Kind: "asset", SourceKey: asset.RelativePath, ExistingID: old.ID, ProviderID: source.ProviderID})
	}
	ids, err := s.resolveCatalogScanIDs(ctx, candidate, token, requests)
	if err != nil {
		return nil, nil, err
	}
	facts := make([]models.CatalogSourceAssetFact, 0, len(rows))
	changes := make([]uint, 0, len(rows))
	for i, row := range rows {
		row.ID = ids[i]
		facts = append(facts, models.CatalogSourceAssetFact{MediaLibrarySourceAsset: row})
		if changedRows[i] {
			changes = append(changes, row.ID)
		}
	}
	if candidate.Kind == "delta" {
		for _, old := range existing {
			if !seen[old.ID] && deleted[old.ProviderID] {
				facts = append(facts, models.CatalogSourceAssetFact{MediaLibrarySourceAsset: old, Tombstone: true})
				changes = append(changes, old.ID)
			}
		}
	} else if !result.Partial {
		for _, old := range existing {
			if !seen[old.ID] {
				changes = append(changes, old.ID)
			}
		}
	}
	return facts, changes, nil
}

func cloneUint(value *uint) *uint {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func mediaLibraryRecognitionArtifactChanged(before, after models.MediaLibraryRecognition) bool {
	return before.Status != after.Status || before.MediaType != after.MediaType || before.Title != after.Title ||
		!sameOptional(before.ReleaseYear, after.ReleaseYear) || !sameOptional(before.TMDBID, after.TMDBID) ||
		before.MetadataJSON != after.MetadataJSON
}
