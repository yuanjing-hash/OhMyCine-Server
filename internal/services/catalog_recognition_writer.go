package services

import (
	"context"
	"errors"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

// Preparation and sealing never hold the scan lock or an immediate writer.
// Callers own semantic CAS (input/manual/profile) and notification readiness;
// this helper owns only the bounded immutable recognition publication.
func (s *MediaLibraryService) publishCatalogRecognitionDelta(ctx context.Context, head models.CatalogHead, records []models.MediaLibraryRecognition, validate func(*gorm.DB, *CatalogReader) error, commit func(*gorm.DB) error) error {
	if s.catalogStore == nil || head.Mode != "versioned" || validate == nil || commit == nil {
		return ErrCatalogInvalid
	}
	if len(records) == 0 || len(records) > CatalogMaxDeltaRows {
		return ErrCatalogBudget
	}
	batches := make([]CatalogFactBatch, 0)
	batch := CatalogFactBatch{}
	var totalBytes, batchBytes int64
	seen := make(map[uint]struct{}, len(records))
	for _, record := range records {
		if record.ID == 0 || record.LibraryID != head.LibraryID {
			return ErrCatalogInvalid
		}
		if _, exists := seen[record.ID]; exists {
			return ErrCatalogInvalid
		}
		seen[record.ID] = struct{}{}
		fact := CatalogRecognitionFromLegacy(record)
		rowBytes := catalogBatchSize(CatalogFactBatch{Recognitions: []models.CatalogRecognitionFact{fact}})
		totalBytes += rowBytes
		if rowBytes > CatalogBatchBytes || totalBytes > CatalogMaxDeltaBytes {
			return ErrCatalogBudget
		}
		if len(batch.Recognitions) >= CatalogBatchRows || batchBytes+rowBytes > CatalogBatchBytes {
			batches = append(batches, batch)
			batch = CatalogFactBatch{}
			batchBytes = 0
		}
		batch.Recognitions = append(batch.Recognitions, fact)
		batchBytes += rowBytes
	}
	if len(batch.Recognitions) > 0 {
		batches = append(batches, batch)
	}
	candidate, token, err := s.catalogStore.BeginCandidate(ctx, CatalogCandidateInput{LibraryID: head.LibraryID, Kind: "delta", ExpectedRevision: head.Revision, SourceEpoch: head.SourceEpoch, SourceFingerprint: head.SourceFingerprint, ConfigFingerprint: head.ConfigFingerprint, LeaseDuration: time.Minute})
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = s.catalogStore.Abandon(cleanup, candidate.ID, token)
		}
	}()
	for _, batch := range batches {
		if err := s.catalogStore.AppendBatch(ctx, candidate.ID, token, batch); err != nil {
			return err
		}
	}
	if err := s.catalogStore.Seal(ctx, candidate.ID, token); err != nil {
		return err
	}
	err = s.catalogStore.Admission().WithForeground(ctx, func() error {
		return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			_, err := s.catalogStore.PublishTx(tx, candidate.ID, token, head.Revision, func(tx *gorm.DB) error {
				reader, err := PinCatalogTx(tx, []uint{head.LibraryID})
				if err != nil {
					return err
				}
				current, _ := reader.Head(head.LibraryID)
				if current.Mode != head.Mode || current.Revision != head.Revision || current.SourceEpoch != head.SourceEpoch || current.SourceFingerprint != head.SourceFingerprint || current.ConfigFingerprint != head.ConfigFingerprint {
					return ErrCatalogFence
				}
				if err := validate(tx, reader); err != nil {
					return err
				}
				return nil
			})
			if err != nil {
				return err
			}
			// Bind downstream work only after the new head exists, in this same
			// transaction. Any downstream failure rolls back the publication too.
			return commit(tx)
		})
	})
	committed = err == nil
	return err
}

type catalogRecognitionContext struct {
	Context           context.Context
	Library           models.MediaLibrary
	Storage           models.Storage
	Profile           models.MediaClassificationProfile
	Head              models.CatalogHead
	SourceFingerprint string
	WorkKey           string
	RecognitionIDs    []uint
}

func catalogRecognitionContextTx(ctx context.Context, tx *gorm.DB, reader *CatalogReader, libraryID uint) (catalogRecognitionContext, error) {
	source := catalogRecognitionContext{Context: ctx}
	if err := tx.First(&source.Library, libraryID).Error; err != nil {
		return source, mediaLibraryNotFound(err)
	}
	if err := tx.First(&source.Storage, source.Library.StorageID).Error; err != nil {
		return source, err
	}
	if err := tx.First(&source.Profile, source.Library.ProfileID).Error; err != nil {
		return source, err
	}
	source.Head, _ = reader.Head(libraryID)
	source.SourceFingerprint = mediaLibraryScanSourceFingerprint(source.Library, source.Storage, source.Profile)
	return source, nil
}

func (s *MediaLibraryService) captureRecognitionWriteContext(libraryID uint) (catalogRecognitionContext, error) {
	var source catalogRecognitionContext
	err := s.withCatalogRead(context.Background(), []uint{libraryID}, func(tx *gorm.DB, reader *CatalogReader) error {
		var err error
		source, err = catalogRecognitionContextTx(context.Background(), tx, reader, libraryID)
		return err
	})
	return source, err
}

func validateCatalogRecognitionContext(tx *gorm.DB, reader *CatalogReader, source catalogRecognitionContext, records []models.MediaLibraryRecognition) error {
	current, err := catalogRecognitionContextTx(source.Context, tx, reader, source.Library.ID)
	if err != nil {
		return err
	}
	if current.SourceFingerprint != source.SourceFingerprint || current.Library.ProfileID != source.Library.ProfileID || current.Library.ProfileRevision != source.Library.ProfileRevision || current.Profile.Revision != source.Profile.Revision || current.Profile.RulesJSON != source.Profile.RulesJSON || current.Library.Enabled != source.Library.Enabled || current.Storage.Enabled != source.Storage.Enabled || current.Head.Mode != source.Head.Mode || current.Head.Revision != source.Head.Revision || current.Head.SourceEpoch != source.Head.SourceEpoch || current.Head.SourceFingerprint != source.Head.SourceFingerprint || current.Head.ConfigFingerprint != source.Head.ConfigFingerprint {
		return ErrCatalogFence
	}
	// Versioned membership was captured in the same pinned read as source.Head.
	// The exact revision/source/config comparison above proves those immutable
	// facts have not changed. Re-reading decorated E here would scan the library
	// under SQLite's writer for a single-work edit. Legacy facts remain mutable
	// without a head revision, so they still need the membership recheck.
	if source.WorkKey != "" && source.Head.Mode != "versioned" {
		var ids []uint
		if err := reader.Entries().Where("library_id = ? AND work_key = ? AND recognition_id IS NOT NULL", source.Library.ID, source.WorkKey).Distinct().Order("recognition_id").Limit(len(source.RecognitionIDs)+1).Pluck("recognition_id", &ids).Error; err != nil {
			return err
		}
		if len(ids) != len(source.RecognitionIDs) {
			return ErrCatalogFence
		}
		for i := range ids {
			if ids[i] != source.RecognitionIDs[i] {
				return ErrCatalogFence
			}
		}
	}
	for offset := 0; offset < len(records); offset += CatalogBatchRows {
		expectedBatch := records[offset:min(offset+CatalogBatchRows, len(records))]
		ids := make([]uint, 0, len(expectedBatch))
		for _, expected := range expectedBatch {
			ids = append(ids, expected.ID)
		}
		var currentRecords []models.MediaLibraryRecognition
		if err := reader.Recognitions().Where("library_id = ? AND id IN ?", source.Library.ID, ids).Find(&currentRecords).Error; err != nil {
			return err
		}
		byID := make(map[uint]models.MediaLibraryRecognition, len(currentRecords))
		for _, record := range currentRecords {
			byID[record.ID] = record
		}
		for _, expected := range expectedBatch {
			record, exists := byID[expected.ID]
			if !exists || record.SourceKey != expected.SourceKey || record.InputFingerprint != expected.InputFingerprint || record.ManualOverride != expected.ManualOverride || record.MetadataJSON != expected.MetadataJSON || record.ProfileID != expected.ProfileID || record.ProfileRevision != expected.ProfileRevision || record.LastGeneration != expected.LastGeneration || !record.UpdatedAt.Equal(expected.UpdatedAt) {
				return ErrCatalogFence
			}
		}
	}
	return nil
}

func recognitionWriteError(err error) error {
	if errors.Is(err, ErrCatalogFence) || errors.Is(err, ErrCatalogBudget) {
		return appError(CodeConflict, "媒体目录已变化或正在合并，请稍后重新加载再保存", err)
	}
	return err
}

func (s *MediaLibraryService) persistVersionedRecognitionResults(source catalogRecognitionContext, updates []catalogMetadataResult, manual bool) error {
	requiresArtifacts := mediaLibraryRequiresArtifacts(source.Storage.Type, source.Library, s.artifacts != nil)
	before := make([]models.MediaLibraryRecognition, 0, len(updates))
	after := make([]models.MediaLibraryRecognition, 0, len(updates))
	now := time.Now().UTC()
	for _, update := range updates {
		if update.Record.LibraryID != source.Library.ID || update.Profile.ID != source.Library.ProfileID || update.Profile.Revision != source.Library.ProfileRevision {
			return recognitionWriteError(ErrCatalogFence)
		}
		metadataJSON, err := marshalRecognitionMetadata(update.Result)
		if err != nil {
			return err
		}
		record, result := update.Record, update.Result
		before = append(before, record)
		record.ProfileID, record.ProfileRevision = update.Profile.ID, update.Profile.Revision
		record.Status, record.ErrorCode, record.MediaType, record.Title = result.Status, result.ErrorCode, result.MediaType, result.Title
		record.ReleaseYear, record.TMDBID, record.Confidence = cloneInt(result.ReleaseYear), cloneInt64(result.TMDBID), cloneFloat64(result.Confidence)
		record.CategoryName, record.MatchedRuleID = result.CategoryName, result.MatchedRuleID
		record.MetadataJSON, record.ManualOverride, record.UpdatedAt = metadataJSON, manual, now
		after = append(after, record)
	}
	var change models.MediaLibraryChange
	var artifactGeneration uint64
	err := s.publishCatalogRecognitionDelta(source.Context, source.Head, after, func(tx *gorm.DB, reader *CatalogReader) error {
		return validateCatalogRecognitionContext(tx, reader, source, before)
	}, func(tx *gorm.DB) error {
		generation := source.Library.BaselineGeneration
		if requiresArtifacts {
			artifactGeneration = max(max(source.Library.DirtyGeneration, source.Library.ArtifactGeneration), source.Library.BaselineGeneration) + 1
			generation = artifactGeneration
			if err := tx.Model(&models.MediaLibrary{}).Where("id = ?", source.Library.ID).Updates(map[string]any{"dirty_generation": generation, "updated_at": now}).Error; err != nil {
				return err
			}
			if _, err := s.artifacts.BindCatalogGenerationTx(tx, source.Library.ID, generation); err != nil {
				return err
			}
		}
		if s.changes == nil {
			return nil
		}
		var err error
		change, err = s.changes.RecordTx(tx, source.Library.ID, generation, models.MediaLibraryChangeMetadata, !requiresArtifacts)
		return err
	})
	if err != nil {
		return recognitionWriteError(err)
	}
	if s.changes != nil && change.Revision > 0 {
		s.changes.NotifyCommitted(change.LibraryID, change.Revision)
	}
	if artifactGeneration > 0 {
		if err := s.artifacts.ScheduleGeneration(source.Library.ID, artifactGeneration); err != nil {
			// The recognition and pending exact-snapshot binding already committed.
			// Queue/filesystem preparation is recoverable post-commit work; reporting
			// this as a failed save would invite duplicate/manual overwrite retries.
			s.log.Warn().Uint("library_id", source.Library.ID).Uint64("generation", artifactGeneration).Str("error_code", "artifact_schedule_pending").Msg("识别结果已保存，媒体产物任务等待恢复调度")
		}
	}
	return nil
}
